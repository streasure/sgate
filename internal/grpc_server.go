package gateway

import (
	"context"
	"fmt"
	"net"
	"sync"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type GRPCServer struct {
	protoGw.UnimplementedGatewayStreamServer
	protoGw.UnimplementedGatewayServer

	gw     *Gateway
	server *grpc.Server

	logicClients map[string]*LogicConn
	mu           sync.RWMutex
}

type LogicConn struct {
	serverID string
	conn     *grpc.ClientConn
	stream   protoGw.GatewayStream_OnDataClient
	sendCh   chan *protoGw.StreamData
	gw       *Gateway
	cancel   context.CancelFunc
}

func NewGRPCServer(gw *Gateway) *GRPCServer {
	return &GRPCServer{
		gw:           gw,
		logicClients: make(map[string]*LogicConn),
	}
}

func (s *GRPCServer) Start(addr string, maxMsgSize, windowSize int) {
	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
	}
	s.server = grpc.NewServer(opts...)
	protoGw.RegisterGatewayStreamServer(s.server, s)
	protoGw.RegisterGatewayServer(s.server, s)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		tlog.Error("gRPC listen failed", "addr", addr, "error", err)
		return
	}
	if err := s.server.Serve(lis); err != nil {
		tlog.Error("gRPC serve failed", "error", err)
	}
}

func (s *GRPCServer) Stop() {
	if s.server != nil {
		s.server.GracefulStop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lc := range s.logicClients {
		lc.cancel()
		if lc.conn != nil {
			lc.conn.Close()
		}
	}
}

// ---- GatewayStreamServer ----

func (s *GRPCServer) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return fmt.Errorf("missing metadata")
	}
	serverIDs := md.Get("sgate-server-id")
	if len(serverIDs) == 0 {
		return fmt.Errorf("missing sgate-server-id")
	}
	serverID := serverIDs[0]

	lc := s.getOrCreateLogicConn(serverID)
	if lc == nil {
		return fmt.Errorf("unknown server: %s", serverID)
	}

	tlog.Info("logic connected", "serverID", serverID)

	for {
		msg, err := stream.Recv()
		if err != nil {
			tlog.Info("logic stream ended", "serverID", serverID, "error", err)
			return err
		}
		if msg.SessionId != "" {
			s.gw.SendToClient(msg.SessionId, msg.Cmd, msg.Data)
		}
	}
}

// ---- GatewayServer: logic->gateway unary RPCs ----

func (s *GRPCServer) CloseSession(ctx context.Context, req *protoGw.CloseSessionReq) (*protoGw.CloseSessionAck, error) {
	sess := s.gw.sessions.GetByID(req.SessionId)
	if sess != nil {
		sess.Conn().Close()
	}
	return &protoGw.CloseSessionAck{}, nil
}

func (s *GRPCServer) KickSession(ctx context.Context, req *protoGw.KickSessionReq) (*protoGw.KickSessionAck, error) {
	sess := s.gw.sessions.GetByID(req.SessionId)
	if sess != nil {
		sess.Conn().Close()
	}
	return &protoGw.KickSessionAck{}, nil
}

func (s *GRPCServer) SendToClient(ctx context.Context, req *protoGw.SendToClientReq) (*protoGw.SendToClientAck, error) {
	s.gw.SendToClient(req.SessionId, req.Cmd, req.Data)
	return &protoGw.SendToClientAck{}, nil
}

func (s *GRPCServer) Broadcast(ctx context.Context, req *protoGw.BroadcastReq) (*protoGw.BroadcastAck, error) {
	for _, gid := range req.GroupId {
		s.gw.SendToGroup(gid, req.Cmd, req.Data)
	}
	return &protoGw.BroadcastAck{}, nil
}

func (s *GRPCServer) BroadcastAll(ctx context.Context, req *protoGw.BroadcastAllReq) (*protoGw.BroadcastAllAck, error) {
	s.gw.Broadcast(req.Cmd, req.Data)
	return &protoGw.BroadcastAllAck{}, nil
}

func (s *GRPCServer) JoinGroup(ctx context.Context, req *protoGw.JoinGroupReq) (*protoGw.JoinGroupAck, error) {
	sess := s.gw.sessions.GetByID(req.SessionId)
	if sess == nil {
		return &protoGw.JoinGroupAck{Code: -1}, nil
	}
	counts := make([]int32, len(req.GroupId))
	for i, gid := range req.GroupId {
		count := s.gw.groups.Join(gid, sess)
		sess.AddGroup(gid)
		counts[i] = int32(count)
	}
	return &protoGw.JoinGroupAck{Code: 0, MemberCount: counts}, nil
}

func (s *GRPCServer) LeaveGroup(ctx context.Context, req *protoGw.LeaveGroupReq) (*protoGw.LeaveGroupAck, error) {
	sess := s.gw.sessions.GetByID(req.SessionId)
	if sess == nil {
		return &protoGw.LeaveGroupAck{Code: -1}, nil
	}
	counts := make([]int32, len(req.GroupId))
	for i, gid := range req.GroupId {
		count := s.gw.groups.Leave(gid, sess)
		sess.RemoveGroup(gid)
		counts[i] = int32(count)
	}
	return &protoGw.LeaveGroupAck{Code: 0, MemberCount: counts}, nil
}

func (s *GRPCServer) GetGroupInfo(ctx context.Context, req *protoGw.GetGroupInfoReq) (*protoGw.GetGroupInfoAck, error) {
	info := s.gw.groups.GetInfo(req.GroupId)
	if info == nil {
		return &protoGw.GetGroupInfoAck{}, nil
	}
	return &protoGw.GetGroupInfoAck{
		GroupId:     info.ID,
		MemberCount: int32(info.MemberCount),
		SessionIds:  info.SessionIDs,
	}, nil
}

// ---- Logic client management ----

func (s *GRPCServer) ConnectLogic(serverID, address string) error {
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8*1024*1024)),
	)
	if err != nil {
		return fmt.Errorf("dial logic %s: %w", address, err)
	}

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"sgate-server-id", s.gw.cfg.ServerID,
	)
	stream, err := protoGw.NewGatewayStreamClient(conn).OnData(ctx)
	if err != nil {
		conn.Close()
		return fmt.Errorf("open stream to %s: %w", address, err)
	}

	ctx2, cancel := context.WithCancel(context.Background())
	lc := &LogicConn{
		serverID: serverID,
		conn:     conn,
		stream:   stream,
		sendCh:   make(chan *protoGw.StreamData, 1024),
		gw:       s.gw,
		cancel:   cancel,
	}

	s.mu.Lock()
	s.logicClients[serverID] = lc
	s.mu.Unlock()

	go lc.sendLoop(ctx2)
	go lc.receiveLoop(ctx2)

	tlog.Info("connected to logic", "serverID", serverID, "address", address)
	return nil
}

func (s *GRPCServer) IsLogicConnected(serverID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	lc, ok := s.logicClients[serverID]
	return ok && lc.stream != nil
}

func (s *GRPCServer) SendToLogic(sess *Session, cmd int32, body []byte) {
	s.mu.RLock()
	lc, ok := s.logicClients[sess.ServerID()]
	s.mu.RUnlock()
	if !ok {
		return
	}

	msg := &protoGw.StreamData{
		SessionId: sess.ID(),
		UserKey:   sess.UserKey(),
		Cmd:       cmd,
		Data:      body,
		ClientIp:  sess.IP(),
	}
	select {
	case lc.sendCh <- msg:
	default:
		tlog.Warn("logic send channel full", "serverID", sess.ServerID())
	}
}

func (s *GRPCServer) NotifyOffline(sess *Session) {
	if !sess.IsBound() {
		return
	}
	s.mu.RLock()
	lc, ok := s.logicClients[sess.ServerID()]
	s.mu.RUnlock()
	if !ok {
		return
	}
	msg := &protoGw.StreamData{
		SessionId: sess.ID(),
		UserKey:   sess.UserKey(),
		Cmd:       CmdUserOffline,
	}
	select {
	case lc.sendCh <- msg:
	default:
	}
}

func (s *GRPCServer) getOrCreateLogicConn(serverID string) *LogicConn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logicClients[serverID]
}

// ---- LogicConn loops ----

func (lc *LogicConn) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-lc.sendCh:
			if err := lc.stream.Send(msg); err != nil {
				tlog.Error("send to logic failed", "serverID", lc.serverID, "error", err)
				return
			}
		}
	}
}

func (lc *LogicConn) receiveLoop(ctx context.Context) {
	for {
		msg, err := lc.stream.Recv()
		if err != nil {
			tlog.Info("logic receive ended", "serverID", lc.serverID, "error", err)
			return
		}
		if msg.SessionId != "" {
			lc.gw.SendToClient(msg.SessionId, msg.Cmd, msg.Data)
		}
	}
}
