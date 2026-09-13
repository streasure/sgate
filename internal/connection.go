package gateway

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/util/tlog"
)

// ConnState 表示连接状态
type ConnState int32

const (
	StateForward ConnState = 0 // 转发状态，连接正常可转发消息
	StateClosed  ConnState = 1 // 已关闭状态
)

// Connection 表示客户端与网关之间的连接，封装了连接状态、用户信息和发送逻辑。
type Connection struct {
	id         string
	Conn       gnet.Conn
	RemoteAddr string
	CreatedAt  int64
	LastActive int64
	activitySeq atomic.Uint32
	Status     int8
	Groups     map[string]struct{}
	state      atomic.Int32
	groupsMu   sync.RWMutex

	// 原子字段，用于无锁并发访问
	userUUID       atomic.Value // 保存用户唯一标识。
	serverID       atomic.Value // 保存绑定的逻辑服标识。
	isWS           atomic.Bool
	logicClient    atomic.Value // LogicClientProvider 缓存，避免每条消息查询连接池
}

// newConnection 创建新的连接对象，初始化基本属性和状态。
func newConnection(id string, conn gnet.Conn, userUUID, remoteAddr string) *Connection {
	now := time.Now().UnixMilli()
	c := &Connection{
		id:         id,
		Conn:       conn,
		RemoteAddr: remoteAddr,
		CreatedAt:  now,
		LastActive: now,
		Groups:     make(map[string]struct{}),
	}
	c.userUUID.Store(userUUID)
	c.state.Store(int32(StateForward))
	return c
}

// ID 返回连接标识。
func (c *Connection) ID() string { return c.id }

// GetState 原子读取连接状态。
func (c *Connection) GetState() ConnState { return ConnState(c.state.Load()) }
// SetState 原子地设置连接状态，仅当当前状态等于old时才更新为new。
func (c *Connection) SetState(old, new ConnState) bool {
	return c.state.CompareAndSwap(int32(old), int32(new))
}

// IsBound 判断连接是否已绑定到服务器。
func (c *Connection) IsBound() bool {
	return c.serverID.Load().(string) != ""
}

// IsAuthenticated 判断连接是否已完成用户认证（UUID非空且不以temp_开头）。
func (c *Connection) IsAuthenticated() bool {
	uuid := c.userUUID.Load().(string)
	return uuid != "" && !strings.HasPrefix(uuid, "temp_")
}

// IsWebSocket 判断连接是否使用 WebSocket 传输。
func (c *Connection) IsWebSocket() bool { return c.isWS.Load() }

// SetUserUUID 设置连接关联的用户 UUID。
func (c *Connection) SetUserUUID(uuid string) { c.userUUID.Store(uuid) }
func (c *Connection) GetUserUUID() string {
	if v := c.userUUID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// GetCachedLogicClient 获取缓存的LogicClient，避免每次消息都查询连接池。
func (c *Connection) GetCachedLogicClient() LogicClientProvider {
	if v := c.logicClient.Load(); v != nil {
		return v.(LogicClientProvider)
	}
	return nil
}

// SetCachedLogicClient 设置缓存的LogicClient。
func (c *Connection) SetCachedLogicClient(lc LogicClientProvider) {
	c.logicClient.Store(lc)
}

// SetServerID 设置连接关联的服务器 ID。
func (c *Connection) SetServerID(sid string) { c.serverID.Store(sid) }
func (c *Connection) GetServerID() string {
	if v := c.serverID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// SetWS 设置连接是否使用 WebSocket 传输。
func (c *Connection) SetWS(v bool) { c.isWS.Store(v) }

// 头部缓冲区对象池，减少内存分配
var headerPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 4)
		return &buf
	},
}

// noopAsyncCallback 是异步写入完成时使用的空回调。
func noopAsyncCallback(_ gnet.Conn, _ error) error { return nil }

// touch 更新连接的最后活跃时间，每64次调用才实际更新一次以减少原子操作开销。
func (c *Connection) touch() {
	seq := c.activitySeq.Add(1)
	if seq&63 != 0 && atomic.LoadInt64(&c.LastActive) != 0 {
		return
	}
	atomic.StoreInt64(&c.LastActive, time.Now().UnixMilli())
}

// Send 向客户端发送数据，WebSocket连接自动封装为WS帧。
func (c *Connection) Send(data []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	if c.IsWebSocket() {
		return c.sendWSFrame(data)
	}
	headerPtr := headerPool.Get().(*[]byte)
	header := *headerPtr
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	err := c.Conn.AsyncWritev([][]byte{header, data}, noopAsyncCallback)
	// 注意：header在多次调用中复用，但AsyncWritev会拷贝数据
	return err
}

// SendMulti 发送已组装好的合并数据。
func (c *Connection) SendMulti(combined []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	return c.Conn.AsyncWrite(combined, noopAsyncCallback)
}

// SendMultiWithCallback 发送合并数据，写入完成后执行回调函数。
func (c *Connection) SendMultiWithCallback(combined []byte, cb func()) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	if c.IsWebSocket() {
		return c.Conn.AsyncWrite(combined, noopAsyncCallback)
	}
	return c.Conn.AsyncWrite(combined, func(_ gnet.Conn, _ error) error {
		if cb != nil {
			cb()
		}
		return nil
	})
}

// sendWSFrame 将数据封装为WebSocket帧并发送，支持不同长度的载荷。
func (c *Connection) sendWSFrame(data []byte) error {
	payloadLen := len(data)
	var frame []byte
	if payloadLen < 126 {
		frame = make([]byte, 0, 2+payloadLen)
		frame = append(frame, 0x82, byte(payloadLen))
	} else if payloadLen <= 65535 {
		frame = make([]byte, 0, 4+payloadLen)
		frame = append(frame, 0x82, 126, byte(payloadLen>>8), byte(payloadLen))
	} else {
		frame = make([]byte, 0, 10+payloadLen)
		frame = append(frame, 0x82, 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(payloadLen))
		frame = append(frame, b[:]...)
	}
	frame = append(frame, data...)
	return c.Conn.AsyncWrite(frame, noopAsyncCallback)
}

func (c *Connection) Close() error {
	return c.Conn.Close()
}

// serverUserKey 表示服务器-用户联合标识，用于关联连接和分组。
type serverUserKey struct {
	serverID string
	userUUID string
}

// ConnectionGroupInfo 表示连接分组信息，包含分组名称和成员列表。
type ConnectionGroupInfo struct {
	Name    string
	Members map[serverUserKey]struct{}
	mu      sync.RWMutex
}

func (g *ConnectionGroupInfo) AddMember(key serverUserKey) {
	g.mu.Lock()
	g.Members[key] = struct{}{}
	g.mu.Unlock()
}

func (g *ConnectionGroupInfo) RemoveMember(key serverUserKey) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.Members, key)
	return len(g.Members) == 0
}

func (g *ConnectionGroupInfo) HasMember(key serverUserKey) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.Members[key]
	return ok
}

func (g *ConnectionGroupInfo) MemberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.Members)
}

// Snapshot 返回分组成员的快照列表。
func (g *ConnectionGroupInfo) Snapshot() []serverUserKey {
	g.mu.RLock()
	defer g.mu.RUnlock()
	members := make([]serverUserKey, 0, len(g.Members))
	for key := range g.Members {
		members = append(members, key)
	}
	return members
}

// SnapshotUsers 返回分组中所有用户UUID的快照列表。
func (g *ConnectionGroupInfo) SnapshotUsers() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	users := make([]string, 0, len(g.Members))
	for key := range g.Members {
		users = append(users, key.userUUID)
	}
	return users
}

// ConnectionManager 管理所有客户端连接，包括连接的增删改查、分组管理和空闲连接检查。
type ConnectionManager struct {
	connections           sync.Map
	userConnections       sync.Map
	serverUserConnections sync.Map
	serverConnections     sync.Map
	groups                sync.Map
	groupMutex            sync.RWMutex
	count                 int32
	stopCh                chan struct{}
	checkDone             chan struct{}

	totalConnections   atomic.Int64
	activeConnections  atomic.Int64
	closedConnections  atomic.Int64
	connectionTimeouts atomic.Int64
}

var connectionIDCounter atomic.Uint64

var connIDBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 48)
		return &b
	},
}

// generateConnectionID 生成唯一的连接标识符，格式为conn_时间戳_自增ID。
func generateConnectionID() string {
	id := connectionIDCounter.Add(1)
	bufPtr := connIDBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]
	buf = append(buf, "conn_"...)
	buf = strconv.AppendInt(buf, time.Now().UnixNano(), 10)
	buf = append(buf, '_')
	buf = strconv.AppendUint(buf, id, 10)
	result := string(buf)
	*bufPtr = buf
	connIDBufPool.Put(bufPtr)
	return result
}

// NewConnectionManager 创建新的连接管理器实例。
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		stopCh:    make(chan struct{}),
		checkDone: make(chan struct{}),
	}
}

// AddConnection 添加新连接，生成唯一ID并建立映射关系。
func (cm *ConnectionManager) AddConnection(conn gnet.Conn, userUUID string) string {
	connectionID := generateConnectionID()
	remoteAddr := ""
	if conn.RemoteAddr() != nil {
		remoteAddr = conn.RemoteAddr().String()
	}
	c := newConnection(connectionID, conn, userUUID, remoteAddr)
	cm.connections.Store(connectionID, c)
	cm.userConnections.Store(userUUID, connectionID)
	atomic.AddInt32(&cm.count, 1)
	cm.totalConnections.Add(1)
	cm.activeConnections.Add(1)
	return connectionID
}

// GetConnection 根据连接ID获取连接对象。
func (cm *ConnectionManager) GetConnection(connectionID string) *Connection {
	v, ok := cm.connections.Load(connectionID)
	if !ok {
		return nil
	}
	return v.(*Connection)
}

// RemoveConnection 移除连接并清理所有关联的映射关系。
func (cm *ConnectionManager) RemoveConnection(connectionID string) {
	v, ok := cm.connections.Load(connectionID)
	if !ok {
		return
	}
	conn := v.(*Connection)
	cm.connections.Delete(connectionID)
	cm.userConnections.Delete(conn.GetUserUUID())
	serverID := conn.GetServerID()
	cm.serverUserConnections.Delete(serverUserKey{serverID: serverID, userUUID: conn.GetUserUUID()})
	if serverID != "" {
		cm.serverConnections.Delete(serverID)
	}
	atomic.AddInt32(&cm.count, -1)
	cm.activeConnections.Add(-1)
	cm.closedConnections.Add(1)
}

// SetConnectionServerID 设置连接关联的服务器ID。
func (cm *ConnectionManager) SetConnectionServerID(connectionID, serverID string) {
	if conn := cm.GetConnection(connectionID); conn != nil {
		conn.SetServerID(serverID)
		cm.serverConnections.Store(serverID, connectionID)
	}
}

// UpdateConnectionUserUUID 更新连接关联的用户UUID，同步更新所有映射。
func (cm *ConnectionManager) UpdateConnectionUserUUID(connectionID, userUUID string) {
	if conn := cm.GetConnection(connectionID); conn != nil {
		oldUUID := conn.GetUserUUID()
		conn.SetUserUUID(userUUID)
		cm.userConnections.Delete(oldUUID)
		cm.userConnections.Store(userUUID, connectionID)
		cm.serverUserConnections.Store(serverUserKey{serverID: conn.GetServerID(), userUUID: userUUID}, connectionID)
	}
}

// UpdateUserConnection 更新用户的连接映射。
func (cm *ConnectionManager) UpdateUserConnection(connectionID, oldUserUUID, newUserUUID string) {
	cm.UpdateConnectionUserUUID(connectionID, newUserUUID)
}

func (cm *ConnectionManager) GetConnectionCount() int {
	return int(atomic.LoadInt32(&cm.count))
}

// BroadcastBytes 向所有处于转发状态的连接广播数据。
func (cm *ConnectionManager) BroadcastBytes(data []byte) {
	cm.connections.Range(func(key, value interface{}) bool {
		conn := value.(*Connection)
		if conn.GetState() == StateForward {
			conn.Send(data)
		}
		return true
	})
}

// SendToUser 向指定用户发送数据。
func (cm *ConnectionManager) SendToUser(userUUID string, data []byte) {
	v, ok := cm.userConnections.Load(userUUID)
	if !ok {
		return
	}
	connectionID := v.(string)
	if conn := cm.GetConnection(connectionID); conn != nil {
		conn.Send(data)
	}
}

// SendToGroupBytes 向指定分组的所有成员发送数据。
func (cm *ConnectionManager) SendToGroupBytes(groupID string, data []byte) {
	cm.groupMutex.RLock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		cm.groupMutex.RUnlock()
		return
	}
	group := v.(*ConnectionGroupInfo)
	cm.groupMutex.RUnlock()

	members := group.Snapshot()
	for _, key := range members {
		v2, ok := cm.serverUserConnections.Load(key)
		if !ok {
			continue
		}
		connectionID := v2.(string)
		if conn := cm.GetConnection(connectionID); conn != nil {
			conn.Send(data)
		}
	}
}

// AddUserToGroup 将用户添加到指定分组。
func (cm *ConnectionManager) AddUserToGroup(groupID, serverID, userUUID string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()

	v, ok := cm.groups.Load(groupID)
	var group *ConnectionGroupInfo
	if !ok {
		group = &ConnectionGroupInfo{
			Name:    groupID,
			Members: make(map[serverUserKey]struct{}),
		}
		cm.groups.Store(groupID, group)
	} else {
		group = v.(*ConnectionGroupInfo)
	}
	group.AddMember(serverUserKey{serverID: serverID, userUUID: userUUID})
}

// RemoveUserFromGroup 从分组中移除用户，分组为空时自动删除。
func (cm *ConnectionManager) RemoveUserFromGroup(groupID, serverID, userUUID string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()

	v, ok := cm.groups.Load(groupID)
	if !ok {
		return
	}
	group := v.(*ConnectionGroupInfo)
	empty := group.RemoveMember(serverUserKey{serverID: serverID, userUUID: userUUID})
	if empty {
		cm.groups.Delete(groupID)
	}
}

// CreateGroup 创建新的连接分组。
func (cm *ConnectionManager) CreateGroup(groupID, groupName string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()

	if _, ok := cm.groups.Load(groupID); !ok {
		cm.groups.Store(groupID, &ConnectionGroupInfo{
			Name:    groupName,
			Members: make(map[serverUserKey]struct{}),
		})
	}
}

// DeleteGroup 删除指定分组。
func (cm *ConnectionManager) DeleteGroup(groupID string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()
	cm.groups.Delete(groupID)
}

// GetGroupMemberCount 获取指定分组的成员数量。
func (cm *ConnectionManager) GetGroupMemberCount(groupID string) int {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		return 0
	}
	return v.(*ConnectionGroupInfo).MemberCount()
}

// GetGroupName 获取指定分组的名称。
func (cm *ConnectionManager) GetGroupName(groupID string) string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		return ""
	}
	return v.(*ConnectionGroupInfo).Name
}

// GetGroupUsers 获取指定分组的所有用户UUID列表。
func (cm *ConnectionManager) GetGroupUsers(groupID string) []string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		return nil
	}
	return v.(*ConnectionGroupInfo).SnapshotUsers()
}

// GetGroupSessions 获取指定分组的所有会话ID列表。
func (cm *ConnectionManager) GetGroupSessions(groupID string) []string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		return nil
	}
	group := v.(*ConnectionGroupInfo)
	members := group.Snapshot()
	sessions := make([]string, 0, len(members))
	for _, key := range members {
		if value, ok := cm.serverUserConnections.Load(key); ok {
			sessions = append(sessions, value.(string))
		}
	}
	return sessions
}

// StartConnectionChecker 启动空闲连接检查器，定期关闭超时的空闲连接。
func (cm *ConnectionManager) StartConnectionChecker(connIdleTimeout, connCheckInterval time.Duration) {
	go func() {
		defer close(cm.checkDone)
		ticker := time.NewTicker(connCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-cm.stopCh:
				return
			case <-ticker.C:
				cm.checkIdleConnections(connIdleTimeout)
			}
		}
	}()
}

// checkIdleConnections 检查并关闭超时的空闲连接。
func (cm *ConnectionManager) checkIdleConnections(timeout time.Duration) {
	now := time.Now().UnixMilli()
	cm.connections.Range(func(key, value interface{}) bool {
		conn := value.(*Connection)
		lastActive := atomic.LoadInt64(&conn.LastActive)
		if now-lastActive > timeout.Milliseconds() {
			tlog.Debug("closing idle connection", "connectionID", conn.ID())
			if conn.Conn != nil {
				conn.Conn.Close()
			}
			cm.RemoveConnection(conn.ID())
			cm.connectionTimeouts.Add(1)
		}
		return true
	})
}

// StopConnectionChecker 停止空闲连接检查器。
func (cm *ConnectionManager) StopConnectionChecker() {
	select {
	case <-cm.stopCh:
	default:
		close(cm.stopCh)
	}
	<-cm.checkDone
}

// CloseAllConnections 关闭所有连接并清理资源。
func (cm *ConnectionManager) CloseAllConnections() {
	cm.connections.Range(func(key, value interface{}) bool {
		conn := value.(*Connection)
		if conn.Conn != nil {
			conn.Conn.Close()
		}
		cm.RemoveConnection(conn.ID())
		return true
	})
}
