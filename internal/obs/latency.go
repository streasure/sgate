package obs

import (
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const latencyShardCount = 16

type latencyShard struct {
	mu      sync.Mutex
	samples []time.Duration
	head    int
	count   int
}

// LatencyTracker 滑动窗口延迟追踪器
// 功能：记录最近 N 个请求的延迟，计算 P50/P95/P99/Max
// 实现：16 分片 + 原子轮转分摊锁竞争（热路径每消息调用，全局锁在多事件循环下会争用）。
type LatencyTracker struct {
	shards [latencyShardCount]latencyShard
	size   int
	rr     atomic.Uint64
}

// NewLatencyTracker 创建延迟追踪器（size 为总窗口，均摊到各分片）
func NewLatencyTracker(size int) *LatencyTracker {
	if size <= 0 {
		size = 10000
	}
	per := max(size/latencyShardCount, 1)
	lt := &LatencyTracker{size: per}
	for i := range lt.shards {
		lt.shards[i].samples = make([]time.Duration, per)
	}
	return lt
}

// Record 记录一个延迟样本
func (lt *LatencyTracker) Record(d time.Duration) {
	s := &lt.shards[lt.rr.Add(1)&(latencyShardCount-1)]
	s.mu.Lock()
	s.samples[s.head] = d
	s.head = (s.head + 1) % lt.size
	if s.count < lt.size {
		s.count++
	}
	s.mu.Unlock()
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
	buf := make([]time.Duration, 0, lt.size*latencyShardCount)
	for i := range lt.shards {
		s := &lt.shards[i]
		s.mu.Lock()
		if s.count < lt.size {
			buf = append(buf, s.samples[:s.count]...)
		} else {
			buf = append(buf, s.samples...)
		}
		s.mu.Unlock()
	}
	if len(buf) == 0 {
		return LatencyStats{}
	}

	slices.Sort(buf)

	getPct := func(p float64) time.Duration {
		idx := max(int(math.Ceil(p*float64(len(buf))))-1, 0)
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
