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

type ConnState int32

const (
	StateForward ConnState = 0
	StateClosed  ConnState = 1
)

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

	// Atomic fields for lock-free access
	userUUID atomic.Value // string
	serverID atomic.Value // string
	isWS     atomic.Bool
}

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

func (c *Connection) ID() string          { return c.id }
func (c *Connection) GetState() ConnState { return ConnState(c.state.Load()) }
func (c *Connection) SetState(old, new ConnState) bool {
	return c.state.CompareAndSwap(int32(old), int32(new))
}

func (c *Connection) IsBound() bool {
	return c.serverID.Load().(string) != ""
}

func (c *Connection) IsAuthenticated() bool {
	uuid := c.userUUID.Load().(string)
	return uuid != "" && !strings.HasPrefix(uuid, "temp_")
}

func (c *Connection) IsWebSocket() bool { return c.isWS.Load() }

func (c *Connection) SetUserUUID(uuid string) { c.userUUID.Store(uuid) }
func (c *Connection) GetUserUUID() string {
	if v := c.userUUID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (c *Connection) SetServerID(sid string) { c.serverID.Store(sid) }
func (c *Connection) GetServerID() string {
	if v := c.serverID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (c *Connection) SetWS(v bool) { c.isWS.Store(v) }

// Header pool for reducing allocations
var headerPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 4)
		return &buf
	},
}

func noopAsyncCallback(_ gnet.Conn, _ error) error { return nil }

func (c *Connection) touch() {
	seq := c.activitySeq.Add(1)
	if seq&63 != 0 && atomic.LoadInt64(&c.LastActive) != 0 {
		return
	}
	atomic.StoreInt64(&c.LastActive, time.Now().UnixMilli())
}

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
	// Note: header is reused across calls, but AsyncWritev copies the data
	return err
}

func (c *Connection) SendMulti(combined []byte) error {
	if c.Conn == nil {
		return fmt.Errorf("connection is nil")
	}
	c.touch()
	return c.Conn.AsyncWrite(combined, noopAsyncCallback)
}

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

type serverUserKey struct {
	serverID string
	userUUID string
}

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

func (g *ConnectionGroupInfo) Snapshot() []serverUserKey {
	g.mu.RLock()
	defer g.mu.RUnlock()
	members := make([]serverUserKey, 0, len(g.Members))
	for key := range g.Members {
		members = append(members, key)
	}
	return members
}

func (g *ConnectionGroupInfo) SnapshotUsers() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	users := make([]string, 0, len(g.Members))
	for key := range g.Members {
		users = append(users, key.userUUID)
	}
	return users
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

func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		stopCh:    make(chan struct{}),
		checkDone: make(chan struct{}),
	}
}

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

func (cm *ConnectionManager) GetConnection(connectionID string) *Connection {
	v, ok := cm.connections.Load(connectionID)
	if !ok {
		return nil
	}
	return v.(*Connection)
}

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

func (cm *ConnectionManager) SetConnectionServerID(connectionID, serverID string) {
	if conn := cm.GetConnection(connectionID); conn != nil {
		conn.SetServerID(serverID)
		cm.serverConnections.Store(serverID, connectionID)
	}
}

func (cm *ConnectionManager) UpdateConnectionUserUUID(connectionID, userUUID string) {
	if conn := cm.GetConnection(connectionID); conn != nil {
		oldUUID := conn.GetUserUUID()
		conn.SetUserUUID(userUUID)
		cm.userConnections.Delete(oldUUID)
		cm.userConnections.Store(userUUID, connectionID)
		cm.serverUserConnections.Store(serverUserKey{serverID: conn.GetServerID(), userUUID: userUUID}, connectionID)
	}
}

func (cm *ConnectionManager) UpdateUserConnection(connectionID, oldUserUUID, newUserUUID string) {
	cm.UpdateConnectionUserUUID(connectionID, newUserUUID)
}

func (cm *ConnectionManager) GetConnectionCount() int {
	return int(atomic.LoadInt32(&cm.count))
}

func (cm *ConnectionManager) BroadcastBytes(data []byte) {
	cm.connections.Range(func(key, value interface{}) bool {
		conn := value.(*Connection)
		if conn.GetState() == StateForward {
			conn.Send(data)
		}
		return true
	})
}

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

func (cm *ConnectionManager) DeleteGroup(groupID string) {
	cm.groupMutex.Lock()
	defer cm.groupMutex.Unlock()
	cm.groups.Delete(groupID)
}

func (cm *ConnectionManager) GetGroupMemberCount(groupID string) int {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		return 0
	}
	return v.(*ConnectionGroupInfo).MemberCount()
}

func (cm *ConnectionManager) GetGroupName(groupID string) string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		return ""
	}
	return v.(*ConnectionGroupInfo).Name
}

func (cm *ConnectionManager) GetGroupUsers(groupID string) []string {
	cm.groupMutex.RLock()
	defer cm.groupMutex.RUnlock()
	v, ok := cm.groups.Load(groupID)
	if !ok {
		return nil
	}
	return v.(*ConnectionGroupInfo).SnapshotUsers()
}

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

func (cm *ConnectionManager) StopConnectionChecker() {
	select {
	case <-cm.stopCh:
	default:
		close(cm.stopCh)
	}
	<-cm.checkDone
}

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
