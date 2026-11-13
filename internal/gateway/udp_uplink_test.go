package gateway

import (
	"encoding/binary"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"google.golang.org/protobuf/proto"
)

// udpDatagramSeq 构造带 SeqId 的 UDP 数据报（阶段 2 seq 去重用）。
func udpDatagramSeq(t *testing.T, cmd int32, body []byte, seq int64) []byte {
	t.Helper()
	frame, err := protoGw.MarshalClientMessage(&protoGw.StreamData{Cmd: cmd, Data: body, SeqId: seq})
	if err != nil {
		t.Fatalf("marshal client message: %v", err)
	}
	buf := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(buf, uint32(len(frame)))
	copy(buf[4:], frame)
	return buf
}

// findErrorFrame 在 written 字节流（可能混有 ASSOCIATE Ack）中查找 CmdError 帧，
// 返回其错误消息。尾部不完整帧（并发写进行中）安全跳过。
func findErrorFrame(b []byte) (string, bool) {
	off := 0
	for off+4 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[off:]))
		if n <= 0 || off+4+n > len(b) {
			return "", false
		}
		msg, ok := protoGw.DecodeClientMessage(b[off+4 : off+4+n])
		if ok {
			if msg.Cmd == protoGw.CmdError {
				var resp commonstruct.ErrorResponse
				err := proto.Unmarshal(msg.Data, &resp)
				protoGw.PutClientMessage(msg)
				if err == nil && resp.Error != nil {
					return resp.Error.Message, true
				}
				return "", true
			}
			protoGw.PutClientMessage(msg)
		}
		off += 4 + n
	}
	return "", false
}

// captureLogicClient 记录转发到 logic 的消息（只存值，避免持有池化对象）。
type captureLogicClient struct {
	count     atomic.Int64
	cmd       atomic.Int32
	sessionID atomic.Value // string
}

func (c *captureLogicClient) IsConnected() bool { return true }

func (c *captureLogicClient) SendMessage(m *protoGw.StreamData) error {
	c.count.Add(1)
	c.cmd.Store(m.Cmd)
	c.sessionID.Store(m.SessionId)
	return nil
}

func (c *captureLogicClient) sid() string {
	v, _ := c.sessionID.Load().(string)
	return v
}

// waitFor 轮询等待异步条件（worker pool 异步处理）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// setupUDPAssoc 注册会话并走完整 token → ASSOCIATE 流程建立关联。
// bind=true 时绑定 serverID（已登录态，可过 pipeline 鉴权）。
// 调用方负责在 gw.Close() 之前 RemoveConnection（defer 顺序：Close 先注册）。
func setupUDPAssoc(t *testing.T, gw *Gateway, ip net.IP, port int, bind bool) (string, *udpMockConn) {
	t.Helper()
	connID := gw.connectionManager.AddConnection(&kickMockConn{}, "user-"+ip.String(), false)
	if bind {
		gw.connectionManager.SetConnectionServerID(connID, "logic:zone:1")
	}
	token, _, ok := gw.issueUDPToken(connID)
	if !ok {
		t.Fatal("issueUDPToken failed")
	}
	c := &udpMockConn{addr: &net.UDPAddr{IP: ip, Port: port}}
	gw.handleUDPDatagram(c, udpDatagram(t, protoGw.CmdUdpAssociate, associateBody(t, token)))
	if c.writtenLen() == 0 {
		t.Fatal("associate ack not written")
	}
	return connID, c
}

// TestUDPUplinkForwardThroughPipeline 上行走通：关联地址的业务消息以真实 WS connID
// 进入现有 pipeline，鉴权通过后转发到该会话的 logic，消息归属 connID 不串号。
func TestUDPUplinkForwardThroughPipeline(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 60), 10000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	connObj := gw.connectionManager.GetConnection(connID)
	if connObj == nil {
		t.Fatal("connection not found")
	}
	spy := &captureLogicClient{}
	connObj.SetCachedLogicClient(spy)

	// 活性时间拨旧，验证收包刷新 lastSeen
	assoc, found := gw.udpAssoc.lookupByAddr(c.addr.String())
	if !found {
		t.Fatal("assoc not found")
	}
	assoc.lastSeen.Store(1)

	recv0 := gw.messagesReceived.Load()
	fwd0 := gw.messagesForwarded.Load()

	// 上行不校验号段（规则③）：UDP 号段业务命令
	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("move"), 777))

	if got := spy.count.Load(); got != 1 {
		t.Fatalf("forwarded to logic = %d, want 1 (sync path)", got)
	}
	if got := spy.cmd.Load(); got != 1200010 {
		t.Fatalf("forwarded cmd = %d, want 1200010", got)
	}
	if got := spy.sid(); got != connID {
		t.Fatalf("forwarded sessionID = %q, want real connID %q (不得串号)", got, connID)
	}
	if got := gw.messagesForwarded.Load(); got != fwd0+1 {
		t.Fatalf("messagesForwarded = %d, want %d", got, fwd0+1)
	}
	if got := gw.messagesReceived.Load(); got != recv0+1 {
		t.Fatalf("messagesReceived = %d, want %d (ASSOCIATE 后首个业务包)", got, recv0+1)
	}
	if got := assoc.lastSeen.Load(); got <= 1 {
		t.Fatalf("lastSeen = %d, want refreshed by uplink", got)
	}
}

// TestUDPUplinkSessionIsolation 两会话各自关联：addr1 的包只进会话1 的 logic，
// addr2 的包只进会话2 的 logic（pipelineTask 按反查结果携带真实 connID，不串号）。
func TestUDPUplinkSessionIsolation(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID1, c1 := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 61), 11000, true)
	defer gw.connectionManager.RemoveConnection(connID1)
	connID2, c2 := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 62), 11001, true)
	defer gw.connectionManager.RemoveConnection(connID2)

	spy1 := &captureLogicClient{}
	gw.connectionManager.GetConnection(connID1).SetCachedLogicClient(spy1)
	spy2 := &captureLogicClient{}
	gw.connectionManager.GetConnection(connID2).SetCachedLogicClient(spy2)

	gw.handleUDPDatagram(c1, udpDatagramSeq(t, 1200010, []byte("a"), 1))
	gw.handleUDPDatagram(c2, udpDatagramSeq(t, 1200010, []byte("b"), 1))

	if spy1.count.Load() != 1 || spy1.sid() != connID1 {
		t.Fatalf("spy1 count=%d sid=%q, want 1/%q", spy1.count.Load(), spy1.sid(), connID1)
	}
	if spy2.count.Load() != 1 || spy2.sid() != connID2 {
		t.Fatalf("spy2 count=%d sid=%q, want 1/%q", spy2.count.Load(), spy2.sid(), connID2)
	}
}

// TestUDPUplinkAsyncWorkerPool 异步路径：任务经 pipelineWorkerPool.Submit 分片投递，
// worker 内以 isUDP 任务完成 pipeline 处理。
func TestUDPUplinkAsyncWorkerPool(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()
	// 池由 gw.Close() 停止（closeBody 内已调 Stop，测试不得重复 Stop 否则二次 close）
	gw.pipelineWorkerPool = NewPipelineWorkerPool(gw, config.PipelineConfig{})

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 63), 12000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	spy := &captureLogicClient{}
	gw.connectionManager.GetConnection(connID).SetCachedLogicClient(spy)

	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("async"), 1))
	waitFor(t, "async forward", func() bool { return spy.count.Load() == 1 })
	if got := spy.sid(); got != connID {
		t.Fatalf("async sessionID = %q, want %q", got, connID)
	}
}

// TestUDPUplinkAuthErrorBackToUDP 未绑定会话（未登录）的业务消息：pipeline 鉴权拒绝，
// 错误帧跟随请求通道回 UDP（错误帧跟随请求通道，不参与号段路由，规则②）。
func TestUDPUplinkAuthErrorBackToUDP(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 64), 13000, false) // 不绑定 serverID
	defer gw.connectionManager.RemoveConnection(connID)
	// 回拨创建时间越过登录宽限期（宽限期内只丢弃不报错）
	connObj := gw.connectionManager.GetConnection(connID)
	connObj.CreatedAt = time.Now().Add(-time.Minute).UnixMilli()

	mark := c.writtenLen()
	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("x"), 1))

	written := c.writtenCopy()
	if len(written) <= mark {
		t.Fatal("no error frame written back to UDP")
	}
	msg, ok := findErrorFrame(written[mark:])
	if !ok {
		t.Fatal("no CmdError frame after business datagram")
	}
	if !strings.Contains(msg, "not bound") {
		t.Fatalf("error message = %q, want contains 'not bound'", msg)
	}
	if got := c.sentToCopy(); got == nil || got.String() != c.addr.String() {
		t.Fatalf("error frame target = %v, want assoc addr %v", got, c.addr)
	}
}

// TestUDPUplinkErrorFrameAsyncPath 异步 worker 的 UDP 错误回程
// （processMessage 的 isUDP 分支 → sendUDPFrame）。
func TestUDPUplinkErrorFrameAsyncPath(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()
	gw.pipelineWorkerPool = NewPipelineWorkerPool(gw, config.PipelineConfig{})

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 65), 14000, false)
	defer gw.connectionManager.RemoveConnection(connID)
	gw.connectionManager.GetConnection(connID).CreatedAt = time.Now().Add(-time.Minute).UnixMilli()

	mark := c.writtenLen()
	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("x"), 1))

	waitFor(t, "async error frame", func() bool {
		_, ok := findErrorFrame(c.writtenCopy()[mark:])
		return ok
	})
}

// TestUDPUplinkConnRateLimit 连接级流控生效：超 protection.maxMessagesPerConn 的
// UDP 上行被 pipeline 拒绝并回错误帧（限流走 pipeline 统一路径，不特判 UDP）。
// 限流窗口恰好在两条之间滚动的极小概率用第三条兜底，保证断言确定性。
func TestUDPUplinkConnRateLimit(t *testing.T) {
	cfg := udpEnabledConfig()
	cfg.Protection.MaxMessagesPerConn = 1
	gw := newTestGateway(cfg)
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 66), 15000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	spy := &captureLogicClient{}
	gw.connectionManager.GetConnection(connID).SetCachedLogicClient(spy)

	mark := c.writtenLen()
	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("1"), 1)) // 窗口内首条：放行
	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("2"), 2))
	if spy.count.Load() == 1 {
		// 第二条被限流（常态）
	} else {
		// 限流窗口恰好在两条之间滚动：第三条必在同窗被拒
		gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("3"), 3))
		if got := spy.count.Load(); got != 2 {
			t.Fatalf("forwarded = %d, want 2 (首条+滚动后一条)", got)
		}
	}
	msg, ok := findErrorFrame(c.writtenCopy()[mark:])
	if !ok {
		t.Fatal("no rate limit error frame written back to UDP")
	}
	if !strings.Contains(msg, "rate limit") {
		t.Fatalf("error message = %q, want contains 'rate limit'", msg)
	}
}

// TestUDPSeqDedupWindow seq 滑动窗口去重单元：首包定基、重复丢、窗口内未见过放行、
// 窗口外旧包丢、前进移位后重放仍丢、大跳清窗、seq<=0 不参与去重。
func TestUDPSeqDedupWindow(t *testing.T) {
	const w = 1024
	a := &udpAssociation{}

	mustPass := func(seq int64, what string) {
		t.Helper()
		if a.seqDuplicate(seq, w) {
			t.Fatalf("seq=%d dropped, want pass (%s)", seq, what)
		}
	}
	mustDrop := func(seq int64, what string) {
		t.Helper()
		if !a.seqDuplicate(seq, w) {
			t.Fatalf("seq=%d accepted, want drop (%s)", seq, what)
		}
	}

	mustPass(100000, "首包定基")
	mustDrop(100000, "重复")
	mustPass(99998, "窗口内乱序未见过")
	mustDrop(99998, "再次乱序 = 重复")
	mustPass(99999, "窗口内补缺")
	mustDrop(95000, "窗口外旧包（gap 5000 >= 1024）")

	mustPass(100100, "前进 100")
	mustDrop(100000, "前进后重放旧包（移位仍标记）")
	mustPass(101024, "前进到窗口边界")
	mustDrop(100000, "边界外旧包（gap 1024 >= 1024）")

	mustPass(200000, "大跳清窗")
	mustDrop(101024, "清窗后旧包一律丢")

	// seq<=0：客户端未携带序号，不参与去重（向后兼容）
	b := &udpAssociation{}
	if b.seqDuplicate(0, w) {
		t.Fatal("seq=0 dropped, want pass (no-seq)")
	}
	if b.seqDuplicate(-5, w) {
		t.Fatal("seq=-5 dropped, want pass (no-seq)")
	}
}

// TestUDPSeqDedupEndToEnd 端到端：重复 SeqId 的数据报不进 pipeline，
// 不同 SeqId 正常转发。
func TestUDPSeqDedupEndToEnd(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 67), 16000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	spy := &captureLogicClient{}
	gw.connectionManager.GetConnection(connID).SetCachedLogicClient(spy)

	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("a"), 500))
	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("a"), 500)) // 重放
	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("b"), 501))

	if got := spy.count.Load(); got != 2 {
		t.Fatalf("forwarded = %d, want 2 (replay dropped)", got)
	}
}

// TestUDPUplinkMTU 单数据报上限：超 udp.maxDatagram 的包静默丢
// （不转发、不回包），限内的正常转发。
func TestUDPUplinkMTU(t *testing.T) {
	cfg := udpEnabledConfig()
	cfg.UDP.MaxDatagram = 100
	gw := newTestGateway(cfg)
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 68), 17000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	spy := &captureLogicClient{}
	gw.connectionManager.GetConnection(connID).SetCachedLogicClient(spy)

	mark := c.writtenLen()
	big := udpDatagramSeq(t, 1200010, make([]byte, 200), 1)
	if len(big) <= 100 {
		t.Fatalf("test setup: big datagram len=%d, want > 100", len(big))
	}
	gw.handleUDPDatagram(c, big)
	if got := spy.count.Load(); got != 0 {
		t.Fatalf("oversize datagram forwarded = %d, want 0", got)
	}
	if got := c.writtenLen(); got != mark {
		t.Fatalf("oversize datagram got %d reply bytes, want silent", got-mark)
	}

	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("ok"), 2))
	if got := spy.count.Load(); got != 1 {
		t.Fatalf("in-limit datagram forwarded = %d, want 1", got)
	}
}

// TestUDPUnassociatedDropCounter 未关联源地址丢包计数（§8.4 埋点：源地址漂移旁证）。
func TestUDPUnassociatedDropCounter(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	c := &udpMockConn{addr: &net.UDPAddr{IP: net.IPv4(203, 0, 113, 69), Port: 18000}}
	before := gw.udpAssocUnassociatedDrops.Load()

	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("x"), 1))

	if got := gw.udpAssocUnassociatedDrops.Load(); got != before+1 {
		t.Fatalf("unassociatedDrops = %d, want %d", got, before+1)
	}
	if c.writtenLen() != 0 {
		t.Fatalf("unassociated got %d reply bytes, want silent", c.writtenLen())
	}
}

// TestUDPSweepWSAliveMetric §8.4 埋点：关联被空闲淘汰时 WS 会话仍存活 → 计数
// （stale 关联缺口签名）；会话已死的淘汰不计。
func TestUDPSweepWSAliveMetric(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	idle := config.DefaultUDPIdleDuration
	now := time.Now().UnixMilli()

	// 活会话（在 connectionManager 中）
	connID, _ := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 70), 19000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	// 死会话（关联残留但会话已不在 manager —— OnClose 前的竞态窗口）
	addr2 := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 71), Port: 19001}
	if a := gw.udpAssoc.create("ghost-conn", addr2, &udpMockConn{addr: addr2}, "t2", now, 4, 100); a == nil {
		t.Fatal("create ghost assoc failed")
	}

	// 两个关联都拨到空闲淘汰线之外
	mine, _ := gw.udpAssoc.lookupByAddr((&net.UDPAddr{IP: net.IPv4(203, 0, 113, 70), Port: 19000}).String())
	if mine == nil {
		t.Fatal("assoc not found")
	}
	stale := now - idle.Milliseconds() - 1
	mine.lastSeen.Store(stale)
	ghost, _ := gw.udpAssoc.lookupByAddr(addr2.String())
	ghost.lastSeen.Store(stale)

	before := gw.udpAssocSweptWSSurviving.Load()
	removed := gw.udpAssocSweepOnce(now, idle)
	if removed != 2 {
		t.Fatalf("sweep removed = %d, want 2", removed)
	}
	if got := gw.udpAssocSweptWSSurviving.Load(); got != before+1 {
		t.Fatalf("sweptWSSurviving = %d, want %d (只计 WS 会话仍存活的淘汰)", got, before+1)
	}
}

// TestUDPUplinkDoesNotTouchSessionActivity UDP 上行不刷新会话活跃时间——
// UDP 不参与会话存活判据（设计 §5.7），存活只由 WS 心跳/入站决定。
func TestUDPUplinkDoesNotTouchSessionActivity(t *testing.T) {
	gw := newTestGateway(udpEnabledConfig())
	defer gw.Close()

	connID, c := setupUDPAssoc(t, gw, net.IPv4(203, 0, 113, 72), 20000, true)
	defer gw.connectionManager.RemoveConnection(connID)

	connObj := gw.connectionManager.GetConnection(connID)
	connObj.LastActive.Store(12345)

	gw.handleUDPDatagram(c, udpDatagramSeq(t, 1200010, []byte("x"), 1))

	if got := connObj.LastActive.Load(); got != 12345 {
		t.Fatalf("LastActive = %d, want 12345 (UDP 不得刷新会话活性)", got)
	}
}
