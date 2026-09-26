package gateway

import (
	"testing"

	protoGw "github.com/streasure/protocol/gateway"
)

// TestIntegrity_PerConnectionKey 验证重放 key 含 connectionID：
// 旧实现用帧内 SessionId/UserKey（解码后恒为空），所有连接共享同一 key 会互丢消息。
func TestIntegrity_PerConnectionKey(t *testing.T) {
	mi := NewMessageIntegrity(60000)
	defer mi.Stop()

	mk := func(cmd int32, seq int64) *protoGw.StreamData {
		return &protoGw.StreamData{Cmd: cmd, SeqId: seq}
	}

	// 不同连接、相同 cmd+seq → 都应放行
	if err := mi.ProcessMessage("conn-A", mk(100, 1)); err != nil {
		t.Fatalf("conn-A first frame rejected: %v", err)
	}
	if err := mi.ProcessMessage("conn-B", mk(100, 1)); err != nil {
		t.Fatalf("conn-B identical frame rejected (shared key regression): %v", err)
	}

	// 同一连接重复 → 重放拒绝
	if err := mi.ProcessMessage("conn-A", mk(100, 1)); err == nil {
		t.Fatal("conn-A replay accepted")
	}
	// 同连接不同 seq → 放行
	if err := mi.ProcessMessage("conn-A", mk(100, 2)); err != nil {
		t.Fatalf("conn-A new seq rejected: %v", err)
	}
}
