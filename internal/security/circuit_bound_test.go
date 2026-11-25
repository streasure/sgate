package security

import (
	"strconv"
	"testing"
	"time"
)

// TestGetCircuitBreaker_Bounded 验证熔断器条数有界：灌超上限后不再新建
// （返回 nil，防已认证连接用随机 cmd 无界建桶），已驻留 name 仍正常命中，
// 且计数不越上限。
func TestGetCircuitBreaker_Bounded(t *testing.T) {
	m := NewCircuitBreakerManager()
	// 单线程执行：占位 CAS 在 LoadOrStore 前完成，计数不会越上限。
	for i := 0; i < maxBreakers+1024; i++ {
		_ = m.GetCircuitBreaker(strconv.Itoa(i), 5, 3, time.Second)
	}
	if n := m.count.Load(); n > maxBreakers {
		t.Fatalf("breaker count = %d, want <= %d", n, maxBreakers)
	}
	// 已驻留 name：命中已有 breaker（不受上限影响）
	if b := m.GetCircuitBreaker("1", 5, 3, time.Second); b == nil {
		t.Fatal("existing breaker should still be returned beyond cap")
	}
	// 上限外的新 name：不再新建，返回 nil（调用方按无熔断放行）
	if b := m.GetCircuitBreaker("beyond-cap-fresh", 5, 3, time.Second); b != nil {
		t.Fatal("new breaker beyond cap should be nil")
	}
	if n := m.count.Load(); n > maxBreakers {
		t.Fatalf("count grew after cap hit: %d", n)
	}
}

// TestRemoveBreaker_ReleasesSlot 移除后名额释放，可再建新 breaker。
func TestRemoveBreaker_ReleasesSlot(t *testing.T) {
	m := NewCircuitBreakerManager()
	b := m.GetCircuitBreaker("r1", 5, 3, time.Second)
	if b == nil {
		t.Fatal("expected breaker")
	}
	if n := m.count.Load(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	m.RemoveBreaker("r1")
	if n := m.count.Load(); n != 0 {
		t.Fatalf("count after remove = %d, want 0", n)
	}
	if _, ok := m.GetBreaker("r1"); ok {
		t.Fatal("removed breaker should be gone")
	}
	// 重复移除不重复扣减
	m.RemoveBreaker("r1")
	if n := m.count.Load(); n != 0 {
		t.Fatalf("count after double remove = %d, want 0", n)
	}
}
