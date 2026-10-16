package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/streasure/sgate/internal/connection"

	protoLogin "github.com/streasure/protocol/loginserver"
	"google.golang.org/grpc"
)

// mockLoginServiceClient 按 accountId 固定返回 valid 结果的 LoginServiceClient。
type mockLoginServiceClient struct {
	protoLogin.LoginServiceClient
	validByAccount map[string]bool
	batchCalls     atomic.Int32
	defaultValid   bool
}

func (m *mockLoginServiceClient) BatchValidateLoginTokens(
	_ context.Context,
	in *protoLogin.BatchValidateLoginTokensReq,
	_ ...grpc.CallOption,
) (*protoLogin.BatchValidateLoginTokensAck, error) {
	m.batchCalls.Add(1)
	valids := make([]bool, len(in.Items))
	for i, item := range in.Items {
		if v, ok := m.validByAccount[item.AccountId]; ok {
			valids[i] = v
		} else {
			valids[i] = m.defaultValid
		}
	}
	return &protoLogin.BatchValidateLoginTokensAck{Valid: valids}, nil
}

func TestParseBatchInterval(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-5s", 0},
		{"bogus", 0},
		{"30s", 30 * time.Second},
		{"1m", time.Minute},
	}
	for _, c := range cases {
		if got := parseBatchInterval(c.in); got != c.want {
			t.Errorf("parseBatchInterval(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestSetConnectionLoginKey(t *testing.T) {
	g := newKickTestGateway()
	mc := &kickMockConn{}
	connID := g.connectionManager.AddConnection(mc, "")
	g.connectionManager.SetConnectionLoginKey(connID, "tk-abc")
	conn := g.connectionManager.GetConnection(connID)
	if conn == nil {
		t.Fatal("conn missing")
	}
	if conn.GetLoginKey() != "tk-abc" {
		t.Fatalf("loginKey=%q want tk-abc", conn.GetLoginKey())
	}
	// 空 key 不覆盖
	g.connectionManager.SetConnectionLoginKey(connID, "")
	if conn.GetLoginKey() != "tk-abc" {
		t.Fatalf("empty key overwrote loginKey=%q", conn.GetLoginKey())
	}
}

func TestRunBatchValidateSweepKicksInvalid(t *testing.T) {
	g := newKickTestGateway()
	mock := &mockLoginServiceClient{
		validByAccount: map[string]bool{"valid-1": true, "banned-1": false},
		defaultValid:   true,
	}
	g.loginServerClient = mock

	// 三条连接：valid / banned / 无 loginKey（跳过）
	add := func(account, key string) *kickMockConn {
		mc := &kickMockConn{}
		id := g.connectionManager.AddConnection(mc, "")
		g.connectionManager.SetConnectionServerID(id, "s1")
		if account != "" {
			g.connectionManager.SetConnectionAccountID(id, account)
		}
		if key != "" {
			g.connectionManager.SetConnectionLoginKey(id, key)
		}
		return mc
	}
	validConn := add("valid-1", "tok-valid")
	bannedConn := add("banned-1", "tok-banned")
	noKeyConn := add("nokey-1", "")

	g.runBatchValidateSweep()

	if validConn.isClosed() {
		t.Fatal("valid conn should not be kicked")
	}
	if !bannedConn.isClosed() {
		t.Fatal("banned conn should be kicked")
	}
	if noKeyConn.isClosed() {
		t.Fatal("conn without loginKey should not be touched")
	}
	if mock.batchCalls.Load() != 1 {
		t.Fatalf("batchCalls=%d want 1", mock.batchCalls.Load())
	}
}

func TestRunBatchValidateSweepNoClientNoop(t *testing.T) {
	g := newKickTestGateway()
	// client 为 nil：直接返回，不应 panic
	g.runBatchValidateSweep()
}

func TestRunBatchValidateSweepSkipIfTokenChanged(t *testing.T) {
	g := newKickTestGateway()
	// 校验前返回 invalid，但 sweep 二次确认时 token 已换 → 不踢
	mock := &mockLoginServiceClient{
		validByAccount: map[string]bool{"acct-x": false},
		defaultValid:   false,
	}
	g.loginServerClient = mock

	mc := &kickMockConn{}
	id := g.connectionManager.AddConnection(mc, "")
	g.connectionManager.SetConnectionServerID(id, "s1")
	g.connectionManager.SetConnectionAccountID(id, "acct-x")
	g.connectionManager.SetConnectionLoginKey(id, "tok-old")
	// mock 在返回前换掉 token（模拟校验期间重登）
	g.loginServerClient = &tokenChangeClient{mockLoginServiceClient: mock, conn: g.connectionManager.GetConnection(id)}

	g.runBatchValidateSweep()

	if mc.isClosed() {
		t.Fatal("conn with changed token should not be kicked")
	}
}

// tokenChangeClient 在返回 invalid 结果前更换连接 token（模拟 mid-sweep 重登）。
type tokenChangeClient struct {
	*mockLoginServiceClient
	conn *connection.Connection
}

func (c *tokenChangeClient) BatchValidateLoginTokens(
	ctx context.Context,
	in *protoLogin.BatchValidateLoginTokensReq,
	opts ...grpc.CallOption,
) (*protoLogin.BatchValidateLoginTokensAck, error) {
	ack, err := c.mockLoginServiceClient.BatchValidateLoginTokens(ctx, in, opts...)
	if c.conn != nil {
		c.conn.SetLoginKey("tok-new")
	}
	return ack, err
}

func TestBatchValidateIntervalDisabledWhenValidationOff(t *testing.T) {
	g := newKickTestGateway()
	// cfg 与全局均为 nil → loginValidationEnabled=false → interval=0
	if d := g.batchValidateInterval(); d != 0 {
		t.Fatalf("interval=%v want 0 when validation disabled", d)
	}
}
