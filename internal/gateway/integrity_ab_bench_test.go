package gateway

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
)

// oldIntegrity 模拟修复前实现：单全局锁 + fmt.Sprintf 拼 key（对比基线）。
type oldIntegrity struct {
	mu    sync.Mutex
	cache map[string]int64
}

func (o *oldIntegrity) process(connectionID string, msg *protoGw.StreamData) error {
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", connectionID, msg.UserKey, msg.Cmd, msg.SeqId)
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.cache[key]; ok {
		return errIntegrityReplay
	}
	o.cache[key] = 0
	return nil
}

var oldConnSeq atomic.Int64

// BenchmarkIntegrityOldGlobalLock 旧实现并行（全局写锁 + Sprintf）。
func BenchmarkIntegrityOldGlobalLock(b *testing.B) {
	o := &oldIntegrity{cache: make(map[string]int64)}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		conn := "conn-" + strconv.FormatInt(oldConnSeq.Add(1), 10)
		msg := &protoGw.StreamData{Cmd: 100, UserKey: "u"}
		seq := 0
		for pb.Next() {
			seq++
			msg.SeqId = int64(seq)
			_ = o.process(conn, msg)
		}
	})
}
