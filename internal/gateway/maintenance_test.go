package gateway

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	protoLogic "github.com/streasure/protocol/logic"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/routes"
	"google.golang.org/protobuf/proto"
)

// loginReqFrameFor 构造指定 serverId 的 LoginGate 帧（loginReqFrame 默认 logic:zone:1）。
func loginReqFrameFor(t *testing.T, serverID string) *protoGw.StreamData {
	t.Helper()
	body, err := proto.Marshal(&protoGw.LoginGateReq{ServerId: serverID, UserId: "u1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &protoGw.StreamData{Cmd: routes.CmdLoginGate, SeqId: 1, Data: body}
}

// kickMockConn 捕获踢人通知帧（Connection.Send → AsyncWritev）并记录关闭。
type kickMockConn struct {
	gnet.Conn
	written []byte
	closed  bool
}

func (c *kickMockConn) AsyncWritev(chunks [][]byte, _ gnet.AsyncCallback) error {
	for _, ch := range chunks {
		c.written = append(c.written, ch...)
	}
	return nil
}

func (c *kickMockConn) Close() error { c.closed = true; return nil }
func (c *kickMockConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

// decodeKickNotify 从 4B 长度 + 帧头输出中解析 CMD_KICK_NOTIFY 的 logic.KickNotify。
func decodeKickNotify(t *testing.T, b []byte) *protoLogic.KickNotify {
	t.Helper()
	if len(b) < 4 {
		return nil
	}
	n := binary.BigEndian.Uint32(b[:4])
	if int(n)+4 > len(b) {
		return nil
	}
	msg, ok := routes.DecodeClientMessage(b[4 : 4+n])
	if !ok || msg.Cmd != routes.CmdKickNotify || len(msg.Data) == 0 {
		return nil
	}
	kn := &protoLogic.KickNotify{}
	if err := proto.Unmarshal(msg.Data, kn); err != nil {
		return nil
	}
	return kn
}

func maintenanceConfig() *config.Config {
	cfg := minimalConfig()
	cfg.Admin.Token = "secret"
	return cfg
}

func maintenanceReq(action, serverID, zone, reason string) *http.Request {
	body, _ := json.Marshal(maintenanceRequest{Action: action, ServerID: serverID, Zone: zone, Reason: reason})
	r := httptest.NewRequest(http.MethodPost, "/admin/maintenance", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	return r
}

func decodeMaintenanceResp(t *testing.T, rec *httptest.ResponseRecorder) maintenanceResponse {
	t.Helper()
	var resp maintenanceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// TestMaintenanceStateEnableDisable 标记的增删与命中判断。
func TestMaintenanceStateEnableDisable(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	if gw.isMaintenanceTarget("s1") {
		t.Fatal("should not be maintenance before enable")
	}
	gw.maintenance.enable("s1", "", "upgrade")
	if !gw.isMaintenanceTarget("s1") {
		t.Fatal("serverId marked should hit")
	}
	if !gw.maintenance.disable("s1", "") {
		t.Fatal("disable should report removed")
	}
	if gw.maintenance.disable("s1", "") {
		t.Fatal("second disable should report not removed")
	}
	if gw.isMaintenanceTarget("s1") {
		t.Fatal("should not hit after disable")
	}
}

// TestMaintenance_ZoneFallback zone 标记通过网关自身 zone 命中（静态/无发现回退）。
func TestMaintenance_ZoneFallback(t *testing.T) {
	gw := newGateway(minimalConfig()) // Zone=default，logicClientPool 为 nil
	defer gw.Close()

	gw.maintenance.enable("", "default", "zone maintenance")
	defer gw.maintenance.disable("", "default")

	if !gw.isMaintenanceTarget("logic:any") {
		t.Fatal("server with fallback zone=default should hit zone mark")
	}
	if gw.isMaintenanceTarget("") {
		t.Fatal("empty serverId should never hit")
	}
}

// TestLoginGate_RejectInMaintenance 维护中的 serverId 拒绝新登录（503 + 关闭）。
func TestLoginGate_RejectInMaintenance(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	gw.maintenance.enable("logic:zone:1", "", "upgrade")
	defer gw.maintenance.disable("logic:zone:1", "")

	c := &loginMockConn{}
	action := gw.handleLoginGate(c, "conn-maint", loginReqFrame(t)) // ServerId=logic:zone:1
	if action != gnet.Close {
		t.Fatalf("action=%v want gnet.Close", action)
	}
	ack := decodeAck(t, c.written)
	if ack == nil || ack.Code != 503 {
		t.Fatalf("ack=%v want code 503", ack)
	}
}

// TestKickForMaintenance 踢人链路：KickNotify 帧推送 + 连接关闭。
func TestKickForMaintenance(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	mc := &kickMockConn{}
	connID := gw.connectionManager.AddConnection(mc, "u1")
	defer gw.connectionManager.RemoveConnection(connID) // 否则 gw.Close() drain 等待 2 分钟
	gw.connectionManager.SetConnectionServerID(connID, "s1")
	conn := gw.connectionManager.GetConnection(connID)
	if conn == nil {
		t.Fatal("setup: connection missing")
	}

	gw.kickForMaintenance(conn, "upgrade")

	kn := decodeKickNotify(t, mc.written)
	if kn == nil || kn.Reason != "upgrade" || kn.Code != 503 {
		t.Fatalf("kickNotify=%v want reason=upgrade code=503", kn)
	}
	if !mc.closed {
		t.Fatal("connection should be closed")
	}
}

// TestSnapshotMaintenanceTargets serverId 精确匹配与 zone 匹配。
func TestSnapshotMaintenanceTargets(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	add := func(user, server string) string {
		id := gw.connectionManager.AddConnection(&kickMockConn{}, user)
		gw.connectionManager.SetConnectionServerID(id, server)
		return id
	}
	var ids []string
	for _, sv := range []struct{ u, s string }{{"u1", "s1"}, {"u2", "s1"}, {"u3", "s2"}, {"u4", ""}} {
		ids = append(ids, add(sv.u, sv.s))
	}
	defer func() { // 否则 gw.Close() drain 等待 2 分钟
		for _, id := range ids {
			gw.connectionManager.RemoveConnection(id)
		}
	}()

	if got := gw.snapshotMaintenanceTargets("s1", ""); len(got) != 2 {
		t.Fatalf("serverId match=%d want 2", len(got))
	}
	// zone=default：s1/s2 均回退网关 zone（无发现记录）
	if got := gw.snapshotMaintenanceTargets("", "default"); len(got) != 3 {
		t.Fatalf("zone match=%d want 3", len(got))
	}
	if got := gw.snapshotMaintenanceTargets("", "other-zone"); len(got) != 0 {
		t.Fatalf("unknown zone match=%d want 0", len(got))
	}
}

// TestHandleAdminMaintenance HTTP 端到端：enable 标记 + matched 计数 + 登录被拒 → disable 恢复。
func TestHandleAdminMaintenance(t *testing.T) {
	gw := newGateway(maintenanceConfig())
	defer gw.Close()

	// 两条在线连接绑到 s1，一条绑到 s2
	s1Mocks := []*kickMockConn{{}, {}}
	var allIDs []string
	for _, mc := range s1Mocks {
		id := gw.connectionManager.AddConnection(mc, "u1")
		gw.connectionManager.SetConnectionServerID(id, "s1")
		allIDs = append(allIDs, id)
	}
	id := gw.connectionManager.AddConnection(&kickMockConn{}, "u3")
	gw.connectionManager.SetConnectionServerID(id, "s2")
	allIDs = append(allIDs, id)
	defer func() { // 否则 gw.Close() drain 等待 2 分钟
		for _, cid := range allIDs {
			gw.connectionManager.RemoveConnection(cid)
		}
	}()

	// enable by serverId
	rec := httptest.NewRecorder()
	gw.handleAdminMaintenance(rec, maintenanceReq("enable", "s1", "", "upgrade"))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeMaintenanceResp(t, rec)
	if !resp.OK || !resp.Accepted || resp.Matched != 2 {
		t.Fatalf("resp=%+v want ok accepted matched=2", resp)
	}
	if !gw.isMaintenanceTarget("s1") || gw.isMaintenanceTarget("s2") {
		t.Fatal("mark should hit s1 only")
	}

	// 维护中登录 s1 → 503
	c := &loginMockConn{}
	if action := gw.handleLoginGate(c, "conn-m1", loginReqFrameFor(t, "s1")); action != gnet.Close {
		t.Fatalf("maintenance login action=%v want close", action)
	}
	if ack := decodeAck(t, c.written); ack == nil || ack.Code != 503 {
		t.Fatalf("ack=%v want 503", ack)
	}

	// 等待异步踢人完成（无 gnet 事件循环，OnClose 不触发；断言 kick 帧 + 连接关闭）
	for i := 0; i < 100 && !(s1Mocks[0].closed && s1Mocks[1].closed); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	for i, mc := range s1Mocks {
		if !mc.closed {
			t.Fatalf("s1 conn %d should be closed by async kick", i)
		}
		kn := decodeKickNotify(t, mc.written)
		if kn == nil || kn.Reason != "upgrade" {
			t.Fatalf("s1 conn %d kickNotify=%v want reason=upgrade", i, kn)
		}
	}

	// disable 后恢复登录检查
	rec = httptest.NewRecorder()
	gw.handleAdminMaintenance(rec, maintenanceReq("disable", "s1", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d", rec.Code)
	}
	resp = decodeMaintenanceResp(t, rec)
	if !resp.Removed {
		t.Fatalf("resp=%+v want removed", resp)
	}
	if gw.isMaintenanceTarget("s1") {
		t.Fatal("s1 should not be maintenance after disable")
	}
}

// TestHandleAdminMaintenance_Validation 参数校验：方法/动作/单选。
func TestHandleAdminMaintenance_Validation(t *testing.T) {
	gw := newGateway(maintenanceConfig())
	defer gw.Close()

	// GET → 405
	rec := httptest.NewRecorder()
	gw.handleAdminMaintenance(rec, httptest.NewRequest(http.MethodGet, "/admin/maintenance", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d want 405", rec.Code)
	}

	// serverId 与 zone 同时给 → 400
	rec = httptest.NewRecorder()
	gw.handleAdminMaintenance(rec, maintenanceReq("enable", "s1", "z1", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("both params status=%d want 400", rec.Code)
	}

	// 非法 action → 400
	rec = httptest.NewRecorder()
	gw.handleAdminMaintenance(rec, maintenanceReq("pause", "s1", "", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action status=%d want 400", rec.Code)
	}

	// 未授权 → 401
	body, _ := json.Marshal(maintenanceRequest{Action: "enable", ServerID: "s1"})
	r := httptest.NewRequest(http.MethodPost, "/admin/maintenance", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	gw.handleAdminMaintenance(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth status=%d want 401", rec.Code)
	}
}
