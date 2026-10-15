package gateway

import (
	"testing"

	"github.com/streasure/sgate/internal/connection"
)

// newKickTestGateway 构造最小可用 Gateway（仅连接管理器 + 踢人所需字段）。
func newKickTestGateway() *Gateway {
	return &Gateway{
		connectionManager: connection.NewConnectionManager(0, 0),
	}
}

func TestKickByAccountsHitsAccountIndex(t *testing.T) {
	g := newKickTestGateway()
	mc := &kickMockConn{}
	connID := g.connectionManager.AddConnection(mc, "")
	g.connectionManager.SetConnectionServerID(connID, "s1")
	g.connectionManager.SetConnectionAccountID(connID, "10001")

	if n := g.KickByAccounts([]string{"99999"}, "r"); n != 0 {
		t.Fatalf("unrelated account kicked=%d, want 0", n)
	}
	if n := g.KickByAccounts([]string{"10001"}, "blacklist"); n != 1 {
		t.Fatalf("kicked=%d, want 1", n)
	}
	if !mc.isClosed() {
		t.Fatal("conn should be closed after KickByAccounts")
	}
}

func TestKickByIPMatchesRemoteHost(t *testing.T) {
	g := newKickTestGateway()
	hit := &kickMockConn{}
	miss := &kickMockConn{}
	hitID := g.connectionManager.AddConnection(hit, "u-hit")
	missID := g.connectionManager.AddConnection(miss, "u-miss")
	_ = hitID
	_ = missID

	// kickMockConn.RemoteAddr 固定 127.0.0.1:1234 → RemoteHost=127.0.0.1
	if n := g.KickByIP("10.9.9.9", "blacklist"); n != 0 {
		t.Fatalf("unrelated ip kicked=%d, want 0", n)
	}
	if n := g.KickByIP("127.0.0.1", "blacklist"); n != 2 {
		t.Fatalf("kicked=%d, want 2", n)
	}
	if !hit.isClosed() || !miss.isClosed() {
		t.Fatal("both conns should be closed")
	}
}
