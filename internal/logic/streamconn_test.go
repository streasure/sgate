package logic

import (
	"context"
	"errors"
	"sync"
	"testing"

	protocol "github.com/streasure/protocol/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type fakeStream struct {
	mu      sync.Mutex
	sendErr error
}

func (f *fakeStream) Send(*protocol.StreamData) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendErr
}
func (f *fakeStream) Recv() (*protocol.StreamData, error) { return nil, nil }
func (f *fakeStream) SetHeader(metadata.MD) error         { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error        { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)              {}
func (f *fakeStream) Context() context.Context            { return context.Background() }
func (f *fakeStream) SendMsg(any) error                   { return nil }
func (f *fakeStream) RecvMsg(any) error                   { return nil }

var _ grpc.ServerStream = (*fakeStream)(nil)

// TestStreamConnCloseNoDoubleClose 回归：网关断开导致发送协程先退出并关闭 done，
// 随后 Close 再次 close(done) 曾触发 panic: close of closed channel 打挂 logic 进程。
func TestStreamConnCloseNoDoubleClose(t *testing.T) {
	fs := &fakeStream{sendErr: errors.New("stream broken")}
	c := newStreamConn(fs, 4, "gw-1")

	if err := c.Send(&protocol.StreamData{}); err != nil {
		t.Fatalf("send before close: %v", err)
	}

	// 等发送协程因 Send 失败自行 shutdown。
	deadline := make(chan struct{})
	go func() {
		for !c.closed.Load() {
		}
		close(deadline)
	}()
	<-deadline

	// OnData 返回时的 defer Close 不得 double close。
	c.Close()
	c.Close()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				_ = c.Send(&protocol.StreamData{})
				c.Close()
			}
		})
	}
	wg.Wait()

	if err := c.Send(&protocol.StreamData{}); err == nil {
		t.Fatal("send on closed stream should return error")
	}
}
