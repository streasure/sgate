package gateway

import (
	"sync"
	"sync/atomic"
	"time"
)

type RateLimiter struct {
	mu                sync.RWMutex
	tokensByDimension map[string]map[string]*TokenBucket
	globalBucket      *TokenBucket
	tokenRefresh      time.Duration
	maxTokens         int
	burstTokens       int
	cleanupInterval   time.Duration
	dimensionConfigs  map[string]DimensionConfig
	maxBucketsPerDim  int
	stopCh            chan struct{}
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
	now := time.Now().UnixNano()
	last := tb.lastUpdate.Load()

	// 按时间差补充令牌（CAS 更新 lastUpdate，只有一个 goroutine 执行补充）
	if now-last > int64(tb.tokenRefresh) {
		if tb.lastUpdate.CompareAndSwap(last, now) {
			elapsed := time.Duration(now - last)
			refill := int64(elapsed/tb.tokenRefresh) * tb.maxTokens
			if refill > 0 {
				current := tb.tokens.Load()
				newTokens := current + refill
				if newTokens > tb.burstTokens {
					newTokens = tb.burstTokens
				}
				tb.tokens.Store(newTokens)
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
		tokensByDimension: make(map[string]map[string]*TokenBucket),
		tokenRefresh:      tokenRefresh,
		maxTokens:         maxTokens,
		burstTokens:       maxTokens * 2,
		cleanupInterval:   5 * time.Minute,
		dimensionConfigs:  make(map[string]DimensionConfig),
		maxBucketsPerDim:  100000,
		stopCh:            make(chan struct{}),
	}

	rl.globalBucket = &TokenBucket{
		maxTokens:    int64(maxTokens * 10),
		burstTokens:  int64(maxTokens * 20),
		tokenRefresh: tokenRefresh,
	}
	rl.globalBucket.tokens.Store(int64(maxTokens * 10))
	rl.globalBucket.lastUpdate.Store(time.Now().UnixNano())

	rl.dimensionConfigs["ip"] = DimensionConfig{
		MaxTokens:    maxTokens,
		BurstTokens:  maxTokens * 2,
		TokenRefresh: tokenRefresh,
	}
	rl.dimensionConfigs["user"] = DimensionConfig{
		MaxTokens:    maxTokens / 2,
		BurstTokens:  maxTokens,
		TokenRefresh: tokenRefresh,
	}
	rl.dimensionConfigs["route"] = DimensionConfig{
		MaxTokens:    maxTokens * 4,
		BurstTokens:  maxTokens * 8,
		TokenRefresh: tokenRefresh,
	}

	go rl.cleanup()

	return rl
}

func (rl *RateLimiter) UpdateRate(maxTokens int, tokenRefresh time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.tokenRefresh = tokenRefresh
	rl.maxTokens = maxTokens
	rl.burstTokens = maxTokens * 2
	rl.globalBucket = &TokenBucket{
		maxTokens:    int64(maxTokens * 10),
		burstTokens:  int64(maxTokens * 20),
		tokenRefresh: tokenRefresh,
	}
	rl.globalBucket.tokens.Store(int64(maxTokens * 10))
	rl.globalBucket.lastUpdate.Store(time.Now().UnixNano())
	rl.dimensionConfigs["ip"] = DimensionConfig{
		MaxTokens:    maxTokens,
		BurstTokens:  maxTokens * 2,
		TokenRefresh: tokenRefresh,
	}
	rl.dimensionConfigs["user"] = DimensionConfig{
		MaxTokens:    maxTokens / 2,
		BurstTokens:  maxTokens,
		TokenRefresh: tokenRefresh,
	}
	rl.dimensionConfigs["route"] = DimensionConfig{
		MaxTokens:    maxTokens * 4,
		BurstTokens:  maxTokens * 8,
		TokenRefresh: tokenRefresh,
	}
}

func (rl *RateLimiter) Allow(dimension, key string) bool {
	if !rl.globalBucket.tryConsume() {
		return false
	}

	rl.mu.RLock()
	bucket, exists := rl.tokensByDimension[dimension][key]
	rl.mu.RUnlock()

	if !exists {
		rl.mu.Lock()
		if _, exists = rl.tokensByDimension[dimension]; !exists {
			rl.tokensByDimension[dimension] = make(map[string]*TokenBucket)
		}
		if bucket, exists = rl.tokensByDimension[dimension][key]; !exists {
			if len(rl.tokensByDimension[dimension]) >= rl.maxBucketsPerDim {
				rl.mu.Unlock()
				return false
			}
			config := rl.dimensionConfigs[dimension]
			if config.MaxTokens == 0 {
				config.MaxTokens = rl.maxTokens
				config.BurstTokens = rl.burstTokens
				config.TokenRefresh = rl.tokenRefresh
			}
			bucket = &TokenBucket{
				maxTokens:    int64(config.MaxTokens),
				burstTokens:  int64(config.BurstTokens),
				tokenRefresh: config.TokenRefresh,
			}
			bucket.tokens.Store(int64(config.MaxTokens))
			bucket.lastUpdate.Store(time.Now().UnixNano())
			rl.tokensByDimension[dimension][key] = bucket
		}
		rl.mu.Unlock()
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
	rl.mu.RLock()
	buckets, exists := rl.tokensByDimension[dimension]
	if !exists {
		rl.mu.RUnlock()
		return rl.maxTokens
	}
	bucket, exists := buckets[key]
	rl.mu.RUnlock()

	if !exists {
		config, exists := rl.dimensionConfigs[dimension]
		if !exists {
			return rl.maxTokens
		}
		return config.MaxTokens
	}

	return int(bucket.tokens.Load())
}

func (rl *RateLimiter) SetDimensionConfig(dimension string, config DimensionConfig) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.dimensionConfigs[dimension] = config
}

func (rl *RateLimiter) GetDimensionConfig(dimension string) DimensionConfig {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
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
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.maxTokens = maxTokens
	rl.burstTokens = maxTokens * 2
}

func (rl *RateLimiter) SetTokenRefresh(tokenRefresh time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.tokenRefresh = tokenRefresh
}

func (rl *RateLimiter) Clear() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.tokensByDimension = make(map[string]map[string]*TokenBucket)
}

func (rl *RateLimiter) ClearDimension(dimension string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.tokensByDimension, dimension)
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for dimension, buckets := range rl.tokensByDimension {
				for key, bucket := range buckets {
					if now.UnixNano()-bucket.lastUpdate.Load() > 30*60*1e9 {
						delete(buckets, key)
					}
				}
				if len(buckets) == 0 {
					delete(rl.tokensByDimension, dimension)
				}
			}
			rl.mu.Unlock()
		}
	}
}

func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

func (rl *RateLimiter) GetStats() map[string]int {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	stats := make(map[string]int)
	for dimension, buckets := range rl.tokensByDimension {
		stats[dimension] = len(buckets)
	}
	return stats
}
