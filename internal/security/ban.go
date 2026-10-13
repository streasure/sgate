package security

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BanRecord 单条用户封禁记录。
// 多网关共享存储（MySQL/etcd）为后续扩展；当前为本地 JSON 快照持久化。
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
// persistPath 非空时 Add/Remove 会写 JSON 快照，启动 Load 恢复。
type BanStore struct {
	mu          sync.RWMutex
	items       map[string]BanRecord
	persistPath string
}

// NewBanStore 创建空封禁表。
func NewBanStore() *BanStore {
	return &BanStore{items: make(map[string]BanRecord)}
}

// SetPersistPath 设置快照文件路径（空 = 关闭持久化）。设置后立即尝试加载已有快照。
func (s *BanStore) SetPersistPath(path string) {
	s.mu.Lock()
	s.persistPath = path
	s.mu.Unlock()
	if path != "" {
		_ = s.Load()
	}
}

// persistLocked 写快照（调用方需持写锁）。失败仅记错误返回，不影响主流程。
func (s *BanStore) persistLocked() {
	if s.persistPath == "" {
		return
	}
	recs := make([]BanRecord, 0, len(s.items))
	for _, rec := range s.items {
		recs = append(recs, rec)
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.persistPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.persistPath)
}

// Load 从快照恢复封禁记录（过滤已过期项）。文件不存在返回 nil。
func (s *BanStore) Load() error {
	s.mu.RLock()
	path := s.persistPath
	s.mu.RUnlock()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var recs []BanRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return err
	}
	now := time.Now()
	s.mu.Lock()
	for _, rec := range recs {
		if rec.UserUUID == "" || rec.Expired(now) {
			continue
		}
		s.items[rec.UserUUID] = rec
	}
	s.mu.Unlock()
	return nil
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
	s.persistLocked()
	s.mu.Unlock()
	return rec
}

// Remove 解除封禁，返回是否曾存在。
func (s *BanStore) Remove(userUUID string) bool {
	s.mu.Lock()
	_, ok := s.items[userUUID]
	delete(s.items, userUUID)
	if ok {
		s.persistLocked()
	}
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
