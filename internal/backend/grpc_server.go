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
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

// GRPCServer gRPC 服务端，处理逻辑服的流式推送和 RPC 请求
type GRPCServer struct {
	protoGw.UnimplementedGatewayStreamServer
	protoGw.UnimplementedGatewayServer
	gateway GatewayInterface
	pool    *LogicClientPool // flip 模式：logic 主动拨入时的附着池
}

// NewGRPCServer 创建 gRPC 服务端实例
func NewGRPCServer(gateway GatewayInterface, pool *LogicClientPool) *GRPCServer {
	return &GRPCServer{
		gateway: gateway,
		pool:    pool,
	}
}

// OnData 处理来自逻辑服的流式数据。
// 带逻辑服握手元数据（logic 主动拨入，flip 模式）时附着到连接池；
// 否则回落到旧版握手（sgate 主动拨入的命令流）。
func (s *GRPCServer) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	if meta, ok := routes.LogicStreamMetadataFromContext(stream.Context()); ok {
		return s.handleLogicStream(meta, stream)
	}
	return s.handleLegacyStream(stream)
}

// handleLogicStream flip 模式接收循环：附着分片后由本 handler 驱动
// 消息路由，流结束即返回，由 gRPC 框架回收；返回前统一解绑分片。
func (s *GRPCServer) handleLogicStream(meta routes.LogicStreamMeta, stream protoGw.GatewayStream_OnDataServer) error {
	if s.pool == nil {
		return fmt.Errorf("logic stream rejected: pool not initialized")
	}
	client, shard, err := s.pool.Attach(meta, stream)
	if err != nil {
		tlog.Warn(context.TODO(), "logic stream attach failed serviceID=%s shard=%d/%d error=%v",
			meta.LogicID, meta.ShardIdx, meta.ShardCount, err)
		return err
	}
	defer client.detachShard(shard, stream)

	tlog.Info(context.TODO(), "logic stream attached serviceID=%s zone=%s shard=%d/%d",
		meta.LogicID, meta.Zone, meta.ShardIdx, meta.ShardCount)

	shard.receiveMessages(client, meta.ShardIdx)
	return nil
}

// sendLegacyFrame 向旧版握手流发送单条消息（StreamBatch 为固定线格式，
// 接收端固定按 StreamBatch 解码，绝不能混发裸帧）。
func sendLegacyFrame(stream interface {
	SendMsg(m any) error
}, msg *protoGw.StreamData) error {
	return stream.SendMsg(&protoGw.StreamBatch{Items: []*protoGw.StreamData{msg}})
}

// handleLegacyStream 旧版握手：sgate 主动拨入时的命令流处理。
func (s *GRPCServer) handleLegacyStream(stream protoGw.GatewayStream_OnDataServer) error {
	connectionID := connection.GenerateConnectionID()

	ctx := map[string]any{
		"connection_id": connectionID,
		"stream":        stream,
	}

	process := func(msg *protoGw.StreamData) {
		s.handleGRPCMessage(connectionID, msg, func(response any) {
			if protoMsg, ok := response.(*protoGw.StreamData); ok {
				if err := sendLegacyFrame(stream, protoMsg); err != nil {
					tlog.Warn(context.TODO(), "OnData: stream.Send failed error=%v", err)
				}
			} else if errorMsg, ok := response.(*commonstruct.ErrorResponse); ok {
				responseMsg := &protoGw.StreamData{
					Data: []byte(errorMsg.Error.Message),
				}
				if err := sendLegacyFrame(stream, responseMsg); err != nil {
					tlog.Warn(context.TODO(), "OnData: stream.Send error response failed error=%v", err)
				}
			}
		}, ctx)
	}

	// 合帧接收缓冲：循环外复用，避免每帧分配 StreamBatch。
	sb := &protoGw.StreamBatch{}

	for {
		sb.Reset()
		if err := stream.RecvMsg(sb); err != nil {
			return err
		}
		for _, m := range sb.Items {
			if m == nil {
				continue
			}
			process(m)
		}
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

	grpcCtx := map[string]any{
		"connection_id": connectionID,
		"context":       ctx,
	}

	var response *protoGw.StreamData
	var wg sync.WaitGroup
	wg.Add(1)

	s.handleGRPCMessage(connectionID, msg, func(resp any) {
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
func (s *GRPCServer) handleGRPCMessage(connectionID string, msg *protoGw.StreamData, callback func(any), ctx map[string]any) {
	if msg.Cmd == 0 {
		callback(routes.NewErrorResponse("error", "Missing cmd", "", ""))
		return
	}
	callback(routes.NewErrorResponse("error", "Gateway does not handle commands locally, forward to logic server", "", ""))
}

// StartGRPCServer 启动 gRPC 服务器，监听指定端口。
// keepalive：logic 作为拨出侧以 30s 间隔发送 keepalive ping，
// 服务端 EnforcementPolicy 必须放宽到 10s 以内，否则会被 GOAWAY 断开；
// ServerParameters 让本侧也能主动探测拨出方失联。
func StartGRPCServer(gw GatewayInterface, pool *LogicClientPool, port string, maxMsgSize int, windowSize int) (*ugrpc.Server, error) {
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
		ugrpc.WithGrpcOptions(
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second,
				PermitWithoutStream: true,
			}),
			grpc.KeepaliveParams(keepalive.ServerParameters{
				Time:    30 * time.Second,
				Timeout: 10 * time.Second,
			}),
		),
	)
	tlog.Info(context.TODO(), "registering GatewayService")
	grpcService := NewGRPCServer(gw, pool)
	protoGw.RegisterGatewayStreamServer(server, grpcService)
	protoGw.RegisterGatewayServer(server, grpcService)

	if err := server.Start(); err != nil {
		tlog.Error(context.TODO(), "gRPC server failed to start error=%v port=%s", err, port)
		return nil, err
	}

	return server, nil
}
