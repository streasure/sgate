package gateway

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/protobuf/proto"
)

// Connection 连接结构体
// 功能: 封装连接的所有信息，替代map[string]interface{}
// 优化: 为支持百万级并发，减少内存占用
//   id: 连接唯一标识
//   UserUUID: 用户UUID
//   Conn: 底层网络连接
//   RemoteAddr: 远程地址
//   CreatedAt: 创建时间戳
//   LastActive: 最后活跃时间
//   Status: 连接状态 (0=active, 1=closing, 2=closed)

type Connection struct {
	id         string
	UserUUID   string
	ServerID   string
	Conn       gnet.Conn
	RemoteAddr string
	CreatedAt  int64
	LastActive int64
	Status     int8
	Groups     map[string]struct{}
	IsWS       bool
	mu         sync.Mutex
}

func (c *Connection) ID() string {
	return c.id
}

// SetWS 标记连接为 WebSocket（线程安全）
func (c *Connection) SetWS(v bool) {
	c.mu.Lock()
	c.IsWS = v
	c.mu.Unlock()
}

// IsWebSocket 返回是否为 WebSocket 连接（线程安全）
func (c *Connection) IsWebSocket() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.IsWS
}

// noopAsyncCallback 空回调，用于 AsyncWrite/AsyncWritev
// 注意: gnet 的 Write/Writev 不是并发安全的，只能用于 event-loop 内
// 从 goroutine（如 receiveMessages）调用必须使用 AsyncWrite/AsyncWritev
func noopAsyncCallback(_ gnet.Conn, _ error) error { return nil }

func (c *Connection) Send(data []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.LastActive = time.Now().UnixMilli()
	if c.IsWS {
		return c.sendWSFrame(data)
	}
	// AsyncWritev 异步发送，buf 所有权转移给 event-loop，必须为独立分配
	// data 来自 proto.Marshal 是独立分配；header 需独立分配（不能用栈数组）
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	return c.Conn.AsyncWritev([][]byte{header, data}, noopAsyncCallback)
}

// SendMulti 在单次 AsyncWrite 中发送多条已组帧的消息，大幅减少 event-loop 入队次数和内存分配。
// combined 必须包含 [4字节长度][payload] 重复格式，且必须是独立分配（所有权转移给 event-loop）。
func (c *Connection) SendMulti(combined []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.LastActive = time.Now().UnixMilli()
	if c.IsWS {
		// WebSocket 不支持批量，逐帧发送
		// 注：此路径在 burst 压测中不会触发（客户端为 TCP 连接）
		return c.Conn.AsyncWrite(combined, noopAsyncCallback)
	}
	return c.Conn.AsyncWrite(combined, noopAsyncCallback)
}

func (c *Connection) sendWSFrame(data []byte) error {
	payloadLen := len(data)
	var frame []byte
	if payloadLen < 126 {
		frame = make([]byte, 0, 2+payloadLen)
		frame = append(frame, 0x82, byte(payloadLen))
	} else if payloadLen <= 65535 {
		frame = make([]byte, 0, 4+payloadLen)
		frame = append(frame, 0x82, 126)
		frame = append(frame, byte(payloadLen>>8), byte(payloadLen))
	} else {
		frame = make([]byte, 0, 10+payloadLen)
		frame = append(frame, 0x82, 127)
		for i := 7; i >= 0; i-- {
			frame = append(frame, byte(uint64(payloadLen)>>(uint(i)*8)))
		}
	}
	frame = append(frame, data...)
	// frame 是独立分配，可直接用于 AsyncWrite
	return c.Conn.AsyncWrite(frame, noopAsyncCallback)
}

func (c *Connection) Close() error {
	return c.Conn.Close()
}

// ConnectionManager 连接管理器
// 功能: 管理所有网络连接，包括添加、移除、获取连接，以及广播消息
// 字段:
//   connections: 本地连接映射，使用 sync.Map 实现，key为连接ID，value为Connection结构体
//   userConnections: 用户连接映射，使用 sync.Map 实现，key为用户UUID，value为连接ID（单一登录模式）
//   groups: 推送组映射，使用 sync.Map 实现，key为组ID，value为用户UUID集合
//   count: 连接数量，使用原子操作进行更新，确保并发安全
//   connectionStats: 连接统计信息，使用原子操作进行更新

type serverUserKey struct {
	serverID string
	userUUID string
}

type GroupInfo struct {
	Name    string
	Members map[serverUserKey]struct{}
}

type ConnectionManager struct {
	connections           sync.Map
	userConnections       sync.Map
	serverUserConnections sync.Map
	serverConnections     sync.Map
	groups                sync.Map
	groupMutex            sync.RWMutex
	count                 int32
	stopCh                chan struct{}
	totalConnections      atomic.Int64
	activeConnections     atomic.Int64
	closedConnections     atomic.Int64
	connectionTimeouts    atomic.Int64
	connectionErrors      atomic.Int64
	totalConnectionTime   atomic.Int64
	totalMessages         atomic.Int64
	failedMessages        atomic.Int64
	totalMessageLatency   atomic.Int64
}

// connectionPool 连接对象池
// 功能: 复用Connection结构体，减少内存分配
var connectionPool = sync.Pool{
	New: func() interface{} {
		return &Connection{}
	},
}

// getConnection 从对象池获取Connection
// 参数:
//
//	connectionID: 连接ID
//	conn: 底层网络连接
//	userUUID: 用户UUID
//	remoteAddr: 远程地址
//	localAddr: 本地地址
//	status: 连接状态
//
// 返回值:
//
//	*Connection: Connection结构体
func getConnection(connectionID string, conn gnet.Conn, userUUID, remoteAddr string) *Connection {
	c := connectionPool.Get().(*Connection)
	c.id = connectionID
	c.UserUUID = userUUID
	c.Conn = conn
	c.RemoteAddr = remoteAddr
	c.CreatedAt = time.Now().UnixMilli()
	c.Status = 0
	c.LastActive = time.Now().UnixMilli()
	return c
}

// putConnection 归还Connection到对象池
func putConnection(c *Connection) {
	c.id = ""
	c.UserUUID = ""
	c.Conn = nil
	c.RemoteAddr = ""
	c.CreatedAt = 0
	c.Status = 0
	c.LastActive = 0
	c.Groups = nil
	c.ServerID = ""
	c.IsWS = false
	connectionPool.Put(c)
}

// NewConnectionManager 创建连接管理器实例
// 返回值:
//
//	*ConnectionManager: 连接管理器实例
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{}
}

// AddConnection 添加连接
// 功能: 将新的网络连接添加到连接管理器中，并生成唯一的连接ID
// 单一登录模式: 如果用户已有连接，会踢掉旧连接
// 参数:
//
//	conn: 网络连接
//	userUUID: 用户UUID
//
// 返回值:
//
//	string: 连接ID
func (cm *ConnectionManager) AddConnection(conn gnet.Conn, userUUID string) string {
	connectionID := generateConnectionID()

	remoteAddr := ""
	if conn != nil && conn.RemoteAddr() != nil {
		remoteAddr = conn.RemoteAddr().String()
	}
	connection := getConnection(connectionID, conn, userUUID, remoteAddr)

	// 存储连接信息到本地
	cm.connections.Store(connectionID, connection)
	atomic.AddInt32(&cm.count, 1)

	cm.totalConnections.Add(1)
	cm.activeConnections.Add(1)

	// 单一登录模式: 如果用户已有连接，踢掉旧连接
	if userUUID != "" {
		if oldConnectionID, ok := cm.userConnections.Load(userUUID); ok {
			// 用户已有连接，踢掉旧连接
			oldConnID := oldConnectionID.(string)
			if oldConnID != connectionID {
				tlog.Info("用户已有连接，踢掉旧连接", "userUUID", userUUID, "oldConnectionID", oldConnID, "newConnectionID", connectionID)
				cm.kickConnection(oldConnID, "new_login", "您的账号在其他地方登录")
			}
		}
		// 更新用户连接映射为新连接
		cm.userConnections.Store(userUUID, connectionID)
	}

	// 输出调试日志
	tlog.Debug("连接已添加", "connectionID", connectionID, "userUUID", userUUID, "count", int(atomic.LoadInt32(&cm.count)))

	return connectionID
}

// kickConnection 踢掉指定连接
// 功能: 向指定连接发送下线通知并断开连接
// 参数:
//
//	connectionID: 要踢掉的连接ID
//	reason: 踢人原因
//	message: 踢人消息
func (cm *ConnectionManager) kickConnection(connectionID string, reason string, message string) {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		return
	}
	if conn.Conn == nil {
		cm.RemoveConnection(connectionID)
		return
	}

	// 发送下线通知
	kickMessage := &protobuf.Message{
		Route: protobuf.RouteServerKick,
		Payload: map[string]string{
			"reason":  reason,
			"message": message,
		},
	}

	// 序列化消息
	responseData, err := proto.Marshal(kickMessage)
	if err != nil {
		tlog.Error("序列化踢人消息失败", "error", err)
	} else {
		conn.Send(responseData)
	}

	// 关闭连接
	if conn.Conn != nil {
		conn.Conn.Close()
	}

	// 从连接管理器中移除
	cm.RemoveConnection(connectionID)

	tlog.Info("连接已被踢掉", "connectionID", connectionID, "reason", reason)
}

// RemoveConnection 移除连接
// 功能: 从连接管理器中移除指定的连接，并更新连接计数
// 参数:
//
//	connectionID: 连接ID
func (cm *ConnectionManager) RemoveConnection(connectionID string) {
	conn, exists := cm.connections.LoadAndDelete(connectionID)
	if exists {
		atomic.AddInt32(&cm.count, -1)

		connection := conn.(*Connection)
		userUUID := connection.UserUUID
		serverID := connection.ServerID

		if connection.Groups != nil {
			cm.groupMutex.Lock()
			key := serverUserKey{serverID: serverID, userUUID: userUUID}
			for groupID := range connection.Groups {
				if groupInfo, ok := cm.groups.Load(groupID); ok {
					info := groupInfo.(*GroupInfo)
					delete(info.Members, key)
					if len(info.Members) == 0 {
						cm.groups.Delete(groupID)
					}
				}
			}
			cm.groupMutex.Unlock()
		}

		connectionTime := time.Now().UnixMilli() - connection.CreatedAt

		cm.activeConnections.Add(-1)
		cm.closedConnections.Add(1)
		cm.totalConnectionTime.Add(connectionTime)

		cm.removeFromServerIndex(serverID, userUUID)

		putConnection(connection)

		// 从用户连接映射中移除该连接ID（单一登录模式）
		if userUUID != "" {
			if currentConnID, ok := cm.userConnections.Load(userUUID); ok {
				// 只有当当前存储的连接ID等于要移除的连接ID时才删除
				if currentConnID.(string) == connectionID {
					cm.userConnections.Delete(userUUID)
				}
			}
		}
	}

	// 输出调试日志
	if exists {
		tlog.Debug("连接已移除", "connectionID", connectionID, "count", int(atomic.LoadInt32(&cm.count)))
	}
}

// GetConnection 获取连接
// 功能: 根据连接ID获取对应的Connection结构体
// 参数:
//
//	connectionID: 连接ID
//
// 返回值:
//
//	*Connection: Connection结构体，如果连接不存在则返回nil
func (cm *ConnectionManager) GetConnection(connectionID string) *Connection {
	if conn, ok := cm.connections.Load(connectionID); ok {
		return conn.(*Connection)
	}
	return nil
}

func (cm *ConnectionManager) SetConnectionServerID(connectionID, serverID string) {
	if conn, ok := cm.connections.Load(connectionID); ok {
		c := conn.(*Connection)
		oldServerID := c.ServerID
		userUUID := c.UserUUID

		if oldServerID != "" && userUUID != "" {
			cm.RemoveUserFromGroup("server:"+oldServerID, oldServerID, userUUID)
		}

		cm.removeFromServerIndex(oldServerID, userUUID)

		c.ServerID = serverID

		cm.addToServerIndex(c)

		if serverID != "" && userUUID != "" {
			cm.AddUserToGroup("server:"+serverID, serverID, userUUID)
			tlog.Info("connection auto-joined server group", "connectionID", connectionID, "serverID", serverID)
		}
	}
}

// GetConnectionByUserUUID 根据用户UUID获取连接
// 功能: 根据用户UUID获取对应的Connection结构体（单一登录模式）
// 参数:
//
//	userUUID: 用户UUID
//
// 返回值:
//
//	*Connection: Connection结构体，如果连接不存在则返回nil
func (cm *ConnectionManager) GetConnectionByUserUUID(userUUID string) *Connection {
	if connectionID, ok := cm.userConnections.Load(userUUID); ok {
		return cm.GetConnection(connectionID.(string))
	}
	return nil
}

// UpdateConnectionUserUUID 更新连接的用户UUID
// 功能: 更新指定连接的用户UUID（单一登录模式：如果新用户已有连接，会踢掉旧连接）
// 参数:
//
//	connectionID: 连接ID
//	newUserUUID: 新的用户UUID
func (cm *ConnectionManager) UpdateConnectionUserUUID(connectionID string, newUserUUID string) {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		tlog.Warn("连接不存在", "connectionID", connectionID)
		return
	}

	oldUserUUID := conn.UserUUID

	if oldUserUUID == newUserUUID {
		return
	}

	serverID := conn.ServerID

	cm.removeFromServerIndex(serverID, oldUserUUID)

	if oldUserUUID != "" {
		if currentConnID, ok := cm.userConnections.Load(oldUserUUID); ok {
			if currentConnID.(string) == connectionID {
				cm.userConnections.Delete(oldUserUUID)
			}
		}
	}

	// 单一登录模式: 如果新用户已有连接，踢掉旧连接
	if newUserUUID != "" {
		if oldConnectionID, ok := cm.userConnections.Load(newUserUUID); ok {
			oldConnID := oldConnectionID.(string)
			if oldConnID != connectionID {
				tlog.Info("用户已有连接，踢掉旧连接", "userUUID", newUserUUID, "oldConnectionID", oldConnID, "newConnectionID", connectionID)
				cm.kickConnection(oldConnID, "user_changed", "您的账号已切换到其他设备")
			}
		}
		// 更新用户连接映射
		cm.userConnections.Store(newUserUUID, connectionID)
	}

	// 更新Connection的UserUUID
	conn.UserUUID = newUserUUID

	cm.addToServerIndex(conn)

	tlog.Debug("连接用户UUID已更新", "connectionID", connectionID, "oldUserUUID", oldUserUUID, "newUserUUID", newUserUUID)
}

func (cm *ConnectionManager) addToServerIndex(conn *Connection) {
	serverID := conn.ServerID
	userUUID := conn.UserUUID
	if serverID == "" || userUUID == "" {
		return
	}

	key := serverUserKey{serverID: serverID, userUUID: userUUID}
	cm.serverUserConnections.Store(key, conn)

	for {
		val, loaded := cm.serverConnections.LoadOrStore(serverID, &sync.Map{})
		userMap := val.(*sync.Map)
		userMap.Store(userUUID, conn)
		if loaded {
			return
		}
		return
	}
}

func (cm *ConnectionManager) removeFromServerIndex(serverID, userUUID string) {
	if serverID == "" || userUUID == "" {
		return
	}

	key := serverUserKey{serverID: serverID, userUUID: userUUID}
	cm.serverUserConnections.Delete(key)

	if val, ok := cm.serverConnections.Load(serverID); ok {
		userMap := val.(*sync.Map)
		userMap.Delete(userUUID)
		empty := true
		userMap.Range(func(_, _ interface{}) bool {
			empty = false
			return false
		})
		if empty {
			cm.serverConnections.Delete(serverID)
		}
	}
}

func (cm *ConnectionManager) GetConnectionByServerUser(serverID, userUUID string) *Connection {
	key := serverUserKey{serverID: serverID, userUUID: userUUID}
	if conn, ok := cm.serverUserConnections.Load(key); ok {
		return conn.(*Connection)
	}
	return nil
}

func (cm *ConnectionManager) GetConnectionsByServerID(serverID string) []*Connection {
	val, ok := cm.serverConnections.Load(serverID)
	if !ok {
		return nil
	}
	userMap := val.(*sync.Map)
	var conns []*Connection
	userMap.Range(func(_, v interface{}) bool {
		if c, ok := v.(*Connection); ok && c != nil {
			conns = append(conns, c)
		}
		return true
	})
	return conns
}

func (cm *ConnectionManager) GetConnectionCountByServerID(serverID string) int {
	val, ok := cm.serverConnections.Load(serverID)
	if !ok {
		return 0
	}
	count := 0
	val.(*sync.Map).Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

func (cm *ConnectionManager) SendToServerUser(serverID, userUUID string, message interface{}) bool {
	conn := cm.GetConnectionByServerUser(serverID, userUUID)
	if conn == nil {
		return false
	}
	return cm.SendToConnection(conn.id, message)
}

func (cm *ConnectionManager) SendToServer(serverID string, message interface{}) int {
	conns := cm.GetConnectionsByServerID(serverID)
	if len(conns) == 0 {
		return 0
	}
	success := 0
	for _, conn := range conns {
		if cm.SendToConnection(conn.id, message) {
			success++
		}
	}
	return success
}

// SendToConnection 发送消息到指定连接
// 功能: 向指定的连接发送消息，并处理连接关闭的情况
// 参数:
//
//	connectionID: 连接ID
//	message: 消息内容
//
// 返回值:
//
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToConnection(connectionID string, message interface{}) bool {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		cm.totalMessages.Add(1)
		cm.failedMessages.Add(1)
		return false
	}

	if conn.Status == 2 {
		cm.RemoveConnection(connectionID)
		cm.totalMessages.Add(1)
		cm.failedMessages.Add(1)
		return false
	}

	var responseData []byte
	var err error

	switch msg := message.(type) {
	case []byte:
		responseData = msg
	case *protobuf.Message:
		responseData, err = proto.Marshal(msg)
	case *protobuf.ErrorResponse:
		responseData, err = proto.Marshal(msg)
	case map[string]string:
		protoMsg := &protobuf.Message{
			Route:   "message",
			Payload: msg,
		}
		responseData, err = proto.Marshal(protoMsg)
	default:
		cm.totalMessages.Add(1)
		cm.failedMessages.Add(1)
		return false
	}

	if err != nil {
		cm.totalMessages.Add(1)
		cm.failedMessages.Add(1)
		return false
	}

	if err := conn.Send(responseData); err != nil {
		cm.RemoveConnection(connectionID)
		cm.totalMessages.Add(1)
		cm.failedMessages.Add(1)
		return false
	}

	cm.totalMessages.Add(1)
	conn.LastActive = time.Now().UnixMilli()

	return true
}

// Broadcast 广播消息
// 功能: 向所有连接广播消息，使用并发方式提高性能
// 参数:
//
//	message: 消息内容
func (cm *ConnectionManager) Broadcast(message interface{}) {
	// 先序列化消息，减少锁的持有时间
	var responseData []byte
	var marshalErr error

	// 处理不同类型的消息
	switch msg := message.(type) {
	case *protobuf.Message:
		// 使用Protocol Buffers序列化
		responseData, marshalErr = proto.Marshal(msg)
	case *protobuf.ErrorResponse:
		// 使用Protocol Buffers序列化错误消息
		responseData, marshalErr = proto.Marshal(msg)
	case map[string]string:
		// 如果是map[string]string，直接创建Protocol Buffers消息
		protoMsg := &protobuf.Message{
			Route:   "broadcast",
			Payload: msg,
		}
		responseData, marshalErr = proto.Marshal(protoMsg)
	case string:
		protoMsg := &protobuf.Message{
			Route: "broadcast",
			Payload: map[string]string{
				"data": msg,
			},
		}
		responseData, marshalErr = proto.Marshal(protoMsg)
	default:
		// 不支持的消息类型
		tlog.Error("不支持的消息类型", "type", message)
		return
	}

	if marshalErr != nil {
		// 输出错误日志
		tlog.Error("序列化广播消息失败", "error", marshalErr)
		return
	}

	// 预先分配切片，减少内存分配
	var connections []*Connection
	cm.connections.Range(func(key, value interface{}) bool {
		connections = append(connections, value.(*Connection))
		return true
	})

	if len(connections) == 0 {
		return
	}

	// 使用并发发送消息
	successCount := int32(0) // 成功发送计数
	errorCount := int32(0)   // 失败发送计数
	var wg sync.WaitGroup

	// 动态调整并发数，根据连接数自动调整
	concurrencyLimit := 200
	if len(connections) < 1000 {
		concurrencyLimit = 50
	} else if len(connections) > 10000 {
		concurrencyLimit = 500
	}

	semaphore := make(chan struct{}, concurrencyLimit)

	for _, c := range connections {
		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(conn *Connection) {
			defer func() {
				wg.Done()
				<-semaphore // 释放信号量
			}()

			if _, err := conn.Conn.Write(responseData); err != nil {
				// 输出错误日志
				tlog.Error("广播消息失败", "error", err)
				atomic.AddInt32(&errorCount, 1)
			} else {
				atomic.AddInt32(&successCount, 1)
				// 更新最后活跃时间
				conn.LastActive = time.Now().UnixMilli()
			}
		}(c)
	}

	// 等待所有发送完成
	wg.Wait()

	// 输出调试日志
	tlog.Debug("广播完成", "success", atomic.LoadInt32(&successCount), "error", atomic.LoadInt32(&errorCount))
}

// GetConnectionCount 获取连接数
// 功能: 获取当前连接管理器中的连接数量
// 返回值:
//
//	int: 连接数
func (cm *ConnectionManager) GetConnectionCount() int {
	return int(atomic.LoadInt32(&cm.count))
}

// CloseAllConnections 关闭所有连接
// 功能: 关闭所有连接并清空连接管理器
func (cm *ConnectionManager) CloseAllConnections() {
	cm.connections.Range(func(key, value interface{}) bool {
		connectionID := key.(string)
		conn := value.(*Connection)
		if err := conn.Conn.Close(); err != nil {
			tlog.Error("关闭连接失败", "connectionID", connectionID, "error", err)
		}
		putConnection(conn)
		cm.connections.Delete(key)
		return true
	})

	cm.serverUserConnections = sync.Map{}
	cm.serverConnections = sync.Map{}
	cm.groups = sync.Map{}

	atomic.StoreInt32(&cm.count, 0)

	tlog.Info("所有连接已关闭")
}

// generateConnectionID 生成连接ID
// 功能: 生成唯一的连接ID，格式为时间戳-随机字符串
// 返回值:
//
//	string: 连接ID
var connIDCounter uint64

func generateConnectionID() string {
	n := atomic.AddUint64(&connIDCounter, 1)
	return fmt.Sprintf("%d%06d", time.Now().UnixMilli(), n%1000000)
}

// CreateGroup 创建推送组
// 功能: 创建一个新的推送组
// 参数:
//
//	groupID: 组ID
//	groupName: 组名称
func (cm *ConnectionManager) CreateGroup(groupID string, groupName string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()

	if _, ok := cm.groups.Load(groupID); ok {
		tlog.Warn("组已存在", "groupID", groupID)
		return
	}

	cm.groups.Store(groupID, &GroupInfo{
		Name:    groupName,
		Members: make(map[serverUserKey]struct{}),
	})
	tlog.Debug("推送组已创建", "groupID", groupID, "groupName", groupName)
}

// DeleteGroup 删除推送组
// 功能: 删除一个推送组
// 参数:
//
//	groupID: 组ID
func (cm *ConnectionManager) DeleteGroup(groupID string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()

	groupInfo, ok := cm.groups.Load(groupID)
	if !ok {
		tlog.Warn("组不存在", "groupID", groupID)
		return
	}

	info := groupInfo.(*GroupInfo)
	for key := range info.Members {
		conn := cm.GetConnectionByServerUser(key.serverID, key.userUUID)
		if conn != nil && conn.Groups != nil {
			delete(conn.Groups, groupID)
		}
	}

	cm.groups.Delete(groupID)
	tlog.Debug("推送组已删除", "groupID", groupID)
}

// AddUserToGroup 添加用户到推送组
// 功能: 将用户添加到指定的推送组
// 参数:
//
//	groupID: 组ID
//	userUUID: 用户UUID
func (cm *ConnectionManager) AddUserToGroup(groupID string, serverID string, userUUID string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()

	key := serverUserKey{serverID: serverID, userUUID: userUUID}

	if groupInfo, ok := cm.groups.Load(groupID); ok {
		groupInfo.(*GroupInfo).Members[key] = struct{}{}
	} else {
		info := &GroupInfo{
			Name:    groupID,
			Members: make(map[serverUserKey]struct{}),
		}
		info.Members[key] = struct{}{}
		cm.groups.Store(groupID, info)
	}

	conn := cm.GetConnectionByServerUser(serverID, userUUID)
	if conn != nil {
		if conn.Groups == nil {
			conn.Groups = make(map[string]struct{})
		}
		conn.Groups[groupID] = struct{}{}
	}
	tlog.Debug("用户已添加到推送组", "groupID", groupID, "serverID", serverID, "userUUID", userUUID)
}

// RemoveUserFromGroup 从推送组中移除用户
// 功能: 将用户从指定的推送组中移除
// 参数:
//
//	groupID: 组ID
//	userUUID: 用户UUID
func (cm *ConnectionManager) RemoveUserFromGroup(groupID string, serverID string, userUUID string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()

	key := serverUserKey{serverID: serverID, userUUID: userUUID}

	if groupInfo, ok := cm.groups.Load(groupID); ok {
		info := groupInfo.(*GroupInfo)
		delete(info.Members, key)
		if len(info.Members) == 0 {
			cm.groups.Delete(groupID)
		}
	}

	conn := cm.GetConnectionByServerUser(serverID, userUUID)
	if conn != nil {
		if conn.Groups != nil {
			delete(conn.Groups, groupID)
		}
	}
	tlog.Debug("用户已从推送组中移除", "groupID", groupID, "serverID", serverID, "userUUID", userUUID)
}

// SendToUser 发送消息到指定用户
// 功能: 向指定用户的连接发送消息（单一登录模式）
// 参数:
//
//	userUUID: 用户UUID
//	message: 消息内容
//
// 返回值:
//
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToUser(userUUID string, message interface{}) bool {
	// 获取用户的连接（单一登录模式）
	connection := cm.GetConnectionByUserUUID(userUUID)
	if connection == nil {
		tlog.Warn("用户不存在或不在线", "userUUID", userUUID)
		return false
	}

	// 发送消息
	return cm.SendToConnection(connection.id, message)
}

// SendToGroup 发送消息到指定推送组
// 功能: 向指定推送组的所有用户发送消息
// 参数:
//
//	groupID: 组ID
//	message: 消息内容
//
// 返回值:
//
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToGroup(groupID string, message interface{}) bool {
	cm.groupMutex.RLock()
	groupInfo, ok := cm.groups.Load(groupID)
	if !ok {
		cm.groupMutex.RUnlock()
		tlog.Warn("组不存在", "groupID", groupID)
		return false
	}

	keys := make([]serverUserKey, 0, len(groupInfo.(*GroupInfo).Members))
	for key := range groupInfo.(*GroupInfo).Members {
		keys = append(keys, key)
	}
	cm.groupMutex.RUnlock()

	success := false
	for _, key := range keys {
		conn := cm.GetConnectionByServerUser(key.serverID, key.userUUID)
		if conn != nil {
			if cm.SendToConnection(conn.id, message) {
				success = true
			}
		}
	}

	return success
}

// GetGroupUsers 获取推送组的所有用户
// 功能: 获取指定推送组的所有用户UUID
// 参数:
//
//	groupID: 组ID
//
// 返回值:
//
//	[]string: 用户UUID列表
func (cm *ConnectionManager) GetGroupUsers(groupID string) []string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()

	groupInfo, ok := cm.groups.Load(groupID)
	if !ok {
		return []string{}
	}

	info := groupInfo.(*GroupInfo)
	userUUIDs := make([]string, 0, len(info.Members))
	for key := range info.Members {
		userUUIDs = append(userUUIDs, key.userUUID)
	}

	return userUUIDs
}

func (cm *ConnectionManager) GetGroupUsersByServer(groupID, serverID string) []string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()

	groupInfo, ok := cm.groups.Load(groupID)
	if !ok {
		return []string{}
	}

	info := groupInfo.(*GroupInfo)
	userUUIDs := make([]string, 0)
	for key := range info.Members {
		if key.serverID == serverID {
			userUUIDs = append(userUUIDs, key.userUUID)
		}
	}

	return userUUIDs
}

func (cm *ConnectionManager) GetGroupName(groupID string) string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()

	if groupInfo, ok := cm.groups.Load(groupID); ok {
		return groupInfo.(*GroupInfo).Name
	}
	return ""
}

func (cm *ConnectionManager) GetGroupMemberCount(groupID string) int {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()

	if groupInfo, ok := cm.groups.Load(groupID); ok {
		return len(groupInfo.(*GroupInfo).Members)
	}
	return 0
}

// UpdateUserConnection 更新连接的用户映射
// 功能: 更新指定连接的用户UUID映射（单一登录模式）
// 参数:
//
//	connectionID: 连接ID
//	oldUserUUID: 旧用户UUID
//	newUserUUID: 新用户UUID
func (cm *ConnectionManager) UpdateUserConnection(connectionID string, oldUserUUID string, newUserUUID string) {
	cm.UpdateConnectionUserUUID(connectionID, newUserUUID)
}

// StartConnectionChecker 启动连接检查器
// 功能: 定期检查不活跃的连接并自动清理
// 参数:
//
//	timeout: 连接超时时间
//	interval: 检查间隔
func (cm *ConnectionManager) StartConnectionChecker(timeout time.Duration, interval time.Duration) {
	cm.stopCh = make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-cm.stopCh:
				return
			case <-ticker.C:
				cm.checkInactiveConnections(timeout)
			}
		}
	}()
}

func (cm *ConnectionManager) StopConnectionChecker() {
	if cm.stopCh != nil {
		select {
		case <-cm.stopCh:
		default:
			close(cm.stopCh)
		}
	}
}

// checkInactiveConnections 检查不活跃的连接
// 功能: 检查所有连接，清理超过超时时间的不活跃连接
// 参数:
//
//	timeout: 连接超时时间
func (cm *ConnectionManager) checkInactiveConnections(timeout time.Duration) {
	now := time.Now().UnixMilli()
	timeoutMs := int64(timeout / time.Millisecond)

	// 收集需要清理的连接ID
	var connectionsToRemove []string

	cm.connections.Range(func(key, value interface{}) bool {
		connectionID := key.(string)
		conn := value.(*Connection)

		// 检查连接是否超过超时时间
		if now-conn.LastActive > timeoutMs {
			connectionsToRemove = append(connectionsToRemove, connectionID)
		}
		return true
	})

	// 清理不活跃的连接
	for _, connectionID := range connectionsToRemove {
		conn := cm.GetConnection(connectionID)
		if conn != nil {
			// 发送超时通知
			timeoutMessage := &protobuf.Message{
				Route: "timeout",
				Payload: map[string]string{
					"reason":  "inactive",
					"message": "Connection timeout due to inactivity",
				},
			}

			// 序列化消息
			responseData, err := proto.Marshal(timeoutMessage)
			if err == nil {
				// 发送超时通知（不检查错误，因为连接可能已关闭）
				conn.Conn.Write(responseData)
			}

			// 关闭连接
			conn.Conn.Close()

			cm.connectionTimeouts.Add(1)

			// 从连接管理器中移除
			cm.RemoveConnection(connectionID)

			tlog.Info("清理不活跃连接", "connectionID", connectionID, "userUUID", conn.UserUUID)
		}
	}
}

// GetConnectionInfo 获取连接信息
// 功能: 获取指定连接的详细信息
// 参数:
//
//	connectionID: 连接ID
//
// GetConnectionStats 获取连接统计信息
// 功能: 获取连接管理器的统计信息
// 返回值:
//
//	map[string]interface{}: 连接统计信息
func (cm *ConnectionManager) GetConnectionStats() map[string]interface{} {
	totalConn := cm.totalConnections.Load()
	closedConn := cm.closedConnections.Load()
	totalConnTime := cm.totalConnectionTime.Load()
	var avgConnTime int64
	if closedConn > 0 {
		avgConnTime = totalConnTime / closedConn
	}

	totalMsg := cm.totalMessages.Load()
	totalMsgLat := cm.totalMessageLatency.Load()
	var avgMsgLat int64
	if totalMsg > 0 {
		avgMsgLat = totalMsgLat / totalMsg
	}

	return map[string]interface{}{
		"totalConnections":    totalConn,
		"activeConnections":   cm.activeConnections.Load(),
		"closedConnections":   closedConn,
		"connectionTimeouts":  cm.connectionTimeouts.Load(),
		"connectionErrors":    cm.connectionErrors.Load(),
		"avgConnectionTime":   avgConnTime,
		"totalConnectionTime": totalConnTime,
		"totalMessages":       totalMsg,
		"failedMessages":      cm.failedMessages.Load(),
		"avgMessageLatency":   avgMsgLat,
		"totalMessageLatency": totalMsgLat,
	}
}
