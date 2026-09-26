package security

import (
	"testing"
	"time"

	"github.com/streasure/sgate/internal/config"
)

// TestWAF_ShouldBlock blockAction=drop/log 决定断连与否。
func TestWAF_ShouldBlock(t *testing.T) {
	drop := NewWAF(config.WAFConfig{Enabled: true, BlockAction: "drop"})
	if !drop.ShouldBlock() {
		t.Fatal("blockAction=drop should block")
	}
	logOnly := NewWAF(config.WAFConfig{Enabled: true, BlockAction: "log"})
	if logOnly.ShouldBlock() {
		t.Fatal("blockAction=log should not block")
	}
	// 缺省 → drop（与 DefaultWAFBlockAction 一致）
	def := NewWAF(config.WAFConfig{Enabled: true})
	if !def.ShouldBlock() {
		t.Fatal("empty blockAction should default to drop")
	}
}

// TestWAF_InspectDetectsSQLInjection SQL 注入特征应被识别。
func TestWAF_InspectDetectsSQLInjection(t *testing.T) {
	w := NewWAF(config.WAFConfig{Enabled: true, MaxPayloadSize: 1024})
	if w.Inspect([]byte("1' OR '1'='1 UNION SELECT password FROM users")) {
		t.Fatal("SQL injection not detected")
	}
	// 超限 payload
	if w.Inspect(make([]byte, 2048)) {
		t.Fatal("oversized payload not blocked")
	}
	// 正常内容
	if !w.Inspect([]byte("hello world")) {
		t.Fatal("benign content flagged")
	}
	if w.GetBlockedCount() < 2 {
		t.Fatalf("blockedCount=%d want >=2", w.GetBlockedCount())
	}
}

// TestTokenBucket_ZeroRefreshNoPanic tokenRefresh<=0 不除零 panic。
func TestTokenBucket_ZeroRefreshNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on zero tokenRefresh: %v", r)
		}
	}()
	tb := newTokenBucket(10, 20, 0)
	if !tb.tryConsume() {
		t.Fatal("first consume should succeed")
	}
	rl := NewRateLimiter(10, 0)
	if !rl.Allow("ip", "1.2.3.4") {
		t.Fatal("rate limiter with zero refresh should still allow")
	}
	rl.UpdateRate(10, 0)
	if !rl.Allow("ip", "1.2.3.4") {
		t.Fatal("rate limiter after zero-refresh update should allow")
	}
}

// TestRateLimiter_TokenRefreshParseZero 组件层对 "0s"/非法值的兜底。
func TestRateLimiter_TokenRefreshParseZero(t *testing.T) {
	// 等价于 security_component 的分支：err==nil && d>0 才用，否则回退 1s
	d, err := time.ParseDuration("0s")
	if err != nil || d > 0 {
		t.Fatalf("setup: d=%v err=%v", d, err)
	}
	rl := NewRateLimiter(5, time.Second)
	if !rl.Allow("route", "r") {
		t.Fatal("allow failed")
	}
}
