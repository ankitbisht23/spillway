package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/resilience"
)

const lookupTimeout = 5 * time.Second

// Resolver looks up a host's addresses; *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Registry holds each upstream's runtime state, keyed by upstream name and
// kept outside any snapshot: its connection pool, resolved addresses,
// settings, retry budget, circuit breaker and caps, and each endpoint's
// health and outlier state, keyed by the endpoint's resolved ip:port. A
// reload keeps the state of the upstreams and endpoints it still has (GW-29,
// ADR-0030). The registry is a Prometheus collector of
// gw_upstream_endpoint_healthy and gw_circuit_state.
type Registry struct {
	allow    Allowlist
	resolver Resolver
	dialer   *net.Dialer
	log      *slog.Logger
	clock    resilience.Clock
	probes   *http.Transport // health checks', apart from the request pools (ADR-0030)
	healthy  *prometheus.Desc
	circuit  *prometheus.Desc
	ctx      context.Context // probers run until it ends (Close)
	cancel   context.CancelFunc

	mu    sync.Mutex                       // serializes changes to pools
	pools atomic.Pointer[map[string]*pool] // read without locking on every request
}

// pool is one upstream's runtime state.
type pool struct {
	name      string
	log       *slog.Logger
	transport *http.Transport
	settings  atomic.Pointer[config.Upstream] // the latest snapshot's, changed in place on reload
	endpoints atomic.Pointer[[]*endpoint]     // one per resolved address, published after each resolve
	next      atomic.Uint64                   // round robin (GW-07)
	budget    *resilience.Budget              // retries (GW-13)
	breaker   *resilience.Breaker             // and its max_concurrent cap (GW-14)
	pending   resilience.Gate                 // max_pending: attempts waiting for a connection
	conns     resilience.Gate                 // max_connections: open connections of transport
	panicking atomic.Bool                     // no endpoint was selectable at the last pick
	ejectMu   sync.Mutex                      // one ejection decision at a time, so the cap holds
	probe     bool                            // runs probers: false for a pool rebuilt for an older snapshot; guarded by mu

	mu         sync.Mutex // guards configured, resolved and the endpoints' probers
	configured []string
	resolved   map[string][]string // per configured endpoint; kept when a lookup fails
}

// endpoint is one resolved address of an upstream, with its health,
// outlier state and attempts in flight (GW-07, GW-08, GW-09).
type endpoint struct {
	addr    string      // resolved ip:port: the endpoint's identity
	host    string      // the configured address it came from, e.g. upstream-1:8080
	healthy atomic.Bool // active health; true without health checks
	outlier resilience.Outlier
	// attempts sent to it and not finished, for p2c: from sending an
	// attempt until its response body is closed, or the attempt fails
	inFlight atomic.Int64

	mu        sync.Mutex // guards the probe counts
	successes int        // probes in a row
	failures  int
	lastErr   string // the last failed probe's reason

	stop context.CancelFunc // its prober, nil if none; guarded by the pool's mu
}

// NewRegistry returns an empty registry. Every connection it opens is checked
// against allow.
func NewRegistry(allow Allowlist, resolver Resolver, log *slog.Logger) *Registry {
	r := &Registry{
		allow:    allow,
		resolver: resolver,
		dialer:   &net.Dialer{KeepAlive: 30 * time.Second, Control: allow.control},
		log:      log,
		clock:    ratelimit.SystemClock,
		healthy: prometheus.NewDesc("gw_upstream_endpoint_healthy",
			"1 while the endpoint takes traffic (healthy and not ejected), else 0.", []string{"upstream", "endpoint"}, nil),
		circuit: prometheus.NewDesc("gw_circuit_state",
			"The upstream's circuit breaker: 0 closed, 1 open, 2 half-open.", []string{"upstream"}, nil),
	}
	r.probes = &http.Transport{
		Proxy:                  nil,
		DialContext:            r.dialer.DialContext,
		MaxIdleConnsPerHost:    1,
		IdleConnTimeout:        90 * time.Second,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.pools.Store(&map[string]*pool{})
	return r
}

// Close stops every health prober.
func (r *Registry) Close() {
	r.cancel()
	r.probes.CloseIdleConnections()
}

// Apply reconciles the registry with a new snapshot before it is swapped in
// (config.Watcher's apply hook). It rejects endpoint IP addresses outside the
// allow-list (SEC-06), keeps the state of upstreams that remain and stores
// their new settings, re-resolves those whose endpoints changed, starts or
// stops their probers as health checks come and go, adds new upstreams, and
// for removed ones closes the idle connections and stops the probers.
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
			p = r.newPool(up)
		}
		r.setUpstream(p, up, true)
		pools[name] = p
	}
	r.pools.Store(&pools)
	for name, p := range old {
		if pools[name] == nil {
			p.transport.CloseIdleConnections()
			p.mu.Lock()
			p.probe = false // a DNS refresh still holding the old map must not restart them
			for _, e := range *p.endpoints.Load() {
				e.stopProbe()
			}
			p.mu.Unlock()
		}
	}
	return nil
}

// pool returns the upstream's pool. A request still on an older snapshot may
// name an upstream that a reload has since removed; it gets a pool built from
// its snapshot, without health checks, which the next Apply drops. That
// pool's names are resolved before taking r.mu, so the lookups never hold up
// a reload.
func (r *Registry) pool(up *config.Upstream) *pool {
	if p := (*r.pools.Load())[up.Name]; p != nil {
		return p
	}
	p := r.newPool(up)
	r.setUpstream(p, up, false)
	r.mu.Lock()
	defer r.mu.Unlock()
	pools := *r.pools.Load()
	if p := pools[up.Name]; p != nil {
		return p
	}
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

// newPool returns fresh state for up; setUpstream stores its settings.
func (r *Registry) newPool(up *config.Upstream) *pool {
	p := &pool{name: up.Name, log: r.log, budget: resilience.NewBudget()}
	p.breaker = resilience.NewBreaker(up.Circuit.Window, p.logCircuit)
	p.transport = &http.Transport{
		Proxy: nil, // never HTTP_PROXY from the environment
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return r.dialCapped(ctx, p, network, addr)
		},
		MaxIdleConnsPerHost:    512,
		IdleConnTimeout:        90 * time.Second,
		ExpectContinueTimeout:  time.Second,
		DisableCompression:     true, // pass Content-Encoding through untouched
		MaxResponseHeaderBytes: 64 << 10,
	}
	p.endpoints.Store(&[]*endpoint{})
	return p
}

// Refusals by an upstream's caps (GW-14, ADR-0030): the gateway's own doing,
// answered 503 and never counted as the upstream's failures.
var (
	errConnLimit    = errors.New("upstream max_connections reached")
	errPendingLimit = errors.New("upstream max_pending reached")
)

// dialCapped opens a connection of p's pool unless p has max_connections
// open, and counts it until it is closed.
func (r *Registry) dialCapped(ctx context.Context, p *pool, network, address string) (net.Conn, error) {
	if !p.conns.TryAcquire(int64(p.settings.Load().Circuit.MaxConnections)) {
		return nil, errConnLimit
	}
	c, err := r.dial(ctx, network, address)
	if err != nil {
		p.conns.Release()
		return nil, err
	}
	return &countedConn{Conn: c, release: sync.OnceFunc(p.conns.Release)}, nil
}

// countedConn releases its place in a gate when it is first closed.
type countedConn struct {
	net.Conn
	release func()
}

func (c *countedConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// CloseWrite lets net/http half-close a client connection the connection
// cap counts (GW-26), as it does a *net.TCPConn.
func (c *countedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// logCircuit logs a transition of p's breaker (ADR-0030).
func (p *pool) logCircuit(t resilience.Transition) {
	switch t.To {
	case resilience.Open:
		p.log.Warn("upstream circuit open", "upstream", p.name, "from", t.From.String(), "error_rate", float64(t.Failed)/float64(t.Total), "requests", t.Total)
	case resilience.HalfOpen:
		p.log.Info("upstream circuit half-open", "upstream", p.name)
	case resilience.Closed:
		p.log.Info("upstream circuit closed", "upstream", p.name)
	}
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

// setUpstream stores up as p's settings, re-resolves p if its endpoints
// changed, and starts or stops probers to match its health check; probe is
// false for a pool rebuilt for an older snapshot.
func (r *Registry) setUpstream(p *pool, up *config.Upstream, probe bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settings.Store(up)
	p.breaker.SetWindow(up.Circuit.Window)
	p.probe = probe
	if p.resolved == nil || !slices.Equal(p.configured, up.Endpoints) {
		p.configured = up.Endpoints
		r.resolve(p)
		return
	}
	r.syncProbes(p)
}

// resolve looks up every configured endpoint of p and publishes one endpoint
// per allowed address. An address p already had keeps its endpoint and so
// its state; a new one starts healthy and not ejected; a removed one's prober
// stops (GW-29). A failed lookup keeps that endpoint's previous addresses.
// p.mu must be held.
func (r *Registry) resolve(p *pool) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	old := map[string]*endpoint{}
	for _, e := range *p.endpoints.Load() {
		old[e.addr] = e
	}
	resolved := make(map[string][]string, len(p.configured))
	var eps []*endpoint
	seen := map[string]bool{}
	for _, ep := range p.configured {
		addrs, err := r.lookup(ctx, p.name, ep)
		if err != nil {
			r.log.Warn("upstream DNS lookup failed; keeping the previous addresses", "upstream", p.name, "endpoint", ep, "err", err)
			addrs = p.resolved[ep]
		}
		resolved[ep] = addrs
		for _, addr := range addrs {
			if seen[addr] {
				continue // two configured names for one address are one endpoint
			}
			seen[addr] = true
			e := old[addr]
			if e == nil {
				e = &endpoint{addr: addr, host: ep}
				e.healthy.Store(true)
			}
			eps = append(eps, e)
		}
	}
	p.resolved = resolved
	p.endpoints.Store(&eps)
	for addr, e := range old {
		if !seen[addr] {
			e.stopProbe() // a request holding it finishes on it
		}
	}
	r.syncProbes(p)
}

// syncProbes starts a prober for each endpoint of p that needs one and has
// none, and when p has no health check stops every prober and marks its
// endpoint healthy. p.mu must be held.
func (r *Registry) syncProbes(p *pool) {
	hc := p.settings.Load().HealthCheck
	for _, e := range *p.endpoints.Load() {
		switch {
		case hc != nil && p.probe && e.stop == nil:
			ctx, cancel := context.WithCancel(r.ctx)
			e.stop = cancel
			go r.prober(ctx, p, e)
		case hc == nil && e.stop != nil:
			e.stopProbe()
			e.mu.Lock()
			e.healthy.Store(true)
			e.successes, e.failures, e.lastErr = 0, 0, ""
			e.mu.Unlock()
		}
	}
}

// stopProbe stops e's prober, if any. The pool's mu must be held.
func (e *endpoint) stopProbe() {
	if e.stop != nil {
		e.stop()
		e.stop = nil
	}
}

// prober probes e until ctx ends (GW-08, ADR-0030): first after a random delay
// within one interval, so that probes do not line up across endpoints and
// replicas, then once per interval from the start of the previous probe. It
// reads the upstream's health check before each probe, so a reload's new
// interval, timeout or path applies from the next one.
func (r *Registry) prober(ctx context.Context, p *pool, e *endpoint) {
	hc := p.settings.Load().HealthCheck
	if hc == nil {
		return
	}
	t := time.NewTimer(rand.N(hc.Interval)) //nolint:gosec // G404: spreads probes out, not security
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		if hc = p.settings.Load().HealthCheck; hc == nil {
			return
		}
		e.observe(ctx, r.probe(ctx, e, hc), hc, p.name, r.log)
		t.Reset(time.Until(start.Add(hc.Interval)))
	}
}

// probe sends one health check to e: GET path with e's configured address
// as Host. A 2xx or 3xx within the timeout is a success; it reads at most 4
// KiB of the body.
func (r *Registry) probe(ctx context.Context, e *endpoint, hc *config.HealthCheck) error {
	ctx, cancel := context.WithTimeout(ctx, hc.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+e.addr+hc.Path, nil)
	if err != nil {
		return err
	}
	req.Host = e.host
	req.Header.Set("User-Agent", "spillway-health-check")
	resp, err := r.probes.RoundTrip(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// observe counts a probe's result, err nil for a success: after hc.Unhealthy
// failures in a row a healthy endpoint becomes unhealthy, and after
// hc.Healthy successes in a row an unhealthy one becomes healthy. A result
// that arrives after its prober was stopped is dropped.
func (e *endpoint) observe(ctx context.Context, err error, hc *config.HealthCheck, upstream string, log *slog.Logger) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		e.failures = 0
		e.successes++
		if !e.healthy.Load() && e.successes >= hc.Healthy {
			e.healthy.Store(true)
			log.Info("upstream endpoint healthy", "upstream", upstream, "endpoint", e.addr, "reason", e.lastErr)
		}
		return
	}
	e.successes = 0
	e.failures++
	e.lastErr = err.Error()
	if e.healthy.Load() && e.failures >= hc.Unhealthy {
		e.healthy.Store(false)
		log.Warn("upstream endpoint unhealthy", "upstream", upstream, "endpoint", e.addr, "reason", e.lastErr)
	}
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

// selectable reports whether e takes traffic at now: healthy and not ejected
// (ADR-0030).
func (e *endpoint) selectable(now time.Time) bool {
	return e.healthy.Load() && !e.outlier.Ejected(now)
}

// pick returns the endpoint for an attempt at now with the upstream's
// algorithm (GW-07, ADR-0030), among the candidates: the selectable
// endpoints that tried does not hold, or all the selectable ones if it holds
// every one. When none is selectable, it is in panic mode: it ignores health
// and uses the endpoints that are not ejected, or all of them if every one
// is. key is the request's filled hash_on template for consistent_hash; ""
// when it could not be filled, and the request is then balanced with p2c.
func (p *pool) pick(now time.Time, key string, tried []*endpoint) (*endpoint, error) {
	eps := *p.endpoints.Load()
	if len(eps) == 0 {
		return nil, errNoAddress
	}
	var buf [16]*endpoint // upstreams hold a few endpoints: no allocation
	cands := p.candidates(buf[:0], eps, now)
	if len(tried) > 0 && slices.ContainsFunc(cands, func(e *endpoint) bool { return !slices.Contains(tried, e) }) {
		cands = slices.DeleteFunc(cands, func(e *endpoint) bool { return slices.Contains(tried, e) })
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	switch lb := p.settings.Load().LB; {
	case lb == config.LBRoundRobin:
		return cands[(p.next.Add(1)-1)%uint64(len(cands))], nil
	case lb == config.LBConsistentHash && key != "":
		return rendezvous(cands, key), nil
	}
	// p2c: two distinct candidates drawn at random; the one with fewer
	// attempts in flight wins, a tie goes to the first drawn.
	i := rand.N(len(cands))     //nolint:gosec // G404: load balancing, not security
	j := rand.N(len(cands) - 1) //nolint:gosec // G404: load balancing, not security
	if j >= i {
		j++
	}
	if a, b := cands[i], cands[j]; b.inFlight.Load() < a.inFlight.Load() {
		return b, nil
	}
	return cands[i], nil
}

// candidates appends to buf the endpoints of eps selectable at now, or in
// panic mode the ones not ejected, or all of them (ADR-0030).
func (p *pool) candidates(buf, eps []*endpoint, now time.Time) []*endpoint {
	for _, e := range eps {
		if e.selectable(now) {
			buf = append(buf, e)
		}
	}
	if len(buf) > 0 {
		if p.panicking.CompareAndSwap(true, false) {
			p.log.Info("upstream has a healthy endpoint again; health checks apply", "upstream", p.name)
		}
		return buf
	}
	if p.panicking.CompareAndSwap(false, true) {
		p.log.Warn("upstream has no healthy endpoint; ignoring health checks", "upstream", p.name)
	}
	for _, e := range eps {
		if !e.ejected(now) {
			buf = append(buf, e)
		}
	}
	if len(buf) > 0 {
		return buf
	}
	return append(buf, eps...)
}

// rendezvous returns the candidate with the highest score
// xxhash64(key + "\x00" + addr) (rendezvous hashing, ADR-0030): every
// replica picks the same endpoint for a key, and when an endpoint leaves the
// candidates only its keys move, each to its next-highest endpoint.
func rendezvous(cands []*endpoint, key string) *endpoint {
	var best *endpoint
	var top uint64
	var d xxhash.Digest
	for _, e := range cands {
		d.Reset()
		_, _ = d.WriteString(key)
		_, _ = d.WriteString("\x00")
		_, _ = d.WriteString(e.addr)
		if score := d.Sum64(); best == nil || score > top {
			best, top = e, score
		}
	}
	return best
}

// begin counts an attempt in flight on e (p2c) until the returned release
// is called; release may be called any number of times, and counts once.
func (e *endpoint) begin() (release func()) {
	e.inFlight.Add(1)
	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			e.inFlight.Add(-1)
		}
	}
}

func count(eps []*endpoint, now time.Time, f func(*endpoint, time.Time) bool) int {
	n := 0
	for _, e := range eps {
		if f(e, now) {
			n++
		}
	}
	return n
}

// record notes the outcome of an attempt sent to e (GW-09, ADR-0030): the
// consecutive_failures-th failure in a row ejects e, unless that would eject
// more than max_ejected_percent of the upstream's endpoints, rounded down.
func (p *pool) record(e *endpoint, now time.Time, failed bool) {
	s := &p.settings.Load().Outlier
	if !e.outlier.Record(now, failed, s) {
		return
	}
	p.ejectMu.Lock()
	defer p.ejectMu.Unlock()
	eps := *p.endpoints.Load()
	if !slices.Contains(eps, e) {
		return // removed by a reload or a DNS change
	}
	if count(eps, now, (*endpoint).ejected)+1 > len(eps)*s.MaxEjectedPercent/100 {
		p.log.Info("upstream endpoint ejection skipped: max_ejected_percent reached", "upstream", p.name, "endpoint", e.addr)
		return
	}
	if d := e.outlier.Eject(now, s); d > 0 {
		p.log.Warn("upstream endpoint ejected", "upstream", p.name, "endpoint", e.addr, "for", d, "times", e.outlier.State().Times)
	}
}

func (e *endpoint) ejected(now time.Time) bool { return e.outlier.Ejected(now) }

// Describe and Collect make the registry a Prometheus collector of
// gw_upstream_endpoint_healthy, one series per endpoint while it exists, and
// gw_circuit_state, one per upstream.
func (r *Registry) Describe(ch chan<- *prometheus.Desc) {
	ch <- r.healthy
	ch <- r.circuit
}

func (r *Registry) Collect(ch chan<- prometheus.Metric) {
	now := r.clock.Now()
	for _, p := range *r.pools.Load() {
		ch <- prometheus.MustNewConstMetric(r.circuit, prometheus.GaugeValue, float64(p.breaker.State()), p.name)
		for _, e := range *p.endpoints.Load() {
			v := 0.0
			if e.selectable(now) {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(r.healthy, prometheus.GaugeValue, v, p.name, e.addr)
		}
	}
}
