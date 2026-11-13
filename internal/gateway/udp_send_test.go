package gateway

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// decodeUDPFrame 解析 sendUDPFrame 的输出（4B 长度头 + payload）。
func decodeUDPFrame(b []byte) ([]byte, bool) {
	if len(b) < 4 {
		return nil, false
	}
	n := int(binary.BigEndian.Uint32(b[:4]))
	if n <= 0 || 4+n > len(b) {
		return nil, false
	}
	return b[4 : 4+n], true
}

// TestSendUDP_NoAssociation 无关联 → errNoUDPAssoc（SendByCmd 据此回落 WS）。
func TestSendUDP_NoAssociation(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	if err := gw.SendUDP("no-such-session", []byte("x")); !errors.Is(err, errNoUDPAssoc) {
		t.Fatalf("err = %v, want errNoUDPAssoc", err)
	}
}

// TestSendUDP_StaleNotAttempted 关联存在但已 stale（超过 idleTimeout/2）
// → 返回错误且不尝试 SendTo（系统调用会成功、错误回不了头，形成静默黑洞）。
func TestSendUDP_StaleNotAttempted(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 70), 15000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	assoc, found := gw.udpAssoc.lookupByConnID(connID)
	if !found {
		t.Fatal("assoc not found")
	}
	// 拨旧到新鲜度窗口之外：阈值 = idle/2，配置缺省 60s → 30s，拨旧 40s
	idle := gw.udpIdleDuration()
	assoc.lastSeen.Store(time.Now().UnixMilli() - idle.Milliseconds()/2 - 10_000)

	before := c.writtenLen() // ASSOCIATE ack 已占用 written，只断言增量
	if err := gw.SendUDP(connID, []byte("stale")); !errors.Is(err, errNoUDPAssoc) {
		t.Fatalf("err = %v, want errNoUDPAssoc (stale)", err)
	}
	if c.writtenLen() != before {
		t.Fatalf("stale 不得尝试 SendTo, written %d -> %d", before, c.writtenLen())
	}
}

// TestSendUDP_FreshSendsToAssocAddr 关联新鲜 → 帧（4B 头 + payload）发往关联地址。
func TestSendUDP_FreshSendsToAssocAddr(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 71), 15001, true)
	defer gw.connectionManager.RemoveConnection(connID)

	payload := []byte("downlink-frame")
	off := c.writtenLen() // 跳过 ASSOCIATE ack
	if err := gw.SendUDP(connID, payload); err != nil {
		t.Fatalf("SendUDP: %v", err)
	}
	got, ok := decodeUDPFrame(c.writtenCopy()[off:])
	if !ok {
		t.Fatalf("frame decode failed, raw=%v", c.writtenCopy()[off:])
	}
	if string(got) != string(payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
	if c.sentToCopy().String() != (&net.UDPAddr{IP: net.IPv4(203, 0, 113, 71), Port: 15001}).String() {
		t.Errorf("sentTo = %v, want 203.0.113.71:15001", c.sentToCopy())
	}
}

// TestSendUDP_ConcurrentWithSweep -race 并发下行安全：多协程 SendUDP 与
// janitor sweep / 显式删除并发（无锁读 byConnID，删除窗口内返回已拆索引
// 关联只产生发送错误 → SendByCmd 回落，无 panic 无副作用）。
func TestSendUDP_ConcurrentWithSweep(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, _ := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 73), 15003, true)
	defer gw.connectionManager.RemoveConnection(connID)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 删除 + 重建循环：与并发 SendUDP 的「无锁读 → 新鲜度 → SendTo」
		// 三步构成持续竞态窗口（读到拆除中的关联 → 发送错误回落，不得 panic）。
		addr := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 73), Port: 15003}
		mc := &udpMockConn{addr: addr}
		for {
			select {
			case <-stop:
				return
			default:
			}
			gw.udpAssoc.deleteByConnID(connID)
			gw.udpAssoc.create(connID, addr, mc, "race-token", time.Now().UnixMilli(), 10, 100)
		}
	}()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				if err := gw.SendUDP(connID, []byte("race")); err != nil && !errors.Is(err, errNoUDPAssoc) {
					t.Errorf("SendUDP: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	<-done
}

// TestSendUDP_AssociateInjectsDownlink 端到端：ASSOCIATE 成功注入
// SetUDPDownlink 后，连接的 SendByCmd（号段内）走 UDP 发往关联地址，
// WS conn 零写入；关联删除后回落 WS（降级不丢）。
func TestSendUDP_AssociateInjectsDownlink(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 72), 15002, true)
	defer gw.connectionManager.RemoveConnection(connID)

	connObj := gw.connectionManager.GetConnection(connID)
	if connObj == nil {
		t.Fatal("connection not found")
	}

	// 号段内下行走 SendByCmd → 注入的 SendUDP
	frame := []byte("cmd-udp-payload")
	off := c.writtenLen() // 跳过 ASSOCIATE ack
	if err := connObj.SendByCmd(1200010, frame); err != nil {
		t.Fatalf("SendByCmd(udp): %v", err)
	}
	got, ok := decodeUDPFrame(c.writtenCopy()[off:])
	if !ok {
		t.Fatalf("udp frame decode failed, raw=%v", c.writtenCopy()[off:])
	}
	if string(got) != string(frame) {
		t.Errorf("udp payload = %q, want %q", got, frame)
	}

	// 关联删除 → SendUDP 返回错误 → 回落 WS（kickMockConn 捕获 AsyncWritev）
	gw.udpAssoc.deleteByConnID(connID)
	if err := connObj.SendByCmd(1200010, []byte("fallback")); err != nil {
		t.Fatalf("SendByCmd(回落): %v", err)
	}
	before := c.writtenLen()
	if err := connObj.SendByCmd(1200010, []byte("after-delete")); err != nil {
		t.Fatalf("SendByCmd(删除后): %v", err)
	}
	if c.writtenLen() != before {
		t.Errorf("删除后仍写 UDP: %d -> %d", before, c.writtenLen())
	}
	if len(connObj.Conn.(*kickMockConn).writtenCopy()) == 0 {
		t.Errorf("删除后必须回落 WS 写出")
	}
}
