package connection

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
)

// mockConn 通过嵌入 gnet.Conn 接口满足桩实现，仅覆写被测代码实际调用的方法。
type mockConn struct {
	gnet.Conn
}

func (mockConn) RemoteAddr() net.Addr { return nil }

// TestShardedMapRange_CallbackCanDelete 验证 Range 在回调中删除自身 key 不死锁。
// 历史 bug：Range 持 shard 锁调用回调，回调 RemoveConnection → Delete 同一把锁 → 死锁。
func TestShardedMapRange_CallbackCanDelete(t *testing.T) {
	m := newShardedMap[string]()
	for i := 0; i < 64; i++ {
		m.Store(string(rune('a'+i%26))+string(rune('0'+i/26)), "v")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Range(func(k, v string) bool {
			m.Delete(k) // 回调中删除自身 — 修复前会自死锁
			return true
		})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shardedMap.Range deadlocked when callback deleted own key")
	}
	if m.Count() != 0 {
		t.Fatalf("count=%d want 0", m.Count())
	}
}

// TestRemoveConnection_Idempotent 验证并发/重复 RemoveConnection 只清理一次计数。
func TestRemoveConnection_Idempotent(t *testing.T) {
	cm := NewConnectionManager(0, 0)
	conn := cm.AddConnection(mockConn{}, "user-1")
	if cm.GetConnectionCount() != 1 {
		t.Fatalf("count=%d want 1", cm.GetConnectionCount())
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cm.RemoveConnection(conn)
		}()
	}
	wg.Wait()

	if got := cm.GetConnectionCount(); got != 0 {
		t.Fatalf("count=%d want 0 (double decrement on concurrent Remove)", got)
	}
}

// TestUpdateConnectionUserUUID_CleansOrphanServerUserKey 验证换绑清理旧 serverUser 键。
func TestUpdateConnectionUserUUID_CleansOrphanServerUserKey(t *testing.T) {
	cm := NewConnectionManager(0, 0)
	connID := cm.AddConnection(mockConn{}, "temp")
	cm.UpdateConnectionUserUUID(connID, "old-user")
	if _, ok := cm.serverUserConnections.Load(serverUserKey{userUUID: "old-user"}); !ok {
		t.Fatal("old serverUser key should exist after first bind")
	}
	cm.UpdateConnectionUserUUID(connID, "new-user")
	if _, ok := cm.serverUserConnections.Load(serverUserKey{userUUID: "old-user"}); ok {
		t.Fatal("old serverUser key should be removed after rebinding")
	}
	if _, ok := cm.serverUserConnections.Load(serverUserKey{userUUID: "new-user"}); !ok {
		t.Fatal("new serverUser key should exist")
	}
}

// TestClaimOfflineNotify 验证下线通知 CAS 幂等。
func TestClaimOfflineNotify(t *testing.T) {
	c := newConnection("id", mockConn{}, "u", "1.2.3.4:1")
	if !c.ClaimOfflineNotify() {
		t.Fatal("first claim should win")
	}
	if c.ClaimOfflineNotify() {
		t.Fatal("second claim should lose")
	}
}
