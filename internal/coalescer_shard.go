package internal

import (
	"hash/fnv"
	"sync"
	"time"
)

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
