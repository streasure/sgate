package gateway

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/routes"
	"google.golang.org/protobuf/proto"
)

// udpEnabledConfig 单机配置 + UDP 监听 + 关联通道开启（阶段 1 测试基线）。
func udpEnabledConfig() *config.Config {
	cfg := minimalConfig()
	cfg.Transports = append(cfg.Transports, config.Transport{Port: 48082, Type: "udp"})
	cfg.UDP.Enabled = true
	return cfg
}

// udpMockConn 可指定源地址的 UDP 数据报 conn（模拟不同客户端公网地址）。
type udpMockConn struct {
	gnet.Conn
	mu      sync.Mutex
	written []byte
	sentTo  net.Addr
	addr    net.Addr
}

func (c *udpMockConn) SendTo(b []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	c.written = append(c.written, b...)
	c.sentTo = addr
	c.mu.Unlock()
	return len(b), nil
}

func (c *udpMockConn) RemoteAddr() net.Addr { return c.addr }

func (c *udpMockConn) writtenLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.written)
}

// sentToCopy 加锁读取最近一次 SendTo 目标地址。
func (c *udpMockConn) sentToCopy() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sentTo
}

// writtenCopy 加锁拷贝已写入字节（错误帧断言用）。
func (c *udpMockConn) writtenCopy() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.written...)
}

// decodeFirstFrame 解析 written 中第一个 4B 长度前缀帧。
func decodeFirstFrame(t *testing.T, b []byte) *protoGw.StreamData {
	t.Helper()
	if len(b) < 4 {
		t.Fatalf("written len=%d, want >=4", len(b))
	}
	n := binary.BigEndian.Uint32(b[:4])
	if int(n)+4 > len(b) {
		t.Fatalf("frame len=%d exceeds written=%d", n, len(b))
	}
	msg, ok := routes.DecodeClientMessage(b[4 : 4+n])
	if !ok {
		t.Fatalf("decode frame failed bytes=%v", b[4:4+n])
	}
	return msg
}

// associateBody 构造 UdpAssociateReq 消息体。
func associateBody(t *testing.T, token string) []byte {
	t.Helper()
	body, err := proto.Marshal(&protoGw.UdpAssociateReq{UdpToken: token, Nonce: []byte("nonce"), Ver: 1})
	if err != nil {
		t.Fatalf("marshal UdpAssociateReq: %v", err)
	}
	return body
}

// TestUDPTokenOneShot 一次性 token：首次消费成功，重放第二个 ASSOCIATE 拿不到。
func TestUDPTokenOneShot(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
	defer gw.connectionManager.RemoveConnection(connID)

	token, _, ok := gw.issueUDPToken(connID)
	if !ok || token == "" {
		t.Fatalf("issueUDPToken ok=%v token=%q, want issued", ok, token)
	}
	got, ok := gw.udpAssoc.consumeToken(token, time.Now().UnixMilli())
	if !ok || got != connID {
		t.Fatalf("first consume connID=%q ok=%v, want %q true", got, ok, connID)
	}
	if _, ok := gw.udpAssoc.consumeToken(token, time.Now().UnixMilli()); ok {
		t.Fatal("token replay accepted, want one-shot (rejected)")
	}
}

// TestUDPTokenExpired 过期 token 拒绝消费；janitor sweep 清除过期残留。
func TestUDPTokenExpired(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
	defer gw.connectionManager.RemoveConnection(connID)

	// 过期 token（TTL 为负，签发即过期）
	expired, err := gw.udpAssoc.issueToken(connID, -time.Second)
	if err != nil {
		t.Fatalf("issueToken: %v", err)
	}
	if _, ok := gw.udpAssoc.consumeToken(expired, time.Now().UnixMilli()); ok {
		t.Fatal("expired token consumed, want rejected")
	}
	// 未消费的正常 token 由 sweep 按 expireAt 清除
	if _, err := gw.udpAssoc.issueToken(connID, -time.Second); err != nil {
		t.Fatalf("issueToken: %v", err)
	}
	removed := gw.udpAssoc.sweep(time.Now().UnixMilli(), config.DefaultUDPIdleDuration.Milliseconds(), nil)
	if removed != 0 {
		t.Fatalf("sweep removed assoc=%d, want 0 (only tokens swept)", removed)
	}
	if _, n := gw.udpAssoc.snapshot(); n != 0 {
		t.Fatalf("pending tokens=%d, want 0 (expired swept)", n)
	}
}

// TestUDPTokenRevokedOnReissue 同会话重登录撤销旧 token（旧 token 不得复活）。
func TestUDPTokenRevokedOnReissue(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
	defer gw.connectionManager.RemoveConnection(connID)

	old, _, ok := gw.issueUDPToken(connID)
	if !ok {
		t.Fatal("first issue failed")
	}
	fresh, _, ok := gw.issueUDPToken(connID)
	if !ok {
		t.Fatal("second issue failed")
	}
	if _, ok := gw.udpAssoc.consumeToken(old, time.Now().UnixMilli()); ok {
		t.Fatal("stale token from previous login accepted, want revoked")
	}
	if _, ok := gw.udpAssoc.consumeToken(fresh, time.Now().UnixMilli()); !ok {
		t.Fatal("fresh token rejected, want accepted")
	}
}

// TestIssueUDPTokenGate 开关与 UDP 监听缺失时不下发 token（ack 保持向后兼容）。
func TestIssueUDPTokenGate(t *testing.T) {
	// enabled=false（预留通道现状）
	gwOff := newGateway(minimalConfig(), "")
	defer gwOff.Close()
	if _, _, ok := gwOff.issueUDPToken("conn-1"); ok {
		t.Fatal("disabled gateway issued token, want refused")
	}

	// enabled=true 但无 type=udp 监听（ASSOCIATE 永远到不了，不发）
	cfg := udpEnabledConfig()
	cfg.Transports = []config.Transport{{Port: 48080}} // 去掉 udp
	gwNoPort := newGateway(cfg, "")
	defer gwNoPort.Close()
	if _, _, ok := gwNoPort.issueUDPToken("conn-1"); ok {
		t.Fatal("token issued without udp transport, want refused")
	}
}

// TestUDPAssociateLifecycle 关联建立 → 双索引反查 → 回 Ack 目标地址正确 →
// 会话销毁幂等拆除 → 旧地址反查失效。
func TestUDPAssociateLifecycle(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)

	token, _, ok := gw.issueUDPToken(connID)
	if !ok {
		t.Fatal("issue token failed")
	}
	c := &udpMockConn{addr: &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 40000}}
	gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, associateBody(t, token)))

	// Ack 回写且目标 = ASSOCIATE 源地址
	if c.writtenLen() == 0 {
		t.Fatal("no associate ack written")
	}
	if got := c.sentToCopy(); got == nil || got.String() != c.addr.String() {
		t.Fatalf("ack target = %v, want %v (回程必须发往关联源地址)", got, c.addr)
	}
	msg := decodeFirstFrame(t, c.written)
	if msg.Cmd != protoGw.CmdUdpAssociateAck {
		t.Fatalf("resp cmd=%d, want CmdUdpAssociateAck=%d", msg.Cmd, protoGw.CmdUdpAssociateAck)
	}
	ack := &protoGw.UdpAssociateAck{}
	if err := proto.Unmarshal(msg.Data, ack); err != nil {
		t.Fatalf("unmarshal UdpAssociateAck: %v", err)
	}
	if len(ack.ServerNonce) != 8 {
		t.Fatalf("serverNonce len=%d, want 8", len(ack.ServerNonce))
	}

	// 双索引可见
	if _, found := gw.udpAssoc.lookupByAddr(c.addr.String()); !found {
		t.Fatal("lookupByAddr after associate: not found")
	}
	if assoc, tokens := gw.udpAssoc.snapshot(); assoc != 1 || tokens != 0 {
		t.Fatalf("assoc=%d tokens=%d, want 1/0 (token consumed)", assoc, tokens)
	}

	// 重放同 token：不再建联、不再回包
	before := c.writtenLen()
	gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, associateBody(t, token)))
	if n := c.writtenLen(); n != before {
		t.Fatalf("replay wrote %d bytes, want 0 (one-shot)", n-before)
	}
	if assoc, _ := gw.udpAssoc.snapshot(); assoc != 1 {
		t.Fatalf("assoc=%d after replay, want 1 (no duplicate)", assoc)
	}

	// 会话销毁：幂等拆除 + 旧地址失效
	if !gw.udpAssoc.deleteByConnID(connID) {
		t.Fatal("first delete: want removed")
	}
	if gw.udpAssoc.deleteByConnID(connID) {
		t.Fatal("second delete: want idempotent no-op")
	}
	if _, found := gw.udpAssoc.lookupByAddr(c.addr.String()); found {
		t.Fatal("stale addr still resolves after release")
	}
	gw.connectionManager.RemoveConnection(connID)
}

// TestUDPAssociateRejectedPaths 关联拒绝路径全静默：会话已死、垃圾包、缺 token、
// per-IP 超限——均不回包、不建联。
func TestUDPAssociateRejectedPaths(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()

	t.Run("session_dead", func(t *testing.T) {
		// token 属于未注册（已死）会话
		token, err := gw.udpAssoc.issueToken("ghost-conn", time.Minute)
		if err != nil {
			t.Fatalf("issueToken: %v", err)
		}
		c := &udpMockConn{addr: &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 1000}}
		gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, associateBody(t, token)))
		if c.writtenLen() != 0 {
			t.Fatalf("dead session got %d reply bytes, want silent", c.writtenLen())
		}
		if assoc, _ := gw.udpAssoc.snapshot(); assoc != 0 {
			t.Fatalf("assoc=%d, want 0", assoc)
		}
	})

	t.Run("junk_body", func(t *testing.T) {
		c := &udpMockConn{addr: &net.UDPAddr{IP: net.IPv4(198, 51, 100, 2), Port: 1000}}
		gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, []byte{0xFF, 0xFF, 0xFF}))
		if c.writtenLen() != 0 {
			t.Fatalf("junk got %d reply bytes, want silent", c.writtenLen())
		}
	})

	t.Run("empty_token", func(t *testing.T) {
		c := &udpMockConn{addr: &net.UDPAddr{IP: net.IPv4(198, 51, 100, 3), Port: 1000}}
		gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, associateBody(t, "")))
		if c.writtenLen() != 0 {
			t.Fatalf("empty token got %d reply bytes, want silent", c.writtenLen())
		}
	})
}

// TestUDPAssociatePerIPLimit per-IP 关联上限：同 IP 第二个会话拒绝，异 IP 放行。
func TestUDPAssociatePerIPLimit(t *testing.T) {
	cfg := udpEnabledConfig()
	cfg.UDP.MaxPerIP = 1
	gw := newGateway(cfg, "")
	defer gw.Close()

	mkAssoc := func(t *testing.T, ip net.IP, port int) bool {
		t.Helper()
		connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
		defer gw.connectionManager.RemoveConnection(connID)
		token, _, ok := gw.issueUDPToken(connID)
		if !ok {
			t.Fatal("issue token failed")
		}
		c := &udpMockConn{addr: &net.UDPAddr{IP: ip, Port: port}}
		gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, associateBody(t, token)))
		return c.writtenLen() > 0
	}

	if !mkAssoc(t, net.IPv4(203, 0, 113, 10), 5000) {
		t.Fatal("first association on IP rejected, want accepted")
	}
	if mkAssoc(t, net.IPv4(203, 0, 113, 10), 5001) {
		t.Fatal("second association same IP accepted, want per-IP limit hit")
	}
	if !mkAssoc(t, net.IPv4(203, 0, 113, 11), 5000) {
		t.Fatal("association other IP rejected, want accepted")
	}
}

// TestUDPAssociateTotalLimit 关联表总上限（防表耗尽）：maxAssociations 打满后
// 新关联一律拒绝（即使 per-IP 未超），且静默丢弃不回包（关联未建立时回帧
// 无处可去，构成放大面）。
func TestUDPAssociateTotalLimit(t *testing.T) {
	cfg := udpEnabledConfig()
	cfg.UDP.MaxAssociations = 1
	gw := newGateway(cfg, "")
	defer gw.Close()

	mkAssoc := func(t *testing.T, ip net.IP, port int) bool {
		t.Helper()
		connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
		defer gw.connectionManager.RemoveConnection(connID)
		token, _, ok := gw.issueUDPToken(connID)
		if !ok {
			t.Fatal("issue token failed")
		}
		c := &udpMockConn{addr: &net.UDPAddr{IP: ip, Port: port}}
		gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, associateBody(t, token)))
		return c.writtenLen() > 0
	}

	// 不同 IP（绕开 per-IP 上限），只能被总上限挡住
	if !mkAssoc(t, net.IPv4(203, 0, 113, 30), 6000) {
		t.Fatal("first association rejected, want accepted")
	}
	if mkAssoc(t, net.IPv4(203, 0, 113, 31), 6001) {
		t.Fatal("association beyond maxAssociations accepted, want total limit hit")
	}
	if n, _ := gw.udpAssoc.snapshot(); n != 1 {
		t.Fatalf("assoc count = %d, want 1", n)
	}
}

// TestUDPAssocIdleSweep 空闲淘汰：超时关联拆除，活跃关联保留。
func TestUDPAssocIdleSweep(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
	defer gw.connectionManager.RemoveConnection(connID)

	addr := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 20), Port: 6000}
	if a := gw.udpAssoc.create(connID, addr, &udpMockConn{addr: addr}, "tok", time.Now().UnixMilli(), 4, 100); a == nil {
		t.Fatal("create failed")
	}
	// 把 lastSeen 拨回超时点
	old, _ := gw.udpAssoc.lookupByAddr(addr.String())
	idle := config.DefaultUDPIdleDuration.Milliseconds()
	old.lastSeen.Store(time.Now().UnixMilli() - idle - 1)

	// 另建一个活跃关联
	connID2 := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
	defer gw.connectionManager.RemoveConnection(connID2)
	addr2 := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 21), Port: 6000}
	if a := gw.udpAssoc.create(connID2, addr2, &udpMockConn{addr: addr2}, "tok2", time.Now().UnixMilli(), 4, 100); a == nil {
		t.Fatal("create 2 failed")
	}

	removed := gw.udpAssoc.sweep(time.Now().UnixMilli(), idle, nil)
	if removed != 1 {
		t.Fatalf("sweep removed=%d, want 1", removed)
	}
	if _, found := gw.udpAssoc.lookupByAddr(addr.String()); found {
		t.Fatal("idle assoc survived sweep")
	}
	if _, found := gw.udpAssoc.lookupByAddr(addr2.String()); !found {
		t.Fatal("active assoc swept, want kept")
	}
}

// TestUDPAssocCreateIdempotentReplace 同会话重复建联幂等替换：计数不涨、旧地址索引拆除。
func TestUDPAssocCreateIdempotentReplace(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)
	defer gw.connectionManager.RemoveConnection(connID)

	addr1 := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 30), Port: 7000}
	addr2 := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 30), Port: 7001} // NAT 换端口
	if a := gw.udpAssoc.create(connID, addr1, &udpMockConn{addr: addr1}, "t1", time.Now().UnixMilli(), 4, 100); a == nil {
		t.Fatal("create 1 failed")
	}
	if a := gw.udpAssoc.create(connID, addr2, &udpMockConn{addr: addr2}, "t2", time.Now().UnixMilli(), 4, 100); a == nil {
		t.Fatal("create 2 failed")
	}
	if n, _ := gw.udpAssoc.snapshot(); n != 1 {
		t.Fatalf("assoc count=%d, want 1 (replace not duplicate)", n)
	}
	if _, found := gw.udpAssoc.lookupByAddr(addr1.String()); found {
		t.Fatal("old addrKey survived replace")
	}
	if _, found := gw.udpAssoc.lookupByAddr(addr2.String()); !found {
		t.Fatal("new addrKey missing")
	}
}

// TestUDPDatagramEnabledUnassociatedSilent 开启后未关联的数据包静默丢：
// 不回错误帧（未授权源地址回包 = 放大面）、不计数（预留通道计数只属于关闭态）。
func TestUDPDatagramEnabledUnassociatedSilent(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	c := &udpMockConn{addr: &net.UDPAddr{IP: net.IPv4(203, 0, 113, 40), Port: 8000}}

	gw.handleUDPDatagram(c, udpDatagram(t, 1100001, []byte("login")))

	if c.writtenLen() != 0 {
		t.Fatalf("unassociated got %d reply bytes, want silent drop", c.writtenLen())
	}
	if got := gw.messagesDroppedUnsupported.Load(); got != 0 {
		t.Fatalf("droppedUnsupported=%d, want 0 (counter belongs to disabled path)", got)
	}
}

// TestReleaseUDPAssocGateway 会话结束钩子幂等（OnClose/RemoveConnection 两路都调）。
func TestReleaseUDPAssocGateway(t *testing.T) {
	gw := newGateway(udpEnabledConfig(), "")
	defer gw.Close()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "", false)

	addr := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 50), Port: 9000}
	if a := gw.udpAssoc.create(connID, addr, &udpMockConn{addr: addr}, "tok", time.Now().UnixMilli(), 4, 100); a == nil {
		t.Fatal("create failed")
	}
	gw.releaseUDPAssoc(connID)
	if n, _ := gw.udpAssoc.snapshot(); n != 0 {
		t.Fatalf("assoc=%d after release, want 0", n)
	}
	gw.releaseUDPAssoc(connID) // 幂等，不 panic 不误计
	gw.releaseUDPAssoc("")     // 空 connID 安全
	gw.connectionManager.RemoveConnection(connID)
}
