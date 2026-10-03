// Package netutil resolves client addresses behind reverse proxies.
package netutil

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ProxyTrust identifies proxies whose X-Forwarded-For header may be believed.
// With no trusted proxies the TCP peer address is always used, so clients can
// never spoof their IP to dodge rate limits.
type ProxyTrust struct{ prefixes []netip.Prefix }

func NewProxyTrust(prefixes []netip.Prefix) ProxyTrust { return ProxyTrust{prefixes: prefixes} }

func (p ProxyTrust) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, pfx := range p.prefixes {
		if pfx.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the originating address: the right-most entry of
// X-Forwarded-For that was not added by a trusted proxy.
func (p ProxyTrust) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if !p.trusted(peer) {
		return peer.Unmap().String()
	}

	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // malformed chain: stop trusting it
		}
		if !p.trusted(hop) {
			return hop.Unmap().String()
		}
	}
	return peer.Unmap().String()
}
