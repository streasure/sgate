package connection

import (
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/panjf2000/gnet/v2"
)

// errReportingConn 捕获 AsyncWrite/AsyncWritev 的完成回调，
// 测试以固定错误触发，模拟 gnet 写完成（失败）事件。
type errReportingConn struct {
	mockConn
	mu       sync.Mutex
	cb       gnet.AsyncCallback
	asyncErr error
}

func (c *errReportingConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func (c *errReportingConn) AsyncWrite(_ []byte, cb gnet.AsyncCallback) error {
	c.mu.Lock()
	c.cb = cb
	c.mu.Unlock()
	return nil
}

func (c *errReportingConn) AsyncWritev(_ [][]byte, cb gnet.AsyncCallback) error {
	c.mu.Lock()
	c.cb = cb
	c.mu.Unlock()
	return nil
}

// fire 以 asyncErr 触发捕获的完成回调（模拟 gnet event loop 写完成）。
func (c *errReportingConn) fire() {
	c.mu.Lock()
	cb := c.cb
	c.cb = nil
	c.mu.Unlock()
	if cb != nil {
		_ = cb(c, c.asyncErr)
	}
}

// TestAsyncWriteCallbackCountsFailure 回归：异步写完成回调曾是 Noop，写错误
// 被吞掉（/stats 完全无感知），端到端 ACK 观测缺"网关异步写客户端失败"这一环。
// 计数回调必须只在 err != nil 时累加，成功路径零计数。
func TestAsyncWriteCallbackCountsFailure(t *testing.T) {
	before := AsyncWriteFailures()

	if err := AsyncWriteCallback(nil, nil); err != nil {
		t.Fatalf("callback on success returned %v, want nil (gnet 行为保持)", err)
	}
	if got := AsyncWriteFailures(); got != before {
		t.Fatalf("success callback counted %d, want 0 delta", got-before)
	}

	if err := AsyncWriteCallback(nil, errors.New("write failed")); err != nil {
		t.Fatalf("callback on failure returned %v, want nil", err)
	}
	if got := AsyncWriteFailures(); got != before+1 {
		t.Fatalf("failure callback count = %d, want %d", got-before, 1)
	}
}

// TestSendCountsAsyncWriteFailure Connection.Send 的完成回调收到写错误时必须
// 计入 asyncWriteFailures（WS/TCP 两条封帧路径共用同一观测点）。
func TestSendCountsAsyncWriteFailure(t *testing.T) {
	before := AsyncWriteFailures()

	gc := &errReportingConn{asyncErr: errors.New("conn reset")}
	// WS 路径（AsyncWrite）
	wsConn := newConnection("id-send-fail-ws", gc, "u", "1.27.0.0.1:1", true)
	if err := wsConn.Send([]byte("payload")); err != nil {
		t.Fatalf("Send WS: %v", err)
	}
	gc.fire()
	if got := AsyncWriteFailures(); got != before+1 {
		t.Fatalf("WS async failure count = %d, want %d", got-before, 1)
	}

	// TCP 路径（AsyncWritev）
	tcpConn := newConnection("id-send-fail-tcp", gc, "u", "1.27.0.0.1:1", false)
	if err := tcpConn.Send([]byte("payload")); err != nil {
		t.Fatalf("Send TCP: %v", err)
	}
	gc.fire()
	if got := AsyncWriteFailures(); got != before+2 {
		t.Fatalf("TCP async failure count = %d, want %d", got-before, 2)
	}
}
