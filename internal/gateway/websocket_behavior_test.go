package gateway

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/dataplane"
)

// wsMockConn 记录写出的字节，供断言 close/pong/HTTP 响应；
// Context/Next 模拟 gnet 连接上下文与读事件数据（经 transport OnTraffic 驱动）。
type wsMockConn struct {
	gnet.Conn
	written bytes.Buffer
	closed  bool
	ctx     any
	in      []byte
}

func (c *wsMockConn) Write(b []byte) (int, error) { return c.written.Write(b) }
func (c *wsMockConn) Close() error                { c.closed = true; return nil }
func (c *wsMockConn) RemoteAddr() net.Addr        { return nil }
func (c *wsMockConn) Context() any                { return c.ctx }
func (c *wsMockConn) SetContext(v any)            { c.ctx = v }
func (c *wsMockConn) Next(int) ([]byte, error)    { return c.in, nil }

// newWSConn 创建 mock 连接与 WS 会话（上下文已绑定，可直接投递读事件）。
func newWSConn() (*wsMockConn, *dataplane.WebSocketConnection) {
	mc := &wsMockConn{}
	ws := dataplane.NewWebSocketConnection(mc)
	mc.ctx = ws
	return mc, ws
}

// newOpenWSConn 创建已握手完成（State=Open）且带连接标识的 WS 会话。
func newOpenWSConn() (*wsMockConn, *dataplane.WebSocketConnection) {
	mc, ws := newWSConn()
	ws.State.Store(int32(dataplane.WSStateOpen))
	ws.SetConnectionID("conn-test")
	return mc, ws
}

// wsTraffic 经 transport 事件入口投递一次读事件（等价 gnet OnTraffic 调用）。
func wsTraffic(gw *Gateway, mc *wsMockConn, data []byte) gnet.Action {
	mc.in = data
	return gw.transportSrv.OnTraffic(mc)
}

// buildMaskedFrame 构造 masked 帧：FIN+op, MASK+len, 4B mask + payload
// （与 transport 包 websocket_frame_test 的同名 helper 同构）。
func buildMaskedFrame(fin bool, op byte, payload []byte) []byte {
	if len(payload) >= 65536 {
		return nil
	}
	b0 := op & 0x0F
	if fin {
		b0 |= 0x80
	}
	n := len(payload)
	var out []byte
	out = append(out, b0)
	if n < 126 {
		out = append(out, 0x80|byte(n))
	} else {
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	}
	mask := [4]byte{0x01, 0x02, 0x03, 0x04}
	out = append(out, mask[:]...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	return out
}

// wsConnID 返回网关中唯一的 temp 连接 ID（测试清理用）。
func wsConnID(gw *Gateway) string {
	var id string
	gw.GetConnectionManager().ForEach(func(c *dataplane.Connection) bool {
		id = c.ID()
		return false
	})
	return id
}

// TestWSHandshake_HalfPacketAccumulation 验证半包跨 packet 累积后再完成握手（H2）。
func TestWSHandshake_HalfPacketAccumulation(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	// 握手会注册连接，Close 需 drain；测试末尾先移除避免 2 分钟等待
	defer func() {
		if id := wsConnID(gw); id != "" {
			gw.GetConnectionManager().RemoveConnection(id)
		}
		gw.Close()
	}()

	mc, ws := newWSConn()

	handshake := "GET / HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"

	// 第一片：不含 \r\n\r\n → 应等待，不关闭
	cut := len(handshake) / 2
	if action := wsTraffic(gw, mc, []byte(handshake[:cut])); action != gnet.None {
		t.Fatalf("partial handshake action=%v want None", action)
	}
	if mc.closed {
		t.Fatal("connection closed on partial handshake")
	}
	// 第二片：补全 → 握手成功
	if action := wsTraffic(gw, mc, []byte(handshake[cut:])); action == gnet.Close {
		t.Fatal("complete handshake closed connection")
	}
	if ws.State.Load() != int32(dataplane.WSStateOpen) {
		t.Fatalf("state=%d want WSStateOpen", ws.State.Load())
	}
	if !bytes.Contains(mc.written.Bytes(), []byte("101")) {
		t.Fatalf("expected 101 response, got %q", mc.written.String())
	}
}

// TestWSHandshake_OversizedRejected 验证半包缓冲上限（16KB）防无限缓冲。
func TestWSHandshake_OversizedRejected(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, _ := newWSConn()

	// 单片超限（> transport.maxWSHandshakeSize = 16KB）或累积超限均应拒绝
	junk := bytes.Repeat([]byte("A"), 16*1024+1)
	if action := wsTraffic(gw, mc, junk); action != gnet.Close {
		t.Fatalf("action=%v want Close on oversized handshake", action)
	}
}

// TestWS_UnmaskedFrameRejected 验证 RFC6455 客户端帧必须 mask（H4）。
func TestWS_UnmaskedFrameRejected(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, _ := newOpenWSConn()
	// FIN+binary, len=3, 无 mask 位
	action := wsTraffic(gw, mc, []byte{0x82, 0x03, 'a', 'b', 'c'})
	if action != gnet.Close {
		t.Fatalf("action=%v want Close on unmasked frame", action)
	}
}

// TestWS_FragmentationReassembly 验证 FIN=0 + continuation 重组（H3）。
func TestWS_FragmentationReassembly(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()

	// 片1：binary FIN=0, payload "par"
	// 片2：continuation FIN=1, payload "tial" → 重组后进入 data frame 处理
	// data frame 解码失败（非 protobuf）→ 回写错误响应而非断连
	f1 := buildMaskedFrame(false, 0x2, []byte("par"))
	f2 := buildMaskedFrame(true, 0x0, []byte("tial"))
	if action := wsTraffic(gw, mc, append(f1, f2...)); action == gnet.Close {
		t.Fatal("fragmented message closed connection")
	}
	if ws.Fragging {
		t.Fatal("fragmentation state not cleared after FIN")
	}
	if len(ws.FragBuf) != 0 {
		t.Fatalf("fragBuf not released: len=%d (capacity may be retained)", len(ws.FragBuf))
	}
}

// TestWS_BufferReuseExactBoundary 精确边界消费后 Buffer 容量必须保留
// （防 nil 置零重分配回归，同 TCP FrameBuf）。
func TestWS_BufferReuseExactBoundary(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()
	frame := buildMaskedFrame(true, 0x2, []byte("x"))
	if action := wsTraffic(gw, mc, frame); action == gnet.Close {
		t.Fatal("first frame closed")
	}
	if cap(ws.Buffer) == 0 {
		t.Fatal("Buffer capacity dropped after exact consumption (old nil-reset behavior)")
	}
	capBefore := cap(ws.Buffer)
	if action := wsTraffic(gw, mc, frame); action == gnet.Close {
		t.Fatal("second frame closed")
	}
	if cap(ws.Buffer) != capBefore {
		t.Fatalf("cap grew %d -> %d, want reuse", capBefore, cap(ws.Buffer))
	}
	if len(ws.Buffer) != 0 || ws.BufOff != 0 {
		t.Fatalf("buffer not reset: len=%d off=%d", len(ws.Buffer), ws.BufOff)
	}
}

// TestWS_BufferLimitRejected 缓冲超限必须断连（同 TCP FrameBuf）。
func TestWS_BufferLimitRejected(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()
	p := gw.GetProtection()
	p.MaxWSBufferSize = 64
	gw.protection.Store(&p)

	mc, ws := newOpenWSConn()
	// 单帧 payload 128 > maxWSBufferSize 64，append 后超限
	big := buildMaskedFrame(true, 0x2, make([]byte, 128))
	if action := wsTraffic(gw, mc, big); action != gnet.Close {
		t.Fatalf("action=%v, want Close on oversized WS buffer", action)
	}
	if ws.Buffer != nil {
		t.Fatalf("Buffer not cleared on limit reject: len=%d", len(ws.Buffer))
	}
}

// TestWS_CoalescedMultiFrames 粘包：一次读事件多帧顺序交付（offset 复用回归）。
func TestWS_CoalescedMultiFrames(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()
	// 两帧粘包：每帧 payload "ab"（解码失败回写错误响应，不断连）
	f1 := buildMaskedFrame(true, 0x2, []byte("ab"))
	f2 := buildMaskedFrame(true, 0x2, []byte("cd"))
	combined := append(append([]byte(nil), f1...), f2...)
	if action := wsTraffic(gw, mc, combined); action == gnet.Close {
		t.Fatal("coalesced frames closed connection")
	}
	if len(ws.Buffer) != 0 || ws.BufOff != 0 {
		t.Fatalf("buffer not reset after coalesced consume: len=%d off=%d", len(ws.Buffer), ws.BufOff)
	}
}

// TestWS_PartialFrameAcrossReads 跨读事件重组：半帧到达等待补全。
func TestWS_PartialFrameAcrossReads(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()
	full := buildMaskedFrame(true, 0x2, []byte("partial"))
	if action := wsTraffic(gw, mc, full[:4]); action == gnet.Close {
		t.Fatal("partial head closed connection")
	}
	if len(ws.Buffer) == 0 {
		t.Fatal("partial frame must be buffered")
	}
	if action := wsTraffic(gw, mc, full[4:]); action == gnet.Close {
		t.Fatal("partial tail closed connection")
	}
	if len(ws.Buffer) != 0 || ws.BufOff != 0 {
		t.Fatalf("buffer not reset after partial reassembly: len=%d off=%d", len(ws.Buffer), ws.BufOff)
	}
}

// TestWS_UnexpectedContinuationRejected 无前置分片的 continuation 帧应断连。
func TestWS_UnexpectedContinuationRejected(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, _ := newOpenWSConn()
	action := wsTraffic(gw, mc, buildMaskedFrame(true, 0x0, []byte("x")))
	if action != gnet.Close {
		t.Fatalf("action=%v want Close on unexpected continuation", action)
	}
}

// TestWS_CloseFrameClearsAndClosesTCP 验证 close 帧清理映射并关闭底层 TCP（H1，防 FD 泄漏）。
func TestWS_CloseFrameClearsAndClosesTCP(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()
	gw.wsConnections.Store(ws, struct{}{})

	// 经读事件投递 masked close 帧（status 1000）
	if action := wsTraffic(gw, mc, buildMaskedFrame(true, 0x8, []byte{0x03, 0xE8})); action == gnet.Close {
		t.Fatal("close frame handling closed connection")
	}
	if !mc.closed {
		t.Fatal("underlying TCP not closed (FD leak)")
	}
	if ws.State.Load() != int32(dataplane.WSStateClosed) {
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
	gw := newTestGateway(minimalConfig())
	defer gw.Close()

	mc, ws := newOpenWSConn()
	before := ws.GetLastPingTime()
	time.Sleep(5 * time.Millisecond)

	if action := wsTraffic(gw, mc, buildMaskedFrame(true, 0x9, []byte("hi"))); action == gnet.Close {
		t.Fatal("ping frame closed connection")
	}
	if mc.written.Len() == 0 {
		t.Fatal("pong frame not written")
	}
	if !ws.GetLastPingTime().After(before) {
		t.Fatal("heartbeat time not refreshed by ping")
	}
}
