package gateway

import (
	"sync"
	"time"
)

// RateLimiter 速率限制器
type RateLimiter struct {
	mu              sync.RWMutex
	tokensByDimension map[string]map[string]*TokenBucket // 按维度分组的令牌桶
	tokenRefresh     time.Duration
	maxTokens        int
	burstTokens      int
	cleanupInterval  time.Duration
	dimensionConfigs map[string]DimensionConfig // 各维度的配置
}

// DimensionConfig 维度配置
type DimensionConfig struct {
	MaxTokens    int           // 最大令牌数
	BurstTokens  int           // 突发令牌数
	TokenRefresh time.Duration // 令牌刷新间隔
}

// TokenBucket 令牌桶
type TokenBucket struct {
	tokens     int
	lastUpdate time.Time
	lastAccess time.Time
	maxTokens  int
	burstTokens int
	tokenRefresh time.Duration
}

// NewRateLimiter 创建速率限制器
// 参数:
//
//	maxTokens: 最大令牌数
//	tokenRefresh: 令牌刷新间隔
//
// 返回值:
//
//	*RateLimiter: 速率限制器实例
func NewRateLimiter(maxTokens int, tokenRefresh time.Duration) *RateLimiter {
	rl := &RateLimiter{
		tokensByDimension: make(map[string]map[string]*TokenBucket),
		tokenRefresh:     tokenRefresh,
		maxTokens:        maxTokens,
		burstTokens:      maxTokens * 2, // 突发令牌数为最大令牌数的2倍
		cleanupInterval:  5 * time.Minute, // 清理间隔
		dimensionConfigs: make(map[string]DimensionConfig),
	}
	
	// 设置默认维度配置
	rl.dimensionConfigs["ip"] = DimensionConfig{
		MaxTokens:    maxTokens,
		BurstTokens:  maxTokens * 2,
		TokenRefresh: tokenRefresh,
	}
	rl.dimensionConfigs["user"] = DimensionConfig{
		MaxTokens:    maxTokens / 2, // 用户级限制更严格
		BurstTokens:  maxTokens,
		TokenRefresh: tokenRefresh,
	}
	rl.dimensionConfigs["route"] = DimensionConfig{
		MaxTokens:    maxTokens * 4, // 路由级限制更宽松
		BurstTokens:  maxTokens * 8,
		TokenRefresh: tokenRefresh,
	}
	
	// 启动清理任务
	go rl.cleanup()
	
	return rl
}

// Allow 检查是否允许请求（单维度）
// 参数:
//
//	dimension: 维度（如ip, user, route）
//	key: 请求键
//
// 返回值:
//
//	bool: 是否允许请求
func (rl *RateLimiter) Allow(dimension, key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// 确保维度映射存在
	if _, exists := rl.tokensByDimension[dimension]; !exists {
		rl.tokensByDimension[dimension] = make(map[string]*TokenBucket)
	}

	bucket, exists := rl.tokensByDimension[dimension][key]
	now := time.Now()
	
	if !exists {
		// 获取维度配置
		config, exists := rl.dimensionConfigs[dimension]
		if !exists {
			// 使用默认配置
			config = DimensionConfig{
				MaxTokens:    rl.maxTokens,
				BurstTokens:  rl.burstTokens,
				TokenRefresh: rl.tokenRefresh,
			}
		}

		// 创建新的令牌桶
		bucket = &TokenBucket{
			tokens:       config.MaxTokens - 1,
			lastUpdate:   now,
			lastAccess:   now,
			maxTokens:    config.MaxTokens,
			burstTokens:  config.BurstTokens,
			tokenRefresh: config.TokenRefresh,
		}
		rl.tokensByDimension[dimension][key] = bucket
		return true
	}
	
	// 更新令牌数
	rl.refreshTokens(bucket, now)
	
	// 检查是否有足够的令牌
	if bucket.tokens > 0 {
		bucket.tokens--
		bucket.lastAccess = now
		return true
	}
	
	return false
}

// AllowMulti 检查是否允许请求（多维度）
// 参数:
//
//	dimensions: 维度映射（如{"ip": "192.168.1.1", "user": "user123", "route": "api/login"}")
//
// 返回值:
//
//	bool: 是否允许请求
func (rl *RateLimiter) AllowMulti(dimensions map[string]string) bool {
	// 检查所有维度
	for dimension, key := range dimensions {
		if !rl.Allow(dimension, key) {
			return false
		}
	}
	return true
}

// refreshTokens 刷新令牌数
// 参数:
//
//	bucket: 令牌桶
//	now: 当前时间
func (rl *RateLimiter) refreshTokens(bucket *TokenBucket, now time.Time) {
	// 计算时间差
	duration := now.Sub(bucket.lastUpdate)
	
	// 计算应该添加的令牌数
	addTokens := int(duration / bucket.tokenRefresh)
	if addTokens > 0 {
		// 更新令牌数
		bucket.tokens += addTokens
		if bucket.tokens > bucket.burstTokens {
			bucket.tokens = bucket.burstTokens
		}
		bucket.lastUpdate = now
	}
}

// Start 启动令牌刷新
func (rl *RateLimiter) Start() {
	// 现在不需要单独的刷新线程，因为令牌是在请求时动态刷新的
}

// GetTokens 获取当前令牌数
// 参数:
//
//	dimension: 维度
//	key: 请求键
//
// 返回值:
//
//	int: 当前令牌数
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
	
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	// 更新令牌数
	rl.refreshTokens(bucket, time.Now())
	
	return bucket.tokens
}

// SetDimensionConfig 设置维度配置
// 参数:
//
//	dimension: 维度
//	config: 配置
func (rl *RateLimiter) SetDimensionConfig(dimension string, config DimensionConfig) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.dimensionConfigs[dimension] = config
}

// GetDimensionConfig 获取维度配置
// 参数:
//
//	dimension: 维度
//
// 返回值:
//
//	DimensionConfig: 配置
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

// SetMaxTokens 设置默认最大令牌数
// 参数:
//
//	maxTokens: 最大令牌数
func (rl *RateLimiter) SetMaxTokens(maxTokens int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.maxTokens = maxTokens
	rl.burstTokens = maxTokens * 2
}

// SetTokenRefresh 设置默认令牌刷新间隔
// 参数:
//
//	tokenRefresh: 令牌刷新间隔
func (rl *RateLimiter) SetTokenRefresh(tokenRefresh time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.tokenRefresh = tokenRefresh
}

// Clear 清除所有令牌
func (rl *RateLimiter) Clear() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.tokensByDimension = make(map[string]map[string]*TokenBucket)
}

// ClearDimension 清除指定维度的令牌
// 参数:
//
//	dimension: 维度
func (rl *RateLimiter) ClearDimension(dimension string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	delete(rl.tokensByDimension, dimension)
}

// cleanup 清理过期的令牌桶
func (rl *RateLimiter) cleanup() {
	for {
		time.Sleep(rl.cleanupInterval)
		
		rl.mu.Lock()
		now := time.Now()
		
		// 清理30分钟未访问的令牌桶
		for dimension, buckets := range rl.tokensByDimension {
			for key, bucket := range buckets {
				if now.Sub(bucket.lastAccess) > 30*time.Minute {
					delete(buckets, key)
				}
			}
			// 如果维度下没有令牌桶，删除该维度
			if len(buckets) == 0 {
				delete(rl.tokensByDimension, dimension)
			}
		}
		
		rl.mu.Unlock()
	}
}

// GetStats 获取限流统计信息
// 返回值:
//
//	map[string]int: 各维度的令牌桶数量
func (rl *RateLimiter) GetStats() map[string]int {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	stats := make(map[string]int)
	for dimension, buckets := range rl.tokensByDimension {
		stats[dimension] = len(buckets)
	}
	return stats
}
