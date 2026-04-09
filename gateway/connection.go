package gateway

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/gateway/protobuf"
	"google.golang.org/protobuf/proto"
	tlog "github.com/streasure/treasure-slog"
)

// Connection 连接结构体
// 功能: 封装连接的所有信息，替代map[string]interface{}
// 字段:
//   ID: 连接唯一标识
//   UserUUID: 用户UUID，用于用户-连接映射
//   Conn: 底层网络连接
//   RemoteAddr: 远程地址
//   LocalAddr: 本地地址
//   CreatedAt: 创建时间戳
//   Status: 连接状态
//   LastActive: 最后活跃时间

type Connection struct {
	ID         string    // 连接唯一标识
	UserUUID   string    // 用户UUID
	Conn       gnet.Conn // 底层网络连接
	RemoteAddr string    // 远程地址
	LocalAddr  string    // 本地地址
	CreatedAt  int64     // 创建时间戳
	Status     string    // 连接状态
	LastActive int64     // 最后活跃时间
}

// ConnectionManager 连接管理器
// 功能: 管理所有网络连接，包括添加、移除、获取连接，以及广播消息
// 字段:
//   connections: 本地连接映射，使用 sync.Map 实现，key为连接ID，value为Connection结构体
//   userConnections: 用户连接映射，使用 sync.Map 实现，key为用户UUID，value为连接ID（单一登录模式）
//   groups: 推送组映射，使用 sync.Map 实现，key为组ID，value为用户UUID集合
//   count: 连接数量，使用原子操作进行更新，确保并发安全

type ConnectionManager struct {
	connections     sync.Map // 本地连接映射，使用 sync.Map 实现并发安全
	userConnections sync.Map // 用户连接映射，使用 sync.Map 实现，key为用户UUID，value为连接ID（单一登录模式）
	groups          sync.Map // 推送组映射，使用 sync.Map 实现，key为组ID，value为用户UUID集合
	count           int32    // 连接数量，使用原子操作进行更新
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
//	connectionID: 连接ID
//	conn: 底层网络连接
//	userUUID: 用户UUID
//	remoteAddr: 远程地址
//	localAddr: 本地地址
//	status: 连接状态
//
// 返回值:
//	*Connection: Connection结构体
func getConnection(connectionID string, conn gnet.Conn, userUUID, remoteAddr, localAddr, status string) *Connection {
	c := connectionPool.Get().(*Connection)
	c.ID = connectionID
	c.UserUUID = userUUID
	c.Conn = conn
	c.RemoteAddr = remoteAddr
	c.LocalAddr = localAddr
	c.CreatedAt = time.Now().UnixMilli()
	c.Status = status
	c.LastActive = time.Now().UnixMilli()
	return c
}

// putConnection 归还Connection到对象池
// 参数:
//	c: Connection结构体
func putConnection(c *Connection) {
	c.ID = ""
	c.UserUUID = ""
	c.Conn = nil
	c.RemoteAddr = ""
	c.LocalAddr = ""
	c.CreatedAt = 0
	c.Status = ""
	c.LastActive = 0
	connectionPool.Put(c)
}

// NewConnectionManager 创建连接管理器实例
// 返回值:
//	*ConnectionManager: 连接管理器实例
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{}
}

// AddConnection 添加连接
// 功能: 将新的网络连接添加到连接管理器中，并生成唯一的连接ID
// 单一登录模式: 如果用户已有连接，会踢掉旧连接
// 参数:
//	conn: 网络连接
//	userUUID: 用户UUID
//
// 返回值:
//	string: 连接ID
func (cm *ConnectionManager) AddConnection(conn gnet.Conn, userUUID string) string {
	// 生成连接ID
	connectionID := generateConnectionID()

	// 获取远程地址和本地地址
	remoteAddr := ""
	localAddr := ""
	if conn != nil {
		if addr := conn.RemoteAddr(); addr != nil {
			remoteAddr = addr.String()
		}
		if addr := conn.LocalAddr(); addr != nil {
			localAddr = addr.String()
		}
	}

	// 从对象池获取Connection
	connection := getConnection(connectionID, conn, userUUID, remoteAddr, localAddr, "active")

	// 存储连接信息到本地
	cm.connections.Store(connectionID, connection)
	atomic.AddInt32(&cm.count, 1)

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
//	connectionID: 要踢掉的连接ID
//	reason: 踢人原因
//	message: 踢人消息
func (cm *ConnectionManager) kickConnection(connectionID string, reason string, message string) {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		return
	}

	// 发送下线通知
	kickMessage := &protobuf.Message{
		Route: "kick",
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
		// 发送下线通知（不检查错误，因为连接可能已关闭）
		conn.Conn.Write(responseData)
	}

	// 关闭连接
	conn.Conn.Close()

	// 从连接管理器中移除
	cm.RemoveConnection(connectionID)

	tlog.Info("连接已被踢掉", "connectionID", connectionID, "reason", reason)
}

// RemoveConnection 移除连接
// 功能: 从连接管理器中移除指定的连接，并更新连接计数
// 参数:
//	connectionID: 连接ID
func (cm *ConnectionManager) RemoveConnection(connectionID string) {
	conn, exists := cm.connections.LoadAndDelete(connectionID)
	if exists {
		atomic.AddInt32(&cm.count, -1)

		// 获取Connection中的userUUID
		connection := conn.(*Connection)
		userUUID := connection.UserUUID

		// 归还Connection到对象池
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
//	connectionID: 连接ID
//
// 返回值:
//	*Connection: Connection结构体，如果连接不存在则返回nil
func (cm *ConnectionManager) GetConnection(connectionID string) *Connection {
	if conn, ok := cm.connections.Load(connectionID); ok {
		return conn.(*Connection)
	}
	return nil
}

// GetGnetConnection 获取底层gnet连接
// 功能: 根据连接ID获取对应的gnet.Conn
// 参数:
//	connectionID: 连接ID
//
// 返回值:
//	gnet.Conn: 网络连接，如果连接不存在则返回nil
func (cm *ConnectionManager) GetGnetConnection(connectionID string) gnet.Conn {
	if conn := cm.GetConnection(connectionID); conn != nil {
		return conn.Conn
	}
	return nil
}

// GetConnectionByUserUUID 根据用户UUID获取连接
// 功能: 根据用户UUID获取对应的Connection结构体（单一登录模式）
// 参数:
//	userUUID: 用户UUID
//
// 返回值:
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
//	connectionID: 连接ID
//	newUserUUID: 新的用户UUID
func (cm *ConnectionManager) UpdateConnectionUserUUID(connectionID string, newUserUUID string) {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		tlog.Warn("连接不存在", "connectionID", connectionID)
		return
	}

	oldUserUUID := conn.UserUUID

	// 如果新旧用户相同，不需要处理
	if oldUserUUID == newUserUUID {
		return
	}

	// 从旧用户的连接映射中移除
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

	// 输出调试日志
	tlog.Debug("连接用户UUID已更新", "connectionID", connectionID, "oldUserUUID", oldUserUUID, "newUserUUID", newUserUUID)
}

// UpdateConnectionStatus 更新连接状态
// 功能: 更新指定连接的状态
// 参数:
//	connectionID: 连接ID
//	status: 新的状态
func (cm *ConnectionManager) UpdateConnectionStatus(connectionID string, status string) {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		tlog.Warn("连接不存在", "connectionID", connectionID)
		return
	}

	conn.Status = status
	conn.LastActive = time.Now().UnixMilli()

	// 输出调试日志
	tlog.Debug("连接状态已更新", "connectionID", connectionID, "status", status)
}

// SendToConnection 发送消息到指定连接
// 功能: 向指定的连接发送消息，并处理连接关闭的情况
// 参数:
//	connectionID: 连接ID
//	message: 消息内容
//
// 返回值:
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToConnection(connectionID string, message interface{}) bool {
	conn := cm.GetConnection(connectionID)
	if conn == nil {
		// 输出警告日志
		tlog.Warn("连接不存在", "connectionID", connectionID)
		return false
	}

	// 检查连接是否已关闭
	if conn.Status == "closed" {
		// 连接已关闭，从连接管理器中移除
		tlog.Warn("连接已关闭，从连接管理器中移除", "connectionID", connectionID)
		cm.RemoveConnection(connectionID)
		return false
	}

	// 序列化消息
	var responseData []byte
	var err error

	switch msg := message.(type) {
	case *protobuf.Message:
		// 使用Protocol Buffers序列化
		responseData, err = proto.Marshal(msg)
	case *protobuf.ErrorResponse:
		// 使用Protocol Buffers序列化错误消息
		responseData, err = proto.Marshal(msg)
	case map[string]string:
		// 转换为Protocol Buffers消息
		protoMsg := &protobuf.Message{
			Route:   "message",
			Payload: msg,
		}
		responseData, err = proto.Marshal(protoMsg)
	default:
		// 不支持的消息类型
		tlog.Error("不支持的消息类型", "type", message)
		return false
	}

	if err != nil {
		// 输出错误日志
		tlog.Error("序列化消息失败", "error", err)
		return false
	}

	if _, err := conn.Conn.Write(responseData); err != nil {
		// 输出错误日志
		tlog.Error("发送消息失败", "connectionID", connectionID, "error", err)
		// 连接可能已关闭，从连接管理器中移除
		cm.RemoveConnection(connectionID)
		return false
	}

	// 更新最后活跃时间
	conn.LastActive = time.Now().UnixMilli()

	return true
}

// Broadcast 广播消息
// 功能: 向所有连接广播消息，使用并发方式提高性能
// 参数:
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
		// 如果是字符串，包装成Protocol Buffers消息
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
//	int: 连接数
func (cm *ConnectionManager) GetConnectionCount() int {
	return int(atomic.LoadInt32(&cm.count))
}

// CloseAllConnections 关闭所有连接
// 功能: 关闭所有连接并清空连接管理器
func (cm *ConnectionManager) CloseAllConnections() {
	// 遍历所有连接并关闭
	cm.connections.Range(func(key, value interface{}) bool {
		connectionID := key.(string)
		conn := value.(*Connection)
		if err := conn.Conn.Close(); err != nil {
			tlog.Error("关闭连接失败", "connectionID", connectionID, "error", err)
		}
		// 归还Connection到对象池
		putConnection(conn)
		cm.connections.Delete(key)
		return true
	})

	// 重置连接数
	atomic.StoreInt32(&cm.count, 0)

	tlog.Info("所有连接已关闭")
}

// generateConnectionID 生成连接ID
// 功能: 生成唯一的连接ID，格式为时间戳-随机字符串
// 返回值:
//	string: 连接ID
func generateConnectionID() string {
	return time.Now().Format("20060102150405") + "-" + randomString(8)
}

// randomString 生成随机字符串
// 功能: 生成指定长度的随机字符串
// 参数:
//	length: 字符串长度
//
// 返回值:
//	string: 随机字符串
func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[time.Now().UnixNano()%int64(len(charset))]
	}
	return string(result)
}

// CreateGroup 创建推送组
// 功能: 创建一个新的推送组
// 参数:
//	groupID: 组ID
//	groupName: 组名称
func (cm *ConnectionManager) CreateGroup(groupID string, groupName string) {
	// 检查组是否已存在
	if _, ok := cm.groups.Load(groupID); ok {
		tlog.Warn("组已存在", "groupID", groupID)
		return
	}

	// 创建新组
	cm.groups.Store(groupID, map[string]struct{}{})
	// 输出调试日志
	tlog.Debug("推送组已创建", "groupID", groupID, "groupName", groupName)
}

// DeleteGroup 删除推送组
// 功能: 删除一个推送组
// 参数:
//	groupID: 组ID
func (cm *ConnectionManager) DeleteGroup(groupID string) {
	// 检查组是否存在
	if _, ok := cm.groups.Load(groupID); !ok {
		tlog.Warn("组不存在", "groupID", groupID)
		return
	}

	// 删除组
	cm.groups.Delete(groupID)
	// 输出调试日志
	tlog.Debug("推送组已删除", "groupID", groupID)
}

// AddUserToGroup 添加用户到推送组
// 功能: 将用户添加到指定的推送组
// 参数:
//	groupID: 组ID
//	userUUID: 用户UUID
func (cm *ConnectionManager) AddUserToGroup(groupID string, userUUID string) {
	// 检查组是否存在
	if groupUsers, ok := cm.groups.Load(groupID); ok {
		// 添加用户到组
		groupUsers.(map[string]struct{})[userUUID] = struct{}{}
		// 输出调试日志
		tlog.Debug("用户已添加到推送组", "groupID", groupID, "userUUID", userUUID)
	} else {
		tlog.Warn("组不存在", "groupID", groupID)
	}
}

// RemoveUserFromGroup 从推送组中移除用户
// 功能: 将用户从指定的推送组中移除
// 参数:
//	groupID: 组ID
//	userUUID: 用户UUID
func (cm *ConnectionManager) RemoveUserFromGroup(groupID string, userUUID string) {
	// 检查组是否存在
	if groupUsers, ok := cm.groups.Load(groupID); ok {
		// 从组中移除用户
		delete(groupUsers.(map[string]struct{}), userUUID)
		// 输出调试日志
		tlog.Debug("用户已从推送组中移除", "groupID", groupID, "userUUID", userUUID)
	} else {
		tlog.Warn("组不存在", "groupID", groupID)
	}
}

// SendToUser 发送消息到指定用户
// 功能: 向指定用户的连接发送消息（单一登录模式）
// 参数:
//	userUUID: 用户UUID
//	message: 消息内容
//
// 返回值:
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToUser(userUUID string, message interface{}) bool {
	// 获取用户的连接（单一登录模式）
	connection := cm.GetConnectionByUserUUID(userUUID)
	if connection == nil {
		tlog.Warn("用户不存在或不在线", "userUUID", userUUID)
		return false
	}

	// 发送消息
	return cm.SendToConnection(connection.ID, message)
}

// SendToGroup 发送消息到指定推送组
// 功能: 向指定推送组的所有用户发送消息
// 参数:
//	groupID: 组ID
//	message: 消息内容
//
// 返回值:
//	bool: 是否发送成功
func (cm *ConnectionManager) SendToGroup(groupID string, message interface{}) bool {
	// 检查组是否存在
	groupUsers, ok := cm.groups.Load(groupID)
	if !ok {
		tlog.Warn("组不存在", "groupID", groupID)
		return false
	}

	// 向组中的所有用户发送消息
	success := false
	for userUUID := range groupUsers.(map[string]struct{}) {
		if cm.SendToUser(userUUID, message) {
			success = true
		}
	}

	return success
}

// GetUserConnections 获取用户的连接（单一登录模式）
// 功能: 获取指定用户的连接ID（单一登录模式下最多返回一个）
// 参数:
//	userUUID: 用户UUID
//
// 返回值:
//	[]string: 连接ID列表（单一登录模式下最多一个）
func (cm *ConnectionManager) GetUserConnections(userUUID string) []string {
	// 获取用户的连接（单一登录模式）
	if connectionID, ok := cm.userConnections.Load(userUUID); ok {
		return []string{connectionID.(string)}
	}
	return []string{}
}

// GetGroupUsers 获取推送组的所有用户
// 功能: 获取指定推送组的所有用户UUID
// 参数:
//	groupID: 组ID
//
// 返回值:
//	[]string: 用户UUID列表
func (cm *ConnectionManager) GetGroupUsers(groupID string) []string {
	// 检查组是否存在
	groupUsers, ok := cm.groups.Load(groupID)
	if !ok {
		return []string{}
	}

	// 提取用户UUID列表
	userMap := groupUsers.(map[string]struct{})
	userUUIDs := make([]string, 0, len(userMap))
	for userUUID := range userMap {
		userUUIDs = append(userUUIDs, userUUID)
	}

	return userUUIDs
}

// UpdateUserConnection 更新连接的用户映射
// 功能: 更新指定连接的用户UUID映射（单一登录模式）
// 参数:
//	connectionID: 连接ID
//	oldUserUUID: 旧用户UUID
//	newUserUUID: 新用户UUID
func (cm *ConnectionManager) UpdateUserConnection(connectionID string, oldUserUUID string, newUserUUID string) {
	// 使用新的UpdateConnectionUserUUID方法
	cm.UpdateConnectionUserUUID(connectionID, newUserUUID)
}

// StartConnectionChecker 启动连接检查器
// 功能: 定期检查不活跃的连接并自动清理
// 参数:
//	timeout: 连接超时时间
//	interval: 检查间隔
func (cm *ConnectionManager) StartConnectionChecker(timeout time.Duration, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				cm.checkInactiveConnections(timeout)
			}
		}
	}()
}

// checkInactiveConnections 检查不活跃的连接
// 功能: 检查所有连接，清理超过超时时间的不活跃连接
// 参数:
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

			// 从连接管理器中移除
			cm.RemoveConnection(connectionID)

			tlog.Info("清理不活跃连接", "connectionID", connectionID, "userUUID", conn.UserUUID)
		}
	}
}

// GetConnectionInfo 获取连接信息
// 功能: 获取指定连接的详细信息
// 参数:
//	connectionID: 连接ID
//
// 返回值:
//	*Connection: Connection结构体，如果连接不存在则返回nil
func (cm *ConnectionManager) GetConnectionInfo(connectionID string) *Connection {
	return cm.GetConnection(connectionID)
}
