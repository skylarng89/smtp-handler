package config

import "net/netip"

// parseCIDROrIP accepts either a CIDR ("10.0.0.0/8") or a bare IP.
func parseCIDROrIP(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ParseTrustedProxies converts validated proxy strings to prefixes.
func ParseTrustedProxies(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := parseCIDROrIP(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
