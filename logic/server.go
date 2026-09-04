//go:build legacy

package logic

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	enums "github.com/streasure/protocol/enums"
	protocol "github.com/streasure/protocol/gateway"
	logicproto "github.com/streasure/protocol/logic"
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
	default:
		return fmt.Errorf("logic: gateway send queue full")
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
		return fmt.Errorf("logic: session %q not found", sessionID)
	}
	return value.(*streamConn).Send(&protocol.StreamData{SessionId: sessionID, Cmd: targetCmd, Data: data})
}

func (s *Server) RegisterUser(userKey, sessionID string) {
	if userKey == "" || sessionID == "" {
		return
	}
	if old, ok := s.userSessions.Swap(userKey, sessionID); ok && old.(string) != sessionID {
		s.sessionUsers.Delete(old.(string))
	}
	if old, ok := s.sessionUsers.Swap(sessionID, userKey); ok && old.(string) != userKey {
		s.userSessions.Delete(old.(string))
	}
}

func (s *Server) UnregisterUser(userKey string) {
	if sessionID, ok := s.userSessions.LoadAndDelete(userKey); ok {
		s.sessionUsers.CompareAndDelete(sessionID.(string), userKey)
	}
}

func (s *Server) GetConnectionIDByUser(userKey string) (string, bool) {
	value, ok := s.userSessions.Load(userKey)
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
func (s *Server) Offline(sessionID, userKey string) {
	if value, ok := s.sessions.Load(sessionID); ok {
		s.sessions.CompareAndDelete(sessionID, value)
	}
	s.leaveAllGroups(sessionID)
	if userKey != "" {
		s.UnregisterUser(userKey)
		return
	}
	if value, ok := s.sessionUsers.LoadAndDelete(sessionID); ok {
		s.userSessions.CompareAndDelete(value.(string), sessionID)
	}
}

func (s *Server) sendControl(cmd enums.Cmd, message proto.Message) int {
	data, err := proto.Marshal(message)
	if err != nil {
		tlog.Error("failed to encode control message", "cmd", cmd, "error", err)
		return 0
	}
	return s.sendRawControl(cmd, data)
}

func (s *Server) sendRawControl(cmd enums.Cmd, data []byte) int {
	count := 0
	sent := make(map[string]struct{})
	s.streams.Range(func(_, value any) bool {
		conn := value.(*streamConn)
		if _, ok := sent[conn.gatewayID]; ok {
			return true
		}
		if conn.Send(&protocol.StreamData{Cmd: int32(cmd), Data: data}) == nil {
			sent[conn.gatewayID] = struct{}{}
			count++
		}
		return true
	})
	return count
}

// SendToGroup asks Gateway instances to fan out targetCmd and data to a group.
func (s *Server) SendToGroup(groupID string, targetCmd int32, data []byte) int {
	return s.sendControl(enums.Cmd_CMD_SEND_TO_GROUP_REQ, &logicproto.SendToGroupReq{GroupId: groupID, TargetCmd: targetCmd, Data: data})
}

// Broadcast asks Gateway instances to fan out targetCmd and data to all clients.
func (s *Server) Broadcast(targetCmd int32, data []byte) int {
	return s.sendControl(enums.Cmd_CMD_BROADCAST_REQ, &logicproto.BroadcastReq{TargetCmd: targetCmd, Data: data})
}

// SendToUser asks Gateway instances to send targetCmd and data to a user key.
func (s *Server) SendToUser(userKey string, targetCmd int32, data []byte) int {
	return s.sendControl(enums.Cmd_CMD_SEND_TO_USER_REQ, &logicproto.SendToUserReq{UserKey: userKey, TargetCmd: targetCmd, Data: data})
}

// Kick accepts either serialized logic.KickNtf bytes or targetCmd and data.
func (s *Server) Kick(sessionID string, args ...any) int {
	if len(args) == 2 {
		targetCmd, ok := args[0].(int32)
		data, dataOK := args[1].([]byte)
		if ok && dataOK {
			return s.sendRawControl(enums.Cmd_CMD_KICK_NTF, mustMarshal(&logicproto.KickNtf{
				SessionId: sessionID,
				Code:      targetCmd,
				Message:   string(data),
			}))
		}
	}
	if len(args) != 1 {
		return 0
	}
	kickNtf, ok := args[0].([]byte)
	if !ok {
		return 0
	}
	notification := &logicproto.KickNtf{}
	if len(kickNtf) > 0 {
		if err := proto.Unmarshal(kickNtf, notification); err != nil {
			tlog.Warn("invalid KickNtf", "sessionID", sessionID, "error", err)
			return 0
		}
	}
	notification.SessionId = sessionID
	data, err := proto.Marshal(notification)
	if err != nil {
		return 0
	}
	return s.sendRawControl(enums.Cmd_CMD_KICK_NTF, data)
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

func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		s.streams.Range(func(_, value any) bool {
			conn := value.(*streamConn)
			conn.Close()
			return true
		})
	})
}
