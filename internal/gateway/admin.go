package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/connection"
	"github.com/streasure/sgate/internal/routes"
	"github.com/streasure/util/tlog"
	"github.com/streasure/util/uhttp"
	"google.golang.org/protobuf/proto"
)

func (g *Gateway) adminAuthed(r *http.Request) bool {
	cfg := g.cfg.Load()
	if cfg == nil {
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
	return subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(token)) == 1
}

// adminHandler 包装管理端接口：统一做方法校验与 Bearer 鉴权，handler 只保留业务逻辑。
func (g *Gateway) adminHandler(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			uhttp.WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !g.adminAuthed(r) {
			uhttp.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

// registerAdminRoutes 在 stats mux 上挂载管理端接口。
// 封禁端点已移除（单向 Kick 模型：封禁经 loginserver 管理，sgate 仅执行 Kick RPC）。
func (g *Gateway) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/maintenance", g.adminHandler(http.MethodPost, g.handleAdminMaintenance))
	mux.HandleFunc("/admin/ratelimit", g.adminHandler(http.MethodPost, g.handleAdminRateLimit))
}

type rateLimitAdminRequest struct {
	MaxTokens       int    `json:"maxTokens"`
	TokenRefresh    string `json:"tokenRefresh"`
	RouteMaxTokens  int    `json:"routeMaxTokens"`
	GlobalMaxTokens int    `json:"globalMaxTokens"`
}

// handleAdminRateLimit 热更限流参数（无需重启）。body 字段缺省 0/"" 表示恢复默认派生值。
func (g *Gateway) handleAdminRateLimit(w http.ResponseWriter, r *http.Request) {
	if g.rateLimiter == nil {
		uhttp.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rate limiter not enabled"})
		return
	}
	var req rateLimitAdminRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uhttp.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	refresh := time.Second
	if req.TokenRefresh != "" {
		d, err := time.ParseDuration(req.TokenRefresh)
		if err != nil || d <= 0 {
			uhttp.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tokenRefresh"})
			return
		}
		refresh = d
	}
	tokens := req.MaxTokens
	if tokens <= 0 {
		tokens = config.DefaultRateLimitMaxTokens
	}
	g.rateLimiter.UpdateRate(tokens, refresh)
	g.rateLimiter.SetRouteMaxTokens(req.RouteMaxTokens)
	g.rateLimiter.SetGlobalMaxTokens(req.GlobalMaxTokens)

	// 同步现役配置，避免下次热更/快照回退旧值
	if cur := g.cfg.Load(); cur != nil {
		clone := *cur
		clone.Security.RateLimit.MaxTokens = tokens
		clone.Security.RateLimit.TokenRefresh = refresh.String()
		clone.Security.RateLimit.RouteMaxTokens = req.RouteMaxTokens
		clone.Security.RateLimit.GlobalMaxTokens = req.GlobalMaxTokens
		g.cfg.Store(&clone)
	}

	tlog.Info(g.ctx, "admin rate limit updated maxTokens=%d refresh=%v routeMaxTokens=%d globalMaxTokens=%d",
		tokens, refresh, req.RouteMaxTokens, req.GlobalMaxTokens)
	uhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"maxTokens":       tokens,
		"tokenRefresh":    refresh.String(),
		"routeMaxTokens":  req.RouteMaxTokens,
		"globalMaxTokens": req.GlobalMaxTokens,
	})
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

// handleLogoutGate 处理客户端登出：撤销 JWT → 通知逻辑服 → 断开，不向客户端再推业务消息。
func (g *Gateway) handleLogoutGate(_ gnet.Conn, connectionID string, message *protoGw.StreamData) gnet.Action {
	connObj := g.connectionManager.GetConnection(connectionID)
	if connObj == nil {
		return gnet.Close
	}
	g.revokeConnJWT(connObj)
	g.notifyLogicOffline(connObj)
	ackBody, _ := proto.Marshal(&protoGw.LoginGateAck{Code: 0, Message: "ok", SessionId: connectionID})
	if data, err := routes.MarshalClientMessage(&protoGw.StreamData{Cmd: routes.CmdLogoutGateAck, Data: ackBody, SeqId: message.SeqId}); err == nil {
		// ack 可靠投递：SendThenClose 在写完成回调内 Close（Windows 无 outbound flush，
		// 立即 Close 会丢 ack）；不可用（连接已空等）回退同步 Close。
		if sendErr := connObj.SendThenClose(data); sendErr == nil {
			return gnet.None
		}
	}
	if connObj.Conn != nil {
		_ = connObj.Conn.Close()
	}
	return gnet.None
}
