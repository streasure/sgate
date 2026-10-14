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

func TestAdminBanIPValidation(t *testing.T) {
	g := &Gateway{}
	mux := http.NewServeMux()
	g.registerAdminRoutes(mux)

	// 无 token → 401（adminAuthed 空 token fail-closed）
	body, _ := json.Marshal(map[string]any{"ip": "10.0.0.1", "reason": "x"})
	req := httptest.NewRequest(http.MethodPost, "/admin/ban", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", rec.Code)
	}
}

func TestBanStoreIPKindKey(t *testing.T) {
	s := security.NewBanStore()
	rec := s.Add("ip:10.0.0.9", "abuse", "", time.Hour)
	if rec.Kind != security.BanKindIP {
		t.Fatalf("kind = %q, want ip", rec.Kind)
	}
	if rec.UserUUID != "10.0.0.9" {
		t.Fatalf("userUuid = %q, want bare ip", rec.UserUUID)
	}
	if rec.Key() != "ip:10.0.0.9" {
		t.Fatalf("Key() = %q", rec.Key())
	}
	if got := s.Get("ip:10.0.0.9"); got == nil {
		t.Fatal("Get by ip key failed")
	}
	if s.IsBanned("10.0.0.9") {
		t.Fatal("bare ip must not match user-key Get")
	}
	if !s.Remove("ip:10.0.0.9") {
		t.Fatal("Remove ip key should report removed")
	}
}

func TestFindBanSkipsIPKind(t *testing.T) {
	old := banStore
	banStore = security.NewBanStore()
	defer func() { banStore = old }()

	banStore.Add("ip:10.1.2.3", "x", "", time.Hour)
	if findBan("10.1.2.3") != nil {
		t.Fatal("findBan must not match IP records as user keys")
	}
}
