package backend

import (
	"testing"
	"time"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
)

// fakeGateway 满足 NewLogicClient 所需的最小 GatewayInterface。
type fakeGateway struct{}

func (fakeGateway) GetConnectionManager() *connection.ConnectionManager { return nil }
func (fakeGateway) GetStreamConfig() config.StreamConfig                { return config.StreamConfig{} }
func (fakeGateway) GetGRPCConfig() config.GRPCConfig                    { return config.GRPCConfig{} }
func (fakeGateway) GetGatewayID() string                                { return "gw-test" }
func (fakeGateway) GetServerID() string                                 { return "gw-test" }
func (fakeGateway) AddPushedToClient(int64)                             {}
func (fakeGateway) AddPushDroppedNoConn(int64)                          {}
func (fakeGateway) GetLogicClient(string) connection.LogicClientProvider {
	return nil
}
func (fakeGateway) GetShardedCoalescer() *connection.ShardedWriteCoalescer { return nil }
func (fakeGateway) KickByAccounts([]string, string) int                    { return 0 }
func (fakeGateway) KickByIP(string, string) int                            { return 0 }

// TestLogicClientStateTransitions：attach 全部分片 → Connected；detach 一片 → Disconnected。
func TestLogicClientStateTransitions(t *testing.T) {
	lc := NewLogicClient(nil)
	lc.initAccepted(2)
	lc.SetServerID("logic-1")

	if got := lc.getState(); got != LogicStateDisconnected {
		t.Fatalf("initial state = %v, want Disconnected", got)
	}

	fs0 := &fakeShardStream{}
	fs1 := &fakeShardStream{}
	if err := lc.attachShard(0, fs0); err != nil {
		t.Fatalf("attach 0: %v", err)
	}
	if got := lc.getState(); got != LogicStateDisconnected {
		t.Fatalf("after 1/2 attach state = %v, want Disconnected (partial)", got)
	}
	if err := lc.attachShard(1, fs1); err != nil {
		t.Fatalf("attach 1: %v", err)
	}
	if got := lc.getState(); got != LogicStateConnected {
		t.Fatalf("after full attach state = %v, want Connected", got)
	}

	// detach 一片 → 断开
	sm := lc.streamManager.Load()
	lc.detachShard(sm.shards[0], fs0)
	if got := lc.getState(); got != LogicStateDisconnected {
		t.Fatalf("after detach state = %v, want Disconnected", got)
	}

	// detach 幂等：同一流再 detach 不改变计数/状态
	lc.detachShard(sm.shards[0], fs0)
	if got := lc.getState(); got != LogicStateDisconnected {
		t.Fatalf("after double detach state = %v, want Disconnected", got)
	}

	// 重拨替换流：attached 不变，仍 Connected 路径
	if err := lc.attachShard(0, fs0); err != nil {
		t.Fatalf("reattach 0: %v", err)
	}
	// 此时 1 片在（fs1）+ 0 片新流 = 2 → Connected
	if got := lc.getState(); got != LogicStateConnected {
		t.Fatalf("after reattach state = %v, want Connected", got)
	}
	// 替换已有分片的流：计数不变
	fs0b := &fakeShardStream{}
	if err := lc.attachShard(0, fs0b); err != nil {
		t.Fatalf("replace stream: %v", err)
	}
	if n := lc.attached.Load(); n != 2 {
		t.Fatalf("attached after replace = %d, want 2", n)
	}
}

// TestSendMessageDisconnectedEnqueues：断线时 SendMessage 入缓存队列。
func TestSendMessageDisconnectedEnqueues(t *testing.T) {
	lc := NewLogicClient(fakeGateway{})
	lc.initAccepted(1)

	msg := &protoGw.StreamData{Cmd: 42, SessionId: "s1"}
	if err := lc.SendMessage(msg); err != nil {
		t.Fatalf("SendMessage while disconnected: %v", err)
	}
	if _, ok := lc.messageQueue.Dequeue(); !ok {
		t.Fatal("expected message enqueued while disconnected")
	}
}

// TestQueuePolicies：drop 策略满队丢最旧；block 策略 Close 唤醒失败。
func TestQueuePolicies(t *testing.T) {
	mq := NewStreamMessageQueue(config.StreamQueueConfig{
		MaxSize: 2,
		Policy:  config.QueuePolicyDrop,
	})
	// 默认/未知策略 = drop：满则丢最旧
	for i := 0; i < 3; i++ {
		if err := mq.Enqueue(&protoGw.StreamData{Cmd: int32(i)}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if n := len(mq.queue); n != 2 {
		t.Fatalf("queue len = %d, want 2 (drop oldest)", n)
	}
	// 队头应是 cmd=1（0 被丢）
	if m, ok := mq.Dequeue(); !ok || m.Cmd != 1 {
		t.Fatalf("head = %+v, want cmd=1", m)
	}

	// block 策略 + Close：入队者应快速失败而非永久挂起
	mqb := NewStreamMessageQueue(config.StreamQueueConfig{
		MaxSize:      1,
		Policy:       config.QueuePolicyBlock,
		BlockTimeout: "100ms",
	})
	if err := mqb.Enqueue(&protoGw.StreamData{Cmd: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- mqb.Enqueue(&protoGw.StreamData{Cmd: 2}) // 队满阻塞
	}()
	time.Sleep(20 * time.Millisecond)
	mqb.Close()
	select {
	case err := <-done:
		if err != ErrConnectionClosing {
			t.Fatalf("block enqueue after close = %v, want ErrConnectionClosing", err)
		}
	case <-time.After(time.Second):
		t.Fatal("block enqueue did not wake up after Close")
	}
}
