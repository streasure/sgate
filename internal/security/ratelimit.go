package security

import (
	"hash/maphash"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// rateShardCount 桶表分片数（2 的幂，用 maphash 取模分片）。
	// 10 万桶共享一把 RWMutex 会在高连接数下成为竞争热点；
	// 分片后每个 key 只与同片竞争，写路径锁粒度降为 1/rateShardCount。
	rateShardCount = 64
	// maxBucketsPerDim 单维度全局限桶总数上限（分摊到各片）。
	maxBucketsPerDim = 100000
)

type RateLimiter struct {
	shards [rateShardCount]rateShard

	// cfgMu 保护配置字段，与分片锁分离：配置写极少（热更/初始化），
	// 热路径只读一次即可拿到新桶参数，不与桶表锁交叉。
	cfgMu            sync.RWMutex
	tokenRefresh     time.Duration
	maxTokens        int
	burstTokens      int
	dimensionConfigs map[string]DimensionConfig

	maxBucketsPerShard int
	cleanupInterval    time.Duration
	stopCh             chan struct{}
	stopOnce           sync.Once
	seed               maphash.Seed
}

// rateShard 单分片桶表。key = dimension + "\x00" + key。
type rateShard struct {
	mu      sync.RWMutex
	buckets map[string]*TokenBucket
}

type DimensionConfig struct {
	MaxTokens    int
	BurstTokens  int
	TokenRefresh time.Duration
}

type TokenBucket struct {
	tokens       atomic.Int64
	lastUpdate   atomic.Int64
	maxTokens    int64
	burstTokens  int64
	tokenRefresh time.Duration
}

// tryConsume 消耗一个令牌，同时按时间差补充令牌
// 补充速率 = maxTokens 个/每个 tokenRefresh 周期，上限为 burstTokens
func (tb *TokenBucket) tryConsume() bool {
	// 防御：tokenRefresh<=0 会除零 panic（配置 "0s" 时）
	if tb.tokenRefresh <= 0 {
		return tb.tokens.Load() > 0
	}
	now := time.Now().UnixNano()
	last := tb.lastUpdate.Load()

	// 按时间差补充令牌（CAS 更新 lastUpdate，只有一个 goroutine 执行补充）
	if now-last > int64(tb.tokenRefresh) {
		if tb.lastUpdate.CompareAndSwap(last, now) {
			elapsed := time.Duration(now - last)
			refill := int64(elapsed/tb.tokenRefresh) * tb.maxTokens
			if refill > 0 {
				// CAS 循环加法：Load+Store 会与并发扣减竞争丢更新。
				for {
					current := tb.tokens.Load()
					newTokens := min(current+refill, tb.burstTokens)
					if tb.tokens.CompareAndSwap(current, newTokens) {
						break
					}
				}
			}
		}
	}

	for {
		current := tb.tokens.Load()
		if current <= 0 {
			return false
		}
		if tb.tokens.CompareAndSwap(current, current-1) {
			return true
		}
	}
}

func NewRateLimiter(maxTokens int, tokenRefresh time.Duration) *RateLimiter {
	rl := &RateLimiter{
		tokenRefresh:       tokenRefresh,
		maxTokens:          maxTokens,
		burstTokens:        maxTokens * 2,
		cleanupInterval:    5 * time.Minute,
		dimensionConfigs:   make(map[string]DimensionConfig),
		maxBucketsPerShard: max(1, maxBucketsPerDim/rateShardCount),
		stopCh:             make(chan struct{}),
		seed:               maphash.MakeSeed(),
	}
	for i := range rl.shards {
		rl.shards[i].buckets = make(map[string]*TokenBucket)
	}

	rl.resetConfig(maxTokens, tokenRefresh)

	go rl.cleanup()

	return rl
}

func (rl *RateLimiter) UpdateRate(maxTokens int, tokenRefresh time.Duration) {
	rl.cfgMu.Lock()
	defer rl.cfgMu.Unlock()
	rl.resetConfig(maxTokens, tokenRefresh)
}

func (rl *RateLimiter) resetConfig(maxTokens int, tokenRefresh time.Duration) {
	rl.tokenRefresh, rl.maxTokens, rl.burstTokens = tokenRefresh, maxTokens, maxTokens*2
	rl.dimensionConfigs = map[string]DimensionConfig{
		"ip": {MaxTokens: maxTokens, BurstTokens: maxTokens * 2, TokenRefresh: tokenRefresh},
		// user 维度限速为总速一半，但不小于 1：maxTokens=1 时 maxTokens/2==0
		// 会让该维度新桶初始令牌为 0，请求被全部拒绝。
		"user":  {MaxTokens: max(1, maxTokens/2), BurstTokens: maxTokens, TokenRefresh: tokenRefresh},
		"route": {MaxTokens: maxTokens * 4, BurstTokens: maxTokens * 8, TokenRefresh: tokenRefresh},
	}
}

func newTokenBucket(maxTokens, burstTokens int, tokenRefresh time.Duration) *TokenBucket {
	if tokenRefresh <= 0 {
		tokenRefresh = time.Second
	}
	bucket := &TokenBucket{maxTokens: int64(maxTokens), burstTokens: int64(burstTokens), tokenRefresh: tokenRefresh}
	bucket.tokens.Store(int64(maxTokens))
	bucket.lastUpdate.Store(time.Now().UnixNano())
	return bucket
}

// shardKey 拼接分片键：dimension 与 key 用 \x00 分隔，避免跨界拼接碰撞
// （dimension="ab", key="c" 与 dimension="a", key="bc"）。
func shardKey(dimension, key string) string {
	return dimension + "\x00" + key
}

func (rl *RateLimiter) shardFor(sk string) *rateShard {
	return &rl.shards[maphash.String(rl.seed, sk)&(rateShardCount-1)]
}

// dimConfig 读维度配置；无配置或零值时回退全局默认。
func (rl *RateLimiter) dimConfig(dimension string) DimensionConfig {
	rl.cfgMu.RLock()
	config := rl.dimensionConfigs[dimension]
	tokenRefresh, maxTokens, burstTokens := rl.tokenRefresh, rl.maxTokens, rl.burstTokens
	rl.cfgMu.RUnlock()
	if config.MaxTokens == 0 {
		config.MaxTokens = maxTokens
		config.BurstTokens = burstTokens
		config.TokenRefresh = tokenRefresh
	}
	return config
}

// Allow 按维度+key 消耗令牌。
// 全局过载判定不在这里：由 gateway 用 overload.IsOverloaded() 在消息入口统一丢弃，
// 避免全局桶在每条消息上做两次共享 CAS（ip+route 各一次）的热点。
func (rl *RateLimiter) Allow(dimension, key string) bool {
	sk := shardKey(dimension, key)
	sh := rl.shardFor(sk)

	sh.mu.RLock()
	bucket, exists := sh.buckets[sk]
	sh.mu.RUnlock()

	if !exists {
		sh.mu.Lock()
		if _, exists = sh.buckets[sk]; !exists {
			if len(sh.buckets) >= rl.maxBucketsPerShard {
				sh.mu.Unlock()
				return false
			}
			config := rl.dimConfig(dimension)
			bucket = newTokenBucket(config.MaxTokens, config.BurstTokens, config.TokenRefresh)
			sh.buckets[sk] = bucket
		}
		sh.mu.Unlock()
		// 修复：新建 bucket 时也要消耗 token，否则换 IP 即可绕过限流
		return bucket.tryConsume()
	}

	return bucket.tryConsume()
}

func (rl *RateLimiter) AllowMulti(dimensions map[string]string) bool {
	for dimension, key := range dimensions {
		if !rl.Allow(dimension, key) {
			return false
		}
	}
	return true
}

func (rl *RateLimiter) GetTokens(dimension, key string) int {
	sk := shardKey(dimension, key)
	sh := rl.shardFor(sk)
	sh.mu.RLock()
	bucket, exists := sh.buckets[sk]
	sh.mu.RUnlock()

	if !exists {
		rl.cfgMu.RLock()
		defer rl.cfgMu.RUnlock()
		if config, ok := rl.dimensionConfigs[dimension]; ok {
			return config.MaxTokens
		}
		return rl.maxTokens
	}

	return int(bucket.tokens.Load())
}

func (rl *RateLimiter) SetDimensionConfig(dimension string, config DimensionConfig) {
	rl.cfgMu.Lock()
	defer rl.cfgMu.Unlock()
	rl.dimensionConfigs[dimension] = config
}

func (rl *RateLimiter) GetDimensionConfig(dimension string) DimensionConfig {
	rl.cfgMu.RLock()
	defer rl.cfgMu.RUnlock()
	config, exists := rl.dimensionConfigs[dimension]
	if !exists {
		return DimensionConfig{
			MaxTokens:    rl.maxTokens,
			BurstTokens:  rl.burstTokens,
			TokenRefresh: rl.tokenRefresh,
		}
	}
	return config
}

func (rl *RateLimiter) SetMaxTokens(maxTokens int) {
	rl.cfgMu.Lock()
	defer rl.cfgMu.Unlock()
	rl.maxTokens = maxTokens
	rl.burstTokens = maxTokens * 2
}

func (rl *RateLimiter) SetTokenRefresh(tokenRefresh time.Duration) {
	rl.cfgMu.Lock()
	defer rl.cfgMu.Unlock()
	rl.tokenRefresh = tokenRefresh
}

func (rl *RateLimiter) Clear() {
	for i := range rl.shards {
		rl.shards[i].mu.Lock()
		clear(rl.shards[i].buckets)
		rl.shards[i].mu.Unlock()
	}
}

func (rl *RateLimiter) ClearDimension(dimension string) {
	prefix := dimension + "\x00"
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.Lock()
		for k := range sh.buckets {
			if strings.HasPrefix(k, prefix) {
				delete(sh.buckets, k)
			}
		}
		sh.mu.Unlock()
	}
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			now := time.Now().UnixNano()
			for i := range rl.shards {
				sh := &rl.shards[i]
				sh.mu.Lock()
				for k, bucket := range sh.buckets {
					if now-bucket.lastUpdate.Load() > int64(30*time.Minute) {
						delete(sh.buckets, k)
					}
				}
				sh.mu.Unlock()
			}
		}
	}
}

func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() {
		close(rl.stopCh)
	})
}

func (rl *RateLimiter) GetStats() map[string]int {
	stats := make(map[string]int)
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.RLock()
		for k := range sh.buckets {
			dimension, _, _ := strings.Cut(k, "\x00")
			stats[dimension]++
		}
		sh.mu.RUnlock()
	}
	return stats
}
