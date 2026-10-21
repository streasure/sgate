package traffic

import (
	"strconv"
	"testing"

	"github.com/streasure/sgate/internal/config"
)

func benchDegradationManager() *DegradationManager {
	rules := make([]config.DegradationRuleConfig, 0, 8)
	for i := 0; i < 8; i++ {
		rules = append(rules, config.DegradationRuleConfig{
			Route:          "route-" + strconv.Itoa(i),
			ErrorThreshold: 0.5,
			WindowSize:     100,
		})
	}
	return NewDegradationManager(rules)
}

// BenchmarkRecordResultMultiRoute 多 route 并行（修复前全局写锁串行化全部 route）。
func BenchmarkRecordResultMultiRoute(b *testing.B) {
	m := benchDegradationManager()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			m.RecordResult("route-"+strconv.Itoa(i&7), false)
			i++
		}
	})
}

// BenchmarkRecordResultSingleRoute 单 route 并行（同规则 windowMu 热点）。
func BenchmarkRecordResultSingleRoute(b *testing.B) {
	m := benchDegradationManager()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.RecordResult("route-0", false)
		}
	})
}
