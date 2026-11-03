package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
)

const lookupTimeout = 5 * time.Second

// Resolver looks up a host's addresses; *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Registry holds each upstream's runtime state, its connection pool and
// resolved addresses, keyed by upstream name and kept outside any snapshot. A
// reload keeps the pools of the upstreams it still has (GW-29).
type Registry struct {
	allow    Allowlist
	resolver Resolver
	dialer   *net.Dialer
	log      *slog.Logger

	mu    sync.Mutex                       // serializes changes to pools
	pools atomic.Pointer[map[string]*pool] // read without locking on every request
}

// pool is one upstream's connection pool and resolved addresses.
type pool struct {
	name      string
	transport *http.Transport
	addrs     atomic.Pointer[[]string] // ip:port of every endpoint, for round robin
	next      atomic.Uint64

	mu        sync.Mutex // guards endpoints and resolved
	endpoints []string
	resolved  map[string][]string // per endpoint; kept when a lookup fails
}

// NewRegistry returns an empty registry. Every connection it opens is checked
// against allow.
func NewRegistry(allow Allowlist, resolver Resolver, log *slog.Logger) *Registry {
	r := &Registry{
		allow:    allow,
		resolver: resolver,
		dialer:   &net.Dialer{KeepAlive: 30 * time.Second, Control: allow.control},
		log:      log,
	}
	r.pools.Store(&map[string]*pool{})
	return r
}

// Apply reconciles the registry with a new snapshot before it is swapped in
// (config.Watcher's apply hook). It rejects endpoint IP addresses outside the
// allow-list (SEC-06), keeps the pools of upstreams that remain, re-resolves
// those whose endpoints changed, adds new ones and closes the idle connections
// of removed ones.
func (r *Registry) Apply(s *config.Snapshot) error {
	for _, up := range s.Upstreams {
		for _, ep := range up.Endpoints {
			host, _, _ := net.SplitHostPort(ep)
			if ip, err := netip.ParseAddr(host); err == nil && !r.allow.Allows(ip) {
				return fmt.Errorf("upstream %q endpoint %q: %w", up.Name, ep, ErrNotAllowed)
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	old := *r.pools.Load()
	pools := make(map[string]*pool, len(s.Upstreams))
	for name, up := range s.Upstreams {
		p := old[name]
		if p == nil {
			p = r.newPool(name)
		}
		r.setEndpoints(p, up.Endpoints)
		pools[name] = p
	}
	r.pools.Store(&pools)
	for name, p := range old {
		if pools[name] == nil {
			p.transport.CloseIdleConnections()
		}
	}
	return nil
}

// pool returns the upstream's pool. A request still on an older snapshot may
// name an upstream that a reload has since removed; it gets a pool built from
// its snapshot, which the next Apply drops.
func (r *Registry) pool(up *config.Upstream) *pool {
	if p := (*r.pools.Load())[up.Name]; p != nil {
		return p
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pools := *r.pools.Load()
	if p := pools[up.Name]; p != nil {
		return p
	}
	p := r.newPool(up.Name)
	r.setEndpoints(p, up.Endpoints)
	next := maps.Clone(pools)
	next[up.Name] = p
	r.pools.Store(&next)
	return p
}

// Run re-resolves every upstream's DNS names each interval until ctx ends
// (GW-06).
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refresh()
		}
	}
}

func (r *Registry) refresh() {
	for _, p := range *r.pools.Load() {
		p.mu.Lock()
		r.resolve(p)
		p.mu.Unlock()
	}
}

func (r *Registry) newPool(name string) *pool {
	p := &pool{name: name, transport: &http.Transport{
		Proxy:                  nil, // never HTTP_PROXY from the environment
		DialContext:            r.dial,
		MaxIdleConnsPerHost:    512,
		IdleConnTimeout:        90 * time.Second,
		ExpectContinueTimeout:  time.Second,
		DisableCompression:     true, // pass Content-Encoding through untouched
		MaxResponseHeaderBytes: 64 << 10,
	}}
	p.addrs.Store(&[]string{})
	return p
}

// dial opens an upstream connection within the connect timeout of the route
// whose request asked for it (GW-11).
func (r *Registry) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if timeout, ok := ctx.Value(connectTimeoutKey{}).(time.Duration); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return r.dialer.DialContext(ctx, network, address)
}

func (r *Registry) setEndpoints(p *pool, endpoints []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolved != nil && slices.Equal(p.endpoints, endpoints) {
		return
	}
	p.endpoints = endpoints
	r.resolve(p)
}

// resolve looks up every endpoint of p and publishes the allowed addresses. A
// failed lookup keeps that endpoint's previous addresses. p.mu must be held.
func (r *Registry) resolve(p *pool) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	resolved := make(map[string][]string, len(p.endpoints))
	var all []string
	for _, ep := range p.endpoints {
		addrs, err := r.lookup(ctx, p.name, ep)
		if err != nil {
			r.log.Warn("upstream DNS lookup failed; keeping the previous addresses", "upstream", p.name, "endpoint", ep, "err", err)
			addrs = p.resolved[ep]
		}
		resolved[ep] = addrs
		all = append(all, addrs...)
	}
	p.resolved = resolved
	p.addrs.Store(&all)
}

func (r *Registry) lookup(ctx context.Context, upstream, endpoint string) ([]string, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = r.resolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	addrs := []string{}
	for _, ip := range ips {
		if !r.allow.Allows(ip) {
			r.log.Warn("upstream address skipped: not in the allowed networks", "upstream", upstream, "endpoint", endpoint, "address", ip)
			continue
		}
		addrs = append(addrs, net.JoinHostPort(ip.Unmap().String(), port))
	}
	return addrs, nil
}

// errNoAddress means an upstream has no usable address right now.
var errNoAddress = errors.New("upstream has no usable address")

// pick returns the next address in round-robin order. Load balancing proper
// arrives in M4 (GW-07).
func (p *pool) pick() (string, error) {
	addrs := *p.addrs.Load()
	if len(addrs) == 0 {
		return "", errNoAddress
	}
	return addrs[(p.next.Add(1)-1)%uint64(len(addrs))], nil
}
