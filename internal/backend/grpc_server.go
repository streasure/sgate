package backend

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/streasure/sgate/internal/routes"

	"github.com/streasure/sgate/internal/connection"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/ugrpc"
	"google.golang.org/protobuf/proto"
)

// GRPCServer gRPC 服务端，处理逻辑服的流式推送和 RPC 请求
type GRPCServer struct {
	protoGw.UnimplementedGatewayStreamServer
	protoGw.UnimplementedGatewayServer
	gateway GatewayInterface
	mu      sync.Mutex
}

// NewGRPCServer 创建 gRPC 服务端实例
func NewGRPCServer(gateway GatewayInterface) *GRPCServer {
	return &GRPCServer{
		gateway: gateway,
	}
}

// OnData 处理来自逻辑服的流式数据（服务端流 RPC）
func (s *GRPCServer) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	connectionID := connection.GenerateConnectionID()

	ctx := map[string]interface{}{
		"connection_id": connectionID,
		"stream":        stream,
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		s.handleGRPCMessage(connectionID, msg, func(response interface{}) {
			if protoMsg, ok := response.(*protoGw.StreamData); ok {
				if err := stream.Send(protoMsg); err != nil {
					tlog.Warn(context.TODO(), "OnData: stream.Send failed error=%v", err)
				}
			} else if errorMsg, ok := response.(*commonstruct.ErrorResponse); ok {
				responseMsg := &protoGw.StreamData{
					Data: []byte(errorMsg.Error.Message),
				}
				if err := stream.Send(responseMsg); err != nil {
					tlog.Warn(context.TODO(), "OnData: stream.Send error response failed error=%v", err)
				}
			}
		}, ctx)
	}
}

// connection 根据会话 ID 获取客户端连接
func (s *GRPCServer) connection(sessionID string) (*connection.Connection, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	conn := s.gateway.GetConnectionManager().GetConnection(sessionID)
	if conn == nil {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	return conn, nil
}

// CloseSession 关闭指定会话的客户端连接
func (s *GRPCServer) CloseSession(_ context.Context, req *protoGw.CloseSessionReq) (*protoGw.CloseSessionAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if err := conn.Close(); err != nil {
		return nil, err
	}
	return &protoGw.CloseSessionAck{}, nil
}

// KickSession 踢出会话，强制关闭客户端连接
func (s *GRPCServer) KickSession(ctx context.Context, req *protoGw.KickSessionReq) (*protoGw.KickSessionAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if err := conn.Close(); err != nil {
		return nil, err
	}
	return &protoGw.KickSessionAck{}, nil
}

// SendToClient 向指定会话推送消息
func (s *GRPCServer) SendToClient(_ context.Context, req *protoGw.SendToClientReq) (*protoGw.SendToClientAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	data, err := encodePushMessage(req.GetCmd(), req.GetData())
	if err != nil {
		return nil, err
	}
	if err := conn.Send(data); err != nil {
		return nil, err
	}
	s.gateway.AddPushedToClient(1)
	return &protoGw.SendToClientAck{}, nil
}

// Broadcast 向指定分组广播消息
func (s *GRPCServer) Broadcast(_ context.Context, req *protoGw.BroadcastReq) (*protoGw.BroadcastAck, error) {
	var totalSent, totalFailed int
	for _, groupID := range req.GetGroupId() {
		sent, failed := s.broadcastGroup(groupID, req.GetCmd(), req.GetData())
		totalSent += sent
		totalFailed += failed
	}
	if totalSent == 0 && totalFailed > 0 {
		return nil, fmt.Errorf("broadcast failed: all %d sessions unreachable", totalFailed)
	}
	if totalFailed > 0 {
		tlog.Warn(context.TODO(), "broadcast partial success sent=%d failed=%d", totalSent, totalFailed)
	}
	return &protoGw.BroadcastAck{}, nil
}

// BroadcastAll 向所有客户端广播消息
func (s *GRPCServer) BroadcastAll(_ context.Context, req *protoGw.BroadcastAllReq) (*protoGw.BroadcastAllAck, error) {
	data, err := encodePushMessage(req.GetCmd(), req.GetData())
	if err != nil {
		return nil, err
	}
	var firstErr error
	s.gateway.GetConnectionManager().ForEach(func(conn *connection.Connection) bool {
		if err := conn.Send(data); err != nil && firstErr == nil {
			firstErr = err
		} else if err == nil {
			s.gateway.AddPushedToClient(1)
		}
		return true
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return &protoGw.BroadcastAllAck{}, nil
}

// JoinGroup 将会话加入指定分组
func (s *GRPCServer) JoinGroup(_ context.Context, req *protoGw.JoinGroupReq) (*protoGw.JoinGroupAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	serverID := conn.GetServerID()
	userUUID := conn.GetUserUUID()
	counts := make([]int32, len(req.GetGroupId()))
	for i, groupID := range req.GetGroupId() {
		s.gateway.GetConnectionManager().AddUserToGroup(groupID, serverID, userUUID)
		counts[i] = int32(s.gateway.GetConnectionManager().GetGroupMemberCount(groupID))
	}
	return &protoGw.JoinGroupAck{Code: 0, MemberCount: counts}, nil
}

// LeaveGroup 将会话移出指定分组
func (s *GRPCServer) LeaveGroup(_ context.Context, req *protoGw.LeaveGroupReq) (*protoGw.LeaveGroupAck, error) {
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	serverID := conn.GetServerID()
	userUUID := conn.GetUserUUID()
	counts := make([]int32, len(req.GetGroupId()))
	for i, groupID := range req.GetGroupId() {
		s.gateway.GetConnectionManager().RemoveUserFromGroup(groupID, serverID, userUUID)
		counts[i] = int32(s.gateway.GetConnectionManager().GetGroupMemberCount(groupID))
	}
	return &protoGw.LeaveGroupAck{Code: 0, MemberCount: counts}, nil
}

// GetGroupInfo 获取分组信息，包括成员数量和会话列表
func (s *GRPCServer) GetGroupInfo(_ context.Context, req *protoGw.GetGroupInfoReq) (*protoGw.GetGroupInfoAck, error) {
	cm := s.gateway.GetConnectionManager()
	return &protoGw.GetGroupInfoAck{
		GroupId:     req.GetGroupId(),
		MemberCount: int32(cm.GetGroupMemberCount(req.GetGroupId())),
		SessionIds:  cm.GetGroupSessions(req.GetGroupId()),
	}, nil
}

// broadcastGroup 向指定分组的所有成员推送消息
func (s *GRPCServer) broadcastGroup(groupID string, cmd int32, data []byte) (sent int, failed int) {
	if groupID == "" {
		return 0, 1
	}
	cm := s.gateway.GetConnectionManager()
	sessions := cm.GetGroupSessions(groupID)
	for _, sessionID := range sessions {
		conn := cm.GetConnection(sessionID)
		if conn == nil {
			failed++
			tlog.Warn(context.TODO(), "group push: session disappeared groupID=%s sessionID=%s", groupID, sessionID)
			continue
		}
		msg, encodeErr := encodePushMessage(cmd, data)
		if encodeErr != nil {
			failed++
			tlog.Warn(context.TODO(), "group push: encode failed groupID=%s error=%v", groupID, encodeErr)
			continue
		}
		if err := conn.Send(msg); err != nil {
			failed++
			tlog.Warn(context.TODO(), "group push: send failed groupID=%s sessionID=%s error=%v", groupID, sessionID, err)
			continue
		}
		sent++
		s.gateway.AddPushedToClient(1)
	}
	if failed > 0 {
		tlog.Warn(context.TODO(), "group push completed with failures groupID=%s total=%d sent=%d failed=%d", groupID, len(sessions), sent, failed)
	}
	return sent, failed
}

// encodePushMessage 编码推送消息为字节流
func encodePushMessage(cmd int32, data []byte) ([]byte, error) {
	msg, err := proto.Marshal(&protoGw.MessageFrame{Cmd: cmd, Body: data})
	if err != nil {
		return nil, fmt.Errorf("marshal push message: %w", err)
	}
	return msg, nil
}

// SendMessage 处理来自逻辑服的单条消息请求（Unary RPC）
func (s *GRPCServer) SendMessage(ctx context.Context, msg *protoGw.StreamData) (*protoGw.StreamData, error) {
	connectionID := connection.GenerateConnectionID()

	grpcCtx := map[string]interface{}{
		"connection_id": connectionID,
		"context":       ctx,
	}

	var response *protoGw.StreamData
	var wg sync.WaitGroup
	wg.Add(1)

	s.handleGRPCMessage(connectionID, msg, func(resp interface{}) {
		defer wg.Done()
		if protoMsg, ok := resp.(*protoGw.StreamData); ok {
			response = protoMsg
		} else if errorMsg, ok := resp.(*commonstruct.ErrorResponse); ok {
			response = &protoGw.StreamData{
				Data: []byte(errorMsg.Error.Message),
			}
		}
	}, grpcCtx)

	wg.Wait()
	if response == nil {
		return nil, fmt.Errorf("no response from message handler")
	}
	return response, nil
}

// handleGRPCMessage 处理 gRPC 消息，返回错误提示（网关不直接处理命令）
func (s *GRPCServer) handleGRPCMessage(connectionID string, msg *protoGw.StreamData, callback func(interface{}), ctx map[string]interface{}) {
	if msg.Cmd == 0 {
		callback(routes.NewErrorResponse("error", "Missing cmd", "", ""))
		return
	}
	callback(routes.NewErrorResponse("error", "Gateway does not handle commands locally, forward to logic server", "", ""))
}

// StartGRPCServer 启动 gRPC 服务器，监听指定端口
func StartGRPCServer(gw GatewayInterface, port string, maxMsgSize int, windowSize int) (*ugrpc.Server, error) {
	if maxMsgSize <= 0 {
		maxMsgSize = 4 * 1024 * 1024
	}
	if windowSize <= 0 {
		windowSize = 524288
	}
	tlog.Info(context.TODO(), "creating gRPC server")
	server := ugrpc.NewServer(
		ugrpc.WithAddr(port),
		ugrpc.WithMaxRecvMsgSize(maxMsgSize),
		ugrpc.WithMaxSendMsgSize(maxMsgSize),
		ugrpc.WithWindowSize(windowSize),
		ugrpc.WithGracefulStopTimeout(5*time.Second),
	)
	tlog.Info(context.TODO(), "registering GatewayService")
	grpcService := NewGRPCServer(gw)
	protoGw.RegisterGatewayStreamServer(server, grpcService)
	protoGw.RegisterGatewayServer(server, grpcService)

	if err := server.Start(); err != nil {
		tlog.Error(context.TODO(), "gRPC server failed to start error=%v port=%s", err, port)
		return nil, err
	}

	return server, nil
}
