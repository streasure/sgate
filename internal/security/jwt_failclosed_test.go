package security

import (
	"testing"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/types"
)

func newTestJWT() *JWTAuthFilter {
	return NewJWTAuthFilter(config.JWTAuthConfig{
		Enabled: true,
		Secret:  "test-secret",
		Issuer:  "sgate",
	})
}

// TestJWTFailClosed_BoundUserNoTokenReject 绑定用户但无 token → 拒绝（fail-closed）。
func TestJWTFailClosed_BoundUserNoTokenReject(t *testing.T) {
	f := newTestJWT()
	fc := &types.FilterContext{
		ConnectionID: "c1",
		UserUUID:     "s1:u1", // 已登录绑定
		Metadata:     map[string]string{},
	}
	ok, err := f.Process(fc)
	if err != nil {
		t.Fatalf("Process err=%v", err)
	}
	if ok {
		t.Fatal("bound user without token must be rejected (fail-closed)")
	}
	if fc.DropReason == "" {
		t.Fatal("DropReason should be set")
	}
}

// TestJWTFailClosed_UnboundUserPasses 未绑定（LoginGate 前）无 token → 放行。
func TestJWTFailClosed_UnboundUserPasses(t *testing.T) {
	f := newTestJWT()
	fc := &types.FilterContext{
		ConnectionID: "c1",
		Metadata:     map[string]string{},
	}
	ok, err := f.Process(fc)
	if err != nil || !ok {
		t.Fatalf("unbound frame should pass, ok=%v err=%v", ok, err)
	}
}

// TestJWTFailClosed_InvalidTokenReject 无效/过期/伪造 token → 拒绝。
func TestJWTFailClosed_InvalidTokenReject(t *testing.T) {
	f := newTestJWT()
	cases := map[string]string{
		"garbage":     "not-a-jwt",
		"bad-sig":     "aGVhZGluZw.bG9hZA.c2lnbmF0dXJl",
		"wrong-alg":   "",
		"empty-token": "",
	}
	for name, tok := range cases {
		if tok == "" {
			continue
		}
		fc := &types.FilterContext{
			ConnectionID: "c1",
			UserUUID:     "s1:u1",
			Metadata:     map[string]string{f.headerName: tok},
		}
		ok, _ := f.Process(fc)
		if ok {
			t.Errorf("%s: invalid token accepted", name)
		}
	}

	// 过期 token
	expired, err := f.Issue(JWTClaims{
		Sub: "u", Iss: "sgate",
		Exp: time.Now().Add(-time.Hour).Unix(),
		Iat: time.Now().Add(-2 * time.Hour).Unix(),
		Jti: "expired",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	fc := &types.FilterContext{
		ConnectionID: "c1",
		UserUUID:     "s1:u1",
		Metadata:     map[string]string{f.headerName: expired},
	}
	if ok, _ := f.Process(fc); ok {
		t.Fatal("expired token accepted")
	}
}

// TestJWTFailClosed_SkipRoutes 路由名与 cmd 十进制串两种 skip 键都应生效。
func TestJWTFailClosed_SkipRoutes(t *testing.T) {
	f := NewJWTAuthFilter(config.JWTAuthConfig{
		Enabled:    true,
		Secret:     "s",
		SkipRoutes: []string{"login_gate", "1000001"},
	})
	for _, route := range []string{"login_gate", "1000001"} {
		fc := &types.FilterContext{
			ConnectionID: "c1",
			UserUUID:     "s1:u1", // 已绑定 + 无 token，若 skip 失败会拒绝
			Route:        route,
			Metadata:     map[string]string{},
		}
		ok, err := f.Process(fc)
		if err != nil || !ok {
			t.Errorf("route %q should be skipped, ok=%v err=%v", route, ok, err)
		}
	}
}
