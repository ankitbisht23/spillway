package proxy

import (
	"fmt"
	"net/netip"
	"strings"
)

// TrustedProxies are the networks of the proxies in front of the gateway,
// whose X-Forwarded-For is believed (SEC-05). Like the Allowlist, they come
// from the gateway's settings, never from a config snapshot.
type TrustedProxies []netip.Prefix

// ParseTrustedProxies parses comma-separated CIDRs; an empty string means no
// trusted proxies, so X-Forwarded-For is ignored.
func ParseTrustedProxies(s string) (TrustedProxies, error) {
	var t TrustedProxies
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: want a CIDR such as 10.0.0.0/8", f)
		}
		// An IPv4-mapped prefix matches the IPv4 addresses it maps, since
		// addresses are unmapped before matching.
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		t = append(t, p.Masked())
	}
	return t, nil
}

func (t TrustedProxies) trusts(ip netip.Addr) bool {
	for _, p := range t {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP is the IP of the client that sent a request, from the TCP peer
// address and the request's X-Forwarded-For field lines (SEC-05, RL-04,
// ADR-0017). X-Forwarded-For counts only when the peer is a trusted proxy:
// then its entries are walked right to left, past trusted proxies, and the
// first untrusted one is the client, or the leftmost if all are trusted. An
// entry that is not a bare IP address makes the peer the client. IPv4-mapped
// IPv6 addresses come back as IPv4.
func (t TrustedProxies) ClientIP(peer netip.Addr, xff []string) netip.Addr {
	peer = peer.Unmap().WithZone("")
	if len(xff) == 0 || !t.trusts(peer) {
		return peer
	}
	entries := strings.Split(strings.Join(xff, ","), ",")
	var ip netip.Addr
	for i := len(entries) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(entries[i]))
		if err != nil || a.Zone() != "" {
			return peer
		}
		if ip = a.Unmap(); !t.trusts(ip) {
			return ip
		}
	}
	return ip
}
