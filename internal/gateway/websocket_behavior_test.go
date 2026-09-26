package gateway

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/connection"
)

// wsMockConn 记录写出的字节，供断言 close/pong/HTTP 响应。
type wsMockConn struct {
	gnet.Conn
	written bytes.Buffer
	closed  bool
}

func (c *wsMockConn) Write(b []byte) (int, error) { return c.written.Write(b) }
func (c *wsMockConn) Close() error                { c.closed = true; return nil }
func (c *wsMockConn) RemoteAddr() net.Addr        { return nil }

func newOpenWSConn() (*wsMockConn, *WebSocketConnection) {
	mc := &wsMockConn{}
	ws := NewWebSocketConnection(mc)
	ws.State.Store(int32(WSStateOpen))
	ws.SetConnectionID("conn-test")
	return mc, ws
}

// wsConnID 返回网关中唯一的 temp 连接 ID（测试清理用）。
func wsConnID(gw *Gateway) string {
	var id string
	gw.GetConnectionManager().ForEach(func(c *connection.Connection) bool {
		id = c.ID()
		return false
	})
	return id
}

// TestWSHandshake_HalfPacketAccumulation 验证半包跨 packet 累积后再完成握手（H2）。
func TestWSHandshake_HalfPacketAccumulation(t *testing.T) {
	gw := newGateway(minimalConfig())
	// 握手会注册连接，Close 需 drain；测试末尾先移除避免 2 分钟等待
	defer func() {
		if id := wsConnID(gw); id != "" {
			gw.GetConnectionManager().RemoveConnection(id)
		}
		gw.Close()
	}()

	mc := &wsMockConn{}
	ws := NewWebSocketConnection(mc)

	handshake := "GET / HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"

	// 第一片：不含 \r\n\r\n → 应等待，不关闭
	cut := len(handshake) / 2
	if action := gw.handleWebSocketMessage(ws, []byte(handshake[:cut])); action != gnet.None {
		t.Fatalf("partial handshake action=%v want None", action)
	}
	if mc.closed {
		t.Fatal("connection closed on partial handshake")
	}
	// 第二片：补全 → 握手成功
	if action := gw.handleWebSocketMessage(ws, []byte(handshake[cut:])); action == gnet.Close {
		t.Fatal("complete handshake closed connection")
	}
	if ws.State.Load() != int32(WSStateOpen) {
		t.Fatalf("state=%d want WSStateOpen", ws.State.Load())
	}
	if !bytes.Contains(mc.written.Bytes(), []byte("101")) {
		t.Fatalf("expected 101 response, got %q", mc.written.String())
	}
}

// TestWSHandshake_OversizedRejected 验证半包缓冲上限（16KB）防无限缓冲。
func TestWSHandshake_OversizedRejected(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	mc := &wsMockConn{}
	ws := NewWebSocketConnection(mc)

	// 单片超限（> maxWSHandshakeSize）或累积超限均应拒绝
	junk := bytes.Repeat([]byte("A"), maxWSHandshakeSize+1)
	if action := gw.handleWebSocketMessage(ws, junk); action != gnet.Close {
		t.Fatalf("action=%v want Close on oversized handshake", action)
	}
}

// TestWS_UnmaskedFrameRejected 验证 RFC6455 客户端帧必须 mask（H4）。
func TestWS_UnmaskedFrameRejected(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	_, ws := newOpenWSConn()
	// FIN+binary, len=3, 无 mask 位
	action := gw.handleWebSocketMessage(ws, []byte{0x82, 0x03, 'a', 'b', 'c'})
	if action != gnet.Close {
		t.Fatalf("action=%v want Close on unmasked frame", action)
	}
}

// TestWS_FragmentationReassembly 验证 FIN=0 + continuation 重组（H3）。
func TestWS_FragmentationReassembly(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	_, ws := newOpenWSConn()

	// 片1：binary FIN=0, payload "par"
	// 片2：continuation FIN=1, payload "tial" → 重组后进入 data frame 处理
	// data frame 解码失败（非 protobuf）→ 回写错误响应而非断连
	f1 := buildMaskedFrame(false, 0x2, []byte("par"))
	f2 := buildMaskedFrame(true, 0x0, []byte("tial"))
	if action := gw.handleWebSocketMessage(ws, append(f1, f2...)); action == gnet.Close {
		t.Fatal("fragmented message closed connection")
	}
	if ws.fragging {
		t.Fatal("fragmentation state not cleared after FIN")
	}
	if ws.fragBuf != nil {
		t.Fatal("fragBuf not released")
	}
}

// TestWS_UnexpectedContinuationRejected 无前置分片的 continuation 帧应断连。
func TestWS_UnexpectedContinuationRejected(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	_, ws := newOpenWSConn()
	action := gw.handleWebSocketMessage(ws, buildMaskedFrame(true, 0x0, []byte("x")))
	if action != gnet.Close {
		t.Fatalf("action=%v want Close on unexpected continuation", action)
	}
}

// TestWS_CloseFrameClearsAndClosesTCP 验证 close 帧清理映射并关闭底层 TCP（H1，防 FD 泄漏）。
func TestWS_CloseFrameClearsAndClosesTCP(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()
	gw.wsConnections.Store(ws, struct{}{})

	if err := gw.handleWebSocketCloseFrame(ws); err != nil {
		t.Fatalf("close frame handler error: %v", err)
	}
	if !mc.closed {
		t.Fatal("underlying TCP not closed (FD leak)")
	}
	if ws.State.Load() != int32(WSStateClosed) {
		t.Fatalf("state=%d want WSStateClosed", ws.State.Load())
	}
	if _, ok := gw.wsConnections.Load(ws); ok {
		t.Fatal("wsConnections entry not removed")
	}
	if gw.GetConnectionManager().GetConnection("conn-test") != nil {
		t.Fatal("connection manager entry not removed")
	}
}

// TestWS_PingUpdatesHeartbeat 验证 ping 帧回写 pong 并刷新心跳时间。
func TestWS_PingUpdatesHeartbeat(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()
	before := ws.getLastPingTime()
	time.Sleep(5 * time.Millisecond)

	if err := gw.handleWebSocketPingFrame(ws, []byte("hi")); err != nil {
		t.Fatalf("ping handler error: %v", err)
	}
	if mc.written.Len() == 0 {
		t.Fatal("pong frame not written")
	}
	if !ws.getLastPingTime().After(before) {
		t.Fatal("heartbeat time not refreshed by ping")
	}
}
