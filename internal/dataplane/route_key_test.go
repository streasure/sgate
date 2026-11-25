package dataplane

import "testing"

// TestRouteKeyFor_Bounded 验证驻留缓存有界：灌超上限后计数不再增长
// （防已认证连接用随机 cmd 无界放大内存），且缓存内/缓存外的 cmd
// 返回值均正确（缓存满退化为 strconv 直算，语义不变）。
func TestRouteKeyFor_Bounded(t *testing.T) {
	// 单线程执行：计数在 LoadOrStore 前判上限，不会越过 routeKeyCacheMax。
	// 包级缓存可能已被同包其他测试写入，故只断言不越上限。
	for i := int32(1); i <= routeKeyCacheMax+1024; i++ {
		_ = routeKeyFor(i)
	}
	if n := routeKeyCacheCount.Load(); n > routeKeyCacheMax {
		t.Fatalf("routeKeyCacheCount = %d, want <= %d", n, routeKeyCacheMax)
	}
	// 缓存内 cmd：驻留命中
	if got := routeKeyFor(42); got != "42" {
		t.Fatalf("routeKeyFor(42) = %q, want 42", got)
	}
	// 缓存外 cmd（上限已满，不驻留）：仍返回正确的键
	if got := routeKeyFor(1 << 30); got != "1073741824" {
		t.Fatalf("routeKeyFor(1<<30) = %q, want 1073741824", got)
	}
}
