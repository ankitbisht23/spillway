// Package proxy routes requests on the current config snapshot and proxies
// them to upstreams through per-upstream connection pools.
package proxy

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"
)

// ErrNotAllowed is returned for an upstream address outside the allowed
// networks (SEC-06).
var ErrNotAllowed = errors.New("address is not in the allowed upstream networks")

// alwaysDenied holds link-local and cloud metadata addresses, which are
// rejected even when an allowed network contains them (SEC-06).
var alwaysDenied = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),     // IPv4 link-local, incl. 169.254.169.254 (AWS, GCP, Azure metadata)
	netip.MustParsePrefix("fe80::/10"),          // IPv6 link-local
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS metadata over IPv6
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
}

// Allowlist is the set of networks that upstream addresses must be in. It
// comes from the gateway's settings, never from a config snapshot, so a config
// change can't widen it (ADR-0008).
type Allowlist []netip.Prefix

// ParseAllowlist parses comma-separated CIDRs, for example
// "172.29.0.0/16,10.0.0.0/8".
func ParseAllowlist(s string) (Allowlist, error) {
	var a Allowlist
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("allowed network %q: want a CIDR such as 10.0.0.0/8", f)
		}
		a = append(a, p.Masked())
	}
	if len(a) == 0 {
		return nil, errors.New("no allowed networks")
	}
	return a, nil
}

// Allows reports whether an upstream may be at ip.
func (a Allowlist) Allows(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	for _, p := range alwaysDenied {
		if p.Contains(ip) {
			return false
		}
	}
	for _, p := range a {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// control is a net.Dialer Control function. It runs after resolution and
// before connect, so every connection is checked against the allow-list,
// whatever DNS returned (SEC-06).
func (a Allowlist) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !a.Allows(ap.Addr()) {
		return fmt.Errorf("dial %s: %w", address, ErrNotAllowed)
	}
	return nil
}
