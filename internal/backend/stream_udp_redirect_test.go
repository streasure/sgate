package backend

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
	"google.golang.org/protobuf/proto"
)

// testUDPCmd UDP 号段业务命令（1200000~1299999 内）。
const testUDPCmd int32 = 1200010

// redirectCaptureConn 自包含捕获 conn：AsyncWritev（Send 路径）与
// AsyncWrite（coalescer SendMulti 路径）都捕获，coalescer 误入时
// 变成可断言的帧而非 nil 嵌入 panic。
type redirectCaptureConn struct {
	gnet.Conn
	mu     sync.Mutex
	frames [][]byte
}

func (c *redirectCaptureConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func (c *redirectCaptureConn) capture(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, append([]byte(nil), b...))
	return nil
}

func (c *redirectCaptureConn) AsyncWritev(bs [][]byte, _ gnet.AsyncCallback) error {
	var joined []byte
	for _, b := range bs {
		joined = append(joined, b...)
	}
	return c.capture(joined)
}

func (c *redirectCaptureConn) AsyncWrite(b []byte, _ gnet.AsyncCallback) error {
	return c.capture(b)
}

func (c *redirectCaptureConn) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.frames))
	copy(out, c.frames)
	return out
}

// decodeClientFrame 解析 Connection.Send 产出的帧（4B 长度头 + StreamData）。
// 必须克隆后再归还池：PutClientMessage 会 Reset 字段，返回池化对象等于 use-after-free。
func decodeClientFrame(t *testing.T, b []byte) *protoGw.StreamData {
	t.Helper()
	if len(b) < 4 {
		t.Fatalf("frame too short: %v", b)
	}
	n := int(binary.BigEndian.Uint32(b[:4]))
	if n <= 0 || 4+n > len(b) {
		t.Fatalf("bad frame length %d (len=%d)", n, len(b))
	}
	msg, ok := protoGw.DecodeClientMessage(b[4 : 4+n])
	if !ok {
		t.Fatalf("decode client message failed: %v", b[4:4+n])
	}
	out := proto.Clone(msg).(*protoGw.StreamData)
	protoGw.PutClientMessage(msg)
	return out
}

// udpDownlinkSpy 记录 connection.UDPDownlink.SendUDP 调用（可配置失败）。
type udpDownlinkSpy struct {
	mu     sync.Mutex
	sids   []string
	frames [][]byte
	err    error
}

func (s *udpDownlinkSpy) SendUDP(sessionID string, frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sids = append(s.sids, sessionID)
	s.frames = append(s.frames, append([]byte(nil), frame...))
	return s.err
}

func (s *udpDownlinkSpy) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sids)
}

// lastFrame 解码最近一次 SendUDP 的帧（克隆后归还池，理由同 decodeClientFrame）。
func (s *udpDownlinkSpy) lastFrame(t *testing.T) *protoGw.StreamData {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		t.Fatal("SendUDP not called")
	}
	msg, ok := protoGw.DecodeClientMessage(s.frames[len(s.frames)-1])
	if !ok {
		t.Fatalf("decode udp frame failed: %v", s.frames[len(s.frames)-1])
	}
	out := proto.Clone(msg).(*protoGw.StreamData)
	protoGw.PutClientMessage(msg)
	return out
}

// streamGW 分流测试用 gateway stub：真实 ConnectionManager + 批量/合批开关
// + 推送计数。
type streamGW struct {
	cm        *connection.ConnectionManager
	batchPush bool
	coal      *connection.ShardedWriteCoalescer
	pushed    int64
	dropped   int64
	mu        sync.Mutex
}

func (g *streamGW) GetConnectionManager() *connection.ConnectionManager { return g.cm }
func (g *streamGW) GetGRPCConfig() config.GRPCConfig                    { return config.GRPCConfig{} }
func (g *streamGW) GetStreamConfig() config.StreamConfig {
	return config.StreamConfig{BatchPush: g.batchPush}
}
func (g *streamGW) GetGatewayID() string                                   { return "gw-test" }
func (g *streamGW) GetServerID() string                                    { return "gw-test" }
func (g *streamGW) GetLogicClient(string) connection.LogicClientProvider   { return nil }
func (g *streamGW) GetShardedCoalescer() *connection.ShardedWriteCoalescer { return g.coal }
func (g *streamGW) KickByAccounts([]string, string) int                    { return 0 }
func (g *streamGW) KickByIP(string, string) int                            { return 0 }

func (g *streamGW) AddPushedToClient(n int64) {
	g.mu.Lock()
	g.pushed += n
	g.mu.Unlock()
}

func (g *streamGW) AddPushDroppedNoConn(n int64) {
	g.mu.Lock()
	g.dropped += n
	g.mu.Unlock()
}

func (g *streamGW) pushedCount() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pushed
}

// feedStream 按序投喂 StreamBatch，耗尽后 RecvMsg 返回 io.EOF 结束接收循环。
type feedStream struct {
	batches [][]*protoGw.StreamData
}

func (f *feedStream) Send(*protoGw.StreamData) error { return nil }
func (f *feedStream) Recv() (*protoGw.StreamData, error) {
	return nil, io.EOF
}
func (f *feedStream) SendMsg(any) error { return nil }

func (f *feedStream) RecvMsg(m any) error {
	if len(f.batches) == 0 {
		return io.EOF
	}
	sb := m.(*protoGw.StreamBatch)
	sb.Items = append(sb.Items, f.batches[0]...)
	f.batches = f.batches[1:]
	return nil
}

// runReceiveMessages 同步驱动接收循环（喂完即 io.EOF 返回）。
func runReceiveMessages(t *testing.T, gw *streamGW, fs *feedStream) {
	t.Helper()
	lc := &LogicClient{gateway: gw, ctx: context.Background()}
	s := &StreamShard{
		stream: fs,
		sendCh: make(chan *protoGw.StreamData, 1),
		stopCh: make(chan struct{}),
		index:  1,
		lc:     lc,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.receiveMessages(lc, 0)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("receiveMessages did not finish")
	}
}

// TestStreamBatchUDPBypassesPushBatch 批量模式：UDP 号段消息入批前分流，
// 立即 SendByCmd 直发；同会话 WS 消息照常进 PushBatch（UDP 不得被裹进批量帧）。
func TestStreamBatchUDPBypassesPushBatch(t *testing.T) {
	gw := &streamGW{
		cm:        connection.NewConnectionManager(100, 10),
		batchPush: true,
	}
	cc := &redirectCaptureConn{}
	sid := gw.cm.AddConnection(cc, "user-1", false)
	spy := &udpDownlinkSpy{}
	gw.cm.GetConnection(sid).SetUDPDownlink(spy)

	runReceiveMessages(t, gw, &feedStream{batches: [][]*protoGw.StreamData{{
		{SessionId: sid, Cmd: testUDPCmd, Data: []byte("udp-payload")},
		{SessionId: sid, Cmd: 7, Data: []byte("ws-payload")},
	}}})

	if spy.callCount() != 1 {
		t.Fatalf("SendUDP calls = %d, want 1", spy.callCount())
	}
	if got := spy.lastFrame(t); got.Cmd != testUDPCmd || string(got.Data) != "udp-payload" {
		t.Errorf("udp frame = cmd %d data %q, want cmd %d udp-payload", got.Cmd, got.Data, testUDPCmd)
	}

	frames := cc.snapshot()
	if len(frames) != 1 {
		t.Fatalf("ws frames = %d, want 1 (仅 PushBatch)", len(frames))
	}
	batchFrame := decodeClientFrame(t, frames[0])
	if batchFrame.Cmd != protoGw.CmdPushBatch {
		t.Fatalf("ws frame cmd = %d, want CmdPushBatch %d", batchFrame.Cmd, protoGw.CmdPushBatch)
	}
	var pb protoGw.PushBatch
	if err := proto.Unmarshal(batchFrame.Data, &pb); err != nil {
		t.Fatalf("unmarshal PushBatch: %v", err)
	}
	if len(pb.Items) != 1 || pb.Items[0].Cmd != 7 {
		t.Fatalf("PushBatch items = %+v, want 1 item cmd=7（UDP 不得入批）", pb.Items)
	}
	if got := gw.pushedCount(); got != 2 {
		t.Errorf("pushed = %d, want 2 (udp 1 + batch 1)", got)
	}
}

// TestStreamBroadcastUDPPerConnection 广播：UDP 号段逐连接 SendByCmd，
// 已关联会话走 UDP，未关联会话回落 WS（各会话关联状态独立）。
func TestStreamBroadcastUDPPerConnection(t *testing.T) {
	gw := &streamGW{cm: connection.NewConnectionManager(100, 10)}
	ccA := &redirectCaptureConn{}
	ccB := &redirectCaptureConn{}
	sidA := gw.cm.AddConnection(ccA, "user-a", false)
	gw.cm.AddConnection(ccB, "user-b", false)
	spy := &udpDownlinkSpy{}
	gw.cm.GetConnection(sidA).SetUDPDownlink(spy)

	runReceiveMessages(t, gw, &feedStream{batches: [][]*protoGw.StreamData{{
		{SessionId: "", Cmd: testUDPCmd, Data: []byte("broadcast")},
	}}})

	if spy.callCount() != 1 {
		t.Fatalf("SendUDP calls = %d, want 1 (仅已关联会话A)", spy.callCount())
	}
	spy.mu.Lock()
	if spy.sids[0] != sidA {
		t.Errorf("SendUDP sid = %q, want %q", spy.sids[0], sidA)
	}
	spy.mu.Unlock()
	if frames := ccA.snapshot(); len(frames) != 0 {
		t.Errorf("会话A 不得落 WS, frames=%d", len(frames))
	}
	framesB := ccB.snapshot()
	if len(framesB) != 1 {
		t.Fatalf("会话B WS frames = %d, want 1 (未关联回落)", len(framesB))
	}
	if got := decodeClientFrame(t, framesB[0]); got.Cmd != testUDPCmd || string(got.Data) != "broadcast" {
		t.Errorf("会话B frame cmd=%d data=%q, want cmd %d broadcast", got.Cmd, got.Data, testUDPCmd)
	}
	if got := gw.pushedCount(); got != 2 {
		t.Errorf("pushed = %d, want 2", got)
	}
}

// TestStreamBroadcastUDPBypassesCoalescer 广播 + coalescer 存在：
// UDP 号段必须绕过合批直接 SendByCmd（决策⑤：UDP 不走 coalescer）。
func TestStreamBroadcastUDPBypassesCoalescer(t *testing.T) {
	gw := &streamGW{cm: connection.NewConnectionManager(100, 10)}
	gw.coal = connection.NewShardedWriteCoalescer(gw.cm, 4)
	defer gw.coal.Stop()

	cc := &redirectCaptureConn{}
	sid := gw.cm.AddConnection(cc, "user-1", false)
	spy := &udpDownlinkSpy{}
	gw.cm.GetConnection(sid).SetUDPDownlink(spy)

	runReceiveMessages(t, gw, &feedStream{batches: [][]*protoGw.StreamData{{
		{SessionId: "", Cmd: testUDPCmd, Data: []byte("bc")},
	}}})

	if spy.callCount() != 1 {
		t.Fatalf("SendUDP calls = %d, want 1 (绕过 coalescer 走 SendByCmd)", spy.callCount())
	}
	if frames := cc.snapshot(); len(frames) != 0 {
		t.Errorf("已关联会话不得落 WS, frames=%d", len(frames))
	}
}

// TestSendToClientRoutesByCmd SendToClient 按 cmd 分流：
// 号段内已关联走 UDP，号段外走 WS。
func TestSendToClientRoutesByCmd(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "")
	cc := &redirectCaptureConn{}
	sid := gw.cm.AddConnection(cc, "user-1", false)
	spy := &udpDownlinkSpy{}
	gw.cm.GetConnection(sid).SetUDPDownlink(spy)

	if _, err := srv.SendToClient(context.Background(), &protoGw.SendToClientReq{
		SessionId: sid, Cmd: testUDPCmd, Data: []byte("to-udp"),
	}); err != nil {
		t.Fatalf("SendToClient(udp): %v", err)
	}
	if spy.callCount() != 1 {
		t.Fatalf("SendUDP calls = %d, want 1", spy.callCount())
	}
	if got := spy.lastFrame(t); got.Cmd != testUDPCmd || string(got.Data) != "to-udp" {
		t.Errorf("udp frame cmd=%d data=%q, want cmd %d to-udp", got.Cmd, got.Data, testUDPCmd)
	}
	if frames := cc.snapshot(); len(frames) != 0 {
		t.Errorf("号段内不得落 WS, frames=%d", len(frames))
	}

	if _, err := srv.SendToClient(context.Background(), &protoGw.SendToClientReq{
		SessionId: sid, Cmd: 7, Data: []byte("to-ws"),
	}); err != nil {
		t.Fatalf("SendToClient(ws): %v", err)
	}
	if spy.callCount() != 1 {
		t.Errorf("号段外不得调 SendUDP, calls=%d", spy.callCount())
	}
	frames := cc.snapshot()
	if len(frames) != 1 {
		t.Fatalf("ws frames = %d, want 1", len(frames))
	}
	if got := decodeClientFrame(t, frames[0]); got.Cmd != 7 || string(got.Data) != "to-ws" {
		t.Errorf("ws frame cmd=%d data=%q, want cmd 7 to-ws", got.Cmd, got.Data)
	}
}

// TestSendToClientFallbackOnDownlinkError 下行 UDP 通道报错 → SendByCmd
// 回落 WS，消息不丢（降级不丢是硬要求）。
func TestSendToClientFallbackOnDownlinkError(t *testing.T) {
	gw := newGRPCFakeGateway()
	srv := NewGRPCServer(gw, nil, "")
	cc := &redirectCaptureConn{}
	sid := gw.cm.AddConnection(cc, "user-1", false)
	spy := &udpDownlinkSpy{err: errors.New("stale association")}
	gw.cm.GetConnection(sid).SetUDPDownlink(spy)

	if _, err := srv.SendToClient(context.Background(), &protoGw.SendToClientReq{
		SessionId: sid, Cmd: testUDPCmd, Data: []byte("must-not-lose"),
	}); err != nil {
		t.Fatalf("SendToClient 应回落成功: %v", err)
	}
	if spy.callCount() != 1 {
		t.Fatalf("SendUDP calls = %d, want 1", spy.callCount())
	}
	frames := cc.snapshot()
	if len(frames) != 1 {
		t.Fatalf("ws frames = %d, want 1 (UDP 失败必须回落)", len(frames))
	}
	if got := decodeClientFrame(t, frames[0]); got.Cmd != testUDPCmd {
		t.Errorf("回落 frame cmd=%d, want %d", got.Cmd, testUDPCmd)
	}
}
