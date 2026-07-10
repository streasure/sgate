package logic

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/streasure/sgate/protobuf"
	tlog "github.com/streasure/treasure-slog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type DisconnectCallback func(connectionID string)

type streamConn struct {
	stream protobuf.GatewayService_StreamMessagesServer
	sendCh chan *protobuf.Message
	done   chan struct{}
}

func newStreamConn(stream protobuf.GatewayService_StreamMessagesServer, sendChSize int) *streamConn {
	if sendChSize <= 0 {
		sendChSize = 1048576
	}
	sc := &streamConn{
		stream: stream,
		sendCh: make(chan *protobuf.Message, sendChSize),
		done:   make(chan struct{}),
	}
	go sc.flushLoop()
	return sc
}

func (sc *streamConn) Send(msg *protobuf.Message) error {
	select {
	case sc.sendCh <- msg:
		return nil
	default:
		return fmt.Errorf("send channel full")
	}
}

func (sc *streamConn) flushLoop() {
	defer close(sc.done)
	const maxBatchCount = 256
	batch := make([]*protobuf.Message, 0, maxBatchCount)

	// sendBatch 发送一批消息：
	// - 预打包的 _batch 消息直接透传
	// - 常规消息按 connID 分组打包：同 connID 的消息用 single-conn 格式（connID 在外层），
	//   不同 connID 的用 multi-conn 格式（connID 在每条条目内）
	// - server.* 路由消息单独发送
	sendBatch := func(msgs []*protobuf.Message) {
		// single-conn batch: 同 connID 的常规消息
		var scBuf []byte
		var scConnID string
		scCount := 0
		flushSingleConn := func() {
			if scCount == 0 {
				return
			}
			_ = sc.stream.Send(&protobuf.Message{
				Route:        protobuf.RouteBatch,
				ConnectionId: scConnID,
				Data:         scBuf,
				Cmd:          int32(scCount),
			})
			scBuf = nil
			scConnID = ""
			scCount = 0
		}
		// multi-conn batch: 不同 connID 的常规消息
		var mcBuf []byte
		mcCount := 0
		flushMultiConn := func() {
			if mcCount == 0 {
				return
			}
			_ = sc.stream.Send(&protobuf.Message{
				Route: protobuf.RouteBatch,
				Data:  mcBuf,
				Cmd:   int32(mcCount),
			})
			mcBuf = nil
			mcCount = 0
		}
		for _, m := range msgs {
			if m.Route == protobuf.RouteBatch {
				// 预打包消息：先刷新累积的常规消息，再直接发送
				flushSingleConn()
				flushMultiConn()
				_ = sc.stream.Send(m)
			} else if len(m.Route) >= 7 && m.Route[:7] == "server." {
				flushSingleConn()
				flushMultiConn()
				_ = sc.stream.Send(m)
			} else {
				data, err := proto.Marshal(m)
				if err != nil {
					continue
				}
				var lenBuf [4]byte
				binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
				connID := m.ConnectionId
				if connID == scConnID && scCount > 0 {
					// 追加到当前 single-conn batch
					scBuf = append(scBuf, lenBuf[:]...)
					scBuf = append(scBuf, data...)
					scCount++
				} else if scCount == 0 {
					// 开启新的 single-conn batch
					flushMultiConn()
					scConnID = connID
					scBuf = append(scBuf, lenBuf[:]...)
					scBuf = append(scBuf, data...)
					scCount = 1
				} else {
					// connID 变了：先刷新 single-conn，再开启新的
					flushSingleConn()
					scConnID = connID
					scBuf = append(scBuf, lenBuf[:]...)
					scBuf = append(scBuf, data...)
					scCount = 1
				}
			}
		}
		flushSingleConn()
		flushMultiConn()
	}

	for {
		msg, ok := <-sc.sendCh
		if !ok {
			return
		}
		batch = batch[:0]
		batch = append(batch, msg)
		drained := true
		for drained {
			select {
			case m, ok := <-sc.sendCh:
				if !ok {
					drained = false
					break
				}
				batch = append(batch, m)
				if len(batch) >= maxBatchCount {
					drained = false
				}
			default:
				drained = false
			}
		}
		sendBatch(batch)
	}
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

	dispatchCh          chan dispatchItem
	dispatchWg          sync.WaitGroup
	streamChSize        int
	dispatchWorkerCount int
}

type dispatchItem struct {
	msg  *protobuf.Message
	conn *streamConn
}

func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		groups:       make(map[string]*pushGroup),
		connGroups:   make(map[string]map[string]struct{}),
		dispatchCh:   make(chan dispatchItem, runtime.NumCPU()*65536),
		streamChSize: 1048576,
	}
	for _, opt := range opts {
		opt(s)
	}

	workerCount := runtime.NumCPU() * 128
	if s.dispatchWorkerCount > 0 {
		workerCount = s.dispatchWorkerCount
	}
	for i := 0; i < workerCount; i++ {
		s.dispatchWg.Add(1)
		go s.dispatchWorker()
	}

	return s
}

func (s *Server) dispatchWorker() {
	defer s.dispatchWg.Done()
	for item := range s.dispatchCh {
		func() {
			defer func() {
				if r := recover(); r != nil {
				}
			}()
			s.dispatchMessage(item.msg, func(response *protobuf.Message) {
				if response != nil {
					if response.ConnectionId == "" {
						response.ConnectionId = item.msg.ConnectionId
					}
					if response.ConnectionId == "" {
						return
					}
					item.conn.Send(response)
				}
			})
		}()
	}
}

type ServerOption func(*Server)

func WithServerID(serverID string) ServerOption {
	return func(s *Server) { s.serverID = serverID }
}

func WithDispatchWorkers(n int) ServerOption {
	return func(s *Server) { s.dispatchWorkerCount = n }
}

func WithDispatchChSize(n int) ServerOption {
	return func(s *Server) {
		s.dispatchCh = make(chan dispatchItem, n)
	}
}

func WithStreamChSize(n int) ServerOption {
	return func(s *Server) { s.streamChSize = n }
}

func (s *Server) GetServerID() string {
	return s.serverID
}

func (s *Server) serverGroupID() string {
	return "server:" + s.serverID
}

func (s *Server) OnDisconnect(cb DisconnectCallback) {
	s.onDisconnect = append(s.onDisconnect, cb)
}

func (s *Server) StreamMessages(stream protobuf.GatewayService_StreamMessagesServer) error {
	connectionID := fmt.Sprintf("conn_%d", time.Now().UnixNano())

	conn := newStreamConn(stream, s.streamChSize)
	s.connections.Store(connectionID, conn)

	if s.serverID != "" {
		s.JoinGroup(s.serverGroupID(), connectionID)
	}

	tlog.Info("new stream connection", "connectionID", connectionID, "serverID", s.serverID)

	for {
		msg, err := stream.Recv()
		if err != nil {
			close(conn.sendCh)
			<-conn.done
			s.connections.CompareAndDelete(connectionID, conn)
			s.leaveAllGroups(connectionID)
			tlog.Info("stream connection closed", "connectionID", connectionID, "serverID", s.serverID)
			for _, cb := range s.onDisconnect {
				go cb(connectionID)
			}
			return err
		}

		if msg.ConnectionId == "" {
			tlog.Warn("received message with empty ConnectionId", "route", msg.Route)
		}

		select {
		case s.dispatchCh <- dispatchItem{msg: msg, conn: conn}:
		default:
			tlog.Warn("dispatch channel full, dropping message", "route", msg.Route, "connectionID", msg.ConnectionId)
		}
	}
}

func (s *Server) SendMessage(ctx context.Context, msg *protobuf.Message) (*protobuf.Message, error) {
	var response *protobuf.Message
	var wg sync.WaitGroup
	wg.Add(1)

	func() {
		defer func() {
			if r := recover(); r != nil {
				tlog.Error("SendMessage dispatchMessage panic recovered", "error", r, "route", msg.Route, "cmd", msg.Cmd)
				wg.Done()
			}
		}()
		s.dispatchMessage(msg, func(resp *protobuf.Message) {
			defer wg.Done()
			response = resp
		})
	}()

	wg.Wait()
	return response, nil
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

func (s *Server) JoinGroupByUser(groupID, serverID, userUUID string) {
	s.PushToServer(&protobuf.Message{
		Route: protobuf.RouteServerJoinGroupByUser,
		Payload: map[string]string{
			"groupID":  groupID,
			"serverID": serverID,
			"userUUID": userUUID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) LeaveGroupByUser(groupID, serverID, userUUID string) {
	s.PushToServer(&protobuf.Message{
		Route: protobuf.RouteServerLeaveGroupByUser,
		Payload: map[string]string{
			"groupID":  groupID,
			"serverID": serverID,
			"userUUID": userUUID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) CreateGroup(groupID, groupName string) {
	s.PushToServer(&protobuf.Message{
		Route: protobuf.RouteServerCreateGroup,
		Payload: map[string]string{
			"groupID":   groupID,
			"groupName": groupName,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) DeleteGroup(groupID string) {
	s.PushToServer(&protobuf.Message{
		Route: protobuf.RouteServerDeleteGroup,
		Payload: map[string]string{
			"groupID": groupID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (s *Server) SendToGroup(groupID string, msg *protobuf.Message) {
	if msg.Payload == nil {
		msg.Payload = make(map[string]string)
	}
	msg.Payload["groupID"] = groupID
	msg.Route = protobuf.RouteServerSendToGroup
	msg.Timestamp = time.Now().UnixMilli()
	s.PushToServer(msg)
}

func (s *Server) GetGroupInfo(groupID string) {
	s.PushToServer(&protobuf.Message{
		Route: protobuf.RouteServerGetGroupInfo,
		Payload: map[string]string{
			"groupID": groupID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}
