package backend

import (
	"context"
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/sgate/internal/routes"
	"google.golang.org/grpc/metadata"
)

// grpcFakeGateway 带真实 ConnectionManager + Kick 计数的 gateway stub。
type grpcFakeGateway struct {
	cm            *connection.ConnectionManager
	kickAccounts  int
	kickIP        int
	kickReason    string
	kickAccountID string
	kickIPAddr    string
}

func newGRPCFakeGateway() *grpcFakeGateway {
	return &grpcFakeGateway{cm: connection.NewConnectionManager(100, 10)}
}

func (f *grpcFakeGateway) GetConnectionManager() *connection.ConnectionManager { return f.cm }
func (f *grpcFakeGateway) GetStreamConfig() config.StreamConfig                { return config.StreamConfig{} }
func (f *grpcFakeGateway) GetGRPCConfig() config.GRPCConfig                    { return config.GRPCConfig{} }
func (f *grpcFakeGateway) GetGatewayID() string                                { return "gw-test" }
func (f *grpcFakeGateway) GetServerID() string                                 { return "gw-test" }
func (f *grpcFakeGateway) AddPushedToClient(int64)                             {}
func (f *grpcFakeGateway) AddPushDroppedNoConn(int64)                          {}
func (f *grpcFakeGateway) GetLogicClient(string) connection.LogicClientProvider {
	return nil
}
func (f *grpcFakeGateway) GetShardedCoalescer() *connection.ShardedWriteCoalescer { return nil }
func (f *grpcFakeGateway) KickByAccounts(ids []string, reason string) int {
	f.kickAccounts++
	f.kickReason = reason
	if len(ids) > 0 {
		f.kickAccountID = ids[0]
	}
	return len(ids)
}
func (f *grpcFakeGateway) KickByIP(ip, reason string) int {
	f.kickIP++
	f.kickIPAddr = ip
	f.kickReason = reason
	return 1
}

func tokenCtx(token string) context.Context {
	if token == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-internal-token", token))
}

func TestKickAccountsAuthAndDispatch(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "sekrit")

	// 无 token → Unauthenticated
	if _, err := srv.KickAccounts(context.Background(), &protoGw.KickAccountsRequest{
		AccountIds: []string{"acc1"},
	}); err == nil {
		t.Fatal("KickAccounts without token must fail")
	}

	// 错 token
	if _, err := srv.KickAccounts(tokenCtx("bad"), &protoGw.KickAccountsRequest{
		AccountIds: []string{"acc1"},
	}); err == nil {
		t.Fatal("KickAccounts with wrong token must fail")
	}

	// 正确 token → 调用 gateway
	ack, err := srv.KickAccounts(tokenCtx("sekrit"), &protoGw.KickAccountsRequest{
		AccountIds: []string{"acc1", "acc2"},
		Reason:     "ban",
	})
	if err != nil {
		t.Fatalf("KickAccounts: %v", err)
	}
	if ack.GetKickedCount() != 2 {
		t.Fatalf("kicked = %d, want 2", ack.GetKickedCount())
	}
	if gw.kickAccounts != 1 || gw.kickAccountID != "acc1" || gw.kickReason != "ban" {
		t.Fatalf("gateway kick not dispatched: %+v", gw)
	}
}

func TestKickIPAuthAndDispatch(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "sekrit")

	if _, err := srv.KickIP(context.Background(), &protoGw.KickIPRequest{Ip: "1.2.3.4"}); err == nil {
		t.Fatal("KickIP without token must fail")
	}
	ack, err := srv.KickIP(tokenCtx("sekrit"), &protoGw.KickIPRequest{Ip: "1.2.3.4", Reason: "flood"})
	if err != nil {
		t.Fatalf("KickIP: %v", err)
	}
	if ack.GetKickedCount() != 1 {
		t.Fatalf("kicked = %d, want 1", ack.GetKickedCount())
	}
	if gw.kickIP != 1 || gw.kickIPAddr != "1.2.3.4" {
		t.Fatalf("gateway KickByIP not called: %+v", gw)
	}
}

func TestConnectionLookupErrors(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "")

	if _, err := srv.connection(""); err == nil {
		t.Fatal("empty session must error")
	}
	if _, err := srv.connection("nope"); err == nil {
		t.Fatal("missing session must error")
	}

	// CloseSession / KickSession 不存在会话
	if _, err := srv.CloseSession(context.Background(), &protoGw.CloseSessionReq{SessionId: "x"}); err == nil {
		t.Fatal("CloseSession missing must error")
	}
	if _, err := srv.KickSession(context.Background(), &protoGw.KickSessionReq{SessionId: "x"}); err == nil {
		t.Fatal("KickSession missing must error")
	}
}

func TestBroadcastEmptyGroupFails(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "")
	// 空 groupID → failed=1，totalSent=0 → 整体失败
	if _, err := srv.Broadcast(context.Background(), &protoGw.BroadcastReq{
		GroupId: []string{""},
		Cmd:     1,
		Data:    []byte("x"),
	}); err == nil {
		t.Fatal("broadcast empty group must fail")
	}
}

func TestEncodePushMessage(t *testing.T) {
	data, err := encodePushMessage(42, []byte("hi"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty frame")
	}
}

// ---- pool ----

func TestPoolAttachLifecycle(t *testing.T) {
	gw := newGRPCFakeGateway()
	pool := NewLogicClientPool(gw)

	meta := routes.LogicStreamMeta{
		LogicID:    "logic-1",
		Zone:       "z1",
		Addr:       "127.0.0.1:50051",
		ShardIdx:   0,
		ShardCount: 2,
	}
	fs0 := &fakeShardStream{}
	client, shard0, err := pool.Attach(meta, fs0)
	if err != nil {
		t.Fatalf("attach 0: %v", err)
	}
	if client == nil || shard0 == nil {
		t.Fatal("nil client/shard")
	}
	if pool.ClientCount() != 1 {
		t.Fatalf("client count = %d, want 1", pool.ClientCount())
	}
	if pool.ZoneOf("logic-1") != "z1" {
		t.Fatalf("zone = %q", pool.ZoneOf("logic-1"))
	}
	if pool.IsClientConnected("logic-1") {
		t.Fatal("not fully attached yet")
	}

	// 第二片 → Connected
	fs1 := &fakeShardStream{}
	meta.ShardIdx = 1
	if _, _, err := pool.Attach(meta, fs1); err != nil {
		t.Fatalf("attach 1: %v", err)
	}
	if !pool.IsClientConnected("logic-1") {
		t.Fatal("want connected after full attach")
	}
	if !pool.IsConnected() {
		t.Fatal("pool.IsConnected want true")
	}

	// GetClient 已连接
	if pool.GetClient("logic-1") == nil {
		t.Fatal("GetClient nil")
	}

	// 分片总数变化 → 重建
	meta.ShardCount = 1
	meta.ShardIdx = 0
	fsNew := &fakeShardStream{}
	client2, _, err := pool.Attach(meta, fsNew)
	if err != nil {
		t.Fatalf("attach rebuild: %v", err)
	}
	if client2 == client {
		t.Fatal("expected new client after shard count change")
	}

	// 越界分片
	meta.ShardIdx = 5
	meta.ShardCount = 1
	if _, _, err := pool.Attach(meta, &fakeShardStream{}); err == nil {
		t.Fatal("out-of-range shard must fail")
	}

	pool.Close()
	// Close 后拒绝接入
	if _, _, err := pool.Attach(routes.LogicStreamMeta{LogicID: "x", ShardCount: 1}, &fakeShardStream{}); err != ErrConnectionClosing {
		t.Fatalf("attach after close = %v, want ErrConnectionClosing", err)
	}
	if pool.ClientCount() != 0 {
		t.Fatalf("clients after close = %d", pool.ClientCount())
	}
}

func TestPoolRegisterAndFastClient(t *testing.T) {
	gw := newGRPCFakeGateway()
	pool := NewLogicClientPool(gw)

	lc := NewLogicClient(gw)
	lc.initAccepted(1)
	pool.RegisterClient("a", lc)
	if pool.fastClient.Load() == nil {
		t.Fatal("single client should set fastClient")
	}
	lc2 := NewLogicClient(gw)
	lc2.initAccepted(1)
	pool.RegisterClient("b", lc2)
	if pool.fastClient.Load() != nil {
		t.Fatal("multi client should clear fastClient")
	}
	// 空 serverID / nil 忽略
	pool.RegisterClient("", lc)
	pool.RegisterClient("c", nil)
	if pool.ClientCount() != 2 {
		t.Fatalf("count = %d, want 2", pool.ClientCount())
	}
	pool.Close()
}
