package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/types"
	"github.com/streasure/util/uhash"
)

// newManualMirror 构造无 worker 的 TrafficMirror（包内测试直达私有字段），
// 使队列投递/丢弃行为完全确定。
func newManualMirror(percent, queueCap int) *TrafficMirror {
	return &TrafficMirror{
		ctx:     context.Background(),
		percent: percent,
		queue:   make(chan *types.FilterContext, queueCap),
		stopCh:  make(chan struct{}),
	}
}

func TestTrafficMirror_Metadata(t *testing.T) {
	tm := NewTrafficMirror(config.TrafficMirrorConfig{Enabled: true, Percent: 10})
	defer tm.Stop()
	mf := &MirrorFilter{TM: tm}
	if mf.Name() != "traffic-mirror" {
		t.Errorf("Name() = %q, want traffic-mirror", mf.Name())
	}
	if mf.Phase() != types.PhasePostAuth {
		t.Errorf("Phase() = %v, want PhasePostAuth", mf.Phase())
	}
	if mf.Priority() != 300 {
		t.Errorf("Priority() = %d, want 300", mf.Priority())
	}
}

func TestTrafficMirror_SampleRateBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		percent int
	}{
		{name: "percent 0 never samples", percent: 0},
		{name: "percent 100 always samples", percent: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := newManualMirror(tt.percent, 16)
			tm.Mirror(&types.FilterContext{ConnectionID: "conn-0", Metadata: map[string]string{}})
			tm.Mirror(&types.FilterContext{ConnectionID: "conn-1", Metadata: map[string]string{}})
			forwarded, dropped := tm.Stats()
			wantForwarded := int64(0)
			if tt.percent == 100 {
				wantForwarded = 2
			}
			if forwarded != wantForwarded {
				t.Errorf("forwarded = %d, want %d", forwarded, wantForwarded)
			}
			if dropped != 0 {
				t.Errorf("dropped = %d, want 0 (queue has capacity)", dropped)
			}
		})
	}
}

func TestTrafficMirror_DeterministicSampling(t *testing.T) {
	const percent = 50
	tm := newManualMirror(percent, 16)

	hitID := findConnIDWithHash(t, percent)
	missID := findConnIDWithoutHash(t, percent)

	tm.Mirror(&types.FilterContext{ConnectionID: hitID})
	tm.Mirror(&types.FilterContext{ConnectionID: missID})
	forwarded, dropped := tm.Stats()
	if forwarded != 1 {
		t.Errorf("forwarded = %d, want 1 (only hash<%d connection)", forwarded, percent)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	// 同一连接重复采样结果稳定
	tm.Mirror(&types.FilterContext{ConnectionID: hitID})
	tm.Mirror(&types.FilterContext{ConnectionID: missID})
	if forwarded, _ := tm.Stats(); forwarded != 2 {
		t.Errorf("forwarded = %d after repeats, want 2", forwarded)
	}
}

func TestTrafficMirror_QueueFullDropsNonBlocking(t *testing.T) {
	tm := newManualMirror(100, 1) // 队列容量 1，无 worker 消费

	tm.Mirror(&types.FilterContext{ConnectionID: "c1"})
	tm.Mirror(&types.FilterContext{ConnectionID: "c2"}) // 队列满 → 丢弃计数
	tm.Mirror(&types.FilterContext{ConnectionID: "c3"}) // 继续丢弃，且不得阻塞

	forwarded, dropped := tm.Stats()
	if forwarded != 1 {
		t.Errorf("forwarded = %d, want 1", forwarded)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
}

func TestTrafficMirror_ClonesContext(t *testing.T) {
	tm := newManualMirror(100, 1)
	orig := &types.FilterContext{
		Ctx:          context.Background(),
		ConnectionID: "c1",
		RemoteIP:     "10.0.0.1",
		Route:        "login",
		Cmd:          1000001,
		Data:         []byte{1, 2, 3},
		UserUUID:     "u-1",
		Metadata:     map[string]string{"k": "v"},
	}
	tm.Mirror(orig)
	queued := <-tm.queue

	if queued == orig {
		t.Fatal("queued context is the original pointer, want a clone")
	}
	if !queued.Mirrored {
		t.Error("clone Mirrored = false, want true")
	}
	if queued.ConnectionID != orig.ConnectionID || queued.Route != orig.Route ||
		queued.Cmd != orig.Cmd || queued.UserUUID != orig.UserUUID || queued.RemoteIP != orig.RemoteIP {
		t.Errorf("clone fields mismatch: %+v", queued)
	}
	if string(queued.Data) != string(orig.Data) {
		t.Errorf("clone Data = %v, want %v", queued.Data, orig.Data)
	}
	// Data 深拷贝：改原始数据不影响已入队副本
	orig.Data[0] = 99
	if queued.Data[0] != 1 {
		t.Error("clone Data aliases original slice")
	}
	// Metadata 浅拷贝：改原始 map 不影响副本
	orig.Metadata["k"] = "changed"
	if queued.Metadata["k"] != "v" {
		t.Error("clone Metadata aliases original map")
	}
}

func TestTrafficMirror_UpdatePercent(t *testing.T) {
	tm := newManualMirror(0, 4)
	tm.Mirror(&types.FilterContext{ConnectionID: "c1"})
	if f, _ := tm.Stats(); f != 0 {
		t.Fatalf("forwarded = %d at percent 0, want 0", f)
	}
	tm.UpdatePercent(100)
	tm.Mirror(&types.FilterContext{ConnectionID: "c1"})
	if f, _ := tm.Stats(); f != 1 {
		t.Errorf("forwarded = %d after UpdatePercent(100), want 1", f)
	}
}

func TestTrafficMirror_MirrorFilterDisabled(t *testing.T) {
	// TM 为 nil：直接放行
	mf := &MirrorFilter{}
	ok, err := mf.Process(&types.FilterContext{Ctx: context.Background()})
	if err != nil || !ok {
		t.Fatalf("Process(nil TM) = (%v, %v), want (true, nil)", ok, err)
	}

	// enabled=false：放行且不投递
	tm := NewTrafficMirror(config.TrafficMirrorConfig{Enabled: false, Percent: 100})
	defer tm.Stop()
	mf = &MirrorFilter{TM: tm}
	ok, err = mf.Process(&types.FilterContext{Ctx: context.Background(), ConnectionID: "c1"})
	if err != nil || !ok {
		t.Fatalf("Process(disabled) = (%v, %v), want (true, nil)", ok, err)
	}
	if f, d := tm.Stats(); f != 0 || d != 0 {
		t.Errorf("Stats() = (%d, %d), want (0, 0) when disabled", f, d)
	}
}

func TestTrafficMirror_MirrorFilterEnabledEnqueues(t *testing.T) {
	tm := NewTrafficMirror(config.TrafficMirrorConfig{Enabled: true, Percent: 100, QueueSize: 16})
	defer tm.Stop()
	mf := &MirrorFilter{TM: tm}

	ok, err := mf.Process(&types.FilterContext{Ctx: context.Background(), ConnectionID: "c1", Metadata: map[string]string{}})
	if err != nil || !ok {
		t.Fatalf("Process(enabled) = (%v, %v), want (true, nil)", ok, err)
	}
	if f, _ := tm.Stats(); f != 1 {
		t.Errorf("forwarded = %d, want 1", f)
	}
}

func TestTrafficMirror_StopIdempotent(t *testing.T) {
	tm := NewTrafficMirror(config.TrafficMirrorConfig{Enabled: true, Percent: 10})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tm.Stop()
		tm.Stop() // stopOnce：重复 Stop 不得 panic/悬挂
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return (worker hang?)")
	}
}

func TestTrafficMirror_WorkersConsumeQueue(t *testing.T) {
	tm := NewTrafficMirror(config.TrafficMirrorConfig{Enabled: true, Percent: 100, QueueSize: 16})
	for i := 0; i < 10; i++ {
		tm.Mirror(&types.FilterContext{ConnectionID: "c", Metadata: map[string]string{}})
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(tm.queue) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue not drained, len = %d", len(tm.queue))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f, _ := tm.Stats(); f != 10 {
		t.Errorf("forwarded = %d, want 10", f)
	}
	tm.Stop()
	// 队列耗尽后 worker 阻塞在 select；Stop 后必须已退出
	done := make(chan struct{})
	go func() { tm.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("second Stop after drain hung")
	}
}

func TestTrafficMirror_MirrorPercentHashConsistentWithCanary(t *testing.T) {
	// 采样口径与 canary 相同：FNV1a32(connID)%100 < percent
	const percent = 30
	tm := newManualMirror(percent, 32)
	for i := 0; i < 20; i++ {
		id := "conn-" + string(rune('a'+i))
		tm.Mirror(&types.FilterContext{ConnectionID: id})
	}
	forwarded, _ := tm.Stats()
	var want int64
	for i := 0; i < 20; i++ {
		id := "conn-" + string(rune('a'+i))
		if int(uhash.FNV1a32(id)%100) < percent {
			want++
		}
	}
	if forwarded != want {
		t.Errorf("forwarded = %d, want %d (hash-consistent)", forwarded, want)
	}
}
