package backend

import (
	"sync"
	"testing"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
)

// fakeSendStream 实现 legacy 发送所需最小接口（仅 SendMsg）。
type fakeSendStream struct {
	sentMsgs []any
}

func (f *fakeSendStream) SendMsg(m any) error {
	f.sentMsgs = append(f.sentMsgs, m)
	return nil
}

// TestSendLegacyFrameAlwaysWraps 回归：单条消息必须包装为 StreamBatch
// （StreamBatch 为固定线格式，接收端固定按其解码，混发裸帧曾导致
// 一端解码失败 → 重拨风暴）。
func TestSendLegacyFrameAlwaysWraps(t *testing.T) {
	msg := &protoGw.StreamData{Cmd: 42, Data: []byte("resp")}
	f := &fakeSendStream{}
	if err := sendLegacyFrame(f, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(f.sentMsgs) != 1 {
		t.Fatalf("sentMsgs = %d, want 1", len(f.sentMsgs))
	}
	sb, ok := f.sentMsgs[0].(*protoGw.StreamBatch)
	if !ok || len(sb.Items) != 1 || sb.Items[0] != msg {
		t.Fatalf("expected single-item StreamBatch, got %#v", f.sentMsgs[0])
	}
}

// fakeShardStream 捕获 startSendLoop 的发送调用（测试合帧回归）。
// 注意：发送成功后消息会归还对象池（字段被清零），因此必须在调用当下
// 快照字段值，不能保存指针事后读取；捕获状态用互斥锁与测试协程同步。
type fakeShardStream struct {
	mu        sync.Mutex
	rawCount  int
	batchCmds [][]int32
}

func (f *fakeShardStream) Send(m *protoGw.StreamData) error {
	f.mu.Lock()
	f.rawCount++
	f.mu.Unlock()
	return nil
}
func (f *fakeShardStream) Recv() (*protoGw.StreamData, error) { return nil, nil }
func (f *fakeShardStream) RecvMsg(m any) error                { return nil }
func (f *fakeShardStream) SendMsg(m any) error {
	sb, ok := m.(*protoGw.StreamBatch)
	if !ok {
		panic("expected *protoGw.StreamBatch")
	}
	cmds := make([]int32, 0, len(sb.Items))
	for _, item := range sb.Items {
		cmds = append(cmds, item.Cmd)
	}
	f.mu.Lock()
	f.batchCmds = append(f.batchCmds, cmds)
	f.mu.Unlock()
	return nil
}

// snapshot 返回 (raw 帧数, 合帧数)。
func (f *fakeShardStream) snapshot() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rawCount, len(f.batchCmds)
}

func waitObserved(cond func() bool) bool {
	for range 500 {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestSendLoopAlwaysWrapsStreamBatch 回归：单条消息也必须包装为 StreamBatch，
// 绝不能混发裸 StreamData（StreamBatch 为固定线格式，无开关）。
func TestSendLoopAlwaysWrapsStreamBatch(t *testing.T) {
	fs := &fakeShardStream{}
	s := &StreamShard{
		stream: fs,
		sendCh: make(chan *protoGw.StreamData, 8),
		stopCh: make(chan struct{}),
		index:  1,
	}
	go s.startSendLoop()
	defer close(s.stopCh)

	if err := s.SendMessage(&protoGw.StreamData{Cmd: 7, Data: []byte("x")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !waitObserved(func() bool { raw, batches := fs.snapshot(); return raw+batches > 0 }) {
		t.Fatal("send not observed in time")
	}
	raw, _ := fs.snapshot()
	if raw != 0 {
		t.Fatalf("must not mix raw frames, got %d", raw)
	}
	fs.mu.Lock()
	cmds := fs.batchCmds
	fs.mu.Unlock()
	if len(cmds) != 1 || len(cmds[0]) != 1 || cmds[0][0] != 7 {
		t.Fatalf("expected 1 batch with 1 item cmd=7, got %v", cmds)
	}
}
