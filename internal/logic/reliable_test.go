package logic

import (
	"errors"
	"testing"
	"time"

	protocol "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/internal/routes"
	"google.golang.org/protobuf/proto"
)

// newReliableFixture 搭建带会话的 Server + 捕获流，返回三元组。
func newReliableFixture(t *testing.T, cfg ReliabilityConfig) (*Server, *capturingStream, *streamConn) {
	t.Helper()
	s := NewServer(WithServerID("logic1"), WithReliableConfig(cfg))
	cs := &capturingStream{}
	conn := newStreamConn(cs, 64, "gw-1", 0)
	t.Cleanup(func() {
		s.Stop() // 停 janitor，避免测试 goroutine 泄漏
		conn.Close()
	})
	s.sessions.Store("sess-1", conn)
	conn.bindSession("sess-1")
	return s, cs, conn
}

// ackBody 构造 CmdMsgAck 的 protobuf 负载。
func ackBody(t *testing.T, seqs ...int64) []byte {
	t.Helper()
	data, err := proto.Marshal(&protoLogic.MsgAck{SeqIds: seqs})
	if err != nil {
		t.Fatalf("marshal MsgAck: %v", err)
	}
	return data
}

// pushReliableBatched 推送 count 条并等待全部捕获（可能跨多个批次），
// 按发送顺序返回全部 item。
func pushReliableBatched(t *testing.T, s *Server, cs *capturingStream, count int) []*protocol.StreamData {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := s.PushReliable("sess-1", 700001, []byte("payload")); err != nil {
			t.Fatalf("PushReliable #%d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := countItems(cs); got >= count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured %d items, want %d", countItems(cs), count)
		}
		time.Sleep(time.Millisecond)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []*protocol.StreamData
	for _, batch := range cs.batches {
		out = append(out, batch...)
	}
	return out[:count]
}

// countItems 统计已捕获批次的 item 总数。
func countItems(cs *capturingStream) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	for _, batch := range cs.batches {
		n += len(batch)
	}
	return n
}

// TestPushReliableSeq单调且回写 回归：可靠推送必须按会话单调分配 SeqId
// 写入 StreamData（决策③：复用既有字段承载下行序号，wire 零改动）。
func TestPushReliableSeqMonotonic(t *testing.T) {
	s, cs, _ := newReliableFixture(t, ReliabilityConfig{})

	items := pushReliableBatched(t, s, cs, 3)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	want := []int64{1, 2, 3}
	for i, it := range items {
		if it.SeqId != want[i] {
			t.Errorf("item[%d].SeqId = %d, want %d", i, it.SeqId, want[i])
		}
		if it.Cmd != 700001 {
			t.Errorf("item[%d].Cmd = %d, want 700001", i, it.Cmd)
		}
	}
	if snap := s.metrics.GetSnapshot(); snap["totalPushed"] != 3 {
		t.Errorf("totalPushed = %d, want 3", snap["totalPushed"])
	}
}

// TestPushReliableWindowFullBackpressure 决策⑥：窗口满必须返回
// ErrReliableWindowFull 背压，绝不静默丢。
func TestPushReliableWindowFullBackpressure(t *testing.T) {
	s, cs, _ := newReliableFixture(t, ReliabilityConfig{WindowSize: 2})

	if err := s.PushReliable("sess-1", 700001, []byte("a")); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if err := s.PushReliable("sess-1", 700001, []byte("b")); err != nil {
		t.Fatalf("second push: %v", err)
	}
	err := s.PushReliable("sess-1", 700001, []byte("c"))
	if !errors.Is(err, ErrReliableWindowFull) {
		t.Fatalf("third push err = %v, want ErrReliableWindowFull", err)
	}
	// 会话不存在：普通错误，非窗口满。
	if err := s.PushReliable("missing", 700001, []byte("x")); errors.Is(err, ErrReliableWindowFull) {
		t.Fatalf("missing session err = %v, want not window-full", err)
	}
	snap := s.metrics.GetSnapshot()
	if snap["totalFailed"] != 2 {
		t.Errorf("totalFailed = %d, want 2", snap["totalFailed"])
	}
	_ = cs
}

// TestMsgAckClearsWindow 决策④ ACK 批量清窗；重复/迟到 ACK 幂等。
func TestMsgAckClearsWindow(t *testing.T) {
	s, cs, _ := newReliableFixture(t, ReliabilityConfig{})

	items := pushReliableBatched(t, s, cs, 3)
	seqs := []int64{items[0].SeqId, items[1].SeqId}

	// 批量确认前两条。
	s.dispatchMessage(&protocol.StreamData{
		Cmd: routes.CmdMsgAck, SessionId: "sess-1", Data: ackBody(t, seqs...),
	}, func(*protocol.StreamData) {})

	w := s.reliable.get("sess-1")
	w.mu.Lock()
	remaining := len(w.entries)
	w.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("entries after ack = %d, want 1", remaining)
	}
	if snap := s.metrics.GetSnapshot(); snap["acked"] != 2 {
		t.Errorf("acked = %d, want 2", snap["acked"])
	}

	// 重复 ACK 同批 seq：幂等，计数不涨、剩余不动。
	s.dispatchMessage(&protocol.StreamData{
		Cmd: routes.CmdMsgAck, SessionId: "sess-1", Data: ackBody(t, seqs...),
	}, func(*protocol.StreamData) {})
	w.mu.Lock()
	remaining = len(w.entries)
	w.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("entries after dup ack = %d, want 1", remaining)
	}
	if snap := s.metrics.GetSnapshot(); snap["acked"] != 2 {
		t.Errorf("acked after dup = %d, want 2 (idempotent)", snap["acked"])
	}

	// 坏负载：只告警不 panic，不消费计数。
	s.dispatchMessage(&protocol.StreamData{
		Cmd: routes.CmdMsgAck, SessionId: "sess-1", Data: []byte{0xff, 0xff},
	}, func(*protocol.StreamData) {})
	if snap := s.metrics.GetSnapshot(); snap["acked"] != 2 {
		t.Errorf("acked after bad payload = %d, want 2", snap["acked"])
	}
}

// TestReliableRetransmitOnTimeout 超时未收 ACK → janitor 指数退避重发，
// 发的是同一条消息（同 SeqId），客户端去重窗口兜住重复执行。
func TestReliableRetransmitOnTimeout(t *testing.T) {
	s, cs, _ := newReliableFixture(t, ReliabilityConfig{
		ScanInterval: 5 * time.Millisecond, InitialBackoff: time.Millisecond, MaxRetries: 3,
	})
	s.startReliableJanitor()

	if err := s.PushReliable("sess-1", 700001, []byte("payload")); err != nil {
		t.Fatalf("push: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := countSeq(cs, 1); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retransmit not observed, seq=1 count = %d", countSeq(cs, 1))
		}
		time.Sleep(2 * time.Millisecond)
	}
	if snap := s.metrics.GetSnapshot(); snap["retransmits"] < 1 {
		t.Errorf("retransmits = %d, want >= 1", snap["retransmits"])
	}
	// 补 ACK 收尾，验证 ACK 后停止重传。
	s.dispatchMessage(&protocol.StreamData{
		Cmd: routes.CmdMsgAck, SessionId: "sess-1", Data: ackBody(t, 1),
	}, func(*protocol.StreamData) {})
	w := s.reliable.get("sess-1")
	w.mu.Lock()
	remaining := len(w.entries)
	w.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("entries after ack = %d, want 0", remaining)
	}
}

// TestReliableGiveUpAfterMaxRetries 达重传上限 → 放弃 + giveUps 计数，
// 条目移出窗口（不再永久占用）。
func TestReliableGiveUpAfterMaxRetries(t *testing.T) {
	s, cs, _ := newReliableFixture(t, ReliabilityConfig{
		ScanInterval: 5 * time.Millisecond, InitialBackoff: time.Millisecond, MaxRetries: 2,
	})
	s.startReliableJanitor()

	if err := s.PushReliable("sess-1", 700001, []byte("payload")); err != nil {
		t.Fatalf("push: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s.metrics.GetSnapshot()["giveUps"] >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("giveUps never reached, snapshot = %v", s.metrics.GetSnapshot())
		}
		time.Sleep(2 * time.Millisecond)
	}
	w := s.reliable.get("sess-1")
	w.mu.Lock()
	remaining := len(w.entries)
	w.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("entries after give-up = %d, want 0", remaining)
	}
	// 总发送 = 首发 + MaxRetries 次重传。
	if n := countSeq(cs, 1); n != 3 {
		t.Errorf("total sends for seq=1 = %d, want 3 (1 + 2 retries)", n)
	}
}

// TestOfflineClearsReliableWindow 决策④：Offline（CmdUserOffline）清窗，
// 未确认条目计入 droppedOffline，janitor 不再重发。
func TestOfflineClearsReliableWindow(t *testing.T) {
	s, cs, _ := newReliableFixture(t, ReliabilityConfig{})

	pushReliableBatched(t, s, cs, 2)
	s.dispatchMessage(&protocol.StreamData{
		Cmd: routes.CmdUserOffline, SessionId: "sess-1", UserKey: "user-1",
	}, func(*protocol.StreamData) {})

	if s.reliable.get("sess-1") != nil {
		t.Fatal("reliable window not cleared after Offline")
	}
	snap := s.metrics.GetSnapshot()
	if snap["droppedOffline"] != 2 {
		t.Errorf("droppedOffline = %d, want 2", snap["droppedOffline"])
	}
}

// TestStreamTeardownClearsReliableWindow 异常断流（不走 Offline 命令）同样清窗，
// 否则 janitor 对已死会话永久重传。
func TestStreamTeardownClearsReliableWindow(t *testing.T) {
	s := NewServer(WithServerID("logic1"), WithReliableConfig(ReliabilityConfig{}))
	t.Cleanup(s.Stop)
	fs := &capturingStream{}
	conn := newStreamConn(fs, 8, "gw-1", 0)
	s.sessions.Store("sess-1", conn)
	conn.bindSession("sess-1")

	if err := s.PushReliable("sess-1", 700001, []byte("payload")); err != nil {
		t.Fatalf("push: %v", err)
	}
	// 模拟 handleStream 退出路径：Close 流连接。
	conn.Close()
	<-conn.done

	// handleStream 的 defer 清理：断流后窗口必须回收。
	// 这里直接调用与 defer 相同的清理序列（sessions → clearReliable）。
	s.sessions.CompareAndDelete("sess-1", conn)
	s.clearReliable("sess-1")

	if s.reliable.get("sess-1") != nil {
		t.Fatal("reliable window not cleared after stream teardown")
	}
	if snap := s.metrics.GetSnapshot(); snap["droppedOffline"] != 1 {
		t.Errorf("droppedOffline = %d, want 1", snap["droppedOffline"])
	}
}

// countSeq 统计捕获流中指定 SeqId 出现次数（原始 Send + 合帧批次）。
func countSeq(cs *capturingStream, seq int64) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	for _, m := range cs.raw {
		if m.SeqId == seq {
			n++
		}
	}
	for _, batch := range cs.batches {
		for _, m := range batch {
			if m.SeqId == seq {
				n++
			}
		}
	}
	return n
}
