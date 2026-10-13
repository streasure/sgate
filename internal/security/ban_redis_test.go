package security

import (
	"context"
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
	if err := s1.InitRedis(ctx, banRedisCfg(mr.Addr())); err != nil {
		t.Fatalf("InitRedis s1: %v", err)
	}
	defer s1.Close()

	s2 := NewBanStore()
	if err := s2.InitRedis(ctx, banRedisCfg(mr.Addr())); err != nil {
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

	// s1 写入后关闭（模拟实例 A 下线），s2 冷启动回源 Redis
	s1 := NewBanStore()
	if err := s1.InitRedis(ctx, banRedisCfg(mr.Addr())); err != nil {
		t.Fatalf("InitRedis s1: %v", err)
	}
	s1.Add("u-cold", "abuse", "", 0)
	_ = s1.Close()

	s2 := NewBanStore()
	if err := s2.InitRedis(ctx, banRedisCfg(mr.Addr())); err != nil {
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
	if err := s.InitRedis(ctx, banRedisCfg(mr.Addr())); err != nil {
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
	if err := s.InitRedis(context.Background(), config.BanRedisConfig{Enabled: false}); err != nil {
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
