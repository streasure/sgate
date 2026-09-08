package logic

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type DisconnectCallback func(connectionID string)

type streamConn struct {
	stream     protocol.GatewayStream_OnDataServer
	sendCh     chan *protocol.StreamData
	done       chan struct{}
	closed     atomic.Bool
	closeOnce  sync.Once
	gatewayID  string
	sessionMu  sync.Mutex
	sessionIDs map[string]struct{}
}

func newStreamConn(stream protocol.GatewayStream_OnDataServer, size int, gatewayID string) *streamConn {
	if size <= 0 {
		size = 1024
	}
	c := &streamConn{
		stream: stream, sendCh: make(chan *protocol.StreamData, size), done: make(chan struct{}),
		gatewayID: gatewayID, sessionIDs: make(map[string]struct{}),
	}
	go func() {
		defer close(c.done)
		for msg := range c.sendCh {
			if err := c.stream.Send(msg); err != nil {
				return
			}
		}
	}()
	return c
}

func (c *streamConn) Send(msg *protocol.StreamData) (err error) {
	if c.closed.Load() {
		return fmt.Errorf("logic: gateway stream closed")
	}
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("logic: gateway stream closed")
		}
	}()
	select {
	case c.sendCh <- msg:
		return nil
	case <-c.done:
		return fmt.Errorf("logic: gateway stream ended")
	}
}

func (c *streamConn) bindSession(sessionID string) {
	c.sessionMu.Lock()
	c.sessionIDs[sessionID] = struct{}{}
	c.sessionMu.Unlock()
}

func (c *streamConn) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.sendCh)
	})
}

type pushGroup struct {
	members map[string]struct{}
}

// PushResult records the outcome of pushing to a single session.
type PushResult struct {
	SessionID string
	Success   bool
	Error     error
}

// PushMetrics tracks push operation statistics for monitoring.
type PushMetrics struct {
	TotalPushed     atomic.Int64
	TotalFailed     atomic.Int64
	GroupPushed     atomic.Int64
	GroupFailed     atomic.Int64
	BroadcastSent   atomic.Int64
	BroadcastFailed atomic.Int64
	RetryAttempts   atomic.Int64
	ScheduledPushes atomic.Int64
}

// GetSnapshot returns a copy of the current metrics values.
func (m *PushMetrics) GetSnapshot() map[string]int64 {
	return map[string]int64{
		"totalPushed":     m.TotalPushed.Load(),
		"totalFailed":     m.TotalFailed.Load(),
		"groupPushed":     m.GroupPushed.Load(),
		"groupFailed":     m.GroupFailed.Load(),
		"broadcastSent":   m.BroadcastSent.Load(),
		"broadcastFailed": m.BroadcastFailed.Load(),
		"retryAttempts":   m.RetryAttempts.Load(),
		"scheduledPushes": m.ScheduledPushes.Load(),
	}
}

type Server struct {
	protocol.UnimplementedGatewayStreamServer
	handlers sync.Map // int32 -> *protoEntry

	streams  sync.Map // gateway stream ID -> *streamConn
	sessions sync.Map // session ID -> *streamConn

	userSessions sync.Map // user key -> session ID
	sessionUsers sync.Map // session ID -> user key

	groupMu       sync.RWMutex
	groups        map[string]*pushGroup
	sessionGroups map[string]map[string]struct{}

	mu           sync.Mutex
	onDisconnect []DisconnectCallback
	serverID     string
	streamSeq    atomic.Uint64
	streamChSize int
	stopOnce     sync.Once
	metrics      PushMetrics
}

type ServerOption func(*Server)

func WithServerID(serverID string) ServerOption { return func(s *Server) { s.serverID = serverID } }
func WithDispatchWorkers(int) ServerOption      { return func(*Server) {} }
func WithDispatchChSize(int) ServerOption       { return func(*Server) {} }
func WithStreamChSize(n int) ServerOption       { return func(s *Server) { s.streamChSize = n } }
func WithServerPassthrough() ServerOption       { return func(*Server) {} }

func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		groups:        make(map[string]*pushGroup),
		sessionGroups: make(map[string]map[string]struct{}),
		streamChSize:  1024,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Server) GetServerID() string { return s.serverID }

func (s *Server) OnDisconnect(cb DisconnectCallback) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDisconnect = append(s.onDisconnect, cb)
}

// OnData dispatches every incoming StreamData solely by Cmd.
func (s *Server) OnData(stream protocol.GatewayStream_OnDataServer) error {
	streamID := fmt.Sprintf("stream_%s_%d", s.serverID, s.streamSeq.Add(1))
	gatewayID := streamID
	if values := metadata.ValueFromIncomingContext(stream.Context(), "sgate-gateway-id"); len(values) > 0 && values[0] != "" {
		gatewayID = values[0]
	}
	conn := newStreamConn(stream, s.streamChSize, gatewayID)
	s.streams.Store(streamID, conn)
	defer func() {
		s.streams.Delete(streamID)
		conn.Close()
		<-conn.done
		conn.sessionMu.Lock()
		for sessionID := range conn.sessionIDs {
			s.sessions.CompareAndDelete(sessionID, conn)
			if userUUID, ok := s.sessionUsers.LoadAndDelete(sessionID); ok {
				s.userSessions.CompareAndDelete(userUUID.(string), sessionID)
			}
		}
		conn.sessionMu.Unlock()
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if msg.SessionId != "" {
			s.sessions.Store(msg.SessionId, conn)
			conn.bindSession(msg.SessionId)
			if msg.UserKey != "" {
				s.RegisterUser(msg.UserKey, msg.SessionId)
			}
		}
		s.dispatchMessage(msg, func(response *protocol.StreamData) {
			if err := conn.Send(response); err != nil {
				tlog.Warn("failed to queue response", "cmd", response.Cmd, "sessionID", response.SessionId, "error", err)
			}
		})
	}
}

func (s *Server) SendMessage(_ context.Context, msg *protocol.StreamData) (*protocol.StreamData, error) {
	var response *protocol.StreamData
	s.dispatchMessage(msg, func(resp *protocol.StreamData) { response = resp })
	return response, nil
}

// PushToConnection sends a business message to one known session.
func (s *Server) PushToConnection(sessionID string, targetCmd int32, data []byte) error {
	value, ok := s.sessions.Load(sessionID)
	if !ok {
		s.metrics.TotalFailed.Add(1)
		return fmt.Errorf("logic: session %q not found", sessionID)
	}
	err := value.(*streamConn).Send(&protocol.StreamData{SessionId: sessionID, Cmd: targetCmd, Data: data})
	if err != nil {
		s.metrics.TotalFailed.Add(1)
	} else {
		s.metrics.TotalPushed.Add(1)
	}
	return err
}

func (s *Server) RegisterUser(userUUID, sessionID string) {
	if userUUID == "" || sessionID == "" {
		return
	}
	if old, ok := s.userSessions.Swap(userUUID, sessionID); ok && old.(string) != sessionID {
		s.sessionUsers.Delete(old.(string))
	}
	if old, ok := s.sessionUsers.Swap(sessionID, userUUID); ok && old.(string) != userUUID {
		s.userSessions.Delete(old.(string))
	}
}

func (s *Server) UnregisterUser(userUUID string) {
	if sessionID, ok := s.userSessions.LoadAndDelete(userUUID); ok {
		s.sessionUsers.CompareAndDelete(sessionID.(string), userUUID)
	}
}

func (s *Server) GetConnectionIDByUser(userUUID string) (string, bool) {
	value, ok := s.userSessions.Load(userUUID)
	if !ok {
		return "", false
	}
	return value.(string), true
}

func (s *Server) JoinGroup(groupID, sessionID string) int {
	if groupID == "" || sessionID == "" {
		return 0
	}
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	group := s.groups[groupID]
	if group == nil {
		group = &pushGroup{members: make(map[string]struct{})}
		s.groups[groupID] = group
	}
	group.members[sessionID] = struct{}{}
	if s.sessionGroups[sessionID] == nil {
		s.sessionGroups[sessionID] = make(map[string]struct{})
	}
	s.sessionGroups[sessionID][groupID] = struct{}{}
	return len(group.members)
}

func (s *Server) LeaveGroup(groupID, sessionID string) int {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	group := s.groups[groupID]
	if group == nil {
		return 0
	}
	delete(group.members, sessionID)
	if len(group.members) == 0 {
		delete(s.groups, groupID)
	}
	if memberships := s.sessionGroups[sessionID]; memberships != nil {
		delete(memberships, groupID)
		if len(memberships) == 0 {
			delete(s.sessionGroups, sessionID)
		}
	}
	return len(group.members)
}

func (s *Server) GetGroupMembers(groupID string) []string {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()
	group := s.groups[groupID]
	if group == nil {
		return nil
	}
	members := make([]string, 0, len(group.members))
	for sessionID := range group.members {
		members = append(members, sessionID)
	}
	return members
}

func (s *Server) GetGroupCount(groupID string) int { return len(s.GetGroupMembers(groupID)) }

func (s *Server) leaveAllGroups(sessionID string) {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	for groupID := range s.sessionGroups[sessionID] {
		group := s.groups[groupID]
		delete(group.members, sessionID)
		if len(group.members) == 0 {
			delete(s.groups, groupID)
		}
	}
	delete(s.sessionGroups, sessionID)
}

// Offline clears the user, group, and session state associated with a client.
func (s *Server) Offline(sessionID, userUUID string) {
	if value, ok := s.sessions.Load(sessionID); ok {
		s.sessions.CompareAndDelete(sessionID, value)
	}
	s.leaveAllGroups(sessionID)
	if userUUID != "" {
		s.UnregisterUser(userUUID)
		return
	}
	if value, ok := s.sessionUsers.LoadAndDelete(sessionID); ok {
		s.userSessions.CompareAndDelete(value.(string), sessionID)
	}
}

func (s *Server) sendRawControl(cmd int32, data []byte) int {
	count := 0
	sent := make(map[string]struct{})
	s.streams.Range(func(_, value any) bool {
		conn := value.(*streamConn)
		if _, ok := sent[conn.gatewayID]; ok {
			return true
		}
		if conn.Send(&protocol.StreamData{Cmd: cmd, Data: data}) == nil {
			sent[conn.gatewayID] = struct{}{}
			count++
		}
		return true
	})
	return count
}

// SendToGroup sends a control message through the stream for gateway fan-out.
func (s *Server) sendToGroupLegacy(groupID string, targetCmd int32, data []byte) int {
	controlData := mustMarshal(&protocol.StreamData{
		Cmd:  targetCmd,
		Data: data,
	})
	return s.sendRawControl(int32(targetCmd), controlData)
}

// Broadcast sends a raw control message to all gateways.
func (s *Server) broadcastLegacy(targetCmd int32, data []byte) int {
	return s.sendRawControl(int32(targetCmd), data)
}

// SendToUser finds the connection by userUUID and sends directly.
// The session ID is an internal routing detail and is not required by callers.
func (s *Server) SendToUser(userUUID string, targetCmd int32, data []byte) int {
	if sessionID, ok := s.GetConnectionIDByUser(userUUID); ok {
		if s.PushToConnection(sessionID, targetCmd, data) == nil {
			return 1
		}
	}
	return 0
}

// Kick sends a kick notification through the stream.
func (s *Server) Kick(sessionID string, args ...any) int {
	var targetCmd int32
	var data []byte
	if len(args) == 2 {
		if cmd, ok := args[0].(int32); ok {
			targetCmd = cmd
		}
		if d, ok := args[1].([]byte); ok {
			data = d
		}
	}
	if targetCmd == 0 {
		targetCmd = 1100012 // CmdUserOffline
	}
	return s.sendRawControl(targetCmd, data)
}

// SendToGroup sends one StreamData to every current member of a group.
func (s *Server) SendToGroup(groupID string, targetCmd int32, data []byte) int {
	members := s.GetGroupMembers(groupID)
	sent := 0
	for _, sessionID := range members {
		if s.PushToConnection(sessionID, targetCmd, data) == nil {
			sent++
			s.metrics.GroupPushed.Add(1)
		} else {
			s.metrics.GroupFailed.Add(1)
		}
	}
	return sent
}

// Broadcast sends one StreamData to every current session.
func (s *Server) Broadcast(targetCmd int32, data []byte) int {
	sent := 0
	s.sessions.Range(func(key, _ any) bool {
		if s.PushToConnection(key.(string), targetCmd, data) == nil {
			sent++
			s.metrics.BroadcastSent.Add(1)
		} else {
			s.metrics.BroadcastFailed.Add(1)
		}
		return true
	})
	return sent
}

func mustMarshal(message proto.Message) []byte {
	data, err := proto.Marshal(message)
	if err != nil {
		return nil
	}
	return data
}

func (s *Server) RegisterGatewayStreamServer(grpcServer *grpc.Server) {
	protocol.RegisterGatewayStreamServer(grpcServer, s)
}

func (s *Server) GetConnectionCount() int {
	count := 0
	s.sessions.Range(func(_, _ any) bool { count++; return true })
	return count
}

// GetMetrics returns a snapshot of push metrics for monitoring.
func (s *Server) GetMetrics() map[string]int64 {
	return s.metrics.GetSnapshot()
}

// SendToGroupWithRetry sends to every group member, retrying failed sessions.
// Returns the number of successful sends and a list of still-failed session IDs.
func (s *Server) SendToGroupWithRetry(groupID string, targetCmd int32, data []byte, maxRetries int) (int, []string) {
	members := s.GetGroupMembers(groupID)
	failedSessions := make([]string, 0)
	for _, sessionID := range members {
		if err := s.PushToConnection(sessionID, targetCmd, data); err != nil {
			failedSessions = append(failedSessions, sessionID)
		}
	}
	for retry := 0; retry < maxRetries && len(failedSessions) > 0; retry++ {
		time.Sleep(10 * time.Millisecond)
		s.metrics.RetryAttempts.Add(1)
		var stillFailed []string
		for _, sessionID := range failedSessions {
			if err := s.PushToConnection(sessionID, targetCmd, data); err != nil {
				stillFailed = append(stillFailed, sessionID)
			}
		}
		failedSessions = stillFailed
	}
	return len(members) - len(failedSessions), failedSessions
}

// JoinGroupForUser joins a user (by userUUID) to a group. The user must have
// an active session registered via RegisterUser.
func (s *Server) JoinGroupForUser(userUUID, groupID string) error {
	sessionID, ok := s.GetConnectionIDByUser(userUUID)
	if !ok {
		return fmt.Errorf("logic: user %q not found", userUUID)
	}
	s.JoinGroup(groupID, sessionID)
	return nil
}

// LeaveGroupForUser removes a user (by userUUID) from a group.
func (s *Server) LeaveGroupForUser(userUUID, groupID string) error {
	sessionID, ok := s.GetConnectionIDByUser(userUUID)
	if !ok {
		return fmt.Errorf("logic: user %q not found", userUUID)
	}
	s.LeaveGroup(groupID, sessionID)
	return nil
}

// ScheduledPush manages a periodic group push that can be stopped.
type ScheduledPush struct {
	GroupID  string
	Cmd      int32
	Data     []byte
	Interval time.Duration
	MaxCount int // 0 = infinite
	stopCh   chan struct{}
	server   *Server
	done     chan struct{}
}

// ScheduleGroupPush starts a periodic push to a group. Returns a handle to stop it.
func (s *Server) ScheduleGroupPush(groupID string, cmd int32, data []byte, interval time.Duration, maxCount int) *ScheduledPush {
	sp := &ScheduledPush{
		GroupID:  groupID,
		Cmd:      cmd,
		Data:     data,
		Interval: interval,
		MaxCount: maxCount,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		server:   s,
	}
	go sp.run()
	return sp
}

func (sp *ScheduledPush) run() {
	defer close(sp.done)
	ticker := time.NewTicker(sp.Interval)
	defer ticker.Stop()
	count := 0
	for {
		select {
		case <-sp.stopCh:
			return
		case <-ticker.C:
			sp.server.SendToGroup(sp.GroupID, sp.Cmd, sp.Data)
			sp.server.metrics.ScheduledPushes.Add(1)
			count++
			if sp.MaxCount > 0 && count >= sp.MaxCount {
				return
			}
		}
	}
}

// Stop terminates the scheduled push and waits for the goroutine to finish.
func (sp *ScheduledPush) Stop() {
	close(sp.stopCh)
	<-sp.done
}

func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		s.streams.Range(func(_, value any) bool {
			conn := value.(*streamConn)
			conn.Close()
			return true
		})
	})
}
