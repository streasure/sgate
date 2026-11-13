package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/streasure/sgate/internal/security"
)

// adminServe 经 admin.Server 注册后的 mux 分发请求（覆盖方法校验 + Bearer 鉴权 + 业务 handler）。
func adminServe(gw *Gateway, rec *httptest.ResponseRecorder, req *http.Request) {
	mux := http.NewServeMux()
	gw.adminSrv.Register(mux)
	mux.ServeHTTP(rec, req)
}

// TestAdminRateLimitAuth 鉴权失败必须 401（配置无令牌 = 管理端关闭）。
func TestAdminRateLimitAuth(t *testing.T) {
	gw := newTestGateway(minimalConfig())
	defer gw.Close()
	gw.rateLimiter = security.NewRateLimiter(100, time.Second)

	req := httptest.NewRequest(http.MethodPost, "/admin/ratelimit", bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	adminServe(gw, rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without configured token", rr.Code)
	}
}

// TestAdminRateLimitApply 热更后 UpdateRate 生效。
func TestAdminRateLimitApply(t *testing.T) {
	gw := newTestGateway(maintenanceConfig()) // Admin.Token=secret
	defer gw.Close()
	gw.rateLimiter = security.NewRateLimiter(100, time.Second)

	body := `{"maxTokens":5000,"tokenRefresh":"2s","routeMaxTokens":20000,"globalMaxTokens":50000}`
	req := httptest.NewRequest(http.MethodPost, "/admin/ratelimit", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	adminServe(gw, rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["maxTokens"] != float64(5000) {
		t.Fatalf("maxTokens = %v, want 5000", resp["maxTokens"])
	}
	if got := gw.rateLimiter.GetDimensionConfig("ip").MaxTokens; got != 5000 {
		t.Fatalf("ip MaxTokens = %d, want 5000", got)
	}
	if got := gw.rateLimiter.GetDimensionConfig("route").MaxTokens; got != 20000 {
		t.Fatalf("route MaxTokens = %d, want 20000", got)
	}
}

// TestAdminRateLimitDisabledRateLimiter 未启用限流返回 503。
func TestAdminRateLimitDisabledRateLimiter(t *testing.T) {
	gw := newTestGateway(maintenanceConfig())
	defer gw.Close()
	gw.rateLimiter = nil

	req := httptest.NewRequest(http.MethodPost, "/admin/ratelimit", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	adminServe(gw, rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
