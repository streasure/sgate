package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/sgate/internal/security"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// banStore 进程内封禁表。
// TODO: 迁移到 MySQL（多网关共享 + 持久化）。
var banStore = security.NewBanStore()

// BanStore 返回全局封禁表（测试/管理接口共用）。
func BanStore() *security.BanStore { return banStore }

type banRequest struct {
	UserUUID string `json:"userUuid"`
	Reason   string `json:"reason"`
	Jti      string `json:"jti"`
	TTLSec   int64  `json:"ttlSeconds"` // <=0 或缺省 = 永久
}

type unbanRequest struct {
	UserUUID string `json:"userUuid"`
}

type banResponse struct {
	OK      bool                `json:"ok"`
	Banned  int                 `json:"banned"`
	Record  *security.BanRecord `json:"record,omitempty"`
	Offline int                 `json:"offline"` // 本实例踢下的连接数
}

func (g *Gateway) adminAuthed(r *http.Request) bool {
	cfg, ok := g.cfg.Load().(*config.Config)
	if !ok || cfg == nil {
		return false
	}
	token := cfg.Admin.Token
	if token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	return auth[len(prefix):] == token
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// registerAdminRoutes 在 stats mux 上挂载管理端封禁接口。
func (g *Gateway) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/ban", g.handleAdminBan)
	mux.HandleFunc("/admin/unban", g.handleAdminUnban)
	mux.HandleFunc("/admin/bans", g.handleAdminBans)
}

func (g *Gateway) handleAdminBan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !g.adminAuthed(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req banRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserUUID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "userUuid required"})
		return
	}
	var ttl time.Duration
	if req.TTLSec > 0 {
		ttl = time.Duration(req.TTLSec) * time.Second
	}
	rec := banStore.Add(req.UserUUID, req.Reason, req.Jti, ttl)
	offline := g.enforceBan(req.UserUUID, rec)
	tlog.Info(context.TODO(), "admin ban user=%s reason=%s ttlSec=%d offline=%d",
		req.UserUUID, req.Reason, req.TTLSec, offline)
	writeJSON(w, http.StatusOK, banResponse{OK: true, Banned: banStore.Len(), Record: &rec, Offline: offline})
}

func (g *Gateway) handleAdminUnban(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !g.adminAuthed(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req unbanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserUUID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "userUuid required"})
		return
	}
	removed := banStore.Remove(req.UserUUID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "banned": banStore.Len()})
}

func (g *Gateway) handleAdminBans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !g.adminAuthed(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bans": banStore.List()})
}

// enforceBan 对本实例上的在线连接执行封禁：撤销 JWT → 推送封禁通知 → 断开。
// 返回被踢下的连接数。
func (g *Gateway) enforceBan(userUUID string, rec security.BanRecord) int {
	offline := 0
	g.connectionManager.ForEach(func(conn *connection.Connection) bool {
		uuid := conn.GetUserUUID()
		if !banMatches(uuid, userUUID) {
			return true
		}
		g.revokeConnJWT(conn)
		g.pushBanNotice(conn, rec)
		g.notifyLogicOffline(conn)
		if conn.Conn != nil {
			_ = conn.Conn.Close()
		}
		offline++
		return true
	})
	return offline
}

// banMatches 判断在线连接的 userUuid 是否命中封禁键。
// 封禁键含 ":" 时仅精确匹配完整 serverId:userId；否则匹配裸 userId（精确或 : 后缀）。
func banMatches(connUUID, banKey string) bool {
	if connUUID == banKey {
		return true
	}
	if strings.Contains(banKey, ":") {
		// 完整键必须精确，避免 s1:u1 误伤 s2:u1
		return false
	}
	if i := strings.LastIndex(connUUID, ":"); i >= 0 {
		return connUUID[i+1:] == banKey
	}
	return false
}

func (g *Gateway) revokeConnJWT(conn *connection.Connection) {
	jti := conn.GetJWTJti()
	if jti == "" || g.jwtAuth == nil {
		return
	}
	exp := conn.GetJWTExp()
	if exp <= 0 {
		exp = time.Now().Unix() + 3600
	}
	g.jwtAuth.Revoke(jti, exp)
}

// pushBanNotice 先向客户端推送封禁通知，再由调用方断开连接。
func (g *Gateway) pushBanNotice(conn *connection.Connection, rec security.BanRecord) {
	ack := &protoGw.LoginGateAck{
		Code:      403,
		Message:   "banned: " + rec.Reason,
		SessionId: conn.ID(),
	}
	body, err := proto.Marshal(ack)
	if err != nil {
		return
	}
	data, err := routes.MarshalClientMessage(&protoGw.StreamData{
		Cmd:  routes.CmdBanNtf,
		Data: body,
	})
	if err != nil {
		return
	}
	// Send 内部走 AsyncWrite，event loop 外安全
	_ = conn.Send(data)
}

// rejectIfBanned 在 LoginGate 绑定前检查封禁；命中返回 true 并回调 403。
func (g *Gateway) rejectIfBanned(connectionID, userID string, send func(code int32, text string)) bool {
	if rec := banStore.Get(userID); rec != nil {
		tlog.Warn(context.TODO(), "banned user login rejected connectionID=%s user=%s reason=%s",
			connectionID, userID, rec.Reason)
		send(403, "banned: "+rec.Reason)
		return true
	}
	if conn := g.connectionManager.GetConnection(connectionID); conn != nil {
		full := conn.GetUserUUID()
		if full != "" && full != userID {
			if rec := banStore.Get(full); rec != nil {
				tlog.Warn(context.TODO(), "banned user login rejected connectionID=%s full=%s reason=%s",
					connectionID, full, rec.Reason)
				send(403, "banned: "+rec.Reason)
				return true
			}
		}
	}
	return false
}

// handleLogoutGate 处理客户端登出：撤销 JWT → 通知逻辑服 → 断开，不向客户端再推业务消息。
func (g *Gateway) handleLogoutGate(c gnet.Conn, connectionID string, message *protoGw.StreamData) gnet.Action {
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj == nil {
		return gnet.Close
	}
	g.revokeConnJWT(connObj)
	g.notifyLogicOffline(connObj)
	ackBody, _ := proto.Marshal(&protoGw.LoginGateAck{Code: 0, Message: "ok", SessionId: connectionID})
	if data, err := routes.MarshalClientMessage(&protoGw.StreamData{Cmd: routes.CmdLogoutGateAck, Data: ackBody, SeqId: message.SeqId}); err == nil {
		writeFrameAsync(c, data)
	}
	if connObj.Conn != nil {
		_ = connObj.Conn.Close()
	}
	return gnet.None
}
