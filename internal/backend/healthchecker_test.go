package backend

import "testing"

// TestHealthChecker_StopIdempotent 双重 Stop 不得 panic（stopOnce 修复）。
func TestHealthChecker_StopIdempotent(t *testing.T) {
	hc := NewHealthChecker(&LogicClient{}, HealthCheckConfig{
		Enabled:  false,
		Interval: 0,
	})
	hc.Start()
	hc.Stop()
	hc.Stop() // 第二次必须安全
}
