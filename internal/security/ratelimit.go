package security

import (
	"hash/maphash"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// rateShardCount 桶表分片数（2 的幂，用 maphash 取模分片）。
	// 10 万桶共享一把 RWMutex 会在高连接数下成为竞争热点；
	// 分片后每个 key 只与同片竞争，写路径锁粒度降为 1/rateShardCount。
	rateShardCount = 64
	// maxBucketsPerDim 单维度全局限桶总数上限（分摊到各片，按维度独立计数，
	// 与分片前 len(tokensByDimension[dimension]) 语义一致：一个维度打满不影响其他维度）。
	maxBucketsPerDim = 100000
)

type RateLimiter struct {
	shards [rateShardCount]rateShard

	// globalShards 全局硬顶（每消息一次，按 spreadKey 分摊到各片，避免单桶 CAS 热点）。
	// 兜底 overload 未启动（benchMode/未配 dropOnOverload）时的全局总量控制。
	globalShards [rateShardCount]*TokenBucket

	// cfgMu 保护配置字段，与分片锁分离：配置写极少（热更/初始化），
	// 热路径只读一次即可拿到新桶参数，不与桶表锁交叉。
	// 锁序：cfgMu → shard.mu（Allow 未命中路径先读 cfg 再拿分片锁，禁止反向嵌套）。
	cfgMu            sync.RWMutex
	tokenRefresh     time.Duration
	maxTokens        int
	burstTokens      int
	routeMaxTokens   int // >0 覆盖 route 维度速率；0 = maxTokens*4
	globalMaxTokens  int // >0 覆盖全局硬顶速率；0 = maxTokens*10
	dimensionConfigs map[string]DimensionConfig

	maxBucketsPerShard int
	cleanupInterval    time.Duration
	stopCh             chan struct{}
	stopOnce           sync.Once
	seed               maphash.Seed
}

// rateShard 单分片桶表。按维度嵌套计数：counts[dimension] 只数本片本维度，
// 恢复分片前"每维度独立上限"语义。
type rateShard struct {
	mu     sync.RWMutex
	dims   map[string]map[string]*TokenBucket // dimension -> key -> bucket
	counts map[string]int                     // dimension -> 本片桶数
}

type DimensionConfig struct {
	MaxTokens    int
	BurstTokens  int
	TokenRefresh time.Duration
}

type TokenBucket struct {
	tokens       atomic.Int64
	lastUpdate   atomic.Int64
	maxTokens    atomic.Int64
	burstTokens  atomic.Int64
	refreshNanos atomic.Int64
}

// tryConsume 消耗一个令牌，同时按时间差补充令牌
// 补充速率 = maxTokens 个/每个 refresh 周期，上限为 burstTokens
func (tb *TokenBucket) tryConsume() bool {
	refresh := time.Duration(tb.refreshNanos.Load())
	// 防御：refresh<=0 会除零 panic（配置 "0s" 时）
	if refresh <= 0 {
		return tb.tokens.Load() > 0
	}
	now := time.Now().UnixNano()
	last := tb.lastUpdate.Load()

	// 按时间差补充令牌（CAS 更新 lastUpdate，只有一个 goroutine 执行补充）
	if now-last > int64(refresh) {
		if tb.lastUpdate.CompareAndSwap(last, now) {
			elapsed := time.Duration(now - last)
			refill := int64(elapsed/refresh) * tb.maxTokens.Load()
			if refill > 0 {
				// CAS 循环加法：Load+Store 会与并发扣减竞争丢更新。
				burst := tb.burstTokens.Load()
				for {
					current := tb.tokens.Load()
					newTokens := min(current+refill, burst)
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

// updateParams 原子更新桶参数（热更时对已存在的桶生效），令牌数截断不超过新 burst。
func (tb *TokenBucket) updateParams(maxTokens, burstTokens int, tokenRefresh time.Duration) {
	if tokenRefresh <= 0 {
		tokenRefresh = time.Second
	}
	tb.maxTokens.Store(int64(maxTokens))
	tb.burstTokens.Store(int64(burstTokens))
	tb.refreshNanos.Store(int64(tokenRefresh))
	// 新 burst 小于当前令牌时截断，避免超发。
	burst := int64(burstTokens)
	for {
		current := tb.tokens.Load()
		if current <= burst {
			return
		}
		if tb.tokens.CompareAndSwap(current, burst) {
			return
		}
	}
}

func newTokenBucket(maxTokens, burstTokens int, tokenRefresh time.Duration) *TokenBucket {
	if tokenRefresh <= 0 {
		tokenRefresh = time.Second
	}
	bucket := &TokenBucket{}
	bucket.maxTokens.Store(int64(maxTokens))
	bucket.burstTokens.Store(int64(burstTokens))
	bucket.refreshNanos.Store(int64(tokenRefresh))
	bucket.tokens.Store(int64(maxTokens))
	bucket.lastUpdate.Store(time.Now().UnixNano())
	return bucket
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
		rl.shards[i].dims = make(map[string]map[string]*TokenBucket)
		rl.shards[i].counts = make(map[string]int)
	}

	rl.cfgMu.Lock()
	rl.resetConfigLocked(maxTokens, tokenRefresh)
	rl.cfgMu.Unlock()
	rl.initGlobalShards()

	go rl.cleanup()

	return rl
}

// SetRouteMaxTokens 覆盖 route 维度速率（0 恢复默认 maxTokens*4），已存在的 route 桶立即生效。
func (rl *RateLimiter) SetRouteMaxTokens(n int) {
	rl.cfgMu.Lock()
	rl.routeMaxTokens = n
	rl.rebuildDimConfigLocked("route")
	cfg := rl.dimensionConfigs["route"]
	rl.cfgMu.Unlock()
	rl.refreshDimBuckets("route", cfg)
}

// SetGlobalMaxTokens 覆盖全局硬顶速率（0 恢复默认 maxTokens*10），分片全局桶立即生效。
func (rl *RateLimiter) SetGlobalMaxTokens(n int) {
	rl.cfgMu.Lock()
	rl.globalMaxTokens = n
	rl.cfgMu.Unlock()
	rl.initGlobalShards()
}

func (rl *RateLimiter) UpdateRate(maxTokens int, tokenRefresh time.Duration) {
	rl.cfgMu.Lock()
	rl.resetConfigLocked(maxTokens, tokenRefresh)
	// 快照新配置后释放锁再遍历分片：锁序 cfgMu → shard.mu，禁止持有 cfgMu 拿分片锁。
	snapshot := make(map[string]DimensionConfig, len(rl.dimensionConfigs))
	maps.Copy(snapshot, rl.dimensionConfigs)
	rl.cfgMu.Unlock()

	for dim, cfg := range snapshot {
		rl.refreshDimBuckets(dim, cfg)
	}
	rl.initGlobalShards()
}

// resetConfigLocked 重建默认维度配置；调用方须持有 cfgMu。
func (rl *RateLimiter) resetConfigLocked(maxTokens int, tokenRefresh time.Duration) {
	rl.tokenRefresh, rl.maxTokens, rl.burstTokens = tokenRefresh, maxTokens, maxTokens*2
	rl.dimensionConfigs = map[string]DimensionConfig{}
	for _, dim := range []string{"ip", "user", "route"} {
		rl.rebuildDimConfigLocked(dim)
	}
}

// rebuildDimConfigLocked 按当前 maxTokens/routeMaxTokens 重算单个维度配置；调用方须持有 cfgMu。
func (rl *RateLimiter) rebuildDimConfigLocked(dimension string) {
	switch dimension {
	case "ip":
		rl.dimensionConfigs["ip"] = DimensionConfig{MaxTokens: rl.maxTokens, BurstTokens: rl.maxTokens * 2, TokenRefresh: rl.tokenRefresh}
	case "user":
		// user 维度限速为总速一半，但不小于 1：maxTokens=1 时 maxTokens/2==0
		// 会让该维度新桶初始令牌为 0，请求被全部拒绝。
		rl.dimensionConfigs["user"] = DimensionConfig{MaxTokens: max(1, rl.maxTokens/2), BurstTokens: rl.maxTokens, TokenRefresh: rl.tokenRefresh}
	case "route":
		rate := rl.routeMaxTokens
		if rate <= 0 {
			rate = rl.maxTokens * 4
		}
		rl.dimensionConfigs["route"] = DimensionConfig{MaxTokens: rate, BurstTokens: rate * 2, TokenRefresh: rl.tokenRefresh}
	}
}

// refreshDimBuckets 把新配置应用到该维度已存在的所有桶（热更立即生效）。
func (rl *RateLimiter) refreshDimBuckets(dimension string, cfg DimensionConfig) {
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.RLock()
		buckets := make([]*TokenBucket, 0, len(sh.dims[dimension]))
		for _, b := range sh.dims[dimension] {
			buckets = append(buckets, b)
		}
		sh.mu.RUnlock()
		for _, b := range buckets {
			b.updateParams(cfg.MaxTokens, cfg.BurstTokens, cfg.TokenRefresh)
		}
	}
}

// initGlobalShards （重）建分片全局硬顶桶。每片速率 = 全局速率/片数（下限 1，避免小配置下整片饿死）。
func (rl *RateLimiter) initGlobalShards() {
	rl.cfgMu.RLock()
	globalMax := rl.globalMaxTokens
	if globalMax <= 0 {
		globalMax = rl.maxTokens * 10
	}
	refresh := rl.tokenRefresh
	rl.cfgMu.RUnlock()

	perShard := max(1, globalMax/rateShardCount)
	for i := range rl.globalShards {
		if rl.globalShards[i] == nil {
			rl.globalShards[i] = newTokenBucket(perShard, perShard*2, refresh)
		} else {
			rl.globalShards[i].updateParams(perShard, perShard*2, refresh)
		}
	}
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

// AllowGlobal 全局硬顶：每条消息只调一次（spreadKey 如 remoteIP 决定落到哪个全局分片）。
// 与旧 globalBucket 的区别：按 spreadKey 分摊 64 个桶，消除单桶 CAS 热点；
// overload 未运行（benchMode/未配 dropOnOverload）时兜住全局总量。
func (rl *RateLimiter) AllowGlobal(spreadKey string) bool {
	idx := maphash.String(rl.seed, "\x00global\x00"+spreadKey) & (rateShardCount - 1)
	return rl.globalShards[idx].tryConsume()
}

// Allow 按维度+key 消耗令牌（per-IP/per-route/user 防滥用限速）。
func (rl *RateLimiter) Allow(dimension, key string) bool {
	sk := shardKey(dimension, key)
	sh := rl.shardFor(sk)

	sh.mu.RLock()
	bucket := sh.dims[dimension][key]
	sh.mu.RUnlock()

	if bucket == nil {
		// 先读配置再拿分片写锁（锁序 cfgMu → shard.mu）。
		config := rl.dimConfig(dimension)
		sh.mu.Lock()
		if bucket = sh.dims[dimension][key]; bucket == nil {
			// 每维度每片独立上限：一个维度打满分片槽位不影响其他维度。
			if sh.counts[dimension] >= rl.maxBucketsPerShard {
				sh.mu.Unlock()
				return false
			}
			if sh.dims[dimension] == nil {
				sh.dims[dimension] = make(map[string]*TokenBucket)
			}
			bucket = newTokenBucket(config.MaxTokens, config.BurstTokens, config.TokenRefresh)
			sh.dims[dimension][key] = bucket
			sh.counts[dimension]++
		}
		sh.mu.Unlock()
		// 修复：新建 bucket 时也要消耗 token，否则换 IP 即可绕过限流
		return bucket.tryConsume()
	}

	return bucket.tryConsume()
}

// AllowMulti 多维度限速：先 peek 全部维度再消耗，避免中途失败留下已扣不退的部分消耗。
// peek 与消耗之间存在并发窗口，极端竞争下仍可能部分扣减，但单连接消息天然串行，
// 实际不会命中该窗口。
func (rl *RateLimiter) AllowMulti(dimensions map[string]string) bool {
	for dimension, key := range dimensions {
		if !rl.peek(dimension, key) {
			return false
		}
	}
	for dimension, key := range dimensions {
		if !rl.Allow(dimension, key) {
			return false
		}
	}
	return true
}

// peek 判断维度桶是否还有令牌（不消耗）。桶不存在视为可放行（Allow 会新建满额桶）。
func (rl *RateLimiter) peek(dimension, key string) bool {
	sk := shardKey(dimension, key)
	sh := rl.shardFor(sk)
	sh.mu.RLock()
	bucket := sh.dims[dimension][key]
	sh.mu.RUnlock()
	if bucket == nil {
		return true
	}
	return bucket.tokens.Load() > 0
}

func (rl *RateLimiter) GetTokens(dimension, key string) int {
	sk := shardKey(dimension, key)
	sh := rl.shardFor(sk)
	sh.mu.RLock()
	bucket := sh.dims[dimension][key]
	sh.mu.RUnlock()

	if bucket == nil {
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
	rl.dimensionConfigs[dimension] = config
	rl.cfgMu.Unlock()
	rl.refreshDimBuckets(dimension, config)
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
		sh := &rl.shards[i]
		sh.mu.Lock()
		clear(sh.dims)
		clear(sh.counts)
		sh.mu.Unlock()
	}
}

func (rl *RateLimiter) ClearDimension(dimension string) {
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.Lock()
		delete(sh.dims, dimension)
		delete(sh.counts, dimension)
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
				for dimension, buckets := range sh.dims {
					for key, bucket := range buckets {
						if now-bucket.lastUpdate.Load() > int64(30*time.Minute) {
							delete(buckets, key)
							sh.counts[dimension]--
						}
					}
					if len(buckets) == 0 {
						delete(sh.dims, dimension)
						delete(sh.counts, dimension)
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

// GetStats 各维度桶数（读分片计数器，不遍历全部桶，O(shards×dims)）。
func (rl *RateLimiter) GetStats() map[string]int {
	stats := make(map[string]int)
	for i := range rl.shards {
		sh := &rl.shards[i]
		sh.mu.RLock()
		for dimension, n := range sh.counts {
			stats[dimension] += n
		}
		sh.mu.RUnlock()
	}
	return stats
}
