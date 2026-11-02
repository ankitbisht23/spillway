// Package redis holds Spillway's Redis Cluster client code.
package redis

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// AddrMapEnv turns on the address map for processes run on the host in dev and
// tests (ADR-0004). It is never set in Helm.
const AddrMapEnv = "SPILLWAY_REDIS_ADDR_MAP"

// ParseAddrMap parses "from=to,from=to", where each side is host:port, for
// example "redis-0:6379=127.0.0.1:7000". An empty string turns the map off and
// returns nil.
func ParseAddrMap(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	m := make(map[string]string)
	for _, entry := range strings.Split(s, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			return nil, fmt.Errorf("%s: entry %q is not from=to", AddrMapEnv, entry)
		}
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		for _, addr := range []string{from, to} {
			if host, port, err := net.SplitHostPort(addr); err != nil || host == "" || port == "" {
				return nil, fmt.Errorf("%s: %q is not host:port", AddrMapEnv, addr)
			}
		}
		if _, dup := m[from]; dup {
			return nil, fmt.Errorf("%s: %q is mapped twice", AddrMapEnv, from)
		}
		m[from] = to
	}
	return m, nil
}

// Dialer returns a go-redis Options.Dialer that dials m[addr] in place of addr
// when addr is mapped, and addr itself otherwise. Redis Cluster nodes announce
// hostnames (redis-N) that resolve only inside the compose network; the map
// sends those to the ports published on the host.
func Dialer(m map[string]string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	// Same timeouts as go-redis's default dialer.
	d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 5 * time.Minute}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if to, ok := m[addr]; ok {
			addr = to
		}
		return d.DialContext(ctx, network, addr)
	}
}
