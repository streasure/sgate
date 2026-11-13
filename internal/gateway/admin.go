package gateway

import (
	"time"

	"github.com/panjf2000/gnet/v2"
	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/admin"
	"github.com/streasure/sgate/internal/dataplane"
	"github.com/streasure/util/tlog"
	"google.golang.org/protobuf/proto"
)

// ============================================================================
// admin.Backend 实现：管理端 HTTP 面经消费方定义接口回调数据面能力
// ============================================================================

// AdminToken 实现 admin.Backend：读现役配置的管理端 Bearer 令牌（空 = 关闭）。
func (g *Gateway) AdminToken() string {
	cfg := g.cfg.Load()
	if cfg == nil {
		return ""
	}
	return cfg.Admin.Token
}

// RateLimitEnabled 实现 admin.Backend：限流器是否启用。
func (g *Gateway) RateLimitEnabled() bool { return g.rateLimiter != nil }

// UpdateRateLimit 实现 admin.Backend：应用限流参数并同步现役配置，
// 返回实际生效值供管理端响应（原 handleAdminRateLimit 业务部分，日志随迁）。
func (g *Gateway) UpdateRateLimit(tokens int, refresh time.Duration, routeMaxTokens, globalMaxTokens int) (admin.RateLimitOutcome, error) {
	if g.rateLimiter == nil {
		return admin.RateLimitOutcome{}, admin.ErrRateLimiterDisabled
	}
	g.rateLimiter.UpdateRate(tokens, refresh)
	g.rateLimiter.SetRouteMaxTokens(routeMaxTokens)
	g.rateLimiter.SetGlobalMaxTokens(globalMaxTokens)

	// 同步现役配置，避免下次热更/快照回退旧值
	if cur := g.cfg.Load(); cur != nil {
		clone := *cur
		clone.Security.RateLimit.MaxTokens = tokens
		clone.Security.RateLimit.TokenRefresh = refresh.String()
		clone.Security.RateLimit.RouteMaxTokens = routeMaxTokens
		clone.Security.RateLimit.GlobalMaxTokens = globalMaxTokens
		g.cfg.Store(&clone)
	}

	tlog.Info(g.ctx, "admin rate limit updated maxTokens=%d refresh=%v routeMaxTokens=%d globalMaxTokens=%d",
		tokens, refresh, routeMaxTokens, globalMaxTokens)
	return admin.RateLimitOutcome{
		MaxTokens:       tokens,
		TokenRefresh:    refresh.String(),
		RouteMaxTokens:  routeMaxTokens,
		GlobalMaxTokens: globalMaxTokens,
	}, nil
}

// ============================================================================
// 登出（业务，不属于管理端 HTTP 面）
// ============================================================================

func (g *Gateway) revokeConnJWT(conn *dataplane.Connection) {
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
	if data, err := protoGw.MarshalClientMessage(&protoGw.StreamData{Cmd: protoGw.CmdLogoutGateAck, Data: ackBody, SeqId: message.SeqId}); err == nil {
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
