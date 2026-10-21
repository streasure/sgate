package traffic

import (
	"strconv"
	"sync"
	"testing"
)

// oldDegradation 模拟修复前实现：单全局写锁保护全部 route（对比基线）。
type oldDegradation struct {
	mu     sync.Mutex
	routes map[string][]bool
	pos    map[string]int
}

func newOldDegradation(n int) *oldDegradation {
	o := &oldDegradation{routes: make(map[string][]bool), pos: make(map[string]int)}
	for i := 0; i < n; i++ {
		k := "route-" + strconv.Itoa(i)
		o.routes[k] = make([]bool, 100)
	}
	return o
}

func (o *oldDegradation) record(route string, isError bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	w, ok := o.routes[route]
	if !ok {
		return
	}
	o.pos[route] = (o.pos[route] + 1) % len(w)
	w[o.pos[route]] = isError
}

// BenchmarkRecordResultOldGlobalLock 旧实现多 route 并行（全局写锁串行化）。
func BenchmarkRecordResultOldGlobalLock(b *testing.B) {
	o := newOldDegradation(8)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			o.record("route-"+strconv.Itoa(i&7), false)
			i++
		}
	})
}
