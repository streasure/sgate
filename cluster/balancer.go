package cluster

import (
	"hash/fnv"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/streasure/sgate/util"
	"github.com/streasure/sgate/internal/config"
	tlog "github.com/streasure/treasure-slog"
)

// BalancerAlgorithm 负载均衡算法
type BalancerAlgorithm string

const (
	BalancerRoundRobin BalancerAlgorithm = "roundRobin"
	BalancerWeighted   BalancerAlgorithm = "weighted"
	BalancerLeastConn  BalancerAlgorithm = "leastConn"
	BalancerConsistent BalancerAlgorithm = "consistent"
)

// BalancerNode 上游节点
type BalancerNode struct {
	ID        string
	Address   string
	Weight    int
	connCount atomic.Int64 // 当前活跃连接数（用于 leastConn）
	healthy   atomic.Int32 // 1=健康 0=故障（被摘除）
	failures  atomic.Int32 // 连续失败计数
}

// Balancer 负载均衡器
type Balancer struct {
	mu        sync.RWMutex
	nodes     []*BalancerNode
	algorithm BalancerAlgorithm
	// 一致性哈希
	ring    []uint32 // 哈希环
	ringMap map[uint32]*BalancerNode
	// 轮询
	rrIndex uint64
	// 摘除策略
	failureThreshold int
	recoverInterval  time.Duration
	stopChan         chan struct{}
}

// NewBalancer 创建负载均衡器
func NewBalancer(cfg config.BalancerConfig) *Balancer {
	b := &Balancer{
		algorithm:        BalancerAlgorithm(cfg.Algorithm),
		ringMap:          make(map[uint32]*BalancerNode),
		failureThreshold: cfg.FailureThreshold,
		recoverInterval:  util.ParseDurationDefault(cfg.RecoverInterval, 30*time.Second),
		stopChan:         make(chan struct{}),
	}
	if b.algorithm == "" {
		b.algorithm = BalancerRoundRobin
	}
	if b.failureThreshold <= 0 {
		b.failureThreshold = 3
	}
	go b.recoverLoop()
	return b
}

// AddNode 添加节点
func (b *Balancer) AddNode(id, addr string, weight int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if weight <= 0 {
		weight = 1
	}
	node := &BalancerNode{
		ID:      id,
		Address: addr,
		Weight:  weight,
	}
	node.healthy.Store(1)
	b.nodes = append(b.nodes, node)
	if b.algorithm == BalancerConsistent {
		b.rebuildRingLocked()
	}
	tlog.Info("balancer: node added", "id", id, "addr", addr, "weight", weight)
}

// RemoveNode 移除节点
func (b *Balancer) RemoveNode(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, n := range b.nodes {
		if n.ID == id {
			b.nodes = append(b.nodes[:i], b.nodes[i+1:]...)
			if b.algorithm == BalancerConsistent {
				b.rebuildRingLocked()
			}
			tlog.Info("balancer: node removed", "id", id)
			return
		}
	}
}

// Pick 选一个健康节点
func (b *Balancer) Pick(key string) *BalancerNode {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.nodes) == 0 {
		return nil
	}
	switch b.algorithm {
	case BalancerRoundRobin:
		return b.pickRoundRobin()
	case BalancerWeighted:
		return b.pickWeighted()
	case BalancerLeastConn:
		return b.pickLeastConn()
	case BalancerConsistent:
		return b.pickConsistent(key)
	default:
		return b.pickRoundRobin()
	}
}

// RecordSuccess / RecordFailure 上游响应结果回写
func (b *Balancer) RecordSuccess(id string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, n := range b.nodes {
		if n.ID == id {
			n.failures.Store(0)
			if n.healthy.CompareAndSwap(0, 1) {
				tlog.Info("balancer: node recovered", "id", id)
			}
			return
		}
	}
}

func (b *Balancer) RecordFailure(id string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, n := range b.nodes {
		if n.ID == id {
			cnt := n.failures.Add(1)
			if int(cnt) >= b.failureThreshold {
				if n.healthy.CompareAndSwap(1, 0) {
					tlog.Warn("balancer: node marked unhealthy", "id", id, "failures", cnt)
				}
			}
			return
		}
	}
}

// AcquireConn / ReleaseConn 活跃连接计数（用于 leastConn）
func (n *BalancerNode) AcquireConn() { n.connCount.Add(1) }
func (n *BalancerNode) ReleaseConn() { n.connCount.Add(-1) }

func (n *BalancerNode) IsHealthy() bool { return n.healthy.Load() == 1 }

// 健康节点列表
func (b *Balancer) healthyNodes() []*BalancerNode {
	out := make([]*BalancerNode, 0, len(b.nodes))
	for _, n := range b.nodes {
		if n.IsHealthy() {
			out = append(out, n)
		}
	}
	return out
}

func (b *Balancer) pickRoundRobin() *BalancerNode {
	h := b.healthyNodes()
	if len(h) == 0 {
		return nil
	}
	idx := atomic.AddUint64(&b.rrIndex, 1)
	return h[idx%uint64(len(h))]
}

func (b *Balancer) pickWeighted() *BalancerNode {
	h := b.healthyNodes()
	if len(h) == 0 {
		return nil
	}
	total := 0
	for _, n := range h {
		total += n.Weight
	}
	if total <= 0 {
		return h[0]
	}
	r := rand.Intn(total)
	for _, n := range h {
		r -= n.Weight
		if r < 0 {
			return n
		}
	}
	return h[0]
}

func (b *Balancer) pickLeastConn() *BalancerNode {
	h := b.healthyNodes()
	if len(h) == 0 {
		return nil
	}
	best := h[0]
	bestCnt := best.connCount.Load()
	for _, n := range h[1:] {
		cnt := n.connCount.Load()
		if cnt < bestCnt {
			best = n
			bestCnt = cnt
		}
	}
	return best
}

func (b *Balancer) pickConsistent(key string) *BalancerNode {
	if len(b.ring) == 0 {
		return nil
	}
	h := fnvHash32(key)
	// 二分查找第一个 >= h 的节点
	lo, hi := 0, len(b.ring)
	for lo < hi {
		mid := (lo + hi) / 2
		if b.ring[mid] < h {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(b.ring) {
		lo = 0
	}
	// 找第一个健康节点
	for i := 0; i < len(b.ring); i++ {
		idx := (lo + i) % len(b.ring)
		if n, ok := b.ringMap[b.ring[idx]]; ok && n.IsHealthy() {
			return n
		}
	}
	return nil
}

// rebuildRingLocked 重建一致性哈希环
func (b *Balancer) rebuildRingLocked() {
	b.ring = b.ring[:0]
	b.ringMap = make(map[uint32]*BalancerNode, len(b.nodes)*160)
	// 每个节点 160 个虚拟节点
	for _, n := range b.nodes {
		for i := 0; i < 160; i++ {
			vh := fnvHash32(n.ID + "-" + itoa(i))
			b.ring = append(b.ring, vh)
			b.ringMap[vh] = n
		}
	}
	// 排序
	for i := 1; i < len(b.ring); i++ {
		for j := i; j > 0 && b.ring[j] < b.ring[j-1]; j-- {
			b.ring[j], b.ring[j-1] = b.ring[j-1], b.ring[j]
		}
	}
}

// recoverLoop 周期性尝试恢复被摘除的节点
func (b *Balancer) recoverLoop() {
	ticker := time.NewTicker(b.recoverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			b.mu.RLock()
			for _, n := range b.nodes {
				if !n.IsHealthy() {
					// 探活：交给上层 grpc 主动健康检查触发；这里仅置回半开状态
					n.failures.Store(0)
					n.healthy.Store(1)
					tlog.Info("balancer: node set to half-open (probe)", "id", n.ID)
				}
			}
			b.mu.RUnlock()
		}
	}
}

// Stop 停止 balancer
func (b *Balancer) Stop() { close(b.stopChan) }

// Stats 返回节点统计
func (b *Balancer) Stats() []map[string]interface{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(b.nodes))
	for _, n := range b.nodes {
		out = append(out, map[string]interface{}{
			"id":       n.ID,
			"address":  n.Address,
			"weight":   n.Weight,
			"healthy":  n.IsHealthy(),
			"conns":    n.connCount.Load(),
			"failures": n.failures.Load(),
		})
	}
	return out
}

func fnvHash32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
