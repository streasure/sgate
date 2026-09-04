package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type logicServer struct {
	protoGw.UnimplementedGatewayStreamServer
	protoGw.UnimplementedGatewayServer

	mu       sync.RWMutex
	sessions map[string]string
	gwConn   *grpc.ClientConn
	gwClient protoGw.GatewayClient
}

func (s *logicServer) connectGateway() {
	if s.gwClient != nil {
		return
	}
	gwAddr := "localhost:50051"
	if v := os.Getenv("GATEWAY_GRPC_ADDR"); v != "" {
		gwAddr = v
	}
	conn, err := grpc.NewClient(gwAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		tlog.Error("connect gateway failed", "addr", gwAddr, "error", err)
		return
	}
	s.gwConn = conn
	s.gwClient = protoGw.NewGatewayClient(conn)
	tlog.Info("connected to gateway for service calls", "addr", gwAddr)
}

func (s *logicServer) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	gatewayID := "unknown"
	if ok {
		if ids := md.Get("sgate-server-id"); len(ids) > 0 {
			gatewayID = ids[0]
		}
	}
	tlog.Info("gateway connected", "gatewayID", gatewayID)

	s.connectGateway()

	for {
		msg, err := stream.Recv()
		if err != nil {
			tlog.Info("gateway stream ended", "gatewayID", gatewayID, "error", err)
			return err
		}
		s.handleMessage(stream, msg)
	}
}

func (s *logicServer) handleMessage(stream protoGw.GatewayStream_OnDataServer, msg *protoGw.StreamData) {
	switch msg.Cmd {
	case 1100001: // CMD_LOGIN_REQ
		var req protoLogic.LoginReq
		if err := proto.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		userID := req.GetUserId()
		if userID == "" {
			userID = msg.SessionId
		}
		userKey := "user_" + userID

		s.mu.Lock()
		s.sessions[msg.SessionId] = userKey
		s.mu.Unlock()

		// Auto-join "bench_group" for group push benchmark
		if s.gwClient != nil {
			s.gwClient.JoinGroup(context.Background(), &protoGw.JoinGroupReq{
				SessionId: msg.SessionId,
				GroupId:   []string{"bench_group"},
			})
		}

		ack := &protoLogic.LoginAck{
			UserKey:    userKey,
			ServerTime: time.Now().UnixMilli(),
		}
		data, _ := proto.Marshal(ack)
		stream.Send(&protoGw.StreamData{
			SessionId: msg.SessionId,
			UserKey:   userKey,
			Cmd:       1100002,
			Data:      data,
		})

	case 1100010: // CMD_HEARTBEAT_REQ
		var req protoLogic.HeartbeatReq
		if err := proto.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		now := time.Now().UnixMilli()
		ack := &protoLogic.HeartbeatAck{
			ServerTime: now,
			RttMs:      now - req.GetClientTime(),
		}
		data, _ := proto.Marshal(ack)
		stream.Send(&protoGw.StreamData{
			SessionId: msg.SessionId,
			UserKey:   msg.UserKey,
			Cmd:       1100011,
			Data:      data,
		})

	case 1100008: // CMD_CHAT_MSG
		var req protoLogic.ChatMsg
		if err := proto.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		if s.gwClient != nil {
			respData, _ := proto.Marshal(&req)
			if req.GetTargetId() != "" {
				// Auto-join sender to target group, then broadcast
				s.gwClient.JoinGroup(context.Background(), &protoGw.JoinGroupReq{
					SessionId: msg.SessionId,
					GroupId:   []string{req.GetTargetId()},
				})
				s.gwClient.Broadcast(context.Background(), &protoGw.BroadcastReq{
					GroupId: []string{req.GetTargetId()},
					Cmd:     1100008,
					Data:    respData,
				})
			} else {
				s.gwClient.BroadcastAll(context.Background(), &protoGw.BroadcastAllReq{
					Cmd:  1100008,
					Data: respData,
				})
			}
		}

	case 1100012: // CMD_USER_OFFLINE_NTF
		s.mu.Lock()
		delete(s.sessions, msg.SessionId)
		s.mu.Unlock()

	default:
		stream.Send(&protoGw.StreamData{
			SessionId: msg.SessionId,
			UserKey:   msg.UserKey,
			Cmd:       msg.Cmd,
			Data:      msg.Data,
		})
	}
}

func (s *logicServer) CloseSession(ctx context.Context, req *protoGw.CloseSessionReq) (*protoGw.CloseSessionAck, error) {
	return &protoGw.CloseSessionAck{}, nil
}
func (s *logicServer) KickSession(ctx context.Context, req *protoGw.KickSessionReq) (*protoGw.KickSessionAck, error) {
	return &protoGw.KickSessionAck{}, nil
}
func (s *logicServer) SendToClient(ctx context.Context, req *protoGw.SendToClientReq) (*protoGw.SendToClientAck, error) {
	return &protoGw.SendToClientAck{}, nil
}
func (s *logicServer) Broadcast(ctx context.Context, req *protoGw.BroadcastReq) (*protoGw.BroadcastAck, error) {
	return &protoGw.BroadcastAck{}, nil
}
func (s *logicServer) BroadcastAll(ctx context.Context, req *protoGw.BroadcastAllReq) (*protoGw.BroadcastAllAck, error) {
	return &protoGw.BroadcastAllAck{}, nil
}
func (s *logicServer) JoinGroup(ctx context.Context, req *protoGw.JoinGroupReq) (*protoGw.JoinGroupAck, error) {
	return &protoGw.JoinGroupAck{Code: 0}, nil
}
func (s *logicServer) LeaveGroup(ctx context.Context, req *protoGw.LeaveGroupReq) (*protoGw.LeaveGroupAck, error) {
	return &protoGw.LeaveGroupAck{Code: 0}, nil
}
func (s *logicServer) GetGroupInfo(ctx context.Context, req *protoGw.GetGroupInfoReq) (*protoGw.GetGroupInfoAck, error) {
	return &protoGw.GetGroupInfoAck{}, nil
}

func main() {
	addr := ":50052"
	if v := os.Getenv("LOGIC_PORT"); v != "" {
		addr = ":" + v
	}

	if _, err := tlog.New("config/tlog.yaml"); err != nil {
		if _, err := tlog.New("../config/tlog.yaml"); err != nil {
			tlog.New("")
		}
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen failed: %v\n", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	s := &logicServer{sessions: make(map[string]string)}
	protoGw.RegisterGatewayStreamServer(srv, s)
	protoGw.RegisterGatewayServer(srv, s)

	tlog.Info("logic server started", "addr", addr)
	if err := srv.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "serve failed: %v\n", err)
		os.Exit(1)
	}
}
