package security

import (
	"testing"
	"time"
)

func TestBanStore_AddGetRemove(t *testing.T) {
	s := NewBanStore()
	if s.IsBanned("u1") {
		t.Fatal("empty store should not ban")
	}

	s.Add("u1", "cheat", "jti-1", 0)
	if !s.IsBanned("u1") {
		t.Fatal("u1 should be banned")
	}
	rec := s.Get("u1")
	if rec == nil || rec.Reason != "cheat" || rec.Jti != "jti-1" {
		t.Fatalf("unexpected record %+v", rec)
	}
	if !rec.ExpiresAt.IsZero() {
		t.Fatal("ttl=0 should be permanent")
	}

	if !s.Remove("u1") {
		t.Fatal("Remove should return true")
	}
	if s.IsBanned("u1") {
		t.Fatal("u1 should be unbanned")
	}
	if s.Remove("u1") {
		t.Fatal("second Remove should return false")
	}
}

func TestBanStore_TTLExpiry(t *testing.T) {
	s := NewBanStore()
	s.Add("u2", "spam", "", 20*time.Millisecond)
	if !s.IsBanned("u2") {
		t.Fatal("u2 should be banned within TTL")
	}
	time.Sleep(30 * time.Millisecond)
	if s.IsBanned("u2") {
		t.Fatal("u2 ban should expire")
	}
	if s.Get("u2") != nil {
		t.Fatal("expired record should be gone")
	}
}

func TestBanStore_ListAndLen(t *testing.T) {
	s := NewBanStore()
	s.Add("a", "r1", "", 0)
	s.Add("b", "r2", "", time.Hour)
	if n := s.Len(); n != 2 {
		t.Fatalf("Len=%d want 2", n)
	}
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("List len=%d want 2", len(list))
	}
}
