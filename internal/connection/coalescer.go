package connection

import (
	"hash/fnv"
	"sync"
	"time"
)

// writeCoalescer 轻量 dirty-set 跟踪器。
// 实际的数据 buffer 下沉到 Connection 对象（per-Connection coalescing），
// 这里只跟踪哪些连接有未 flush 的数据，避免全量遍历连接池。
//
// addMulti: 调 conn.AppendCoalesced（per-Connection 锁，几乎无竞争）
//   - 将 connID 加入 dirty-set（coalescer 级锁，仅 map+slice 操作）
//
// flush:    swap 出 dirty-set → 逐个调 conn.FlushCoalesced（IO 时完全无锁）
type writeCoalescer struct {
	mu        sync.Mutex
	dirty     []string            // 有未 flush 数据的连接 ID
	dirtySet  map[string]struct{} // 去重
	lastFlush time.Time
}

// coalescerBufPool 复用 coalescer 的 data buffer，避免每帧 append 分配导致 GC 风暴。
// buffer 初始容量 4KB，可动态扩展。归还时保留扩展后的容量（上限 1MB）以复用。
var coalescerBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 4096)
		return &b
	},
}

const (
	coalesceFlushCount    = 50000                // 累积 5 万条消息后 flush
	coalesceFlushInterval = 5 * time.Millisecond // 5ms 超时 flush，限制推送延迟
	coalescerMaxBufCap    = 1 << 20              // 1MB：归还到池的 buffer 容量上限
)

func newWriteCoalescer() *writeCoalescer {
	return &writeCoalescer{
		dirty:     make([]string, 0, 64),
		dirtySet:  make(map[string]struct{}, 64),
		lastFlush: time.Now(),
	}
}

// addMulti 将一条消息追加到连接的 coalescing buffer，并将连接标记为 dirty。
func (wc *writeCoalescer) addMulti(connID string, payload []byte, conn *Connection) bool {
	conn.AppendCoalesced(payload)

	wc.mu.Lock()
	if _, exists := wc.dirtySet[connID]; !exists {
		wc.dirty = append(wc.dirty, connID)
		wc.dirtySet[connID] = struct{}{}
	}
	wc.mu.Unlock()
	return true
}

func (wc *writeCoalescer) shouldFlush(totalCount int) bool {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return totalCount >= coalesceFlushCount || time.Since(wc.lastFlush) >= coalesceFlushInterval
}

// flush swap 出 dirty-set，然后逐个调 conn.FlushCoalesced（IO 无锁）。
func (wc *writeCoalescer) flush(cm *ConnectionManager) int64 {
	wc.mu.Lock()
	if len(wc.dirty) == 0 {
		wc.mu.Unlock()
		return 0
	}
	dirty := wc.dirty
	wc.dirty = make([]string, 0, cap(dirty))
	wc.dirtySet = make(map[string]struct{}, cap(dirty))
	wc.lastFlush = time.Now()
	wc.mu.Unlock()

	var pushed int64
	for _, connID := range dirty {
		conn := cm.GetConnection(connID)
		if conn != nil {
			pushed += conn.FlushCoalesced()
		}
	}
	return pushed
}

// ShardedWriteCoalescer 将 dirty-set 按连接 ID 分片，
// 每个分片独立加锁，减少高频 addMulti 下的锁竞争。
type ShardedWriteCoalescer struct {
	shards []*writeCoalescer
	shardN uint32
	cm     *ConnectionManager
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewShardedWriteCoalescer 创建分片写合并器并启动后台 flush 循环。
func NewShardedWriteCoalescer(cm *ConnectionManager, shardCount int) *ShardedWriteCoalescer {
	if shardCount <= 0 {
		shardCount = 16
	}
	sw := &ShardedWriteCoalescer{
		shards: make([]*writeCoalescer, shardCount),
		shardN: uint32(shardCount),
		cm:     cm,
		stopCh: make(chan struct{}),
	}
	for i := 0; i < shardCount; i++ {
		sw.shards[i] = newWriteCoalescer()
	}
	sw.wg.Add(1)
	go sw.flushLoop()
	return sw
}

func (sw *ShardedWriteCoalescer) getShard(connID string) *writeCoalescer {
	h := fnv.New32a()
	h.Write([]byte(connID))
	return sw.shards[h.Sum32()%sw.shardN]
}

// AddMulti 将消息追加到连接的 coalescing buffer，并将连接标记为 dirty。
func (sw *ShardedWriteCoalescer) AddMulti(connID string, payload []byte, conn *Connection) bool {
	return sw.getShard(connID).addMulti(connID, payload, conn)
}

// flushLoop 定时刷新所有分片的 dirty-set。
func (sw *ShardedWriteCoalescer) flushLoop() {
	defer sw.wg.Done()
	ticker := time.NewTicker(coalesceFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sw.stopCh:
			for _, s := range sw.shards {
				s.flush(sw.cm)
			}
			return
		case <-ticker.C:
			// 统计所有分片的总 dirty 数量，决定是否需要 flush
			totalDirty := 0
			for _, s := range sw.shards {
				s.mu.Lock()
				totalDirty += len(s.dirty)
				s.mu.Unlock()
			}
			if totalDirty > 0 {
				for _, s := range sw.shards {
					if s.shouldFlush(totalDirty) {
						s.flush(sw.cm)
					}
				}
			}
		}
	}
}

// Stop 停止 flush 循环并执行最终刷新。
func (sw *ShardedWriteCoalescer) Stop() {
	close(sw.stopCh)
	sw.wg.Wait()
}
