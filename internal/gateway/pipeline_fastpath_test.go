package gateway

import (
	"sync/atomic"
	"testing"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/types"
)

// countingAbortFilter 记录调用次数并中止链，用于验证过滤器是否被执行。
type countingAbortFilter struct {
	calls atomic.Int64
}

func (f *countingAbortFilter) Name() string             { return "counting-abort" }
func (f *countingAbortFilter) Phase() types.FilterPhase { return types.PhaseForward }
func (f *countingAbortFilter) Priority() int            { return 100 }
func (f *countingAbortFilter) Process(fc *types.FilterContext) (bool, error) {
	f.calls.Add(1)
	fc.Abort = true
	return false, nil
}

// fakeLogicClient 满足 connection.LogicClientProvider 的桩。
type fakeLogicClient struct{}

func (fakeLogicClient) IsConnected() bool                     { return true }
func (fakeLogicClient) SendMessage(*protoGw.StreamData) error { return nil }

// TestPipelineFastPathSkippedWhenFilterChainNonEmpty 回归：超级快速路径曾只检查
// 安全组件指针，不检查 filterChain。仅启用 jwtAuth（过滤器链非空、安全组件全 nil）
// 时整条过滤器链被跳过 = JWT 校验被绕过。链非空必须走完整路径并执行过滤器。
func TestPipelineFastPathSkippedWhenFilterChainNonEmpty(t *testing.T) {
	gw := newGateway(minimalConfig())
	filter := &countingAbortFilter{}
	fc := types.NewFilterChain()
	fc.AddFilter(filter)
	gw.filterChain = fc

	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "real-user")
	gw.connectionManager.SetConnectionServerID(connID, "logic:zone:1")
	connObj := gw.connectionManager.GetConnection(connID)
	if connObj == nil {
		t.Fatal("connection not found")
	}
	connObj.SetCachedLogicClient(fakeLogicClient{})

	msg := &protoGw.StreamData{Cmd: 1100010, Data: []byte("x"), SeqId: 1}
	res := gw.pipeline.Process(&kickMockConn{}, msg.Data, msg, connID)

	if got := filter.calls.Load(); got != 1 {
		t.Fatalf("filter calls = %d, want 1 (fast path must not skip non-empty filter chain)", got)
	}
	if res.Action != gnet.None {
		t.Fatalf("action = %v, want gnet.None (filter aborted)", res.Action)
	}
	if res.ProtoMsg != nil {
		t.Fatalf("ProtoMsg = %v, want nil (aborted filter must not forward)", res.ProtoMsg)
	}
}

// TestPipelineFastPathEmptyChainStillFast 保底：链为空且无安全组件时仍走快速路径
// （不调用过滤器，直接转发）。
func TestPipelineFastPathEmptyChainStillFast(t *testing.T) {
	gw := newGateway(minimalConfig())
	gw.filterChain = types.NewFilterChain() // 空链

	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "real-user")
	gw.connectionManager.SetConnectionServerID(connID, "logic:zone:1")
	connObj := gw.connectionManager.GetConnection(connID)
	if connObj == nil {
		t.Fatal("connection not found")
	}
	connObj.SetCachedLogicClient(fakeLogicClient{})

	msg := &protoGw.StreamData{Cmd: 1100010, Data: []byte("x"), SeqId: 1}
	res := gw.pipeline.Process(&kickMockConn{}, msg.Data, msg, connID)

	if res.Action != gnet.None {
		t.Fatalf("action = %v, want gnet.None", res.Action)
	}
	if res.Error != nil {
		t.Fatalf("error = %v, want nil", res.Error)
	}
	if res.ProtoMsg == nil {
		t.Fatal("ProtoMsg = nil, want forwarded message")
	}
}
