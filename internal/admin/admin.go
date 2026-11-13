// Package admin 提供管理端 HTTP 面：Bearer 鉴权、方法校验与 /admin 端点注册。
//
// 业务能力（限流热更、切维护踢人）经本包定义的 Backend 接口回调数据面——
// 消费方定义接口：admin 不 import 数据面，由 dataplane.Engine 实现本接口，
// 组合根/控制面在装配时注入。
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/uhttp"
)

// Backend 管理端需要的数据面能力（消费方定义接口，由 *dataplane.Engine 实现）。
type Backend interface {
	// AdminToken 返回管理端 Bearer 令牌；空串 = 管理端关闭。
	AdminToken() string
	// RateLimitEnabled 报告限流器是否启用（未启用时 /admin/ratelimit 直接 503）。
	RateLimitEnabled() bool
	// UpdateRateLimit 应用限流参数并同步现役配置；返回实际生效值。
	UpdateRateLimit(tokens int, refresh time.Duration, routeMaxTokens, globalMaxTokens int) (RateLimitOutcome, error)
	// SetMaintenance 执行切维护（action: enable|disable）：打标记、快照在线连接、
	// 异步踢人（enable）或移除标记（disable）。
	SetMaintenance(action, serverID, zone, reason string) (MaintenanceOutcome, error)
}

// Backend 侧哨兵错误 → admin 映射 HTTP 状态码。
var (
	// ErrRateLimiterDisabled 限流器未启用（→ 503）。
	ErrRateLimiterDisabled = errors.New("rate limiter not enabled")
	// ErrMaintenanceNotInitialized 维护状态未初始化（→ 500）。
	ErrMaintenanceNotInitialized = errors.New("maintenance not initialized")
	// ErrBadMaintenanceAction action 非 enable/disable（→ 400）。
	ErrBadMaintenanceAction = errors.New("action must be enable or disable")
)

// RateLimitOutcome UpdateRateLimit 实际生效值（响应体数据源）。
type RateLimitOutcome struct {
	MaxTokens       int
	TokenRefresh    string
	RouteMaxTokens  int
	GlobalMaxTokens int
}

// MaintenanceOutcome SetMaintenance 执行结果（响应体数据源）。
type MaintenanceOutcome struct {
	Action  string
	Matched int    // enable：命中的在线连接数
	Removed bool   // disable：标记确实被移除
	Reason  string // enable：生效的维护原因（空则默认 maintenance）
}

// Server 管理端 HTTP 服务：鉴权包装 + 端点注册，业务经 Backend 回调。
type Server struct {
	b Backend
}

// New 创建管理端服务。
func New(b Backend) *Server { return &Server{b: b} }

// Register 在 stats mux 上挂载管理端接口。
// 封禁端点已移除（单向 Kick 模型：封禁经 loginserver 管理，sgate 仅执行 Kick RPC）。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/maintenance", s.handler(http.MethodPost, s.handleMaintenance))
	mux.HandleFunc("/admin/ratelimit", s.handler(http.MethodPost, s.handleRateLimit))
}

// authed 校验 Bearer 令牌（常数时间比较）；令牌空 = 管理端关闭。
func (s *Server) authed(r *http.Request) bool {
	token := s.b.AdminToken()
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

// handler 统一做方法校验与 Bearer 鉴权，handler 只保留业务逻辑。
func (s *Server) handler(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			uhttp.WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !s.authed(r) {
			uhttp.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

// RateLimitRequest POST /admin/ratelimit 请求体。
type RateLimitRequest struct {
	MaxTokens       int    `json:"maxTokens"`
	TokenRefresh    string `json:"tokenRefresh"`
	RouteMaxTokens  int    `json:"routeMaxTokens"`
	GlobalMaxTokens int    `json:"globalMaxTokens"`
}

// handleRateLimit 热更限流参数（无需重启）。body 字段缺省 0/"" 表示恢复默认派生值。
func (s *Server) handleRateLimit(w http.ResponseWriter, r *http.Request) {
	if !s.b.RateLimitEnabled() {
		uhttp.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rate limiter not enabled"})
		return
	}
	var req RateLimitRequest
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

	out, err := s.b.UpdateRateLimit(tokens, refresh, req.RouteMaxTokens, req.GlobalMaxTokens)
	if err != nil {
		if errors.Is(err, ErrRateLimiterDisabled) {
			uhttp.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rate limiter not enabled"})
			return
		}
		uhttp.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "update failed"})
		return
	}
	uhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"maxTokens":       out.MaxTokens,
		"tokenRefresh":    out.TokenRefresh,
		"routeMaxTokens":  out.RouteMaxTokens,
		"globalMaxTokens": out.GlobalMaxTokens,
	})
}

// MaintenanceRequest POST /admin/maintenance 请求体。
type MaintenanceRequest struct {
	Action   string `json:"action"`   // enable | disable
	ServerID string `json:"serverId"` // 与 zone 二选一
	Zone     string `json:"zone"`     // 与 serverId 二选一
	Reason   string `json:"reason"`   // 可选，踢人通知附带原因
}

// MaintenanceResponse POST /admin/maintenance 响应体。
type MaintenanceResponse struct {
	OK       bool   `json:"ok"`
	Action   string `json:"action"`
	ServerID string `json:"serverId,omitempty"`
	Zone     string `json:"zone,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Matched  int    `json:"matched"`            // 命中的在线连接数
	Accepted bool   `json:"accepted,omitempty"` // enable：踢人任务已异步受理
	Removed  bool   `json:"removed,omitempty"`  // disable：标记确实被移除
}

// handleMaintenance POST /admin/maintenance 切维护接口（方法与鉴权由 handler 统一处理）。
func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	var req MaintenanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uhttp.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	serverID := strings.TrimSpace(req.ServerID)
	zone := strings.TrimSpace(req.Zone)
	if (serverID == "") == (zone == "") {
		uhttp.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "serverId or zone required (exactly one)"})
		return
	}

	action := strings.ToLower(strings.TrimSpace(req.Action))
	out, err := s.b.SetMaintenance(action, serverID, zone, strings.TrimSpace(req.Reason))
	if err != nil {
		if errors.Is(err, ErrMaintenanceNotInitialized) {
			uhttp.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "maintenance not initialized"})
			return
		}
		if errors.Is(err, ErrBadMaintenanceAction) {
			uhttp.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "action must be enable or disable"})
			return
		}
		uhttp.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "maintenance update failed"})
		return
	}

	switch action {
	case "enable":
		uhttp.WriteJSON(w, http.StatusOK, MaintenanceResponse{
			OK: true, Action: "enable", ServerID: serverID, Zone: zone,
			Reason: out.Reason, Matched: out.Matched, Accepted: true,
		})
	case "disable":
		uhttp.WriteJSON(w, http.StatusOK, MaintenanceResponse{
			OK: true, Action: "disable", ServerID: serverID, Zone: zone, Removed: out.Removed,
		})
	}
}
