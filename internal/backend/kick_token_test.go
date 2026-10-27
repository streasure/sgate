package backend

import (
	"context"
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/grpc/metadata"
)

func TestKickTokenOK(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-internal-token", "secret"))

	if !kickTokenOK(ctx, "secret") {
		t.Fatal("matching token should pass")
	}
	if kickTokenOK(ctx, "wrong") {
		t.Fatal("wrong token should fail")
	}
	if kickTokenOK(ctx, "") {
		t.Fatal("empty expected token should fail (kick disabled)")
	}
	if kickTokenOK(context.Background(), "secret") {
		t.Fatal("missing metadata should fail")
	}
}

// TestKickSessionRequiresToken KickSession 与 KickAccounts/KickIP 同口径：
// 无 token 一律鉴权拒绝（不触达会话查找），带 token 才进入会话查找。
func TestKickSessionRequiresToken(t *testing.T) {
	srv := NewGRPCServer(newGRPCFakeGateway(), nil, "sekrit")

	if _, err := srv.KickSession(context.Background(), &protoGw.KickSessionReq{SessionId: "x"}); err != errKickUnauthenticated {
		t.Fatalf("KickSession without token: err=%v, want errKickUnauthenticated", err)
	}
	// 带 token → 通过鉴权，因会话不存在在查找处报错（非鉴权错误）
	if _, err := srv.KickSession(tokenCtx("sekrit"), &protoGw.KickSessionReq{SessionId: "x"}); err == nil || err == errKickUnauthenticated {
		t.Fatalf("KickSession with token (missing session): err=%v, want lookup error", err)
	}
}
