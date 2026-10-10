package security

import (
	"sync"
	"testing"
	"time"
)

// TestRateLimiterUserDimensionFloor 回归：maxTokens=1 时 user 维度 MaxTokens 曾为
// maxTokens/2=0，新桶初始令牌为 0，该维度请求被全部拒绝。
func TestRateLimiterUserDimensionFloor(t *testing.T) {
	rl := NewRateLimiter(1, time.Second)
	defer rl.Stop()

	cfg := rl.GetDimensionConfig("user")
	if cfg.MaxTokens < 1 {
		t.Fatalf("user MaxTokens = %d, want >= 1", cfg.MaxTokens)
	}
	if !rl.Allow("user", "u1") {
		t.Fatal("user dimension should allow at least one request at maxTokens=1")
	}
}

// TestTokenBucketRefillNoLostUpdate 并发扣减 + 补充下令牌不得为负、
// 补充的 CAS 循环不得覆盖并发扣减（Load+Store 丢更新回归）。
func TestTokenBucketRefillNoLostUpdate(t *testing.T) {
	tb := newTokenBucket(100, 200, time.Millisecond)
	// 直接耗尽当前令牌。
	for tb.tokens.Load() > 0 {
		if !tb.tryConsume() {
			break
		}
	}
	if tb.tryConsume() {
		t.Fatal("bucket should be empty")
	}

	// 等一个刷新周期后并发扣减 + 多次 tryConsume 触发补充路径。
	time.Sleep(2 * time.Millisecond)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				tb.tryConsume()
			}
		})
	}
	wg.Wait()

	if got := tb.tokens.Load(); got < 0 {
		t.Fatalf("tokens = %d, want >= 0 (refill Store raced with concurrent decrement)", got)
	}
}
