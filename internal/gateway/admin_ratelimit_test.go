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

// TestAdminRateLimitAuth 鉴权失败必须 401。
func TestAdminRateLimitAuth(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()
	gw.rateLimiter = security.NewRateLimiter(100, time.Second)

	req := httptest.NewRequest(http.MethodPost, "/admin/ratelimit", bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	gw.handleAdminRateLimit(rr, req)
	// 未走 adminHandler 鉴权包装时 handler 本身不校验；验证 adminHandler 包装行为
	h := gw.adminHandler(http.MethodPost, gw.handleAdminRateLimit)
	rr2 := httptest.NewRecorder()
	h(rr2, req)
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without Bearer token", rr2.Code)
	}
}

// TestAdminRateLimitApply 热更后 UpdateRate 生效。
func TestAdminRateLimitApply(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()
	gw.rateLimiter = security.NewRateLimiter(100, time.Second)

	body := `{"maxTokens":5000,"tokenRefresh":"2s","routeMaxTokens":20000,"globalMaxTokens":50000}`
	req := httptest.NewRequest(http.MethodPost, "/admin/ratelimit", bytes.NewReader([]byte(body)))
	rr := httptest.NewRecorder()
	gw.handleAdminRateLimit(rr, req)
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
	gw := newGateway(minimalConfig())
	defer gw.Close()
	gw.rateLimiter = nil

	req := httptest.NewRequest(http.MethodPost, "/admin/ratelimit", bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	gw.handleAdminRateLimit(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
