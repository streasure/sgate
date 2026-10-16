package gateway

import "testing"

func TestParseProxyProtocolV1(t *testing.T) {
	cases := []struct {
		name       string
		data       string
		wantIP     string
		wantHdrLen int
		wantOK     bool
	}{
		{"tcp4", "PROXY TCP4 192.168.0.1 192.168.0.11 56324 443\r\n", "192.168.0.1", 47, true},
		{"tcp6", "PROXY TCP6 2001:db8::1 2001:db8::2 56324 443\r\n", "2001:db8::1", 46, true},
		{"unknown", "PROXY UNKNOWN\r\n", "", 15, true},
		{"not proxy", "GET / HTTP/1.1\r\n", "", 0, false},
		{"no crlf", "PROXY TCP4 1.2.3.4 5.6.7.8 1 2", "", 0, false},
		{"bad ip", "PROXY TCP4 not-an-ip 5.6.7.8 1 2\r\n", "", 0, false},
		{"bad proto", "PROXY UDP4 1.2.3.4 5.6.7.8 1 2\r\n", "", 0, false},
		{"empty", "", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, hdrLen, ok := parseProxyProtocolV1([]byte(tc.data))
			if ok != tc.wantOK || ip != tc.wantIP || hdrLen != tc.wantHdrLen {
				t.Errorf("got (ip=%q len=%d ok=%v), want (ip=%q len=%d ok=%v)", ip, hdrLen, ok, tc.wantIP, tc.wantHdrLen, tc.wantOK)
			}
		})
	}
}
