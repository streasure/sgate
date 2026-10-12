package security

import (
	"strconv"
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
// （Allow 不消耗全局桶），ClearDimension 只清目标维度。
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

// TestRateLimiterPerDimCapIndependent 回归：分片后每维度独立上限——
// 单维度打满分片槽位不影响同分片的其他维度（恢复分片前 per-dimension 语义）。
func TestRateLimiterPerDimCapIndependent(t *testing.T) {
	rl := NewRateLimiter(100, time.Hour)
	defer rl.Stop()
	rl.maxBucketsPerShard = 1

	if !rl.Allow("ip", "k0") {
		t.Fatal("ip k0 should pass")
	}
	target := rl.shardFor(shardKey("ip", "k0"))

	// 找一个落在同一分片的 ip key：该片 ip 槽已满，应被拒。
	collide := findCollidingKey(rl, target, "ip")
	if rl.Allow("ip", collide) {
		t.Fatal("ip dim should be capped in the full shard")
	}
	// 同分片的 route 维度不受 ip 维度封顶影响。
	routeKey := findCollidingKey(rl, target, "route")
	if !rl.Allow("route", routeKey) {
		t.Fatal("route dim should not be blocked by ip dim cap in same shard")
	}
}

func findCollidingKey(rl *RateLimiter, target *rateShard, dimension string) string {
	for i := range 100000 {
		cand := "c" + strconv.Itoa(i)
		if rl.shardFor(shardKey(dimension, cand)) == target {
			return cand
		}
	}
	panic("no colliding key found")
}

// TestRateLimiterUpdateRateRefreshesBuckets 回归：UpdateRate 热更必须刷新
// 已存在桶的参数，而非只影响之后新建的桶。
func TestRateLimiterUpdateRateRefreshesBuckets(t *testing.T) {
	rl := NewRateLimiter(1, time.Hour)
	defer rl.Stop()
	if !rl.Allow("ip", "hot") {
		t.Fatal("ip hot first should pass")
	}
	if rl.Allow("ip", "hot") {
		t.Fatal("ip hot should be exhausted at maxTokens=1")
	}

	rl.UpdateRate(5, time.Hour)
	if got := rl.GetDimensionConfig("ip").MaxTokens; got != 5 {
		t.Fatalf("ip MaxTokens = %d, want 5", got)
	}

	sh := rl.shardFor(shardKey("ip", "hot"))
	sh.mu.RLock()
	bucket := sh.dims["ip"]["hot"]
	sh.mu.RUnlock()
	if bucket == nil {
		t.Fatal("existing bucket should survive UpdateRate")
	}
	if got := bucket.maxTokens.Load(); got != 5 {
		t.Fatalf("existing bucket maxTokens = %d, want 5 (hot update applied)", got)
	}
}

// TestRateLimiterAllowMultiNoPartialConsume 回归：AllowMulti 任一维度耗尽时
// 整体拒绝且不消耗其他维度令牌（peek-then-consume）。
func TestRateLimiterAllowMultiNoPartialConsume(t *testing.T) {
	rl := NewRateLimiter(1, time.Hour)
	defer rl.Stop()
	if !rl.Allow("ip", "a") {
		t.Fatal("ip a first should pass")
	}
	if rl.AllowMulti(map[string]string{"user": "u1", "ip": "a"}) {
		t.Fatal("AllowMulti should fail when any dim exhausted")
	}
	if !rl.Allow("user", "u1") {
		t.Fatal("user should not be partially consumed by failed AllowMulti")
	}
}

// TestRateLimiterGlobalCeiling 回归：AllowGlobal 全局硬顶——整桶预算耗尽后
// 被拒（overload 未运行时的兜底），与 per-IP 维度互不影响。
func TestRateLimiterGlobalCeiling(t *testing.T) {
	rl := NewRateLimiter(1, time.Hour)
	defer rl.Stop()
	rl.SetGlobalMaxTokens(2)

	if !rl.AllowGlobal() {
		t.Fatal("first global consume should pass")
	}
	if !rl.AllowGlobal() {
		t.Fatal("second global consume should pass within globalMax=2")
	}
	if rl.AllowGlobal() {
		t.Fatal("global ceiling should reject after globalMax exhausted")
	}
	// 全局硬顶独立于 per-dim：per-IP 桶仍可用。
	if !rl.Allow("ip", "spread") {
		t.Fatal("per-ip dim should be unaffected by global ceiling state")
	}
}
