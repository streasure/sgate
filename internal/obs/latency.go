package obs

import (
	"math"
	"sort"
	"sync"
	"time"
)

// LatencyTracker 滑动窗口延迟追踪器
// 功能：记录最近 N 个请求的延迟，计算 P50/P95/P99/Max
type LatencyTracker struct {
	mu      sync.Mutex
	samples []time.Duration
	head    int
	count   int
	size    int
}

// NewLatencyTracker 创建延迟追踪器
func NewLatencyTracker(size int) *LatencyTracker {
	if size <= 0 {
		size = 10000
	}
	return &LatencyTracker{
		samples: make([]time.Duration, size),
		size:    size,
	}
}

// Record 记录一个延迟样本
func (lt *LatencyTracker) Record(d time.Duration) {
	lt.mu.Lock()
	lt.samples[lt.head] = d
	lt.head = (lt.head + 1) % lt.size
	if lt.count < lt.size {
		lt.count++
	}
	lt.mu.Unlock()
}

// LatencyStats 延迟统计
type LatencyStats struct {
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`
	P99 time.Duration `json:"p99"`
	Max time.Duration `json:"max"`
	Cnt int           `json:"count"`
}

// GetStats 计算延迟分位数
func (lt *LatencyTracker) GetStats() LatencyStats {
	lt.mu.Lock()
	if lt.count == 0 {
		lt.mu.Unlock()
		return LatencyStats{}
	}
	// 复制有效样本
	buf := make([]time.Duration, lt.count)
	if lt.count < lt.size {
		copy(buf, lt.samples[:lt.count])
	} else {
		copy(buf, lt.samples)
	}
	lt.mu.Unlock()

	sort.Slice(buf, func(i, j int) bool { return buf[i] < buf[j] })

	getPct := func(p float64) time.Duration {
		idx := int(math.Ceil(p*float64(len(buf)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(buf) {
			idx = len(buf) - 1
		}
		return buf[idx]
	}

	return LatencyStats{
		P50: getPct(0.50),
		P95: getPct(0.95),
		P99: getPct(0.99),
		Max: buf[len(buf)-1],
		Cnt: len(buf),
	}
}
