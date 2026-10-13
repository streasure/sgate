package gateway

import (
	"errors"
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/types"
)

// failingLogicClient SendMessage 恒失败的桩。
type failingLogicClient struct{}

func (failingLogicClient) IsConnected() bool { return true }
func (failingLogicClient) SendMessage(*protoGw.StreamData) error {
	return errors.New("logic stream down")
}

// TestPipelineForwardFailureCountsFailedNotDroppedFull 回归：转发失败曾同时计入
// messagesDroppedFull，污染 droppedTotal 语义（droppedFull 专指 overload 队列满）。
// 失败必须只计 messagesFailed（stats.failed）。
func TestPipelineForwardFailureCountsFailedNotDroppedFull(t *testing.T) {
	gw := newGateway(minimalConfig())
	gw.filterChain = types.NewFilterChain()

	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "real-user")
	gw.connectionManager.SetConnectionServerID(connID, "logic:zone:1")
	connObj := gw.connectionManager.GetConnection(connID)
	if connObj == nil {
		t.Fatal("connection not found")
	}
	connObj.SetCachedLogicClient(failingLogicClient{})

	var failed0, full0 int64 = gw.messagesFailed.Load(), gw.messagesDroppedFull.Load()

	msg := &protoGw.StreamData{Cmd: 1100010, Data: []byte("x"), SeqId: 1}
	res := gw.pipeline.Process(&kickMockConn{}, msg.Data, msg, connID)

	if res.Error == nil {
		t.Fatal("expected forward error")
	}
	if got := gw.messagesFailed.Load(); got != failed0+1 {
		t.Fatalf("messagesFailed = %d, want %d", got, failed0+1)
	}
	if got := gw.messagesDroppedFull.Load(); got != full0 {
		t.Fatalf("messagesDroppedFull = %d, want %d (forward failure is not queue-full)", got, full0)
	}
}

// TestPipelineForwardSuccessCountsForwarded 成功路径转发计数。
func TestPipelineForwardSuccessCountsForwarded(t *testing.T) {
	gw := newGateway(minimalConfig())
	gw.filterChain = types.NewFilterChain()

	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "real-user")
	gw.connectionManager.SetConnectionServerID(connID, "logic:zone:1")
	connObj := gw.connectionManager.GetConnection(connID)
	if connObj == nil {
		t.Fatal("connection not found")
	}
	connObj.SetCachedLogicClient(fakeLogicClient{})

	fwd0 := gw.messagesForwarded.Load()
	failed0 := gw.messagesFailed.Load()

	msg := &protoGw.StreamData{Cmd: 1100010, Data: []byte("x"), SeqId: 1}
	res := gw.pipeline.Process(&kickMockConn{}, msg.Data, msg, connID)

	if res.Error != nil || res.ProtoMsg == nil {
		t.Fatalf("forward failed: %v", res.Error)
	}
	if got := gw.messagesForwarded.Load(); got != fwd0+1 {
		t.Fatalf("messagesForwarded = %d, want %d", got, fwd0+1)
	}
	if got := gw.messagesFailed.Load(); got != failed0 {
		t.Fatalf("messagesFailed = %d, want %d", got, failed0)
	}
}
