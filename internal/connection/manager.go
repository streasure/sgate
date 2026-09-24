package connection

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/util/tlog"
)

type ConnectionManager struct {
	connections           *shardedMap[*Connection] // 分片 map，按 connectionID 索引
	userConnections       *shardedMap[string]      // 分片 map，按 userUUID → connectionID
	serverUserConnections sync.Map
	serverConnections     sync.Map
	groups                sync.Map
	groupMutex            sync.RWMutex
	count                 atomic.Int32
	stopCh                chan struct{}
	checkDone             chan struct{}

	ipConnections map[string]int32 // IP → 当前连接数
	ipMu          sync.RWMutex

	maxConnections      int // 网关最大总连接数，0=不限制
	maxConnectionsPerIP int // 单 IP 最大连接数，0=不限制

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

// GenerateConnectionID 生成唯一的连接标识符，格式为conn_时间戳_自增ID。
func GenerateConnectionID() string {
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
func NewConnectionManager(maxConn, maxConnPerIP int) *ConnectionManager {
	return &ConnectionManager{
		connections:         newShardedMap[*Connection](),
		userConnections:     newShardedMap[string](),
		ipConnections:       make(map[string]int32),
		maxConnections:      maxConn,
		maxConnectionsPerIP: maxConnPerIP,
		stopCh:              make(chan struct{}),
		checkDone:           make(chan struct{}),
	}
}

// UpdateLimits 运行时更新连接限制参数（热配置）。
func (cm *ConnectionManager) UpdateLimits(maxConn, maxConnPerIP int) {
	if maxConn >= 0 {
		cm.maxConnections = maxConn
	}
	if maxConnPerIP >= 0 {
		cm.maxConnectionsPerIP = maxConnPerIP
	}
	tlog.Info(context.TODO(), "connection manager limits updated maxConnections=%d maxConnectionsPerIP=%d",
		cm.maxConnections,
		cm.maxConnectionsPerIP)
}

// CanAccept 检查是否允许接受新连接（总连接数 + 单 IP 连接数）。
func (cm *ConnectionManager) CanAccept(remoteIP string) bool {
	if cm.maxConnections > 0 && int(cm.count.Load()) >= cm.maxConnections {
		return false
	}
	if cm.maxConnectionsPerIP > 0 && remoteIP != "" {
		cm.ipMu.RLock()
		ipCount := cm.ipConnections[remoteIP]
		cm.ipMu.RUnlock()
		if ipCount >= int32(cm.maxConnectionsPerIP) {
			return false
		}
	}
	return true
}

// MaxConnections 返回网关最大总连接数（0=不限制）。
func (cm *ConnectionManager) MaxConnections() int { return cm.maxConnections }

// MaxConnectionsPerIP 返回单 IP 最大连接数（0=不限制）。
func (cm *ConnectionManager) MaxConnectionsPerIP() int { return cm.maxConnectionsPerIP }

// incrementIP 增加指定 IP 的连接计数。
func (cm *ConnectionManager) incrementIP(remoteIP string) {
	if remoteIP == "" {
		return
	}
	cm.ipMu.Lock()
	cm.ipConnections[remoteIP]++
	cm.ipMu.Unlock()
}

// decrementIP 减少指定 IP 的连接计数，计数归零时删除条目。
func (cm *ConnectionManager) decrementIP(remoteIP string) {
	if remoteIP == "" {
		return
	}
	cm.ipMu.Lock()
	if n := cm.ipConnections[remoteIP]; n <= 1 {
		delete(cm.ipConnections, remoteIP)
	} else {
		cm.ipConnections[remoteIP] = n - 1
	}
	cm.ipMu.Unlock()
}

// GetIPConnectionCount 获取指定 IP 的当前连接数。
func (cm *ConnectionManager) GetIPConnectionCount(remoteIP string) int {
	cm.ipMu.RLock()
	defer cm.ipMu.RUnlock()
	return int(cm.ipConnections[remoteIP])
}

// AddConnection 添加新连接，生成唯一ID并建立映射关系。
func (cm *ConnectionManager) AddConnection(conn gnet.Conn, userUUID string) string {
	connectionID := GenerateConnectionID()
	remoteAddr := ""
	if conn.RemoteAddr() != nil {
		remoteAddr = conn.RemoteAddr().String()
	}
	c := newConnection(connectionID, conn, userUUID, remoteAddr)
	cm.connections.Store(connectionID, c)
	cm.userConnections.Store(userUUID, connectionID)
	cm.count.Add(1)
	cm.totalConnections.Add(1)
	cm.activeConnections.Add(1)
	// 追踪 IP 连接数
	remoteIP := extractIP(remoteAddr)
	cm.incrementIP(remoteIP)
	return connectionID
}

// GetConnection 根据连接ID获取连接对象。
func (cm *ConnectionManager) GetConnection(connectionID string) *Connection {
	v, ok := cm.connections.Load(connectionID)
	if !ok {
		return nil
	}
	return v
}

// GetUserConnection 根据用户UUID获取关联的连接ID。
func (cm *ConnectionManager) GetUserConnection(userUUID string) (string, bool) {
	return cm.userConnections.Load(userUUID)
}

// RemoveConnection 移除连接并清理所有关联的映射关系。
func (cm *ConnectionManager) RemoveConnection(connectionID string) {
	conn, ok := cm.connections.Load(connectionID)
	if !ok {
		return
	}
	cm.connections.Delete(connectionID)
	cm.userConnections.Delete(conn.GetUserUUID())
	serverID := conn.GetServerID()
	cm.serverUserConnections.Delete(serverUserKey{serverID: serverID, userUUID: conn.GetUserUUID()})
	if serverID != "" {
		cm.serverConnections.Delete(serverID)
	}
	// 清理该连接所属的所有 group 成员关系
	conn.groupsMu.RLock()
	for groupID := range conn.Groups {
		cm.RemoveUserFromGroup(groupID, serverID, conn.GetUserUUID())
	}
	conn.groupsMu.RUnlock()
	// 追踪 IP 连接数
	remoteIP := extractIP(conn.RemoteAddr)
	cm.decrementIP(remoteIP)
	cm.count.Add(-1)
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
	return int(cm.count.Load())
}

// BroadcastBytes 向所有处于转发状态的连接广播数据。
func (cm *ConnectionManager) BroadcastBytes(data []byte) {
	cm.connections.Range(func(key string, conn *Connection) bool {
		if conn.GetState() == StateForward {
			conn.Send(data)
		}
		return true
	})
}

// SendToUser 向指定用户发送数据。
func (cm *ConnectionManager) SendToUser(userUUID string, data []byte) {
	connectionID, ok := cm.userConnections.Load(userUUID)
	if !ok {
		return
	}
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
	cm.connections.Range(func(key string, conn *Connection) bool {
		lastActive := conn.LastActive.Load()
		if now-lastActive > timeout.Milliseconds() {
			tlog.Debug(context.TODO(), "closing idle connection connectionID=%s", conn.ID())
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
	cm.connections.Range(func(key string, conn *Connection) bool {
		if conn.Conn != nil {
			conn.Conn.Close()
		}
		cm.RemoveConnection(conn.ID())
		return true
	})
}

// ForEach 遍历所有连接。回调返回 false 时终止遍历。
func (cm *ConnectionManager) ForEach(fn func(conn *Connection) bool) {
	cm.connections.Range(func(_ string, conn *Connection) bool {
		return fn(conn)
	})
}

// extractIP 从地址字符串中提取 IP 部分（去掉端口）。
func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
