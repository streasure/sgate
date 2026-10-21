package gateway

import (
	"strconv"
	"sync/atomic"
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
)

func BenchmarkIntegrityProcessMessage(b *testing.B) {
	mi := NewMessageIntegrity(60000)
	defer mi.Stop()
	var connSeq atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		conn := "conn-" + strconv.FormatInt(connSeq.Add(1), 10)
		msg := &protoGw.StreamData{Cmd: 100, UserKey: "u"}
		seq := 0
		for pb.Next() {
			seq++
			msg.SeqId = int64(seq)
			_ = mi.ProcessMessage(conn, msg)
		}
	})
}

// BenchmarkIntegrityProcessMessageSingleConn 单连接串行（同片热点，测锁开销下限）。
func BenchmarkIntegrityProcessMessageSingleConn(b *testing.B) {
	mi := NewMessageIntegrity(60000)
	defer mi.Stop()
	msg := &protoGw.StreamData{Cmd: 100, SeqId: 1, UserKey: "u"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg.SeqId = int64(i + 1)
		_ = mi.ProcessMessage("conn-1", msg)
	}
}
