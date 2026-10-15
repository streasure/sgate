package security

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"
)

// BanRecord 单条封禁记录。
// Kind 为 "user"（默认，按 userUuid）或 "ip"（按远程 IP）。
type BanRecord struct {
	UserUUID  string    `json:"userUuid"` // Kind=user 时为账号键；Kind=ip 时为 IP 字面量
	Kind      string    `json:"kind,omitempty"`
	Reason    string    `json:"reason"`
	Jti       string    `json:"jti,omitempty"`
	BannedAt  time.Time `json:"bannedAt"`
	ExpiresAt time.Time `json:"expiresAt"` // 零值 = 永久封禁
}

// BanKindUser / BanKindIP 封禁维度。
const (
	BanKindUser = "user"
	BanKindIP   = "ip"
)

// Key 返回封禁存储键：IP 封禁带 "ip:" 前缀，避免与账号键冲突。
func (r *BanRecord) Key() string {
	if r.Kind == BanKindIP {
		return "ip:" + r.UserUUID
	}
	return r.UserUUID
}

// Expired 判断封禁是否已到期（永久封禁返回 false）。
func (r *BanRecord) Expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// NormalizeIP 归一化 IP 字面量（IPv4-mapped 转 v4，统一压缩形式）。
// 非法返回 ok=false。
func NormalizeIP(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String(), true
	}
	return ip.String(), true
}

// NormalizeIPKey 封禁键归一：ip: 前缀 + NormalizeIP；非法原样返回（调用方校验）。
func NormalizeIPKey(raw string) string {
	if norm, ok := NormalizeIP(raw); ok {
		return "ip:" + norm
	}
	return "ip:" + strings.TrimSpace(raw)
}

// BanEvent 封禁变更事件（Pub/Sub 载荷）。
type BanEvent struct {
	Op        string `json:"op"`   // ban | unban
	Kind      string `json:"kind"` // user | ip
	Key       string `json:"key"`  // 存储键（IP 为 ip:addr，账号为 uuid）
	Reason    string `json:"reason,omitempty"`
	Jti       string `json:"jti,omitempty"`
	TTLSec    int64  `json:"ttlSec,omitempty"`
	Origin    string `json:"origin,omitempty"`
	TS        int64  `json:"ts"`
	BannedAt  string `json:"bannedAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// BanEventHandler 处理远端封禁事件（更新本地 + 踢连接）。nil 则仅更新表。
type BanEventHandler func(ev BanEvent)

type negEntry struct {
	expiresAt time.Time
}

// BanStore 封禁表。
// 本地内存为热路径缓存；配置 Redis 后 Add/Remove 写穿，Get 未命中回源（可负缓存），
// 可选 Pub/Sub 事件增量同步，后台周期全量校准。
type BanStore struct {
	mu    sync.RWMutex
	items map[string]BanRecord
	neg   map[string]negEntry

	rdb      *redis.Client
	keyPfx   string
	syncIval time.Duration
	negTTL   time.Duration
	eventsOn bool
	channel  string
	origin   string
	handler  BanEventHandler
	stopCh   chan struct{}
	stopped  chan struct{}
	subDone  chan struct{}
	pubsub   *redis.PubSub

	redisDown   atomic.Bool
	evApplied   atomic.Int64
	evPublished atomic.Int64
	syncFails   atomic.Int64
	redisErrs   atomic.Int64
}

// NewBanStore 创建空封禁表（仅内存）。
func NewBanStore() *BanStore {
	return &BanStore{
		items: make(map[string]BanRecord),
		neg:   make(map[string]negEntry),
	}
}

func (s *BanStore) key(userUUID string) string {
	return s.keyPfx + userUUID
}

// SetOrigin 标记本实例 ID（事件 origin 过滤）。
func (s *BanStore) SetOrigin(id string) { s.origin = id }

// SetEventHandler 注册远端事件处理（更新表之外的副作用，如踢连接）。
func (s *BanStore) SetEventHandler(h BanEventHandler) { s.handler = h }

// RedisDown Redis 是否处于降级（最近操作失败）。
func (s *BanStore) RedisDown() bool { return s.redisDown.Load() }

// Metrics 返回事件/同步/错误计数快照。
func (s *BanStore) Metrics() (evApplied, evPublished, syncFails, redisErrs int64) {
	return s.evApplied.Load(), s.evPublished.Load(), s.syncFails.Load(), s.redisErrs.Load()
}

func (s *BanStore) markRedisErr() {
	if !s.redisDown.Swap(true) {
		tlog.Warn(context.TODO(), "ban redis degraded (local-only until recovery)")
	}
	s.redisErrs.Add(1)
}

func (s *BanStore) markRedisUp() {
	if s.redisDown.Swap(false) {
		tlog.Info(context.TODO(), "ban redis recovered")
	}
}

// InitRedis 连接 Redis 并启用共享存储：启动全量加载 + 可选事件订阅 + 周期校准。
// enabled=false 或 address 为空时 no-op 返回 nil。
func (s *BanStore) InitRedis(ctx context.Context, cfg config.BanRedisConfig, origin string) error {
	if !cfg.Enabled || cfg.Address == "" {
		return nil
	}
	pfx := cfg.KeyPrefix
	if pfx == "" {
		pfx = config.DefaultBanRedisKeyPrefix
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
		s.markRedisErr()
		return err
	}

	syncIval := parseDurOr(cfg.SyncInterval, 30*time.Second)
	if cfg.EventsOn() && cfg.SyncInterval == "" {
		syncIval = parseDurOr(config.DefaultBanRedisSyncIntervalWithEvents, 10*time.Minute)
	}
	var negTTL time.Duration
	if cfg.CacheNegativeTtl != "" {
		if d, err := time.ParseDuration(cfg.CacheNegativeTtl); err == nil {
			negTTL = d
		}
	}
	if negTTL == 0 {
		negTTL = parseDurOr(config.DefaultBanNegativeCacheTtl, 10*time.Second)
	}

	s.mu.Lock()
	s.rdb = rdb
	s.keyPfx = pfx
	s.syncIval = syncIval
	s.negTTL = negTTL
	s.eventsOn = cfg.EventsOn()
	s.channel = cfg.EventsChannel
	if s.channel == "" {
		s.channel = config.DefaultBanRedisEventsChannel
	}
	s.origin = origin
	s.stopCh = make(chan struct{})
	if syncIval > 0 {
		s.stopped = make(chan struct{})
	}
	if cfg.EventsOn() {
		s.subDone = make(chan struct{})
	}
	s.mu.Unlock()

	if err := s.SyncFromRedis(ctx); err != nil {
		_ = rdb.Close()
		s.mu.Lock()
		s.rdb, s.stopCh, s.stopped, s.subDone = nil, nil, nil, nil
		s.mu.Unlock()
		s.markRedisErr()
		return err
	}
	s.markRedisUp()
	if s.eventsOn {
		ready := make(chan struct{})
		go s.subscribeLoop(ready, s.subDone)
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			tlog.Warn(context.TODO(), "ban redis events subscribe timeout (will keep retrying)")
		}
	}
	if syncIval > 0 {
		go s.syncLoop(syncIval)
	}
	return nil
}

func parseDurOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

// Close 停止订阅/同步并关闭 Redis。
func (s *BanStore) Close() error {
	s.mu.Lock()
	stop, stopped, subDone, rdb, ps := s.stopCh, s.stopped, s.subDone, s.rdb, s.pubsub
	s.stopCh, s.stopped, s.subDone, s.rdb, s.pubsub = nil, nil, nil, nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if stopped != nil {
		<-stopped
	}
	if subDone != nil {
		<-subDone
	}
	if ps != nil {
		_ = ps.Close()
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
			if err := s.SyncFromRedis(context.Background()); err != nil {
				s.syncFails.Add(1)
				s.markRedisErr()
			}
		}
	}
}

func (s *BanStore) subscribeLoop(ready, done chan struct{}) {
	if done != nil {
		defer close(done)
	}
	closeReady := sync.Once{}
	signalReady := func() {
		closeReady.Do(func() {
			if ready != nil {
				close(ready)
			}
		})
	}
	defer signalReady()
	for {
		s.mu.RLock()
		rdb, ch, stop := s.rdb, s.channel, s.stopCh
		s.mu.RUnlock()
		if rdb == nil || stop == nil {
			return
		}
		ps := rdb.Subscribe(context.Background(), ch)
		s.mu.Lock()
		s.pubsub = ps
		s.mu.Unlock()
		if _, err := ps.Receive(context.Background()); err != nil {
			_ = ps.Close()
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				continue
			}
		}
		signalReady()
		tlog.Info(context.TODO(), "ban redis events subscribed channel=%s", ch)
		msgCh := ps.Channel()
		for {
			select {
			case <-stop:
				_ = ps.Close()
				return
			case msg, ok := <-msgCh:
				if !ok {
					goto reconnected
				}
				s.handleEventMessage(msg.Payload)
			}
		}
	reconnected:
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *BanStore) handleEventMessage(payload string) {
	var ev BanEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return
	}
	if ev.Key == "" || (ev.Op != "ban" && ev.Op != "unban") {
		return
	}
	if ev.Origin != "" && ev.Origin == s.origin {
		return
	}
	now := time.Now()
	if ev.Op == "unban" {
		s.mu.Lock()
		delete(s.items, ev.Key)
		delete(s.neg, ev.Key)
		s.mu.Unlock()
	} else {
		rec := BanRecord{
			UserUUID: strings.TrimPrefix(ev.Key, "ip:"),
			Kind:     ev.Kind,
			Reason:   ev.Reason,
			Jti:      ev.Jti,
		}
		if rec.Kind == "" {
			if strings.HasPrefix(ev.Key, "ip:") {
				rec.Kind = BanKindIP
			} else {
				rec.Kind = BanKindUser
			}
		}
		if ev.BannedAt != "" {
			if t, err := time.Parse(time.RFC3339, ev.BannedAt); err == nil {
				rec.BannedAt = t
			}
		}
		if rec.BannedAt.IsZero() {
			rec.BannedAt = now
		}
		if ev.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, ev.ExpiresAt); err == nil {
				rec.ExpiresAt = t
			}
		} else if ev.TTLSec > 0 {
			rec.ExpiresAt = now.Add(time.Duration(ev.TTLSec) * time.Second)
		}
		if rec.Expired(now) {
			return
		}
		s.mu.Lock()
		s.items[ev.Key] = rec
		delete(s.neg, ev.Key)
		s.mu.Unlock()
	}
	s.evApplied.Add(1)
	if h := s.handler; h != nil {
		h(ev)
	}
}

func (s *BanStore) publishEvent(ev BanEvent) {
	s.mu.RLock()
	rdb, ch := s.rdb, s.channel
	s.mu.RUnlock()
	if rdb == nil || !s.eventsOn {
		return
	}
	ev.Origin = s.origin
	ev.TS = time.Now().Unix()
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := rdb.Publish(context.Background(), ch, data).Err(); err != nil {
		s.redisErrs.Add(1)
		return
	}
	s.evPublished.Add(1)
}

// SyncFromRedis 全量拉取 Redis 封禁键刷入本地。
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
		keys, next, err := rdb.Scan(ctx, cursor, pfx+"*", 1000).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			for start := 0; start < len(keys); start += 256 {
				end := min(start+256, len(keys))
				vals, err := rdb.MGet(ctx, keys[start:end]...).Result()
				if err != nil {
					return err
				}
				for i, v := range vals {
					sv, ok := v.(string)
					if !ok || sv == "" {
						continue
					}
					var rec BanRecord
					if err := json.Unmarshal([]byte(sv), &rec); err != nil {
						continue
					}
					if rec.Expired(time.Now()) {
						continue
					}
					localKey := strings.TrimPrefix(keys[start+i], pfx)
					if localKey == "" {
						continue
					}
					if rec.Kind == "" {
						if strings.HasPrefix(localKey, "ip:") {
							rec.Kind = BanKindIP
						} else {
							rec.Kind = BanKindUser
						}
					}
					remote[localKey] = rec
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	nextMap := make(map[string]BanRecord, len(remote))
	maps.Copy(nextMap, remote)
	s.mu.Lock()
	for id := range s.items {
		if _, ok := nextMap[id]; !ok {
			delete(s.items, id)
		}
	}
	maps.Copy(s.items, nextMap)
	for k := range s.neg {
		if _, ok := s.items[k]; ok {
			delete(s.neg, k)
		}
	}
	s.mu.Unlock()
	s.markRedisUp()
	return nil
}

func (s *BanStore) redisPut(ctx context.Context, key string, rec BanRecord) error {
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
	rkey := s.key(key)
	if !rec.ExpiresAt.IsZero() {
		ttl := time.Until(rec.ExpiresAt)
		if ttl <= 0 {
			return rdb.Del(ctx, rkey).Err()
		}
		return rdb.Set(ctx, rkey, data, ttl).Err()
	}
	return rdb.Set(ctx, rkey, data, 0).Err()
}

func (s *BanStore) redisDel(ctx context.Context, userUUID string) error {
	s.mu.RLock()
	rdb := s.rdb
	s.mu.RUnlock()
	if rdb == nil {
		return nil
	}
	return rdb.Del(ctx, s.key(userUUID)).Err()
}

func (s *BanStore) redisGet(ctx context.Context, userUUID string) *BanRecord {
	s.mu.RLock()
	rdb := s.rdb
	s.mu.RUnlock()
	if rdb == nil {
		return nil
	}
	val, err := rdb.Get(ctx, s.key(userUUID)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			s.markRedisErr()
		}
		return nil
	}
	s.markRedisUp()
	var rec BanRecord
	if err := json.Unmarshal([]byte(val), &rec); err != nil {
		return nil
	}
	if rec.UserUUID == "" || rec.Expired(time.Now()) {
		return nil
	}
	return &rec
}

func (s *BanStore) negHit(key string) bool {
	if s.negTTL <= 0 {
		return false
	}
	s.mu.RLock()
	e, ok := s.neg[key]
	s.mu.RUnlock()
	return ok && time.Now().Before(e.expiresAt)
}

func (s *BanStore) negStore(key string) {
	if s.negTTL <= 0 {
		return
	}
	s.mu.Lock()
	s.neg[key] = negEntry{expiresAt: time.Now().Add(s.negTTL)}
	s.mu.Unlock()
}

func (s *BanStore) negClear(key string) {
	s.mu.Lock()
	delete(s.neg, key)
	s.mu.Unlock()
}

// Add 写入或覆盖封禁记录。ttl<=0 表示永久。
// key 为存储键：账号 uuid 或 "ip:<addr>"（建议先 NormalizeIP）。
func (s *BanStore) Add(key, reason, jti string, ttl time.Duration) BanRecord {
	now := time.Now()
	rec := BanRecord{
		UserUUID: key,
		Reason:   reason,
		Jti:      jti,
		BannedAt: now,
	}
	if strings.HasPrefix(key, "ip:") {
		rec.Kind = BanKindIP
		rec.UserUUID = strings.TrimPrefix(key, "ip:")
	} else {
		rec.Kind = BanKindUser
	}
	if ttl > 0 {
		rec.ExpiresAt = now.Add(ttl)
	}
	s.mu.Lock()
	s.items[key] = rec
	delete(s.neg, key)
	s.mu.Unlock()
	if err := s.redisPut(context.Background(), key, rec); err != nil {
		s.markRedisErr()
	} else {
		s.markRedisUp()
	}
	s.publishEvent(BanEvent{
		Op:        "ban",
		Kind:      rec.Kind,
		Key:       key,
		Reason:    reason,
		Jti:       jti,
		TTLSec:    int64(ttl / time.Second),
		BannedAt:  now.Format(time.RFC3339),
		ExpiresAt: formatExpires(rec.ExpiresAt),
	})
	return rec
}

func formatExpires(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// Remove 解除封禁，返回是否曾存在（本地或 Redis）。
func (s *BanStore) Remove(key string) bool {
	s.mu.Lock()
	_, ok := s.items[key]
	delete(s.items, key)
	delete(s.neg, key)
	s.mu.Unlock()
	kind := BanKindUser
	if strings.HasPrefix(key, "ip:") {
		kind = BanKindIP
	}
	if s.rdb != nil {
		if n, err := s.rdb.Exists(context.Background(), s.key(key)).Result(); err == nil && n > 0 {
			ok = true
		} else if err != nil {
			s.markRedisErr()
		}
		if err := s.redisDel(context.Background(), key); err != nil {
			s.markRedisErr()
		}
	}
	s.publishEvent(BanEvent{Op: "unban", Kind: kind, Key: key})
	return ok
}

// Get 返回当前有效封禁记录；未封禁或已过期返回 nil。
func (s *BanStore) Get(key string) *BanRecord {
	s.mu.RLock()
	rec, ok := s.items[key]
	s.mu.RUnlock()
	if ok {
		if rec.Expired(time.Now()) {
			s.Remove(key)
			return nil
		}
		out := rec
		return &out
	}
	if s.rdb == nil {
		return nil
	}
	if s.negHit(key) {
		return nil
	}
	if fresher := s.redisGet(context.Background(), key); fresher != nil {
		s.mu.Lock()
		s.items[key] = *fresher
		s.mu.Unlock()
		s.negClear(key)
		out := *fresher
		return &out
	}
	if !s.RedisDown() {
		s.negStore(key)
	}
	return nil
}

// IsBanned 键是否处于有效封禁中。
func (s *BanStore) IsBanned(key string) bool {
	return s.Get(key) != nil
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
