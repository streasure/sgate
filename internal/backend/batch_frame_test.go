package backend

import (
	"sync"
	"testing"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
)

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
// 曾因混发导致一端解码失败 → 重拨风暴。
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

// TestSendLoopBatchesMultipleMessages 回归：连续多条应合帧为单个 StreamBatch。
func TestSendLoopBatchesMultipleMessages(t *testing.T) {
	fs := &fakeShardStream{}
	s := &StreamShard{
		stream: fs,
		sendCh: make(chan *protoGw.StreamData, 16),
		stopCh: make(chan struct{}),
		index:  1,
	}
	// 先入队再启循环：5 条全部已在 channel 中，首轮 receive+非阻塞 drain 必收齐为单批，
	// 结果确定（先启循环时首条可能单独成批、余下拆第二批，曾在 -race 全量并发下偶发失败）。
	for i := int32(1); i <= 5; i++ {
		if err := s.SendMessage(&protoGw.StreamData{Cmd: i, Data: []byte("x")}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	go s.startSendLoop()
	defer close(s.stopCh)

	// 首批即唯一批（SendMsg 调用当下已快照全部条目），等待仅为批记录落地。
	if !waitObserved(func() bool { _, batches := fs.snapshot(); return batches > 0 }) {
		t.Fatal("send not observed in time")
	}
	raw, batches := fs.snapshot()
	if raw != 0 {
		t.Fatalf("must not mix raw frames, got %d", raw)
	}
	if batches != 1 {
		t.Fatalf("expected 1 batch, got %d", batches)
	}
	fs.mu.Lock()
	cmds := fs.batchCmds
	fs.mu.Unlock()
	if len(cmds[0]) != 5 {
		t.Fatalf("expected 5 items in batch, got %d", len(cmds[0]))
	}
}

// TestSendMessageAfterStopReturnsError：stop 后 SendMessage 快速失败。
func TestSendMessageAfterStopReturnsError(t *testing.T) {
	s := &StreamShard{
		sendCh: make(chan *protoGw.StreamData, 1),
		stopCh: make(chan struct{}),
		index:  1,
	}
	s.stop()
	if err := s.SendMessage(&protoGw.StreamData{Cmd: 1}); err != ErrNotConnected {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}
