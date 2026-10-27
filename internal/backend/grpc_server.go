package backend

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/internal/routes"

	"github.com/streasure/sgate/internal/connection"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/ugrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// GRPCServer gRPC 服务端，处理逻辑服的流式推送和 RPC 请求
type GRPCServer struct {
	protoGw.UnimplementedGatewayStreamServer
	protoGw.UnimplementedGatewayServer
	protoGw.UnimplementedKickServiceServer
	gateway   GatewayInterface
	pool      *LogicClientPool // flip 模式：logic 主动拨入时的附着池
	kickToken string           // KickService 鉴权 token（空=拒绝全部 Kick RPC）
}

// NewGRPCServer 创建 gRPC 服务端实例
func NewGRPCServer(gateway GatewayInterface, pool *LogicClientPool, kickToken string) *GRPCServer {
	return &GRPCServer{
		gateway:   gateway,
		pool:      pool,
		kickToken: kickToken,
	}
}

// OnData 处理来自逻辑服的流式数据。
// logic 主动拨入（flip 模式）必须携带握手元数据；缺失即拒绝（旧版
// sgate 主动拨入的命令流已移除，唯一 dialer 恒附 metadata）。
func (s *GRPCServer) OnData(stream protoGw.GatewayStream_OnDataServer) error {
	meta, ok := routes.LogicStreamMetadataFromContext(stream.Context())
	if !ok {
		return status.Error(codes.InvalidArgument, "logic stream metadata required")
	}
	return s.handleLogicStream(meta, stream)
}

// handleLogicStream flip 模式接收循环：附着分片后由本 handler 驱动
// 消息路由，流结束即返回，由 gRPC 框架回收；返回前统一解绑分片。
func (s *GRPCServer) handleLogicStream(meta routes.LogicStreamMeta, stream protoGw.GatewayStream_OnDataServer) error {
	ctx := stream.Context()
	if s.pool == nil {
		return fmt.Errorf("logic stream rejected: pool not initialized")
	}
	client, shard, err := s.pool.Attach(meta, stream)
	if err != nil {
		tlog.Warn(ctx, "logic stream attach failed serviceID=%s shard=%d/%d error=%v",
			meta.LogicID, meta.ShardIdx, meta.ShardCount, err)
		return err
	}
	defer client.detachShard(shard, stream)

	tlog.Info(ctx, "logic stream attached serviceID=%s zone=%s shard=%d/%d",
		meta.LogicID, meta.Zone, meta.ShardIdx, meta.ShardCount)

	shard.receiveMessages(client, meta.ShardIdx)
	return nil
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
	// 与 KickAccounts/KickIP 同口径：Kick 系 RPC 一律经 x-internal-token 鉴权
	if !kickTokenOK(ctx, s.kickToken) {
		return nil, errKickUnauthenticated
	}
	conn, err := s.connection(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if err := conn.Close(); err != nil {
		return nil, err
	}
	return &protoGw.KickSessionAck{}, nil
}

// KickAccounts 按登录账号批量踢本地连接（loginserver 单向 Kick 模型）
func (s *GRPCServer) KickAccounts(ctx context.Context, req *protoGw.KickAccountsRequest) (*protoGw.KickAccountsResponse, error) {
	if !kickTokenOK(ctx, s.kickToken) {
		return nil, errKickUnauthenticated
	}
	n := s.gateway.KickByAccounts(req.GetAccountIds(), req.GetReason())
	return &protoGw.KickAccountsResponse{KickedCount: int32(n)}, nil
}

// KickIP 按对端 IP 踢本地全部连接（反代场景的源 IP 由 loginserver 解析后下发）
func (s *GRPCServer) KickIP(ctx context.Context, req *protoGw.KickIPRequest) (*protoGw.KickIPResponse, error) {
	if !kickTokenOK(ctx, s.kickToken) {
		return nil, errKickUnauthenticated
	}
	n := s.gateway.KickByIP(req.GetIp(), req.GetReason())
	return &protoGw.KickIPResponse{KickedCount: int32(n)}, nil
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
func (s *GRPCServer) Broadcast(ctx context.Context, req *protoGw.BroadcastReq) (*protoGw.BroadcastAck, error) {
	var totalSent, totalFailed int
	for _, groupID := range req.GetGroupId() {
		sent, failed := s.broadcastGroup(ctx, groupID, req.GetCmd(), req.GetData())
		totalSent += sent
		totalFailed += failed
	}
	if totalSent == 0 && totalFailed > 0 {
		return nil, fmt.Errorf("broadcast failed: all %d sessions unreachable", totalFailed)
	}
	if totalFailed > 0 {
		tlog.Warn(ctx, "broadcast partial success sent=%d failed=%d", totalSent, totalFailed)
	}
	return &protoGw.BroadcastAck{}, nil
}

// BroadcastAll 向所有客户端广播消息。
// 百万连接时同步遍历会阻塞 gRPC handler 数秒：先快照再分片并发 Send
// （Send 内部走 AsyncWrite，真正写盘在 gnet loop；并发只加速遍历与入队）。
// 返回时仅保证「已分发」，不等待全部写盘完成（与 SendToClient 语义一致）。
func (s *GRPCServer) BroadcastAll(ctx context.Context, req *protoGw.BroadcastAllReq) (*protoGw.BroadcastAllAck, error) {
	data, err := encodePushMessage(req.GetCmd(), req.GetData())
	if err != nil {
		return nil, err
	}
	// 快照：避免长时间持有分片锁
	var matched []*connection.Connection
	s.gateway.GetConnectionManager().ForEach(func(conn *connection.Connection) bool {
		if conn.GetState() == connection.StateForward {
			matched = append(matched, conn)
		}
		return true
	})
	if len(matched) == 0 {
		return &protoGw.BroadcastAllAck{}, nil
	}
	// 分片并发入队（pattern 同 maintenance.kickConnectionsAsync）
	var sent, failed atomic.Int64
	var firstErr atomic.Pointer[error]
	var wg sync.WaitGroup
	const workers = 32
	total := len(matched)
	chunkSize := (total + workers - 1) / workers
	for w := range workers {
		lo := w * chunkSize
		if lo >= total {
			break
		}
		hi := min(lo+chunkSize, total)
		chunk := matched[lo:hi]
		wg.Go(func() {
			for _, conn := range chunk {
				if err := conn.Send(data); err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, &err)
				} else {
					sent.Add(1)
				}
			}
		})
	}
	wg.Wait()
	s.gateway.AddPushedToClient(sent.Load())
	if failed.Load() > 0 {
		tlog.Warn(ctx, "broadcast all partial sent=%d failed=%d total=%d", sent.Load(), failed.Load(), total)
		if e := firstErr.Load(); e != nil {
			return nil, *e
		}
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
func (s *GRPCServer) broadcastGroup(ctx context.Context, groupID string, cmd int32, data []byte) (sent int, failed int) {
	if groupID == "" {
		return 0, 1
	}
	cm := s.gateway.GetConnectionManager()
	sessions := cm.GetGroupSessions(groupID)
	// 编码提到循环外：cmd/data 全组相同，逐成员重复 Marshal 是 O(组员数) 次冗余编码。
	msg, encodeErr := encodePushMessage(cmd, data)
	if encodeErr != nil {
		tlog.Warn(ctx, "group push: encode failed groupID=%s error=%v", groupID, encodeErr)
		return 0, len(sessions)
	}
	for _, sessionID := range sessions {
		conn := cm.GetConnection(sessionID)
		if conn == nil {
			failed++
			tlog.Warn(ctx, "group push: session disappeared groupID=%s sessionID=%s", groupID, sessionID)
			continue
		}
		if err := conn.Send(msg); err != nil {
			failed++
			tlog.Warn(ctx, "group push: send failed groupID=%s sessionID=%s error=%v", groupID, sessionID, err)
			continue
		}
		sent++
		s.gateway.AddPushedToClient(1)
	}
	if failed > 0 {
		tlog.Warn(ctx, "group push completed with failures groupID=%s total=%d sent=%d failed=%d", groupID, len(sessions), sent, failed)
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

// KickService 鉴权：x-internal-token metadata 常量时间比较；
// 未配置 token 时拒绝全部 Kick RPC（默认关闭）。
var errKickUnauthenticated = status.Error(codes.Unauthenticated, "token required")

func kickTokenOK(ctx context.Context, expected string) bool {
	if expected == "" {
		return false
	}
	md, _ := metadata.FromIncomingContext(ctx)
	got := md.Get("x-internal-token")
	return len(got) > 0 && subtle.ConstantTimeCompare([]byte(got[0]), []byte(expected)) == 1
}

// StartGRPCServer 启动 gRPC 服务器，监听指定端口。
// keepalive：logic 作为拨出侧以 30s 间隔发送 keepalive ping，
// 服务端 EnforcementPolicy 必须放宽到 10s 以内，否则会被 GOAWAY 断开；
// ServerParameters 让本侧也能主动探测拨出方失联。
func StartGRPCServer(gw GatewayInterface, pool *LogicClientPool, port string, maxMsgSize int, windowSize int, kickToken string) (*ugrpc.Server, error) {
	if maxMsgSize <= 0 {
		maxMsgSize = 4 * 1024 * 1024
	}
	if windowSize <= 0 {
		windowSize = 524288
	}
	tlog.Info(context.Background(), "creating gRPC server")
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
	tlog.Info(context.Background(), "registering GatewayService")
	grpcService := NewGRPCServer(gw, pool, kickToken)
	protoGw.RegisterGatewayStreamServer(server, grpcService)
	protoGw.RegisterGatewayServer(server, grpcService)
	protoGw.RegisterKickServiceServer(server, grpcService)

	if err := server.Start(); err != nil {
		tlog.Error(context.Background(), "gRPC server failed to start error=%v port=%s", err, port)
		return nil, err
	}

	return server, nil
}
