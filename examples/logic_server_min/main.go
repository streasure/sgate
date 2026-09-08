package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
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
		if ids := md.Get("sgate-gateway-id"); len(ids) > 0 {
			gatewayID = ids[0]
		} else if ids := md.Get("sgate-server-id"); len(ids) > 0 {
			gatewayID = ids[0]
		}
	}
	tlog.Info("gateway connected", "gatewayID", gatewayID)

	s.connectGateway()

	for {
		msg, err := stream.Recv()
		if err != nil {
			tlog.Debug("gateway stream ended", "gatewayID", gatewayID, "error", err)
			return err
		}
		s.handleMessage(stream, msg)
	}
}

func (s *logicServer) handleMessage(stream protoGw.GatewayStream_OnDataServer, msg *protoGw.StreamData) {
	if err := stream.Send(&protoGw.StreamData{
		SessionId: msg.SessionId,
		UserKey:   msg.UserKey,
		Cmd:       msg.Cmd,
		SeqId:     msg.SeqId,
		Data:      msg.Data,
		ClientIp:  msg.ClientIp,
	}); err != nil {
		tlog.Debug("echo stream send ended", "error", err)
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
