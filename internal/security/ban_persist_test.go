package security

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBanStorePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")

	s1 := NewBanStore()
	s1.SetPersistPath(path)
	s1.Add("u1", "spam", "jti-1", time.Hour)
	s1.Add("u2", "abuse", "", 0) // 永久
	s1.Remove("u1")

	s2 := NewBanStore()
	s2.SetPersistPath(path)
	if s2.Len() != 1 {
		t.Fatalf("loaded entries = %d, want 1 (u1 removed before persist)", s2.Len())
	}
	if !s2.IsBanned("u2") {
		t.Fatal("u2 should be banned after load")
	}
	if s2.IsBanned("u1") {
		t.Fatal("u1 was removed, should not be banned")
	}
}

func TestBanStorePersistExpiredFiltered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")

	s1 := NewBanStore()
	s1.SetPersistPath(path)
	s1.Add("expired", "x", "", time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	s2 := NewBanStore()
	s2.SetPersistPath(path)
	if s2.IsBanned("expired") {
		t.Fatal("expired ban must not survive load")
	}
}

func TestBanStorePersistDisabled(t *testing.T) {
	s := NewBanStore()
	s.Add("u", "r", "", 0)
	// persistPath 为空：Add 不应创建文件
	if _, err := os.Stat("bans.json"); err == nil {
		t.Fatal("unexpected bans.json created without persist path")
	}
	if !s.IsBanned("u") {
		t.Fatal("in-memory ban should work without persist")
	}
}
