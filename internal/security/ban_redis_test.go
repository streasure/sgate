package security

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/streasure/sgate/internal/config"
)

func startTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	return mr
}

func banRedisCfg(addr string) config.BanRedisConfig {
	return config.BanRedisConfig{
		Enabled:      true,
		Address:      addr,
		KeyPrefix:    "sgate:ban:",
		SyncInterval: "-1s", // 单测关闭周期同步，手动 SyncFromRedis
	}
}

func TestBanStoreRedisSharedBetweenStores(t *testing.T) {
	mr := startTestRedis(t)
	ctx := context.Background()

	s1 := NewBanStore()
	if err := s1.InitRedis(ctx, banRedisCfg(mr.Addr()), "s1"); err != nil {
		t.Fatalf("InitRedis s1: %v", err)
	}
	defer s1.Close()

	s2 := NewBanStore()
	if err := s2.InitRedis(ctx, banRedisCfg(mr.Addr()), "s2"); err != nil {
		t.Fatalf("InitRedis s2: %v", err)
	}
	defer s2.Close()

	s1.Add("u-shared", "spam", "jti-1", time.Hour)
	if !s2.IsBanned("u-shared") {
		t.Fatal("s2 should see ban written by s1 via redis")
	}

	if !s2.Remove("u-shared") {
		t.Fatal("s2 unban should report removed")
	}
	if err := s1.SyncFromRedis(ctx); err != nil {
		t.Fatalf("s1 SyncFromRedis: %v", err)
	}
	if s1.IsBanned("u-shared") {
		t.Fatal("s1 should see unban after redis del + sync")
	}
}

func TestBanStoreRedisGetFallback(t *testing.T) {
	mr := startTestRedis(t)
	ctx := context.Background()

	s1 := NewBanStore()
	if err := s1.InitRedis(ctx, banRedisCfg(mr.Addr()), "s1"); err != nil {
		t.Fatalf("InitRedis s1: %v", err)
	}
	s1.Add("u-cold", "abuse", "", 0)
	_ = s1.Close()

	s2 := NewBanStore()
	if err := s2.InitRedis(ctx, banRedisCfg(mr.Addr()), "s2"); err != nil {
		t.Fatalf("InitRedis s2: %v", err)
	}
	defer s2.Close()

	if !s2.IsBanned("u-cold") {
		t.Fatal("cold start should load ban from redis")
	}
}

func TestBanStoreRedisExpiry(t *testing.T) {
	mr := startTestRedis(t)
	ctx := context.Background()

	s := NewBanStore()
	if err := s.InitRedis(ctx, banRedisCfg(mr.Addr()), "s1"); err != nil {
		t.Fatalf("InitRedis: %v", err)
	}
	defer s.Close()

	s.Add("u-ttl", "x", "", 50*time.Millisecond)
	mr.FastForward(time.Second)

	if err := s.SyncFromRedis(ctx); err != nil {
		t.Fatalf("SyncFromRedis: %v", err)
	}
	if s.IsBanned("u-ttl") {
		t.Fatal("expired ban must not survive redis TTL + sync")
	}
}

func TestBanStoreRedisDisabledNoop(t *testing.T) {
	s := NewBanStore()
	if err := s.InitRedis(context.Background(), config.BanRedisConfig{Enabled: false}, ""); err != nil {
		t.Fatalf("disabled InitRedis should no-op: %v", err)
	}
	s.Add("u", "r", "", 0)
	if !s.IsBanned("u") {
		t.Fatal("memory ban should work without redis")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNormalizeIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"192.168.1.1", "192.168.1.1", true},
		{"  10.0.0.1  ", "10.0.0.1", true},
		{"::ffff:192.168.1.1", "192.168.1.1", true},
		{"2001:db8::1", "2001:db8::1", true},
		{"", "", false},
		{"not-an-ip", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeIP(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("NormalizeIP(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeIPKey(t *testing.T) {
	if k := NormalizeIPKey("::ffff:10.0.0.2"); k != "ip:10.0.0.2" {
		t.Fatalf("NormalizeIPKey mapped = %q", k)
	}
	if k := NormalizeIPKey("bad"); k != "ip:bad" {
		t.Fatalf("NormalizeIPKey invalid = %q", k)
	}
}

func TestBanStoreNegativeCache(t *testing.T) {
	mr := startTestRedis(t)
	ctx := context.Background()
	s := NewBanStore()
	cfg := banRedisCfg(mr.Addr())
	cfg.CacheNegativeTtl = "1h"
	if err := s.InitRedis(ctx, cfg, "s1"); err != nil {
		t.Fatalf("InitRedis: %v", err)
	}
	defer s.Close()

	// miss → negative cache
	if s.IsBanned("u-neg") {
		t.Fatal("should not be banned")
	}
	// second Get must hit neg cache (no redis trip) — just assert still nil
	if s.Get("u-neg") != nil {
		t.Fatal("neg cache should keep nil")
	}
	s.mu.RLock()
	_, hasNeg := s.neg["u-neg"]
	s.mu.RUnlock()
	if !hasNeg {
		t.Fatal("expected negative cache entry")
	}

	// remote ban appears in redis → Get 回源前 negHit 会挡住？
	// negHit only skips redis if true; wait — currently Get checks negHit first.
	// So a remote ban within neg TTL would be missed until neg expires or Sync.
	// Design: events clear neg; for no-events mode, rely on SyncFromRedis.
	// Simulate remote write + Sync.
	rdb := s.rdb
	rec := BanRecord{UserUUID: "u-neg", Kind: BanKindUser, Reason: "late", BannedAt: time.Now()}
	data, _ := json.Marshal(rec)
	if err := rdb.Set(ctx, s.key("u-neg"), data, 0).Err(); err != nil {
		t.Fatalf("redis set: %v", err)
	}
	if err := s.SyncFromRedis(ctx); err != nil {
		t.Fatalf("SyncFromRedis: %v", err)
	}
	if !s.IsBanned("u-neg") {
		t.Fatal("sync should clear neg and load ban")
	}
}

func TestBanStoreRedisEvents(t *testing.T) {
	mr := startTestRedis(t)
	ctx := context.Background()
	f := func(addr string, origin string) config.BanRedisConfig {
		c := banRedisCfg(addr)
		on := true
		c.EventsEnabled = &on
		c.SyncInterval = "-1s"
		return c
	}

	s1 := NewBanStore()
	if err := s1.InitRedis(ctx, f(mr.Addr(), "s1"), "s1"); err != nil {
		t.Fatalf("InitRedis s1: %v", err)
	}
	defer s1.Close()

	s2 := NewBanStore()
	if err := s2.InitRedis(ctx, f(mr.Addr(), "s2"), "s2"); err != nil {
		t.Fatalf("InitRedis s2: %v", err)
	}
	defer s2.Close()

	gotCh := make(chan BanEvent, 16)
	s2.SetEventHandler(func(ev BanEvent) { gotCh <- ev })

	s1.Add("u-ev", "evt", "", time.Hour)

	var banEv BanEvent
	select {
	case banEv = <-gotCh:
	case <-time.After(3 * time.Second):
		t.Fatal("s2 should receive ban event")
	}
	if banEv.Op != "ban" || banEv.Key != "u-ev" {
		t.Fatalf("banEv = %+v, want op=ban key=u-ev", banEv)
	}
	if !s2.IsBanned("u-ev") {
		t.Fatal("s2 table should contain ban after event")
	}

	s1.Remove("u-ev")
	select {
	case banEv = <-gotCh:
	case <-time.After(3 * time.Second):
		t.Fatal("s2 should receive unban event")
	}
	if banEv.Op != "unban" || banEv.Key != "u-ev" {
		t.Fatalf("unbanEv = %+v, want op=unban key=u-ev", banEv)
	}
	// unban 后依赖 handler 之外的本地表删除（handleEventMessage 已删）
	if s2.Get("u-ev") != nil {
		// 若 Redis 仍有键（Remove 已删），Get 应回源；Remove 已 del，应为 nil
		t.Fatal("s2 should clear ban after unban event")
	}
}

func TestBanStoreIPKeyShared(t *testing.T) {
	mr := startTestRedis(t)
	ctx := context.Background()
	s1 := NewBanStore()
	if err := s1.InitRedis(ctx, banRedisCfg(mr.Addr()), "s1"); err != nil {
		t.Fatalf("InitRedis s1: %v", err)
	}
	defer s1.Close()
	s2 := NewBanStore()
	if err := s2.InitRedis(ctx, banRedisCfg(mr.Addr()), "s2"); err != nil {
		t.Fatalf("InitRedis s2: %v", err)
	}
	defer s2.Close()

	key := NormalizeIPKey("::ffff:10.1.2.3")
	s1.Add(key, "bad-ip", "", 0)
	if !s2.IsBanned(key) {
		t.Fatal("ip ban should sync via redis")
	}
	rec := s2.Get(key)
	if rec == nil || rec.Kind != BanKindIP || rec.UserUUID != "10.1.2.3" {
		t.Fatalf("rec = %+v, want kind=ip user=10.1.2.3", rec)
	}
}
