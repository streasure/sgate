package gateway

import (
	"net/netip"
	"testing"

	"github.com/streasure/util/netutil"
)

func TestResolveRealClientIP(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"empty", map[string]string{}, ""},
		{"x-real-ip", map[string]string{"x-real-ip": "203.0.113.5"}, "203.0.113.5"},
		{"xff single", map[string]string{"x-forwarded-for": "203.0.113.5"}, "203.0.113.5"},
		{"xff chain leftmost", map[string]string{"x-forwarded-for": "203.0.113.5, 198.51.100.1, 192.0.2.1"}, "203.0.113.5"},
		{"x-real-ip preferred over xff", map[string]string{"x-real-ip": "10.0.0.1", "x-forwarded-for": "203.0.113.5"}, "10.0.0.1"},
		{"invalid x-real-ip falls to xff", map[string]string{"x-real-ip": "not-an-ip", "x-forwarded-for": "203.0.113.5"}, "203.0.113.5"},
		{"invalid xff", map[string]string{"x-forwarded-for": "evil, not-an-ip"}, ""},
		{"ipv6", map[string]string{"x-real-ip": "2001:db8::1"}, "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRealClientIP(tc.headers); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

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
