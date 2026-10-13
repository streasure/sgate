package gateway

import (
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/obs"
	"github.com/streasure/sgate/internal/types"
)

// TestPipelineRecordsLatency 回归：延迟记录曾与 tracer 强耦合（span != nil 才 Record），
// tracing 关闭时 /stats 的 P50/P95/P99 恒 0。解耦后每条转发消息必须产生非零延迟样本。
func TestPipelineRecordsLatency(t *testing.T) {
	gw := newGateway(minimalConfig())
	gw.filterChain = types.NewFilterChain()

	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "real-user")
	gw.connectionManager.SetConnectionServerID(connID, "logic:zone:1")
	connObj := gw.connectionManager.GetConnection(connID)
	if connObj == nil {
		t.Fatal("connection not found")
	}
	connObj.SetCachedLogicClient(fakeLogicClient{})

	// 生产中 tracker 由 obs 组件(200) 在 Gateway.Start(1000) 时注入；单测直接注入。
	gw.latencyTracker = obs.NewLatencyTracker(10000)

	for i := range 100 {
		msg := &protoGw.StreamData{Cmd: 1100010, Data: []byte("x"), SeqId: int64(i + 1)}
		res := gw.pipeline.Process(&kickMockConn{}, msg.Data, msg, connID)
		if res.ProtoMsg == nil {
			t.Fatalf("msg %d not forwarded: %v", i, res.Error)
		}
	}

	// 每 16 条采样 1 条（QPC syscall 开销控制）：100 条 → 6 个样本（16/32/48/64/80/96）
	st := gw.latencyTracker.GetStats()
	if st.Cnt != 6 {
		t.Fatalf("latency samples = %d, want 6 (1 per 16 messages)", st.Cnt)
	}
	if st.P50 <= 0 {
		t.Fatalf("P50 = %d ns, want > 0 (latency must not depend on tracer)", st.P50.Nanoseconds())
	}
	if st.Max < st.P50 {
		t.Fatalf("Max (%v) < P50 (%v)", st.Max, st.P50)
	}
}
