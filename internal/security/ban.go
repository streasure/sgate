package security

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/streasure/sgate/internal/config"
)

// BanRecord 单条用户封禁记录。
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

// BanStore 封禁表。
// 本地内存为热路径缓存；配置 Redis 后 Add/Remove 写穿，Get 未命中回源 Redis，后台周期全量同步。
type BanStore struct {
	mu    sync.RWMutex
	items map[string]BanRecord

	rdb      *redis.Client
	keyPfx   string
	syncIval time.Duration
	stopCh   chan struct{}
	stopped  chan struct{}
}

// NewBanStore 创建空封禁表（仅内存）。
func NewBanStore() *BanStore {
	return &BanStore{items: make(map[string]BanRecord)}
}

// key 拼接 Redis key。
func (s *BanStore) key(userUUID string) string {
	return s.keyPfx + userUUID
}

// InitRedis 连接 Redis 并启用共享存储：启动全量加载 + 周期同步。
// enabled=false 或 address 为空时 no-op 返回 nil。
// 连接失败返回错误（配置 enabled 却连不上时上层决定是否继续内存模式）。
func (s *BanStore) InitRedis(ctx context.Context, cfg config.BanRedisConfig) error {
	if !cfg.Enabled || cfg.Address == "" {
		return nil
	}
	pfx := cfg.KeyPrefix
	if pfx == "" {
		pfx = "sgate:ban:"
	}
	opts := &redis.Options{
		Addr:     cfg.Address,
		Password: cfg.Password,
		DB:       cfg.DB,
	}
	if cfg.DialTimeout != "" {
		if d, err := time.ParseDuration(cfg.DialTimeout); err == nil {
			opts.DialTimeout = d
		}
	}
	if cfg.ReadTimeout != "" {
		if d, err := time.ParseDuration(cfg.ReadTimeout); err == nil {
			opts.ReadTimeout = d
		}
	}
	if cfg.WriteTimeout != "" {
		if d, err := time.ParseDuration(cfg.WriteTimeout); err == nil {
			opts.WriteTimeout = d
		}
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return err
	}

	syncIval := 30 * time.Second
	if cfg.SyncInterval != "" {
		if d, err := time.ParseDuration(cfg.SyncInterval); err == nil {
			if d < 0 {
				syncIval = 0 // 关闭周期同步
			} else {
				syncIval = d
			}
		}
	}

	s.mu.Lock()
	s.rdb = rdb
	s.keyPfx = pfx
	s.syncIval = syncIval
	s.stopCh = make(chan struct{})
	if syncIval > 0 {
		s.stopped = make(chan struct{})
	}
	s.mu.Unlock()

	if err := s.SyncFromRedis(ctx); err != nil {
		_ = rdb.Close()
		s.mu.Lock()
		s.rdb, s.stopCh, s.stopped = nil, nil, nil
		s.mu.Unlock()
		return err
	}
	if syncIval > 0 {
		go s.syncLoop(syncIval)
	}
	return nil
}

// Close 停止同步协程并关闭 Redis 连接。
func (s *BanStore) Close() error {
	s.mu.Lock()
	stop, stopped, rdb := s.stopCh, s.stopped, s.rdb
	s.stopCh, s.stopped, s.rdb = nil, nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if stopped != nil {
		<-stopped
	}
	if rdb != nil {
		return rdb.Close()
	}
	return nil
}

func (s *BanStore) syncLoop(ival time.Duration) {
	defer close(s.stopped)
	t := time.NewTicker(ival)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			_ = s.SyncFromRedis(context.Background())
		}
	}
}

// SyncFromRedis 全量拉取 Redis 封禁键刷入本地（丢弃已过期；删除 Redis 中已不存在的本地项）。
func (s *BanStore) SyncFromRedis(ctx context.Context) error {
	s.mu.RLock()
	rdb, pfx := s.rdb, s.keyPfx
	s.mu.RUnlock()
	if rdb == nil {
		return nil
	}
	var cursor uint64
	remote := make(map[string]BanRecord)
	for {
		keys, next, err := rdb.Scan(ctx, cursor, pfx+"*", 256).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			vals, err := rdb.MGet(ctx, keys...).Result()
			if err != nil {
				return err
			}
			for _, v := range vals {
				sv, ok := v.(string)
				if !ok || sv == "" {
					continue
				}
				var rec BanRecord
				if err := json.Unmarshal([]byte(sv), &rec); err != nil {
					continue
				}
				if rec.UserUUID == "" || rec.Expired(time.Now()) {
					continue
				}
				remote[rec.UserUUID] = rec
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	s.mu.Lock()
	for id := range s.items {
		if _, ok := remote[id]; !ok {
			delete(s.items, id)
		}
	}
	maps.Copy(s.items, remote)
	s.mu.Unlock()
	return nil
}

// redisPut 写穿 Redis。ttl<=0 永久（无 EXPIRE）。
func (s *BanStore) redisPut(ctx context.Context, rec BanRecord) error {
	s.mu.RLock()
	rdb := s.rdb
	s.mu.RUnlock()
	if rdb == nil {
		return nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	key := s.key(rec.UserUUID)
	if !rec.ExpiresAt.IsZero() {
		ttl := time.Until(rec.ExpiresAt)
		if ttl <= 0 {
			return rdb.Del(ctx, key).Err()
		}
		return rdb.Set(ctx, key, data, ttl).Err()
	}
	return rdb.Set(ctx, key, data, 0).Err()
}

// redisDel 删除 Redis 键。
func (s *BanStore) redisDel(ctx context.Context, userUUID string) error {
	s.mu.RLock()
	rdb := s.rdb
	s.mu.RUnlock()
	if rdb == nil {
		return nil
	}
	return rdb.Del(ctx, s.key(userUUID)).Err()
}

// redisGet 单键回源。未命中或反序列化失败返回 nil。
func (s *BanStore) redisGet(ctx context.Context, userUUID string) *BanRecord {
	s.mu.RLock()
	rdb := s.rdb
	s.mu.RUnlock()
	if rdb == nil {
		return nil
	}
	val, err := rdb.Get(ctx, s.key(userUUID)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return nil
	}
	var rec BanRecord
	if err := json.Unmarshal([]byte(val), &rec); err != nil {
		return nil
	}
	if rec.UserUUID == "" || rec.Expired(time.Now()) {
		return nil
	}
	return &rec
}

// Add 写入或覆盖封禁记录。ttl<=0 表示永久。
// Redis 启用时写穿共享存储；本地同步更新。
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
	_ = s.redisPut(context.Background(), rec)
	return rec
}

// Remove 解除封禁，返回是否曾存在（本地或 Redis）。
func (s *BanStore) Remove(userUUID string) bool {
	s.mu.Lock()
	_, ok := s.items[userUUID]
	delete(s.items, userUUID)
	s.mu.Unlock()
	if s.rdb != nil {
		if n, err := s.rdb.Exists(context.Background(), s.key(userUUID)).Result(); err == nil && n > 0 {
			ok = true
		}
		_ = s.redisDel(context.Background(), userUUID)
	}
	return ok
}

// Get 返回当前有效封禁记录；未封禁或已过期返回 nil。
// 本地未命中且 Redis 已启用时回源 Redis。
func (s *BanStore) Get(userUUID string) *BanRecord {
	s.mu.RLock()
	rec, ok := s.items[userUUID]
	s.mu.RUnlock()
	if ok {
		if rec.Expired(time.Now()) {
			s.Remove(userUUID)
			return nil
		}
		out := rec
		return &out
	}
	if s.rdb == nil {
		return nil
	}
	if fresher := s.redisGet(context.Background(), userUUID); fresher != nil {
		s.mu.Lock()
		s.items[userUUID] = *fresher
		s.mu.Unlock()
		out := *fresher
		return &out
	}
	return nil
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
