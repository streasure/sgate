package logic

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/streasure/protocol/gateway"
)

// blockingSendStream SendMsg 阻塞直到 release 关闭，模拟慢/卡死的 gRPC 流。
type blockingSendStream struct {
	fakeStream
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingSendStream) SendMsg(any) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return errors.New("stream released")
}

// TestStreamConnSendTimeoutWhenQueueFull 回归：sendCh 满且发送协程卡死时，
// Send 必须限时返回错误，而不是永久阻塞调用方（曾造成 logic 侧队头阻塞）。
func TestStreamConnSendTimeoutWhenQueueFull(t *testing.T) {
	old := defaultStreamSendTimeout
	defaultStreamSendTimeout = 50 * time.Millisecond
	defer func() { defaultStreamSendTimeout = old }()

	fs := &blockingSendStream{release: make(chan struct{}), entered: make(chan struct{})}
	c := newStreamConn(fs, 1, "gw-1")
	defer func() {
		close(fs.release)
		c.Close()
	}()

	// 第一条被发送协程取出并卡在 SendMsg。
	if err := c.Send(&protocol.StreamData{}); err != nil {
		t.Fatalf("first send: %v", err)
	}
	select {
	case <-fs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("send goroutine did not start SendMsg")
	}

	// 第二条填满容量为 1 的通道。
	if err := c.Send(&protocol.StreamData{}); err != nil {
		t.Fatalf("second send: %v", err)
	}

	// 第三条：通道满 + 发送协程卡死 → 必须超时报错，不得永久阻塞。
	done := make(chan error, 1)
	go func() { done <- c.Send(&protocol.StreamData{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected queue-full error, got nil")
		}
		if !strings.Contains(err.Error(), "queue full") {
			t.Fatalf("error = %v, want queue full", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send blocked indefinitely on full queue (head-of-line blocking)")
	}
}
