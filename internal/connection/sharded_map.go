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

// DeleteIf 当 value 匹配时才删除（compare-and-delete），避免旧连接删除新连接的映射。
func (m *shardedMap[V]) DeleteIf(key string, value V) bool {
	s := m.getShard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.data[key]
	if !ok {
		return false
	}
	// comparable 类型用 ==；string 等 V 均可比较
	if any(cur) != any(value) {
		return false
	}
	delete(s.data, key)
	return true
}

// Range 遍历所有分片中的条目。回调返回 false 时终止遍历。
// 先在读锁内快照再回调，允许回调中执行 Delete/Store（否则同分片 Lock 会与 RLock 自死锁）。
func (m *shardedMap[V]) Range(fn func(key string, value V) bool) {
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		keys := make([]string, 0, len(s.data))
		vals := make([]V, 0, len(s.data))
		for k, v := range s.data {
			keys = append(keys, k)
			vals = append(vals, v)
		}
		s.mu.RUnlock()
		for j := range keys {
			if !fn(keys[j], vals[j]) {
				return
			}
		}
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
