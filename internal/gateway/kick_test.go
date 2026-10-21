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

// TestKickByAccountsMultiConnection 同账号多连接（不同逻辑服）全部踢中，不漏踢。
func TestKickByAccountsMultiConnection(t *testing.T) {
	g := newKickTestGateway()
	mc1, mc2 := &kickMockConn{}, &kickMockConn{}
	id1 := g.connectionManager.AddConnection(mc1, "")
	id2 := g.connectionManager.AddConnection(mc2, "")
	g.connectionManager.SetConnectionServerID(id1, "s1")
	g.connectionManager.SetConnectionServerID(id2, "s2")
	g.connectionManager.SetConnectionAccountID(id1, "20002")
	g.connectionManager.SetConnectionAccountID(id2, "20002")

	if n := g.KickByAccounts([]string{"20002"}, "blacklist"); n != 2 {
		t.Fatalf("kicked=%d, want 2 (both conns of same account)", n)
	}
	if !mc1.isClosed() || !mc2.isClosed() {
		t.Fatal("both connections of the account should be closed")
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

func TestKickByIPCIDRMatchesSubnet(t *testing.T) {
	g := newKickTestGateway()
	inSubnet := &kickMockConn{}
	outSubnet := &kickMockConn{}
	g.connectionManager.AddConnection(inSubnet, "u-in")
	g.connectionManager.AddConnection(outSubnet, "u-out")

	// inSubnet/outSubnet RemoteHost 均为 127.0.0.1（mock 固定）
	// 10.0.0.0/8 不含 127.0.0.1 → 0
	if n := g.KickByIP("10.0.0.0/8", "blacklist"); n != 0 {
		t.Fatalf("cidr 10.0.0.0/8 kicked=%d, want 0", n)
	}
	// 127.0.0.0/8 含 127.0.0.1 → 2
	if n := g.KickByIP("127.0.0.0/8", "blacklist"); n != 2 {
		t.Fatalf("cidr 127.0.0.0/8 kicked=%d, want 2", n)
	}
	if !inSubnet.isClosed() || !outSubnet.isClosed() {
		t.Fatal("both conns should be closed by 127.0.0.0/8")
	}
}

func TestKickByIPInvalidTarget(t *testing.T) {
	g := newKickTestGateway()
	g.connectionManager.AddConnection(&kickMockConn{}, "u1")
	if n := g.KickByIP("not-an-ip", "blacklist"); n != 0 {
		t.Fatalf("invalid target kicked=%d, want 0", n)
	}
	if n := g.KickByIP("10.0.0.0/33", "blacklist"); n != 0 {
		t.Fatalf("invalid cidr kicked=%d, want 0", n)
	}
}
