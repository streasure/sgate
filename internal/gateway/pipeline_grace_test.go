package gateway

import (
	"testing"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
)

// TestPipelineUnboundGraceDropNoClose 登录宽限期：新连接（绑定在后台协程完成）
// 内到达的非 preAuth 消息只丢弃不断连；宽限期归零后恢复断连语义。
func TestPipelineUnboundGraceDropNoClose(t *testing.T) {
	gw := newGateway(minimalConfig())
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "temp_grace_user")
	msg := &protoGw.StreamData{Cmd: 1100010, Data: []byte("hb"), SeqId: 1}

	res := gw.pipeline.Process(&kickMockConn{}, []byte("x"), msg, connID)
	if res.Action != gnet.None {
		t.Fatalf("grace window action = %v, want gnet.None", res.Action)
	}
	if res.Error != nil {
		t.Fatalf("grace window error = %v, want nil", res.Error)
	}

	old := loginBindGrace
	loginBindGrace = 0
	defer func() { loginBindGrace = old }()

	connID2 := gw.connectionManager.AddConnection(&kickMockConn{}, "temp_grace_user2")
	res2 := gw.pipeline.Process(&kickMockConn{}, []byte("x"), msg, connID2)
	if res2.Action != gnet.Close {
		t.Fatalf("expired grace action = %v, want gnet.Close", res2.Action)
	}
}

// TestPipelineWorkerPoolQueueFloor 队列边界：queueSize < shards 时每分片至少 1
// （否则退化为无缓冲通道，Submit 在 default 分支全量丢弃）。
func TestPipelineWorkerPoolQueueFloor(t *testing.T) {
	p := NewPipelineWorkerPool(nil, config.PipelineConfig{WorkerShards: 64, WorkerQueueSize: 10})
	defer p.Stop()
	if got := cap(p.workers[0].taskCh); got != 1 {
		t.Fatalf("per-shard queue cap = %d, want 1", got)
	}
	for i, w := range p.workers {
		if cap(w.taskCh) < 1 {
			t.Fatalf("shard %d queue cap = 0", i)
		}
	}

	// 零值回退默认（与 config.DefaultPipelineWorkerQueueSize 统一）。
	p2 := NewPipelineWorkerPool(nil, config.PipelineConfig{WorkerShards: 8})
	defer p2.Stop()
	if got, want := cap(p2.workers[0].taskCh), config.DefaultPipelineWorkerQueueSize/8; got != want {
		t.Fatalf("default per-shard queue cap = %d, want %d", got, want)
	}
}
