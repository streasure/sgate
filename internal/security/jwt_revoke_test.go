package security

import (
	"testing"
	"time"

	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/sgate/internal/types"
)

func TestJWTAuthFilter_RevokeRejectsToken(t *testing.T) {
	f := NewJWTAuthFilter(config.JWTAuthConfig{
		Enabled: true,
		Secret:  "test-secret",
		Issuer:  "sgate",
	})
	claims := JWTClaims{
		Sub:   "user-1",
		Iss:   "sgate",
		Exp:   time.Now().Add(time.Hour).Unix(),
		Iat:   time.Now().Unix(),
		Jti:   "jti-abc",
		Route: "login",
	}
	token, err := f.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := f.Validate(token); err != nil {
		t.Fatalf("Validate before revoke: %v", err)
	}

	f.Revoke("jti-abc", claims.Exp)
	if _, err := f.Validate(token); err == nil {
		t.Fatal("Validate should fail after Revoke")
	}
}

func TestJWTAuthFilter_ProcessSetsJtiAndExpMetadata(t *testing.T) {
	f := NewJWTAuthFilter(config.JWTAuthConfig{
		Enabled: true,
		Secret:  "test-secret",
		Issuer:  "sgate",
	})
	exp := time.Now().Add(time.Hour).Unix()
	token, err := f.Issue(JWTClaims{Sub: "u", Iss: "sgate", Exp: exp, Iat: time.Now().Unix(), Jti: "j1"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	fc := &types.FilterContext{
		ConnectionID: "c1",
		Metadata:     map[string]string{f.headerName: token},
	}
	ok, err := f.Process(fc)
	if err != nil || !ok {
		t.Fatalf("Process ok=%v err=%v", ok, err)
	}
	if fc.Metadata["jwt.jti"] != "j1" {
		t.Fatalf("jti metadata = %q", fc.Metadata["jwt.jti"])
	}
	if fc.Metadata["jwt.exp"] == "" {
		t.Fatal("exp metadata missing")
	}
}
