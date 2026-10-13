package obs

import (
	"testing"
	"time"
)

// TestLatencyTrackerShardedRecord 分片改造后：Record 的值必须原样出现在 GetStats 中。
func TestLatencyTrackerShardedRecord(t *testing.T) {
	lt := NewLatencyTracker(1600) // 每分片 100
	for i := 1; i <= 500; i++ {
		lt.Record(time.Duration(i) * time.Microsecond)
	}
	st := lt.GetStats()
	if st.Cnt != 500 {
		t.Fatalf("Cnt = %d, want 500", st.Cnt)
	}
	if st.P50 != 250*time.Microsecond {
		t.Fatalf("P50 = %v, want 250µs", st.P50)
	}
	if st.Max != 500*time.Microsecond {
		t.Fatalf("Max = %v, want 500µs", st.Max)
	}
	if st.P99 < 495*time.Microsecond {
		t.Fatalf("P99 = %v, want >= 495µs", st.P99)
	}
}

// TestLatencyTrackerSubMicrosecond 亚微秒样本不得被截断成 0。
func TestLatencyTrackerSubMicrosecond(t *testing.T) {
	lt := NewLatencyTracker(1600)
	for i := 1; i <= 400; i++ {
		lt.Record(time.Duration(700 * i)) // 700ns 步进
	}
	st := lt.GetStats()
	if st.Cnt != 400 {
		t.Fatalf("Cnt = %d, want 400", st.Cnt)
	}
	if st.P50 <= 0 {
		t.Fatalf("P50 = %v (%d ns), want > 0", st.P50, st.P50.Nanoseconds())
	}
}
