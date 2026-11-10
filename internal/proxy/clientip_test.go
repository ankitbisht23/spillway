package proxy

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	for _, tt := range []struct {
		in, want string // want: the parsed prefixes, comma-separated
	}{
		{"", ""},
		{" , ", ""},
		{"10.0.0.0/8", "10.0.0.0/8"},
		{" 10.1.2.3/8 , 2001:db8::/32 ", "10.0.0.0/8,2001:db8::/32"},
		{"::ffff:10.0.0.0/104", "10.0.0.0/8"}, // IPv4-mapped: matches the IPv4 addresses
		{"192.0.2.1/32", "192.0.2.1/32"},
	} {
		got, err := ParseTrustedProxies(tt.in)
		var s []string
		for _, p := range got {
			s = append(s, p.String())
		}
		if err != nil || strings.Join(s, ",") != tt.want {
			t.Errorf("ParseTrustedProxies(%q) = %v, %v; want %s", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"10.0.0.1", "10.0.0.0/33", "proxy.example/24", "10.0.0.0/8,nope"} {
		if _, err := ParseTrustedProxies(in); err == nil || !strings.Contains(err.Error(), "want a CIDR") {
			t.Errorf("ParseTrustedProxies(%q) error = %v, want a CIDR error", in, err)
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.0.0.0/8, 2001:db8:1::/48")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		trusted TrustedProxies
		peer    string
		xff     []string
		want    string
	}{
		{"no trusted proxies: X-Forwarded-For ignored", nil, "203.0.113.7", []string{"198.51.100.1"}, "203.0.113.7"},
		{"untrusted peer: X-Forwarded-For ignored", trusted, "203.0.113.7", []string{"198.51.100.1"}, "203.0.113.7"},
		{"trusted peer without X-Forwarded-For", trusted, "10.0.0.1", nil, "10.0.0.1"},
		{"trusted peer, one entry", trusted, "10.0.0.1", []string{"198.51.100.1"}, "198.51.100.1"},
		{"chain of trusted proxies", trusted, "10.0.0.1", []string{"198.51.100.1, 10.2.0.1, 10.3.0.1"}, "198.51.100.1"},
		{"spoofed left-most entries", trusted, "10.0.0.1", []string{"1.1.1.1, 2.2.2.2, 198.51.100.1, 10.2.0.1"}, "198.51.100.1"},
		{"several field lines, joined in order", trusted, "10.0.0.1", []string{"1.1.1.1", "198.51.100.1", "10.2.0.1"}, "198.51.100.1"},
		{"every entry trusted: the left-most", trusted, "10.0.0.1", []string{"10.9.0.1, 10.2.0.1"}, "10.9.0.1"},
		{"malformed entry left of the client is never read", trusted, "10.0.0.1", []string{"not-an-ip, 198.51.100.1"}, "198.51.100.1"},
		{"malformed entry: the peer", trusted, "10.0.0.1", []string{"198.51.100.1, not-an-ip"}, "10.0.0.1"},
		{"entry with a port: the peer", trusted, "10.0.0.1", []string{"198.51.100.1:443"}, "10.0.0.1"},
		{"bracketed IPv6 entry: the peer", trusted, "10.0.0.1", []string{"[2001:db8::1]"}, "10.0.0.1"},
		{"IPv6 entry with a zone: the peer", trusted, "10.0.0.1", []string{"fe80::1%eth0"}, "10.0.0.1"},
		{"empty entry: the peer", trusted, "10.0.0.1", []string{"198.51.100.1,,10.2.0.1"}, "10.0.0.1"},
		{"empty field line: the peer", trusted, "10.0.0.1", []string{""}, "10.0.0.1"},
		{"IPv6 peer and entries", trusted, "2001:db8:1::5", []string{"2001:db8:9::1, 2001:db8:1::6"}, "2001:db8:9::1"},
		{"IPv6 untrusted peer", trusted, "2001:db8:2::5", []string{"198.51.100.1"}, "2001:db8:2::5"},
		{"IPv4-mapped peer is trusted as IPv4", trusted, "::ffff:10.0.0.1", []string{"198.51.100.1"}, "198.51.100.1"},
		{"IPv4-mapped untrusted peer comes back as IPv4", trusted, "::ffff:203.0.113.7", nil, "203.0.113.7"},
		{"IPv4-mapped entries are normalised", trusted, "10.0.0.1", []string{"::ffff:198.51.100.1, ::ffff:10.2.0.1"}, "198.51.100.1"},
		{"peer with a zone", trusted, "fe80::1%eth0", []string{"198.51.100.1"}, "fe80::1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trusted.ClientIP(netip.MustParseAddr(tt.peer), tt.xff); got.String() != tt.want {
				t.Errorf("ClientIP(%s, %q) = %s, want %s", tt.peer, tt.xff, got, tt.want)
			}
		})
	}
	// A peer address that did not parse (no TCP peer) is never trusted.
	if got := trusted.ClientIP(netip.Addr{}, []string{"198.51.100.1"}); got.IsValid() {
		t.Errorf("ClientIP(invalid peer) = %s, want the invalid address", got)
	}
}
