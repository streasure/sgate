package transport

import (
	"testing"
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
