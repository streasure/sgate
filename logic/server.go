package logic

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
)

type RouteHandler func(msg *protobuf.Message) *protobuf.Message

type DisconnectCallback func(connectionID string)

type streamConn struct {
	stream protobuf.GatewayService_StreamMessagesServer
	mu     sync.Mutex
}

func (sc *streamConn) Send(msg *protobuf.Message) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.stream.Send(msg)
}

type pushGroup struct {
	members map[string]struct{}
}

type Server struct {
	protobuf.UnimplementedGatewayServiceServer
	routes       sync.Map
	connections  sync.Map
	onDisconnect []DisconnectCallback

	serverID   string
	groupMu    sync.RWMutex
	groups     map[string]*pushGroup
	connGroups map[string]map[string]struct{}
}

func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		groups:     make(map[string]*pushGroup),
		connGroups: make(map[string]map[string]struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type ServerOption func(*Server)

func WithServerID(serverID string) ServerOption {
	return func(s *Server) { s.serverID = serverID }
}

func (s *Server) GetServerID() string {
	return s.serverID
}

func (s *Server) serverGroupID() string {
	return "server:" + s.serverID
}

func (s *Server) RegisterRoute(route string, handler RouteHandler) {
	s.routes.Store(route, handler)
	tlog.Info("route registered", "route", route)
}

func (s *Server) GetRoutes() []string {
	var routes []string
	s.routes.Range(func(key, value interface{}) bool {
		routes = append(routes, key.(string))
		return true
	})
	return routes
}

func (s *Server) OnDisconnect(cb DisconnectCallback) {
	s.onDisconnect = append(s.onDisconnect, cb)
}

func (s *Server) StreamMessages(stream protobuf.GatewayService_StreamMessagesServer) error {
	connectionID := fmt.Sprintf("conn_%d", time.Now().UnixNano())

	conn := &streamConn{stream: stream}
	s.connections.Store(connectionID, conn)

	if s.serverID != "" {
		s.JoinGroup(s.serverGroupID(), connectionID)
	}

	tlog.Info("new stream connection", "connectionID", connectionID, "serverID", s.serverID)

	for {
		msg, err := stream.Recv()
		if err != nil {
			if s.connections.CompareAndDelete(connectionID, conn) {
				s.leaveAllGroups(connectionID)
				tlog.Info("stream connection closed", "connectionID", connectionID, "serverID", s.serverID)
				for _, cb := range s.onDisconnect {
					go cb(connectionID)
				}
			}
			return err
		}

		s.dispatchMessage(msg, func(response *protobuf.Message) {
			if response != nil {
				if response.ConnectionId == "" {
					response.ConnectionId = msg.ConnectionId
				}
				conn.Send(response)
			}
		})
	}
}

func (s *Server) SendMessage(ctx context.Context, msg *protobuf.Message) (*protobuf.Message, error) {
	var response *protobuf.Message
	var wg sync.WaitGroup
	wg.Add(1)

	s.dispatchMessage(msg, func(resp *protobuf.Message) {
		defer wg.Done()
		response = resp
	})

	wg.Wait()
	return response, nil
}

func (s *Server) dispatchMessage(msg *protobuf.Message, callback func(*protobuf.Message)) {
	if msg.Route == "" {
		callback(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "error",
			Payload:      map[string]string{"message": "Missing route", "code": "400"},
			Timestamp:    time.Now().UnixMilli(),
		})
		return
	}

	handler, ok := s.routes.Load(msg.Route)
	if !ok {
		callback(&protobuf.Message{
			ConnectionId: msg.ConnectionId,
			Route:        "error",
			Payload:      map[string]string{"message": "Route not found", "code": "404", "details": msg.Route},
			Timestamp:    time.Now().UnixMilli(),
		})
		return
	}

	response := handler.(RouteHandler)(msg)
	callback(response)
}

func (s *Server) PushToConnection(connectionID string, msg *protobuf.Message) error {
	val, ok := s.connections.Load(connectionID)
	if !ok {
		return fmt.Errorf("connection %s not found", connectionID)
	}
	conn := val.(*streamConn)
	if msg.ConnectionId == "" {
		msg.ConnectionId = connectionID
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	return conn.Send(msg)
}

func (s *Server) PushToServer(msg *protobuf.Message, exclude ...string) int {
	if s.serverID == "" {
		return 0
	}
	return s.PushToGroup(s.serverGroupID(), msg, exclude...)
}

func (s *Server) Broadcast(msg *protobuf.Message, exclude ...string) int {
	excludeSet := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		excludeSet[id] = struct{}{}
	}

	sent := 0
	s.connections.Range(func(key, val interface{}) bool {
		connID := key.(string)
		if _, excluded := excludeSet[connID]; excluded {
			return true
		}

		conn := val.(*streamConn)
		pushMsg := &protobuf.Message{
			ConnectionId: connID,
			Route:        msg.Route,
			Payload:      msg.Payload,
			Timestamp:    time.Now().UnixMilli(),
		}
		if err := conn.Send(pushMsg); err == nil {
			sent++
		}
		return true
	})
	return sent
}

func (s *Server) JoinGroup(groupID, connectionID string) int {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()

	g, ok := s.groups[groupID]
	if !ok {
		g = &pushGroup{members: make(map[string]struct{})}
		s.groups[groupID] = g
	}
	g.members[connectionID] = struct{}{}

	groups, ok := s.connGroups[connectionID]
	if !ok {
		groups = make(map[string]struct{})
		s.connGroups[connectionID] = groups
	}
	groups[groupID] = struct{}{}

	return len(g.members)
}

func (s *Server) LeaveGroup(groupID, connectionID string) int {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()

	g, ok := s.groups[groupID]
	if !ok {
		return 0
	}
	delete(g.members, connectionID)
	memberCount := len(g.members)

	if memberCount == 0 {
		delete(s.groups, groupID)
	}

	if groups, ok := s.connGroups[connectionID]; ok {
		delete(groups, groupID)
		if len(groups) == 0 {
			delete(s.connGroups, connectionID)
		}
	}

	return memberCount
}

func (s *Server) leaveAllGroups(connectionID string) {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()

	groups, ok := s.connGroups[connectionID]
	if !ok {
		return
	}

	for groupID := range groups {
		if g, ok := s.groups[groupID]; ok {
			delete(g.members, connectionID)
			if len(g.members) == 0 {
				delete(s.groups, groupID)
			}
		}
	}

	delete(s.connGroups, connectionID)
}

func (s *Server) PushToGroup(groupID string, msg *protobuf.Message, exclude ...string) int {
	s.groupMu.RLock()
	g, ok := s.groups[groupID]
	if !ok {
		s.groupMu.RUnlock()
		return 0
	}

	members := make([]string, 0, len(g.members))
	for connID := range g.members {
		members = append(members, connID)
	}
	s.groupMu.RUnlock()

	excludeSet := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		excludeSet[id] = struct{}{}
	}

	sent := 0
	for _, connID := range members {
		if _, excluded := excludeSet[connID]; excluded {
			continue
		}

		val, ok := s.connections.Load(connID)
		if !ok {
			continue
		}

		conn := val.(*streamConn)
		pushMsg := &protobuf.Message{
			ConnectionId: connID,
			Route:        msg.Route,
			Payload:      msg.Payload,
			Timestamp:    time.Now().UnixMilli(),
		}
		if err := conn.Send(pushMsg); err == nil {
			sent++
		}
	}
	return sent
}

func (s *Server) GetGroupMembers(groupID string) []string {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()

	g, ok := s.groups[groupID]
	if !ok {
		return nil
	}

	members := make([]string, 0, len(g.members))
	for connID := range g.members {
		members = append(members, connID)
	}
	return members
}

func (s *Server) GetGroupCount(groupID string) int {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()

	g, ok := s.groups[groupID]
	if !ok {
		return 0
	}
	return len(g.members)
}

func (s *Server) GetConnectionGroups(connectionID string) []string {
	s.groupMu.RLock()
	defer s.groupMu.RUnlock()

	groups, ok := s.connGroups[connectionID]
	if !ok {
		return nil
	}

	result := make([]string, 0, len(groups))
	for groupID := range groups {
		result = append(result, groupID)
	}
	return result
}

func (s *Server) GetServerPlayerCount() int {
	if s.serverID == "" {
		return 0
	}
	return s.GetGroupCount(s.serverGroupID())
}

func (s *Server) ForEachConnection(fn func(connectionID string) bool) {
	s.connections.Range(func(key, _ interface{}) bool {
		return fn(key.(string))
	})
}

func (s *Server) ConnectionExists(connectionID string) bool {
	_, ok := s.connections.Load(connectionID)
	return ok
}

func (s *Server) RegisterGatewayServiceServer(srv *grpc.Server) {
	protobuf.RegisterGatewayServiceServer(srv, s)
}

func (s *Server) GetConnectionCount() int {
	count := 0
	s.connections.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}
