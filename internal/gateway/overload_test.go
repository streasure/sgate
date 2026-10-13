package gateway

import (
	"os"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

// TestSmoothEMA EMA 数学：零值采样不得瞬间把平滑值打到 0。
func TestSmoothEMA(t *testing.T) {
	if got := smoothEMA(10, 0, 0.5); got != 5 {
		t.Fatalf("smoothEMA(10,0,0.5) = %v, want 5", got)
	}
	if got := smoothEMA(10, 10, 0.167); got != 10 {
		t.Fatalf("smoothEMA steady = %v, want 10", got)
	}
	if got := smoothEMA(100, 0, 0); got != 0 {
		t.Fatalf("alpha<=0 must return raw, got %v", got)
	}
	// 连续 10 个 0 采样后仍应显著高于 0（旧实现直读单点会立即归零）
	v := 10.0
	for range 10 {
		v = smoothEMA(v, 0, 0.167)
	}
	if v < 1.0 {
		t.Fatalf("after 10 zero samples smoothed = %v, want > 1", v)
	}
}

func newTestProtector(t *testing.T, observeOnly bool, cpuThreshold float64) *OverloadProtector {
	t.Helper()
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("NewProcess: %v", err)
	}
	return &OverloadProtector{
		cpuThreshold:   cpuThreshold,
		dropOnOverload: true,
		observeOnly:    observeOnly,
		checkInterval:  200 * time.Millisecond,
		stopChan:       make(chan struct{}),
		lastCheckTime:  time.Now().Add(-time.Second), // 保证首个 check() deltaWall>0
		proc:           proc,
	}
}

// TestOverloadObserveOnly benchMode：采样但不置过载标志（压测不因过载丢消息）。
func TestOverloadObserveOnly(t *testing.T) {
	op := newTestProtector(t, true, 0)
	// 预置高平滑值：首个 check() 的 EMA 由 prev 平滑而来 → 必然 > 阈值 0
	high := 50.0
	op.cpuPercent.Store(&high)

	op.check()
	if op.IsOverloaded() {
		t.Fatal("observeOnly must never set overload flag")
	}
	cpu, _, overloaded, _ := op.Stats()
	if overloaded {
		t.Fatal("Stats.overloaded must be false in observeOnly")
	}
	if cpu <= 0 {
		t.Fatalf("cpu sample = %v, want > 0 (EMA from prev)", cpu)
	}
}

// TestOverloadDropsWhenNotObserveOnly 对照：非 observe-only 超阈值必须置标志。
func TestOverloadDropsWhenNotObserveOnly(t *testing.T) {
	op := newTestProtector(t, false, 0)
	high := 50.0
	op.cpuPercent.Store(&high)

	op.check()
	if !op.IsOverloaded() {
		t.Fatal("overload flag must be set when above threshold")
	}
}

// TestOverloadBelowThreshold 平滑值低于阈值不触发。
func TestOverloadBelowThreshold(t *testing.T) {
	op := newTestProtector(t, false, 90)
	low := 1.0
	op.cpuPercent.Store(&low)

	op.check()
	if op.IsOverloaded() {
		t.Fatal("must not overload below threshold")
	}
}
