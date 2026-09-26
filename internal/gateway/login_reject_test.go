package gateway

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/sgate/internal/security"
	"google.golang.org/protobuf/proto"
)

// loginMockConn 捕获 LoginGate 回写的 ack 帧（writeMsgFrame 走 Writev）。
type loginMockConn struct {
	gnet.Conn
	written []byte
	closed  bool
}

func (c *loginMockConn) Write(b []byte) (int, error) {
	c.written = append(c.written, b...)
	return len(b), nil
}

func (c *loginMockConn) Writev(chunks [][]byte) (int, error) {
	n := 0
	for _, ch := range chunks {
		c.written = append(c.written, ch...)
		n += len(ch)
	}
	return n, nil
}

func (c *loginMockConn) Close() error { c.closed = true; return nil }
func (c *loginMockConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func loginReqFrame(t *testing.T) *protoGw.StreamData {
	t.Helper()
	body, err := proto.Marshal(&protoGw.LoginGateReq{ServerId: "logic:zone:1", UserId: "u1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &protoGw.StreamData{Cmd: routes.CmdLoginGate, SeqId: 1, Data: body}
}

// decodeAck 从 writeFrame 输出（4B 长度 + protobuf 帧）解析 LoginGateAck。
func decodeAck(t *testing.T, b []byte) *protoGw.LoginGateAck {
	t.Helper()
	if len(b) < 4 {
		return nil
	}
	n := binary.BigEndian.Uint32(b[:4])
	if int(n)+4 > len(b) {
		return nil
	}
	msg, ok := routes.DecodeClientMessage(b[4 : 4+n])
	if !ok || len(msg.Data) == 0 {
		return nil
	}
	ack := &protoGw.LoginGateAck{}
	if err := proto.Unmarshal(msg.Data, ack); err != nil {
		return nil
	}
	return ack
}

// TestLoginGate_RateLimited429 IP 限流命中 → 429。
func TestLoginGate_RateLimited429(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	// 测试未起 SecurityComponent，直接注入限流器
	gw.rateLimiter = security.NewRateLimiter(1, time.Hour)
	gw.rateLimiter.SetDimensionConfig("ip", security.DimensionConfig{
		MaxTokens: 1, BurstTokens: 1, TokenRefresh: time.Hour,
	})
	ip := "127.0.0.1"
	if !gw.rateLimiter.Allow("ip", ip) {
		t.Fatal("setup: first allow should pass")
	}

	c := &loginMockConn{}
	action := gw.handleLoginGate(c, "conn-rl", loginReqFrame(t))
	if action == gnet.Close {
		t.Fatal("rate-limited login should not close connection")
	}
	ack := decodeAck(t, c.written)
	if ack == nil || ack.Code != 429 {
		t.Fatalf("ack=%v want code 429", ack)
	}
}

// TestLoginGate_ConcurrentSlotFull429 并发登录槽满 → 429。
func TestLoginGate_ConcurrentSlotFull429(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	for i := cap(gw.loginSlots); i > 0; i-- {
		if !gw.acquireLoginSlot() {
			t.Fatalf("slot %d unexpectedly full", i)
		}
	}

	c := &loginMockConn{}
	action := gw.handleLoginGate(c, "conn-slot", loginReqFrame(t))
	if action == gnet.Close {
		t.Fatal("slot-full login should not close connection")
	}
	ack := decodeAck(t, c.written)
	if ack == nil || ack.Code != 429 {
		t.Fatalf("ack=%v want code 429", ack)
	}
	gw.releaseLoginSlot()
}
