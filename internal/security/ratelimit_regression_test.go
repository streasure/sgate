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

// TestRateLimiterShardedIsolation 回归：分片锁改造后各 key/维度桶互相隔离
// （无全局桶），ClearDimension 只清目标维度。
func TestRateLimiterShardedIsolation(t *testing.T) {
	// tokenRefresh 拉长到测试期间不补充，便于确定性断言。
	rl := NewRateLimiter(2, time.Hour)
	defer rl.Stop()

	if !rl.Allow("ip", "a") {
		t.Fatal("ip a first request should pass")
	}
	if !rl.Allow("ip", "a") {
		t.Fatal("ip a second request should pass")
	}
	if rl.Allow("ip", "a") {
		t.Fatal("ip a should be exhausted")
	}
	if !rl.Allow("ip", "b") {
		t.Fatal("ip b should have its own tokens (no shared global bucket)")
	}
	if !rl.Allow("route", "a") {
		t.Fatal("route a should have its own tokens")
	}

	rl.ClearDimension("ip")
	if !rl.Allow("ip", "a") {
		t.Fatal("ip a should be reset after ClearDimension")
	}

	stats := rl.GetStats()
	if stats["ip"] != 1 {
		t.Fatalf("ip bucket count = %d, want 1 (recreated only ip a)", stats["ip"])
	}
	if stats["route"] != 1 {
		t.Fatalf("route bucket count = %d, want 1 (route survives ClearDimension(\"ip\"))", stats["route"])
	}
}
