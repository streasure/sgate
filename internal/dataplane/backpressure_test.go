package dataplane

import (
	"errors"
	"testing"

	"github.com/panjf2000/gnet/v2"
)

// 下行写背压（有界化）测试：benchmark-report「场景 C OOM」遗留修复建议 1。
// 覆盖裁定语义「先延迟后丢弃 + 每连接上限/全局安全阀」的六个面：
// 合批缓冲硬顶、flush 延迟、全局阀份额豁免、直发拒绝、在途字节记账生命周期、
// coalescer 延迟后重标脏。
// 注意：pendingWriteTotal/backpressureDrops/pendingValveShare 为包级共享原子，
// 本文件测试一律串行（不加 t.Parallel），并用 saveWriteState 保存/清零/还原。

// saveWriteState 保存背压共享原子的当前值并清零，测试结束时还原，
// 避免污染同包其他测试的断言基线。
func saveWriteState(t *testing.T) {
	t.Helper()
	pt, drops, share := pendingWriteTotal.Load(), backpressureDrops.Load(), pendingValveShare.Load()
	pendingWriteTotal.Store(0)
	backpressureDrops.Store(0)
	pendingValveShare.Store(0)
	t.Cleanup(func() {
		pendingWriteTotal.Store(pt)
		backpressureDrops.Store(drops)
		pendingValveShare.Store(share)
	})
}

// failWriteConn 模拟 gnet 入队失败（AsyncWrite 返回非 nil：回调不触发）。
type failWriteConn struct {
	mockConn
}

func (failWriteConn) AsyncWrite(_ []byte, _ gnet.AsyncCallback) error {
	return errors.New("enqueue failed")
}

func (failWriteConn) AsyncWritev(_ [][]byte, _ gnet.AsyncCallback) error {
	return errors.New("enqueue failed")
}

// TestAppendCoalescedHardCap 合批缓冲硬顶（先延迟后丢弃的最后一步）：
// 攒积超过 maxCoalescedBytes 后丢新并计数，缓冲不再增长；
// 空缓冲首条不设限（单条大消息放行，上限只约束攒积）。
func TestAppendCoalescedHardCap(t *testing.T) {
	saveWriteState(t)

	c := newConnection("bp-cap", mockConn{}, "u", "1.2.3.4:1", false)
	payload := make([]byte, 8192) // 单条 8KB（4+8192=8196B/条），256KB 顶攒 31 条

	var accepted int
	for range 64 {
		if c.AppendCoalesced(payload) {
			accepted++
		}
	}
	const wantAccepted = 31 // 31×8196=254,076 ≤ 262,144；第 32 条 262,272 > 262,144 → 拒
	if accepted != wantAccepted {
		t.Fatalf("accepted=%d want %d", accepted, wantAccepted)
	}
	if got := BackpressureDrops(); got != 64-wantAccepted {
		t.Fatalf("drops=%d want %d", got, 64-wantAccepted)
	}
	if !c.hasCoalesced() {
		t.Fatal("buffer should retain accepted messages")
	}
	// 缓冲尺寸不得越过硬顶
	c.coalescedMu.Lock()
	size := len(c.coalescedData)
	c.coalescedMu.Unlock()
	if size > maxCoalescedBytes {
		t.Fatalf("buffer size=%d exceeds hard cap %d", size, maxCoalescedBytes)
	}

	// 空缓冲首条超顶放行：上限只约束攒积，不截断单条大消息
	big := newConnection("bp-cap-big", mockConn{}, "u", "1.2.3.4:1", false)
	if !big.AppendCoalesced(make([]byte, maxCoalescedBytes+4096)) {
		t.Fatal("first message on empty buffer must be accepted regardless of size")
	}
	if big.AppendCoalesced([]byte("x")) {
		t.Fatal("append after oversized first message must be rejected")
	}
}

// TestFlushCoalescedDeferredOnPendingLimit 每连接在途写超限 → flush 延迟：
// 返回 0、缓冲原样保留、全局预算不占；解除限制后重试成功。
func TestFlushCoalescedDeferredOnPendingLimit(t *testing.T) {
	saveWriteState(t)

	gc := &cbCapturingConn{}
	c := newConnection("bp-flush", gc, "u", "1.2.3.4:1", false)

	// 在途写字节顶到每连接上限（模拟慢消费者积压）
	c.pendingWriteBytes.Store(maxConnPendingWrite)

	if !c.AppendCoalesced([]byte("deferred")) {
		t.Fatal("append should succeed while buffer is empty")
	}
	if got := c.FlushCoalesced(); got != 0 {
		t.Fatalf("FlushCoalesced=%d want 0 (deferred)", got)
	}
	if !c.hasCoalesced() {
		t.Fatal("buffer must be retained when flush is deferred")
	}
	if got := PendingWriteTotal(); got != 0 {
		t.Fatalf("pendingWriteTotal=%d want 0 (deferred flush must not consume budget)", got)
	}
	if gc.takeCallback() != nil {
		t.Fatal("no async write should be enqueued while deferred")
	}

	// 解除限制 → 重试成功，入队并占预算
	c.pendingWriteBytes.Store(0)
	if got := c.FlushCoalesced(); got != 1 {
		t.Fatalf("FlushCoalesced=%d want 1 after release", got)
	}
	wantN := int64(4 + len("deferred"))
	if got := PendingWriteTotal(); got != wantN {
		t.Fatalf("pendingWriteTotal=%d want %d", got, wantN)
	}
	if c.pendingWriteBytes.Load() != wantN {
		t.Fatalf("conn pending=%d want %d", c.pendingWriteBytes.Load(), wantN)
	}
	if c.hasCoalesced() {
		t.Fatal("buffer should be empty after successful flush")
	}

	// 完成回调归还
	cb := gc.takeCallback()
	if cb == nil {
		t.Fatal("async callback should be captured")
	}
	_ = cb(gc, nil)
	if got := PendingWriteTotal(); got != 0 {
		t.Fatalf("pendingWriteTotal=%d want 0 after completion", got)
	}
	if got := c.pendingWriteBytes.Load(); got != 0 {
		t.Fatalf("conn pending=%d want 0 after completion", got)
	}
}

// TestFlushCoalescedGlobalValveShareExemption 全局安全阀份额豁免：
// 阀触发（全局在途 > 1GB）时只约束超过公平份额的连接，
// 份额以下的健康连接照常 flush，不被慢连接囤积饿死。
func TestFlushCoalescedGlobalValveShareExemption(t *testing.T) {
	saveWriteState(t)

	const share = int64(1024)
	pendingWriteTotal.Store(maxGlobalPendingWrite) // 阀已触发
	pendingValveShare.Store(share)

	healthy := newConnection("bp-healthy", &cbCapturingConn{}, "u", "1.2.3.4:1", false)
	hog := newConnection("bp-hog", &cbCapturingConn{}, "u", "1.2.3.4:1", false)
	hog.pendingWriteBytes.Store(2 * share) // 囤积连接已超公平份额

	if !healthy.AppendCoalesced([]byte("ok")) || !hog.AppendCoalesced([]byte("ok")) {
		t.Fatal("append should succeed")
	}

	if got := healthy.FlushCoalesced(); got != 1 {
		t.Fatalf("healthy flush=%d want 1 (below share must not be starved)", got)
	}
	if got := hog.FlushCoalesced(); got != 0 {
		t.Fatalf("hog flush=%d want 0 (above share deferred by valve)", got)
	}
	if !hog.hasCoalesced() {
		t.Fatal("hog buffer must be retained when deferred")
	}
}

// TestSendRejectedByBackpressure 直发超预算 → ErrWriteBackpressure：
// 计数且不入队（gnet AsyncWrite 不被调用）。
func TestSendRejectedByBackpressure(t *testing.T) {
	saveWriteState(t)

	gc := &errReportingConn{}
	c := newConnection("bp-send", gc, "u", "1.2.3.4:1", false)
	c.pendingWriteBytes.Store(maxConnPendingWrite)

	err := c.Send([]byte("payload"))
	if !errors.Is(err, ErrWriteBackpressure) {
		t.Fatalf("Send err=%v want ErrWriteBackpressure", err)
	}
	if got := BackpressureDrops(); got != 1 {
		t.Fatalf("drops=%d want 1", got)
	}
	gc.mu.Lock()
	enqueued := gc.cb != nil
	gc.mu.Unlock()
	if enqueued {
		t.Fatal("rejected message must not be enqueued")
	}
	if got := PendingWriteTotal(); got != 0 {
		t.Fatalf("pendingWriteTotal=%d want 0", got)
	}
}

// TestPendingWriteAccountingLifecycle 在途字节记账生命周期：
// 入队成功 → 本地/全局同步占位；完成回调 → 成对归零；
// 入队失败（AsyncWrite 返回非 nil，回调不触发）→ 同步回滚且不计背压拒绝。
func TestPendingWriteAccountingLifecycle(t *testing.T) {
	saveWriteState(t)

	// 入队成功 → 占位，回调 → 归还
	gc := &errReportingConn{}
	c := newConnection("bp-acct", gc, "u", "1.2.3.4:1", false)
	if err := c.Send([]byte("payload")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wantN := int64(4 + len("payload"))
	if got := c.pendingWriteBytes.Load(); got != wantN {
		t.Fatalf("conn pending=%d want %d", got, wantN)
	}
	if got := PendingWriteTotal(); got != wantN {
		t.Fatalf("pendingWriteTotal=%d want %d", got, wantN)
	}
	gc.fire()
	if got := c.pendingWriteBytes.Load(); got != 0 {
		t.Fatalf("conn pending=%d want 0 after completion", got)
	}
	if got := PendingWriteTotal(); got != 0 {
		t.Fatalf("pendingWriteTotal=%d want 0 after completion", got)
	}

	// 入队失败 → 回滚占位；不计背压拒绝（背压只统计预算拒绝）
	c2 := newConnection("bp-acct-fail", failWriteConn{}, "u", "1.2.3.4:1", false)
	before := BackpressureDrops()
	if err := c2.Send([]byte("payload")); err == nil {
		t.Fatal("Send should fail when enqueue fails")
	}
	if got := c2.pendingWriteBytes.Load(); got != 0 {
		t.Fatalf("conn pending=%d want 0 after enqueue failure rollback", got)
	}
	if got := PendingWriteTotal(); got != 0 {
		t.Fatalf("pendingWriteTotal=%d want 0 after enqueue failure rollback", got)
	}
	if got := BackpressureDrops(); got != before {
		t.Fatalf("drops delta=%d want 0 (enqueue failure is not backpressure)", got-before)
	}
}

// TestCoalescerRedirtyAfterDeferredFlush flush 被背压延迟后必须重新标脏：
// 否则「只剩这批延迟数据」的连接会滞留到下条推送才被唤醒。
func TestCoalescerRedirtyAfterDeferredFlush(t *testing.T) {
	saveWriteState(t)

	cm := NewConnectionManager(0, 0)
	gc := &cbCapturingConn{}
	id := cm.AddConnection(gc, "u", false)
	conn := cm.GetConnection(id)
	if conn == nil {
		t.Fatal("connection not found after AddConnection")
	}
	wc := newWriteCoalescer()

	// 在途写顶满每连接上限 → flush 延迟
	conn.pendingWriteBytes.Store(maxConnPendingWrite)
	if !wc.addMulti(id, []byte("payload"), conn) {
		t.Fatal("addMulti should succeed (buffer empty)")
	}
	if got := wc.flush(cm); got != 0 {
		t.Fatalf("flush=%d want 0 (deferred)", got)
	}
	wc.mu.Lock()
	nDirty := len(wc.dirty)
	wc.mu.Unlock()
	if nDirty != 1 {
		t.Fatalf("dirty=%d want 1 (deferred conn must be re-marked dirty)", nDirty)
	}
	if !conn.hasCoalesced() {
		t.Fatal("buffer must be retained")
	}

	// 解除限制 → 下轮重试成功，dirty 清空
	conn.pendingWriteBytes.Store(0)
	if got := wc.flush(cm); got != 1 {
		t.Fatalf("flush=%d want 1 after release", got)
	}
	wc.mu.Lock()
	nDirty = len(wc.dirty)
	wc.mu.Unlock()
	if nDirty != 0 {
		t.Fatalf("dirty=%d want 0 after successful flush", nDirty)
	}
}
