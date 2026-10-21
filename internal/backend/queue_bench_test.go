package backend

import (
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
)

func benchQueue(maxSize int) *StreamMessageQueue {
	return NewStreamMessageQueue(config.StreamQueueConfig{
		MaxSize: maxSize,
		Policy:  config.QueuePolicyDrop,
	})
}

// BenchmarkEnqueueDropOverflow 满队丢最旧路径（含归池 PutStreamData）。
func BenchmarkEnqueueDropOverflow(b *testing.B) {
	mq := benchQueue(1024)
	for i := 0; i < 1024; i++ {
		_ = mq.Enqueue(&protoGw.StreamData{Cmd: 1, SeqId: int64(i)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mq.Enqueue(&protoGw.StreamData{Cmd: 1, SeqId: int64(i)})
	}
}

// BenchmarkRequeueFrontFlush 队首回插（Flush 重试热路径，复制移窗）。
func BenchmarkRequeueFrontFlush(b *testing.B) {
	mq := benchQueue(1024)
	for i := 0; i < 1024; i++ {
		_ = mq.Enqueue(&protoGw.StreamData{Cmd: 1, SeqId: int64(i)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mq.RequeueFront(&protoGw.StreamData{Cmd: 1, SeqId: int64(i)})
	}
}
