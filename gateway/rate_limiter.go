package gateway

import (
	"sync"
	"time"
)

// RateLimiter 速率限制器
type RateLimiter struct {
	mu           sync.RWMutex
	tokens       map[string]*TokenBucket
	tokenRefresh time.Duration
	maxTokens    int
	burstTokens  int
	cleanupInterval time.Duration
}

// TokenBucket 令牌桶
type TokenBucket struct {
	tokens     int
	lastUpdate time.Time
	lastAccess time.Time
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
		tokens:          make(map[string]*TokenBucket),
		tokenRefresh:    tokenRefresh,
		maxTokens:       maxTokens,
		burstTokens:     maxTokens * 2, // 突发令牌数为最大令牌数的2倍
		cleanupInterval: 5 * time.Minute, // 清理间隔
	}
	
	// 启动清理任务
	go rl.cleanup()
	
	return rl
}

// Allow 检查是否允许请求
// 参数:
//
//	key: 请求键（如IP地址）
//
// 返回值:
//
//	bool: 是否允许请求
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	bucket, exists := rl.tokens[key]
	now := time.Now()
	
	if !exists {
		// 创建新的令牌桶
		bucket = &TokenBucket{
			tokens:     rl.maxTokens - 1,
			lastUpdate: now,
			lastAccess: now,
		}
		rl.tokens[key] = bucket
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

// refreshTokens 刷新令牌数
// 参数:
//
//	bucket: 令牌桶
//	now: 当前时间
func (rl *RateLimiter) refreshTokens(bucket *TokenBucket, now time.Time) {
	// 计算时间差
	duration := now.Sub(bucket.lastUpdate)
	
	// 计算应该添加的令牌数
	addTokens := int(duration / rl.tokenRefresh)
	if addTokens > 0 {
		// 更新令牌数
		bucket.tokens += addTokens
		if bucket.tokens > rl.burstTokens {
			bucket.tokens = rl.burstTokens
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
//	key: 请求键（如IP地址）
//
// 返回值:
//
//	int: 当前令牌数
func (rl *RateLimiter) GetTokens(key string) int {
	rl.mu.RLock()
	bucket, exists := rl.tokens[key]
	rl.mu.RUnlock()

	if !exists {
		return rl.maxTokens
	}
	
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	// 更新令牌数
	rl.refreshTokens(bucket, time.Now())
	
	return bucket.tokens
}

// SetMaxTokens 设置最大令牌数
// 参数:
//
//	maxTokens: 最大令牌数
func (rl *RateLimiter) SetMaxTokens(maxTokens int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.maxTokens = maxTokens
	rl.burstTokens = maxTokens * 2
	
	// 更新所有令牌桶
	now := time.Now()
	for key, bucket := range rl.tokens {
		rl.refreshTokens(bucket, now)
		if bucket.tokens > rl.burstTokens {
			bucket.tokens = rl.burstTokens
		}
		rl.tokens[key] = bucket
	}
}

// SetTokenRefresh 设置令牌刷新间隔
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

	rl.tokens = make(map[string]*TokenBucket)
}

// cleanup 清理过期的令牌桶
func (rl *RateLimiter) cleanup() {
	for {
		time.Sleep(rl.cleanupInterval)
		
		rl.mu.Lock()
		now := time.Now()
		
		// 清理30分钟未访问的令牌桶
		for key, bucket := range rl.tokens {
			if now.Sub(bucket.lastAccess) > 30*time.Minute {
				delete(rl.tokens, key)
			}
		}
		
		rl.mu.Unlock()
	}
}

// SetBurstTokens 设置突发令牌数
// 参数:
//
//	burstTokens: 突发令牌数
func (rl *RateLimiter) SetBurstTokens(burstTokens int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.burstTokens = burstTokens
}

// GetBurstTokens 获取突发令牌数
// 返回值:
//
//	int: 突发令牌数
func (rl *RateLimiter) GetBurstTokens() int {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	return rl.burstTokens
}