package connection

import (
	"hash/fnv"
	"sync"
)

// ============================================================================
// 分片 map — 替代 sync.Map，减少锁竞争和内存开销
// ============================================================================

const shardCount = 256 // 必须是 2 的幂

// shard 是分片 map 的单个分片，持有独立的读写锁。
type shard[V any] struct {
	mu   sync.RWMutex
	data map[string]V
}

// shardedMap 是基于 FNV-1a 哈希的分片并发 map，适用于高读少写的场景。
type shardedMap[V any] struct {
	shards [shardCount]shard[V]
}

func newShardedMap[V any]() *shardedMap[V] {
	m := &shardedMap[V]{}
	for i := range m.shards {
		m.shards[i].data = make(map[string]V)
	}
	return m
}

func (m *shardedMap[V]) getShard(key string) *shard[V] {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &m.shards[h.Sum32()%shardCount]
}

func (m *shardedMap[V]) Load(key string) (V, bool) {
	s := m.getShard(key)
	s.mu.RLock()
	v, ok := s.data[key]
	s.mu.RUnlock()
	return v, ok
}

func (m *shardedMap[V]) Store(key string, value V) {
	s := m.getShard(key)
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
}

func (m *shardedMap[V]) Delete(key string) {
	s := m.getShard(key)
	s.mu.Lock()
	delete(s.data, key)
	s.mu.Unlock()
}

// Range 遍历所有分片中的条目。回调返回 false 时终止遍历。
func (m *shardedMap[V]) Range(fn func(key string, value V) bool) {
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		for k, v := range s.data {
			if !fn(k, v) {
				s.mu.RUnlock()
				return
			}
		}
		s.mu.RUnlock()
	}
}

// Count 返回 map 中的总条目数（遍历所有分片，开销较大，仅用于统计）。
func (m *shardedMap[V]) Count() int {
	total := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		total += len(s.data)
		s.mu.RUnlock()
	}
	return total
}
