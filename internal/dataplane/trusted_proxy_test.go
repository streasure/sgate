package dataplane

import (
	"net/netip"
	"testing"
)

func TestIsTrustedProxy(t *testing.T) {
	g := &Engine{}
	// 空列表 → 不信任任何
	if g.IsTrustedProxy("10.0.0.1") {
		t.Error("empty trustedProxies should not trust")
	}

	g.trustedProxies = mustParseAddrs(t, "192.168.1.1", "10.1.2.3")
	if !g.IsTrustedProxy("192.168.1.1") {
		t.Error("192.168.1.1 exact should be trusted")
	}
	if !g.IsTrustedProxy("10.1.2.3") {
		t.Error("10.1.2.3 exact should be trusted")
	}
	// 仅精确匹配：同网段非精确条目不信任（CIDR 已移除）
	if g.IsTrustedProxy("10.1.2.4") {
		t.Error("non-exact same-subnet ip should not be trusted")
	}
	if g.IsTrustedProxy("11.0.0.1") {
		t.Error("11.0.0.1 not in trusted list")
	}
	if g.IsTrustedProxy("not-an-ip") {
		t.Error("invalid IP should not be trusted")
	}
	// 网段串不是合法条目（配置校验已拒；此处防御性断言语义）
	if g.IsTrustedProxy("10.0.0.0/8") {
		t.Error("cidr string should not be trusted")
	}
}

func mustParseAddrs(t *testing.T, ss ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		if addr, err := netip.ParseAddr(s); err == nil {
			out = append(out, addr)
		}
	}
	return out
}
