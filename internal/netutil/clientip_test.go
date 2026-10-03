package netutil

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	trust := NewProxyTrust([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	none := NewProxyTrust(nil)

	tests := []struct {
		name  string
		trust ProxyTrust
		peer  string
		xff   string
		want  string
	}{
		{"no proxies configured ignores XFF", none, "203.0.113.9:1234", "1.2.3.4", "203.0.113.9"},
		{"untrusted peer cannot spoof", trust, "203.0.113.9:1234", "1.2.3.4", "203.0.113.9"},
		{"trusted proxy single hop", trust, "10.0.0.5:80", "198.51.100.7", "198.51.100.7"},
		{"client-supplied prefix is ignored", trust, "10.0.0.5:80", "1.1.1.1, 198.51.100.7", "198.51.100.7"},
		{"chain of trusted proxies", trust, "10.0.0.5:80", "198.51.100.7, 10.1.1.1", "198.51.100.7"},
		{"malformed header falls back to peer", trust, "10.0.0.5:80", "garbage", "10.0.0.5"},
		{"no header", trust, "10.0.0.5:80", "", "10.0.0.5"},
		{"ipv6 mapped", trust, "[::ffff:10.0.0.5]:80", "198.51.100.7", "198.51.100.7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.peer, Header: http.Header{}}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := tc.trust.ClientIP(r); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
