package connection

import (
	"net"
	"sync"
	"testing"

	"github.com/panjf2000/gnet/v2"
)

// cbCapturingConn 捕获 AsyncWrite 的回调，供测试手动触发完成事件。
type cbCapturingConn struct {
	mockConn
	mu sync.Mutex
	cb gnet.AsyncCallback
}

func (c *cbCapturingConn) AsyncWrite(_ []byte, cb gnet.AsyncCallback) error {
	c.mu.Lock()
	c.cb = cb
	c.mu.Unlock()
	return nil
}

func (c *cbCapturingConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func (c *cbCapturingConn) takeCallback() gnet.AsyncCallback {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cb
}

// TestSendMultiWithCallbackWSCarriesCallback 回归：WS 连接曾走 noopAsyncCallback
// 分支丢弃回调，导致 FlushCoalesced 的池化缓冲永不归还（coalescerBufPool 只出不进）。
func TestSendMultiWithCallbackWSCarriesCallback(t *testing.T) {
	gc := &cbCapturingConn{}
	c := newConnection("id-ws", gc, "u", "1.27.0.0.1:1")
	c.SetWS(true)

	var called bool
	if err := c.SendMultiWithCallback([]byte("payload"), func() { called = true }); err != nil {
		t.Fatalf("SendMultiWithCallback: %v", err)
	}

	cb := gc.takeCallback()
	if cb == nil {
		t.Fatal("AsyncWrite callback = nil; WS path must not substitute noop")
	}
	if err := cb(gc, nil); err != nil {
		t.Fatalf("callback invocation: %v", err)
	}
	if !called {
		t.Fatal("user callback not invoked on WS connection (pool buffer would never return)")
	}
}

// TestFlushCoalescedWSInvokesCallback 端到端：WS 连接 AppendCoalesced + FlushCoalesced
// 后，AsyncWrite 完成回调必须触发（旧实现 WS 分支用 noop，缓冲归还逻辑被跳过）。
func TestFlushCoalescedWSInvokesCallback(t *testing.T) {
	gc := &cbCapturingConn{}
	c := newConnection("id-ws-flush", gc, "u", "1.27.0.0.1:1")
	c.SetWS(true)

	if !c.AppendCoalesced([]byte("msg-a")) {
		t.Fatal("AppendCoalesced failed")
	}
	if got := c.FlushCoalesced(); got != 1 {
		t.Fatalf("FlushCoalesced = %d, want 1", got)
	}

	cb := gc.takeCallback()
	if cb == nil {
		t.Fatal("FlushCoalesced on WS must pass a real callback to AsyncWrite (pool return depends on it)")
	}
	// 触发完成回调：归还池化缓冲不得 panic。
	if err := cb(gc, nil); err != nil {
		t.Fatalf("callback: %v", err)
	}
}
