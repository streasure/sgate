package connection

import (
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/panjf2000/gnet/v2"
	"github.com/streasure/sgate/internal/routes"
)

// sendCaptureConn 捕获 TCP 路径写出的帧（Connection.Send → AsyncWritev）。
type sendCaptureConn struct {
	gnet.Conn
	mu     sync.Mutex
	frames [][]byte
}

func (c *sendCaptureConn) RemoteAddr() net.Addr { return nil }

func (c *sendCaptureConn) AsyncWritev(bs [][]byte, _ gnet.AsyncCallback) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var joined []byte
	for _, b := range bs {
		joined = append(joined, b...)
	}
	c.frames = append(c.frames, joined)
	return nil
}

func (c *sendCaptureConn) frameCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// fakeDownlink 记录 SendUDP 调用的桩（可配置失败）。
type fakeDownlink struct {
	mu      sync.Mutex
	calls   int
	lastSID string
	lastFrm []byte
	err     error
}

func (f *fakeDownlink) SendUDP(sessionID string, frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastSID = sessionID
	f.lastFrm = append([]byte(nil), frame...)
	return f.err
}

func (f *fakeDownlink) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestConn(id string) (*Connection, *sendCaptureConn) {
	gc := &sendCaptureConn{}
	return newConnection(id, gc, "user-1", "10.0.0.1:1234", false), gc
}

// TestSendByCmd_UDPCmdInjections 走 UDP：号段内 + 已注入 → SendUDP 收到
// 会话 ID 与原始帧，WS 不写。
func TestSendByCmd_UDPCmdInjections(t *testing.T) {
	c, gc := newTestConn("sess-1")
	d := &fakeDownlink{}
	c.SetUDPDownlink(d)

	data := []byte("udp-payload")
	if err := c.SendByCmd(routes.CmdUdpPong, data); err != nil {
		t.Fatalf("SendByCmd: %v", err)
	}
	if d.callCount() != 1 {
		t.Fatalf("SendUDP calls = %d, want 1", d.callCount())
	}
	if d.lastSID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", d.lastSID)
	}
	if string(d.lastFrm) != string(data) {
		t.Errorf("frame = %q, want %q", d.lastFrm, data)
	}
	if gc.frameCount() != 0 {
		t.Errorf("WS frames = %d, want 0 (UDP 号段不得落 WS)", gc.frameCount())
	}
}

// TestSendByCmd_WSCmdWithInjection 号段外：即使已注入也走 WS。
func TestSendByCmd_WSCmdWithInjection(t *testing.T) {
	c, gc := newTestConn("sess-2")
	d := &fakeDownlink{}
	c.SetUDPDownlink(d)

	if err := c.SendByCmd(routes.CmdPushBatch, []byte("ws-payload")); err != nil {
		t.Fatalf("SendByCmd: %v", err)
	}
	if d.callCount() != 0 {
		t.Errorf("SendUDP calls = %d, want 0 (号段外走 WS)", d.callCount())
	}
	if gc.frameCount() != 1 {
		t.Errorf("WS frames = %d, want 1", gc.frameCount())
	}
}

// TestSendByCmd_NoInjection 号段内但未注入（未 ASSOCIATE）→ 走 WS。
func TestSendByCmd_NoInjection(t *testing.T) {
	c, gc := newTestConn("sess-3")

	if err := c.SendByCmd(routes.CmdUdpPong, []byte("fallback")); err != nil {
		t.Fatalf("SendByCmd: %v", err)
	}
	if gc.frameCount() != 1 {
		t.Errorf("WS frames = %d, want 1 (无关联回落 WS)", gc.frameCount())
	}
}

// TestSendByCmd_FallbackOnError SendUDP 失败 → 回落 WS，消息不丢（硬要求）。
func TestSendByCmd_FallbackOnError(t *testing.T) {
	c, gc := newTestConn("sess-4")
	d := &fakeDownlink{err: errors.New("stale association")}
	c.SetUDPDownlink(d)

	if err := c.SendByCmd(routes.CmdUdpPong, []byte("must-not-lose")); err != nil {
		t.Fatalf("SendByCmd 应回落成功: %v", err)
	}
	if d.callCount() != 1 {
		t.Errorf("SendUDP calls = %d, want 1", d.callCount())
	}
	if gc.frameCount() != 1 {
		t.Errorf("WS frames = %d, want 1 (UDP 失败必须回落)", gc.frameCount())
	}
}

// TestSendByCmd_ConcurrentInjectAndSend 并发注入与发送（atomic.Pointer，-race 验证）。
func TestSendByCmd_ConcurrentInjectAndSend(t *testing.T) {
	c, _ := newTestConn("sess-5")

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			c.SetUDPDownlink(&fakeDownlink{})
		}
	})
	wg.Go(func() {
		for range 100 {
			_ = c.SendByCmd(routes.CmdUdpPong, []byte("x"))
		}
	})
	wg.Wait()
}
