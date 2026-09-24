package gateway

import (
	"testing"
	"time"
)

func TestBanMatches(t *testing.T) {
	cases := []struct {
		conn, ban string
		want      bool
	}{
		{"s1:u1", "s1:u1", true},
		{"s1:u1", "u1", true},
		{"s1:u1", "s2:u1", false},
		{"temp_x", "u1", false},
		{"u1", "u1", true},
		{"s1:u1", "s1:u2", false},
	}
	for _, c := range cases {
		if got := banMatches(c.conn, c.ban); got != c.want {
			t.Errorf("banMatches(%q,%q)=%v want %v", c.conn, c.ban, got, c.want)
		}
	}
}

func TestRejectIfBanned_BareAndFullKey(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	// 清理全局 banStore 状态
	for _, rec := range BanStore().List() {
		BanStore().Remove(rec.UserUUID)
	}

	if gw.rejectIfBanned("c1", "free-user", func(code int32, text string) {
		t.Fatalf("unexpected reject code=%d text=%s", code, text)
	}) {
		t.Fatal("free user should not be banned")
	}

	BanStore().Add("banned-user", "test", "", time.Hour)
	sent := 0
	if !gw.rejectIfBanned("c1", "banned-user", func(code int32, text string) {
		sent++
		if code != 403 {
			t.Errorf("code=%d want 403", code)
		}
	}) {
		t.Fatal("banned user should be rejected")
	}
	if sent != 1 {
		t.Fatalf("send callback count=%d want 1", sent)
	}

	BanStore().Remove("banned-user")
}

func TestAdminAuthed_RequiresToken(t *testing.T) {
	gw := newGateway(minimalConfig())
	defer gw.Close()

	// 无 admin.token → fail-closed
	req := newAuthorizedRequest("")
	if gw.adminAuthed(req) {
		t.Fatal("empty admin token should deny")
	}

	cfg := minimalConfig()
	cfg.Admin.Token = "secret"
	stored := *cfg
	gw.cfg.Store(&stored)

	req = newAuthorizedRequest("secret")
	if !gw.adminAuthed(req) {
		t.Fatal("correct bearer should allow")
	}
	req = newAuthorizedRequest("wrong")
	if gw.adminAuthed(req) {
		t.Fatal("wrong bearer should deny")
	}
}
