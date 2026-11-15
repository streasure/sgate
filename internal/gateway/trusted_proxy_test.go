package gateway

import (
	"net/netip"
	"testing"

	"github.com/streasure/util/netutil"
)

func TestIsTrustedProxy(t *testing.T) {
	g := &Gateway{}
	// 空列表 → 不信任任何
	if g.IsTrustedProxy("10.0.0.1") {
		t.Error("empty trustedProxies should not trust")
	}

	g.trustedProxies = mustParsePrefixes(t, "10.0.0.0/8", "192.168.1.1")
	if !g.IsTrustedProxy("10.255.255.1") {
		t.Error("10.255.255.1 should be in 10.0.0.0/8")
	}
	if !g.IsTrustedProxy("192.168.1.1") {
		t.Error("192.168.1.1 exact should be trusted")
	}
	if g.IsTrustedProxy("11.0.0.1") {
		t.Error("11.0.0.1 not in any trusted prefix")
	}
	if g.IsTrustedProxy("not-an-ip") {
		t.Error("invalid IP should not be trusted")
	}
}

func mustParsePrefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		if p, err := netutil.ParseIPOrPrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}
