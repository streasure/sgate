package security

import (
	"maps"
	"sync"
	"time"
)

// BanRecord 单条用户封禁记录。
// TODO: 后续将封禁状态从内存迁移到 MySQL（含过期清理与多网关共享）。
type BanRecord struct {
	UserUUID  string    `json:"userUuid"`
	Reason    string    `json:"reason"`
	Jti       string    `json:"jti,omitempty"`
	BannedAt  time.Time `json:"bannedAt"`
	ExpiresAt time.Time `json:"expiresAt"` // 零值 = 永久封禁
}

// Expired 判断封禁是否已到期（永久封禁返回 false）。
func (r *BanRecord) Expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// BanStore 进程内封禁表（按 userUuid 索引）。
// TODO: 改为 MySQL 持久化 + 启动加载；多网关实例需共享存储。
type BanStore struct {
	mu    sync.RWMutex
	items map[string]BanRecord
}

// NewBanStore 创建空封禁表。
func NewBanStore() *BanStore {
	return &BanStore{items: make(map[string]BanRecord)}
}

// Add 写入或覆盖封禁记录。ttl<=0 表示永久。
func (s *BanStore) Add(userUUID, reason, jti string, ttl time.Duration) BanRecord {
	now := time.Now()
	rec := BanRecord{
		UserUUID: userUUID,
		Reason:   reason,
		Jti:      jti,
		BannedAt: now,
	}
	if ttl > 0 {
		rec.ExpiresAt = now.Add(ttl)
	}
	s.mu.Lock()
	s.items[userUUID] = rec
	s.mu.Unlock()
	return rec
}

// Remove 解除封禁，返回是否曾存在。
func (s *BanStore) Remove(userUUID string) bool {
	s.mu.Lock()
	_, ok := s.items[userUUID]
	delete(s.items, userUUID)
	s.mu.Unlock()
	return ok
}

// Get 返回当前有效封禁记录；未封禁或已过期返回 nil。
func (s *BanStore) Get(userUUID string) *BanRecord {
	s.mu.RLock()
	rec, ok := s.items[userUUID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	if rec.Expired(time.Now()) {
		s.Remove(userUUID)
		return nil
	}
	out := rec
	return &out
}

// IsBanned 用户是否处于有效封禁中。
func (s *BanStore) IsBanned(userUUID string) bool {
	return s.Get(userUUID) != nil
}

// List 返回当前有效封禁快照。
func (s *BanStore) List() []BanRecord {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BanRecord, 0, len(s.items))
	maps.DeleteFunc(s.items, func(_ string, rec BanRecord) bool {
		return rec.Expired(now)
	})
	for _, rec := range s.items {
		out = append(out, rec)
	}
	return out
}

// Len 返回有效封禁条数。
func (s *BanStore) Len() int {
	return len(s.List())
}
