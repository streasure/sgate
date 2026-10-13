//go:build windows

package obs

import "testing"

// TestMonoNowNsNoAlloc QPC 时钟必须零分配（曾因 LazyProc.Call 每次构造
// []uintptr 参数切片产生 ~9.6B/条的热路径分配）。
func TestMonoNowNsNoAlloc(t *testing.T) {
	if got := testing.AllocsPerRun(1000, func() { MonoNowNs() }); got != 0 {
		t.Fatalf("MonoNowNs allocs per call = %v, want 0", got)
	}
}
