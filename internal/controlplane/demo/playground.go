package demo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ankitbisht23/spillway/internal/secret"
)

// The Playground (UI-07, SC-04, ADR-0096 §3): runs of traffic that the
// control plane sends itself, open model, round robin over the demo
// gateways, with one Playground key per tenant that never leaves its
// memory. Each run belongs to the caller who started it, sends for at
// most a minute and is capped per run, per caller, per client IP and in
// total; its results go to its owner over a per-run SSE stream. Nothing
// here switches a fault, changes config or identity (beyond issuing its
// own keys) or blocks anything.

// The bounds of ADR-0096 §3.
const (
	MaxRate          = 200              // a custom run's highest rate, per second
	MinDuration      = 5 * time.Second  // a custom run's shortest duration
	MaxDuration      = 60 * time.Second // a custom run's longest duration
	DefaultDuration  = 30 * time.Second // a custom run's duration when none is given
	ScenarioDuration = 60 * time.Second // every scenario run's duration

	perIPActive   = 1   // active runs per client IP
	perIPHour     = 30  // starts per client IP per rolling hour
	perSubActive  = 3   // active runs per caller
	perSubHour    = 120 // starts per caller per rolling hour
	totalActive   = 3   // active runs in all
	totalScenario = 1   // active scenario runs in all
	totalRate     = 700 // the active runs' rates in all, per second

	maxInFlight    = 256             // requests in flight per run; one due past it is dropped
	requestTimeout = 5 * time.Second // per request
	keepResults    = 5 * time.Minute // a run's results after it ends
	streamsPerRun  = 2
	streamsTotal   = 50
	streamRetry    = 5 // seconds, the Retry-After of a refused stream

	keyRefresh = 5 * time.Minute  // a key with less left is replaced before a run
	keyReady   = 10 * time.Second // how long a run waits for gateways to accept a new key

	tick         = time.Second
	keepalive    = 15 * time.Second
	writeTimeout = 10 * time.Second
	hour         = time.Hour
)

// The run kinds, tenants, routes and scenarios.
const (
	KindCustom   = "custom"
	KindScenario = "scenario"
)

var (
	PlaygroundTenants   = []string{"free-co", "pro-co", "enterprise-co"}
	PlaygroundRoutes    = []string{"orders", "catalog", "payments"}
	PlaygroundScenarios = []string{"d1", "d2", "d3"}
)

// Load is one tenant's traffic on one route at a rate per second.
type Load struct {
	Tenant string
	Route  string
	Rate   int
}

// Shape is what a run sends.
type Shape struct {
	Kind     string
	Scenario string // a scenario run's
	Duration time.Duration
	Loads    []Load
}

// CustomShape is one tenant's traffic on one route.
func CustomShape(tenant, route string, rate int, d time.Duration) Shape {
	return Shape{Kind: KindCustom, Duration: d, Loads: []Load{{tenant, route, rate}}}
}

// scenarios are ADR-0096 §2's D1–D3 shapes on the Playground consumers.
var scenarios = map[string][]Load{
	"d1": {{"free-co", "orders", 30}, {"pro-co", "orders", 50}},
	"d2": {{"free-co", "orders", 500}, {"pro-co", "orders", 50}, {"enterprise-co", "orders", 100}},
	"d3": {{"pro-co", "payments", 200}},
}

// ScenarioShape is scenario name's shape, false for no such scenario.
func ScenarioShape(name string) (Shape, bool) {
	loads, ok := scenarios[name]
	return Shape{Kind: KindScenario, Scenario: name, Duration: ScenarioDuration, Loads: slices.Clone(loads)}, ok
}

// Rate is the shape's requests per second in all.
func (s Shape) Rate() int {
	n := 0
	for _, l := range s.Loads {
		n += l.Rate
	}
	return n
}

// tenants are the shape's tenants, in PlaygroundTenants' order.
func (s Shape) tenants() []string {
	var out []string
	for _, t := range PlaygroundTenants {
		if slices.ContainsFunc(s.Loads, func(l Load) bool { return l.Tenant == t }) {
			out = append(out, t)
		}
	}
	return out
}

// KeyStore issues the Playground's keys: one per tenant, for its
// <tenant>-playground consumer (ADR-0095 §3).
type KeyStore interface {
	// PlaygroundConsumer is tenant's Playground consumer's ID, or
	// ErrNoConsumer.
	PlaygroundConsumer(ctx context.Context, tenant string) (int64, error)
	// IssueKey issues a key for the consumer that expires at expires and
	// returns its ID and the key.
	IssueKey(ctx context.Context, consumerID int64, expires *time.Time) (int64, secret.Value, error)
}

var (
	// ErrNoConsumer is a tenant without its Playground consumer.
	ErrNoConsumer = errors.New("the demo seed's playground consumers are missing (make demo-reset restores them)")
	// ErrGatewaysDown is every demo gateway failing its first request.
	ErrGatewaysDown = errors.New("no demo gateway answers")
	// ErrNoRun is a run that does not exist or is not the caller's.
	ErrNoRun = errors.New("no such run")
)

// CapError is a run or stream refused by a cap: 429 with Retry-After.
type CapError struct {
	Msg        string
	RetryAfter int // whole seconds
}

func (e *CapError) Error() string { return e.Msg }

// Caller is who starts a run: a token's sub and the client IP the caps
// count it under (per /64 for IPv6). The IP is never logged.
type Caller struct {
	Sub string
	IP  string
}

// PlaygroundConfig is a Playground's setup.
type PlaygroundConfig struct {
	Gateways []string // base URLs, round robin
	Keys     Keys     // the 30-minute rule every issued key meets
	Store    KeyStore
	Log      *slog.Logger
}

// Playground runs the runs.
type Playground struct {
	gateways []gateway
	keys     Keys
	store    KeyStore
	log      *slog.Logger
	probe    *http.Client
	ctx      context.Context // ends at Shutdown
	stop     context.CancelFunc

	mu      sync.Mutex
	runs    map[string]*Run
	starts  map[string][]time.Time // by "sub:" or "ip:" key, the rolling hour's starts
	streams int
	closed  bool

	keyMu    sync.Mutex
	tenantKs map[string]tenantKey
}

type gateway struct{ base, name string }

type tenantKey struct {
	id      int64
	key     secret.Value
	expires time.Time
}

// NewPlayground returns a Playground over cfg.
func NewPlayground(cfg PlaygroundConfig) *Playground {
	ctx, stop := context.WithCancel(context.Background())
	p := &Playground{
		keys: cfg.Keys, store: cfg.Store, log: cfg.Log, ctx: ctx, stop: stop,
		probe: &http.Client{Timeout: requestTimeout},
		runs:  map[string]*Run{}, starts: map[string][]time.Time{}, tenantKs: map[string]tenantKey{},
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	hosts := map[string]int{}
	for _, g := range cfg.Gateways {
		if u, err := url.Parse(g); err == nil {
			hosts[u.Hostname()]++
		}
	}
	for _, g := range cfg.Gateways {
		name := g
		if u, err := url.Parse(g); err == nil {
			name = u.Host
			if hosts[u.Hostname()] == 1 {
				name = u.Hostname()
			}
		}
		p.gateways = append(p.gateways, gateway{base: g, name: name})
	}
	return p
}

// now is the caps' and the runs' clock.
func (p *Playground) now() time.Time { return time.Now() }

// keyNow is the keys' clock, the 30-minute rule's.
func (p *Playground) keyNow() time.Time {
	if p.keys.Now != nil {
		return p.keys.Now()
	}
	return time.Now()
}

// Usage is a caller's active runs and starts left this hour, the smaller
// of its own and its client IP's.
func (p *Playground) Usage(c Caller) (active, left int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, r := range p.runs {
		if r.Sub == c.Sub && r.active() {
			active++
		}
	}
	left = min(perSubHour-len(p.recent("sub:"+c.Sub, now)), perIPHour-len(p.recent("ip:"+c.IP, now)))
	return active, max(left, 0)
}

// recent prunes and returns key's starts in the rolling hour; p.mu held.
func (p *Playground) recent(key string, now time.Time) []time.Time {
	s := p.starts[key]
	i := 0
	for i < len(s) && !s[i].After(now.Add(-hour)) {
		i++
	}
	if s = s[i:]; len(s) == 0 {
		delete(p.starts, key)
		return nil
	}
	p.starts[key] = s
	return s
}

// secondsUntil is t − now in whole seconds, rounded up, at least 1.
func secondsUntil(t, now time.Time) int {
	return max(int(math.Ceil(t.Sub(now).Seconds())), 1)
}

// admit checks every cap and, when none is reached, reserves the run (it
// counts as active from now) and its start; p.mu held.
func (p *Playground) admit(c Caller, s Shape, now time.Time) (*Run, error) {
	if p.closed {
		return nil, &CapError{"the control plane is shutting down: try again", streamRetry}
	}
	var mine, ip, all, scen []*Run
	rate := s.Rate()
	for _, r := range p.runs {
		if !r.active() {
			continue
		}
		all = append(all, r)
		rate += r.Shape.Rate()
		if r.Sub == c.Sub {
			mine = append(mine, r)
		}
		if r.ip == c.IP {
			ip = append(ip, r)
		}
		if r.Shape.Kind == KindScenario {
			scen = append(scen, r)
		}
	}
	firstEnd := func(rs []*Run) int {
		end := rs[0].EndsAt
		for _, r := range rs[1:] {
			if r.EndsAt.Before(end) {
				end = r.EndsAt
			}
		}
		return secondsUntil(end, now)
	}
	hourly := func(key string, limit int) (bool, int) {
		st := p.recent(key, now)
		if len(st) < limit {
			return false, 0
		}
		return true, secondsUntil(st[len(st)-limit].Add(hour), now)
	}
	switch {
	case len(ip) >= perIPActive:
		return nil, &CapError{fmt.Sprintf("a run from your address is still running (at most %d at a time): stop it or wait", perIPActive), firstEnd(ip)}
	case len(mine) >= perSubActive:
		return nil, &CapError{fmt.Sprintf("you have %d runs running (at most %d): stop one or wait", len(mine), perSubActive), firstEnd(mine)}
	case len(all) >= totalActive:
		return nil, &CapError{fmt.Sprintf("the Playground runs %d runs already (at most %d in all): try again shortly", len(all), totalActive), firstEnd(all)}
	case s.Kind == KindScenario && len(scen) >= totalScenario:
		return nil, &CapError{"another scenario run is running (one at a time): try again shortly", firstEnd(scen)}
	case rate > totalRate:
		return nil, &CapError{fmt.Sprintf("the running runs and this one would send over %d requests per second in all: try a lower rate or wait", totalRate), firstEnd(all)}
	}
	if full, after := hourly("ip:"+c.IP, perIPHour); full {
		return nil, &CapError{fmt.Sprintf("your address started %d runs in the last hour (the most): try again later", perIPHour), after}
	}
	if full, after := hourly("sub:"+c.Sub, perSubHour); full {
		return nil, &CapError{fmt.Sprintf("you started %d runs in the last hour (the most): try again later", perSubHour), after}
	}
	r := &Run{
		ID: uuid.NewString(), Sub: c.Sub, Shape: s, ip: c.IP, pg: p,
		StartedAt: now, EndsAt: now.Add(keyReady + requestTimeout + s.Duration), // until it starts
		state: stateStarting, changed: make(chan struct{}),
	}
	p.runs[r.ID] = r
	p.starts["ip:"+c.IP] = append(p.starts["ip:"+c.IP], now)
	p.starts["sub:"+c.Sub] = append(p.starts["sub:"+c.Sub], now)
	return r, nil
}

// unreserve takes back a run that did not start, and its start.
func (p *Playground) unreserve(r *Run, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.runs, r.ID)
	for _, key := range []string{"ip:" + r.ip, "sub:" + r.Sub} {
		if i := slices.Index(p.starts[key], at); i >= 0 {
			p.starts[key] = slices.Delete(p.starts[key], i, i+1)
		}
	}
}

// Start admits a run for c against the caps, makes sure each of its
// tenants has a Playground key that every gateway accepts, and starts
// sending. It returns a *CapError, ErrNoConsumer, ErrGatewaysDown or the
// store's error; it logs one line, without the client IP or any key.
func (p *Playground) Start(ctx context.Context, c Caller, s Shape) (*Run, error) {
	p.mu.Lock()
	at := p.now()
	r, err := p.admit(c, s, at)
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	keys, err := p.prepare(ctx, s)
	if err != nil {
		p.unreserve(r, at)
		return nil, err
	}
	if !r.begin(keys) {
		p.unreserve(r, at)
		return nil, &CapError{"the control plane is shutting down: try again", streamRetry}
	}
	p.log.Info("playground run started", "run", r.ID, "sub", r.Sub, "kind", s.Kind, "scenario", s.Scenario, "loads", loadsText(s.Loads), "duration_s", int(s.Duration/time.Second))
	return r, nil
}

func loadsText(ls []Load) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = fmt.Sprintf("%s %s %d/s", l.Tenant, l.Route, l.Rate)
	}
	return out
}

// prepare returns a key per tenant of s, each with at least keyRefresh
// left (issuing a new one through Keys' 30-minute rule when not), after
// one request per tenant to every gateway: when every one of them fails
// to answer, the gateways are down. A gateway that answers a new key with
// 401 is asked again every 100 ms, for at most keyReady, until it has the
// key's identity version.
func (p *Playground) prepare(ctx context.Context, s Shape) (map[string]secret.Value, error) {
	keys := map[string]secret.Value{}
	fresh := map[string]bool{}
	for _, t := range s.tenants() {
		k, issued, err := p.key(ctx, t)
		if err != nil {
			return nil, err
		}
		keys[t], fresh[t] = k, issued
	}
	route := map[string]string{}
	for _, l := range s.Loads {
		route[l.Tenant] = l.Route
	}
	var answered atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(keyReady)
	for t, k := range keys {
		for _, g := range p.gateways {
			wg.Go(func() {
				for {
					status, err := p.send(ctx, p.probe, g, route[t], k, "pg-probe-"+uuid.NewString())
					if err == nil {
						answered.Add(1)
					}
					if err != nil || status.code != http.StatusUnauthorized || !fresh[t] || time.Now().After(deadline) || ctx.Err() != nil {
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
			})
		}
	}
	wg.Wait()
	if answered.Load() == 0 {
		return nil, ErrGatewaysDown
	}
	return keys, ctx.Err()
}

// key is tenant's Playground key, issued anew when it has none or less
// than keyRefresh left; issued reports a new key.
func (p *Playground) key(ctx context.Context, tenant string) (secret.Value, bool, error) {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	if k, ok := p.tenantKs[tenant]; ok && k.expires.Sub(p.keyNow()) >= keyRefresh {
		return k.key, false, nil
	}
	consumer, err := p.store.PlaygroundConsumer(ctx, tenant)
	if err != nil {
		return secret.Value{}, false, err
	}
	expires, err := p.keys.Expiry(nil)
	if err != nil || expires == nil {
		return secret.Value{}, false, errors.New("playground: no expiry for a key: demo mode is off")
	}
	id, k, err := p.store.IssueKey(ctx, consumer, expires)
	if err != nil {
		return secret.Value{}, false, err
	}
	p.tenantKs[tenant] = tenantKey{id: id, key: k, expires: *expires}
	p.log.Info("playground key issued", "tenant", tenant, "key_id", id, "expires_at", expires.UTC())
	return k, true, nil
}

// answer is what a run keeps of a response: its status and its rate-limit
// headers (policy names and numbers only, ADR-0018).
type answer struct {
	code                          int
	ratelimit, policy, retryAfter *string
}

func header(h http.Header, name string) *string {
	if v := h.Values(name); len(v) > 0 {
		s := v[0]
		return &s
	}
	return nil
}

// routeRequest is a route's method and path at the gateways.
func routeRequest(route string) (string, string) {
	if route == "payments" {
		return http.MethodPost, "/v1/payments"
	}
	return http.MethodGet, "/v1/" + route
}

// send sends one request with key to g and reads the answer to its end.
func (p *Playground) send(ctx context.Context, c *http.Client, g gateway, route string, key secret.Value, requestID string) (answer, error) {
	method, path := routeRequest(route)
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, http.NoBody)
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("X-API-Key", key.Reveal())
	req.Header.Set("X-Request-Id", requestID)
	res, err := c.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return answer{}, err
	}
	return answer{code: res.StatusCode, ratelimit: header(res.Header, "RateLimit"), policy: header(res.Header, "RateLimit-Policy"), retryAfter: header(res.Header, "Retry-After")}, nil
}

// Run returns the caller's run id, or ErrNoRun.
func (p *Playground) Run(sub, id string) (*Run, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.runs[id]
	if !ok || r.Sub != sub {
		return nil, ErrNoRun
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateStarting {
		return nil, ErrNoRun
	}
	return r, nil
}

// Stop ends the caller's run id early (reason stopped) and returns it, or
// ErrNoRun; a run that already ended stays as it is.
func (p *Playground) Stop(sub, id string) (*Run, error) {
	r, err := p.Run(sub, id)
	if err != nil {
		return nil, err
	}
	r.halt(ReasonStopped)
	return r, nil
}

// Shutdown ends every run (reason shutdown), which ends their streams,
// and refuses new runs; register it with http.Server.RegisterOnShutdown.
func (p *Playground) Shutdown() {
	p.mu.Lock()
	p.closed = true
	runs := slices.Collect(maps.Values(p.runs))
	p.mu.Unlock()
	for _, r := range runs {
		r.halt(ReasonShutdown)
	}
	p.stop()
}

// The end reasons (the spec's PlaygroundEnd).
const (
	ReasonDone         = "done"
	ReasonStopped      = "stopped"
	ReasonTokenExpired = "token_expired"
	ReasonShutdown     = "shutdown"
)

const (
	stateStarting = "starting" // admitted, keys not ready: invisible
	stateRunning  = "running"
	stateEnded    = "ended"
)

// Run is one run.
type Run struct {
	ID        string
	Sub       string
	Shape     Shape
	StartedAt time.Time
	EndsAt    time.Time
	ip        string
	pg        *Playground

	halted  atomic.Pointer[string] // the reason it was halted early
	cancel  context.CancelFunc     // stops the schedule
	abandon context.CancelFunc     // also ends the requests in flight (shutdown)

	mu       sync.Mutex
	state    string
	cur      map[string]*counts // this second's, by tenant
	total    map[string]*counts
	replicas map[string]int64 // this second's requests by replica
	last     map[string]Last
	frames   [][]byte      // the tick events so far
	end      []byte        // the end event, once ended
	changed  chan struct{} // closed and replaced when frames or end change
	streams  int
}

// active reports whether r counts against the caps: from its admission
// until it is stopped or ends (a stopped run's requests in flight, at
// most 256, finish within 5 s); r.pg.mu held.
func (r *Run) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state != stateEnded && r.halted.Load() == nil
}

// Info is r as the spec's PlaygroundRun.
type Info struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Tenant    *string   `json:"tenant,omitempty"`
	Route     *string   `json:"route,omitempty"`
	Rate      *int32    `json:"rate,omitempty"`
	Scenario  *string   `json:"scenario,omitempty"`
	DurationS int32     `json:"duration_s"`
	StartedAt time.Time `json:"started_at"`
	EndsAt    time.Time `json:"ends_at"`
	State     string    `json:"state"`
}

// Info is r's PlaygroundRun now.
func (r *Run) Info() Info {
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	in := Info{ID: r.ID, Kind: r.Shape.Kind, DurationS: int32(r.Shape.Duration / time.Second), StartedAt: r.StartedAt.UTC(), EndsAt: r.EndsAt.UTC(), State: state} //nolint:gosec // G115: at most 60
	if r.Shape.Kind == KindCustom {
		l := r.Shape.Loads[0]
		rate := int32(l.Rate) //nolint:gosec // G115: at most 200
		in.Tenant, in.Route, in.Rate = &l.Tenant, &l.Route, &rate
	} else {
		in.Scenario = &r.Shape.Scenario
	}
	return in
}

// begin starts sending with keys: the schedule, the ticker and the end;
// false once the Playground is shutting down.
func (r *Run) begin(keys map[string]secret.Value) bool {
	p := r.pg
	p.mu.Lock() // admit reads EndsAt under p.mu
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	reqCtx, abandon := context.WithCancel(p.ctx)
	ctx, cancel := context.WithCancel(reqCtx)
	start := p.now()
	r.mu.Lock()
	r.state, r.StartedAt, r.EndsAt = stateRunning, start, start.Add(r.Shape.Duration)
	r.cancel, r.abandon = cancel, abandon
	r.cur, r.total, r.replicas, r.last = map[string]*counts{}, map[string]*counts{}, map[string]int64{}, map[string]Last{}
	for _, t := range r.Shape.tenants() {
		r.cur[t], r.total[t] = newCounts(), newCounts()
	}
	r.mu.Unlock()

	client := &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: requestTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        maxInFlight,
		MaxIdleConnsPerHost: maxInFlight,
		IdleConnTimeout:     30 * time.Second,
	}}
	sem := make(chan struct{}, maxInFlight)
	var n atomic.Int64
	var requests, loads sync.WaitGroup
	for _, l := range r.Shape.Loads {
		loads.Go(func() {
			end := r.EndsAt
			for i := int64(0); ; i++ {
				due := start.Add(time.Duration(i) * time.Second / time.Duration(l.Rate))
				if !due.Before(end) || !sleepUntil(ctx, due) {
					return
				}
				select {
				case sem <- struct{}{}:
				default:
					r.dropped(l.Tenant)
					continue
				}
				seq := n.Add(1) - 1
				g := p.gateways[int(seq%int64(len(p.gateways)))]
				r.sent(l.Tenant, g.name)
				requests.Go(func() {
					defer func() { <-sem }()
					a, err := p.send(reqCtx, client, g, l.Route, keys[l.Tenant], fmt.Sprintf("pg-%s-%d", r.ID, seq))
					r.record(l.Tenant, g.name, a, err, time.Since(due))
				})
			}
		})
	}
	done := make(chan struct{})
	go func() {
		loads.Wait()
		requests.Wait()
		client.CloseIdleConnections()
		close(done)
	}()
	go r.ticks(start, done)
	return true
}

// sleepUntil waits until t; false when ctx ended first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// halt stops r's schedule early for reason; shutdown also ends the
// requests in flight.
func (r *Run) halt(reason string) {
	r.mu.Lock()
	running := r.state == stateRunning
	r.mu.Unlock()
	if !running || !r.halted.CompareAndSwap(nil, &reason) {
		return
	}
	r.cancel()
	if reason == ReasonShutdown {
		r.abandon()
	}
}

// ticks writes a tick event every second until done, then a last tick and
// the end event; the run's results go 5 minutes later.
func (r *Run) ticks(start time.Time, done <-chan struct{}) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			r.flush(now, start)
			continue
		case <-done:
		}
		break
	}
	r.flush(time.Now(), start)
	reason := ReasonDone
	if h := r.halted.Load(); h != nil {
		reason = *h
	}
	p := r.pg
	totals := r.Totals()
	p.log.Info("playground run ended", "run", r.ID, "sub", r.Sub, "reason", reason, "totals", totalsText(totals))
	r.mu.Lock()
	r.state = stateEnded
	r.end = frame("end", End{Reason: reason, Totals: totals})
	r.broadcastLocked()
	r.mu.Unlock()
	r.abandon()
	time.AfterFunc(keepResults, func() {
		p.mu.Lock()
		delete(p.runs, r.ID)
		p.mu.Unlock()
	})
}

func totalsText(ts map[string]Counts) []string {
	var out []string
	for _, t := range PlaygroundTenants {
		if c, ok := ts[t]; ok {
			out = append(out, fmt.Sprintf("%s sent %d ok %d limited %d forbidden %d failed %d other %d dropped %d", t, c.Sent, c.OK, c.Limited, c.Forbidden, c.Failed, c.Other, c.Dropped))
		}
	}
	return out
}

// flush writes this second's tick event and starts the next second.
func (r *Run) flush(now, start time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tk := Tick{T: now.UTC().Truncate(time.Millisecond), ElapsedS: int32(now.Sub(start) / time.Second), Tenants: map[string]Counts{}, Replicas: r.replicas, Last: maps.Clone(r.last)} //nolint:gosec // G115: a run lasts at most 60 s plus its drain
	for t, c := range r.cur {
		tk.Tenants[t] = c.out()
		c.reset()
	}
	for _, g := range r.pg.gateways {
		if _, ok := tk.Replicas[g.name]; !ok {
			tk.Replicas[g.name] = 0
		}
	}
	r.replicas = map[string]int64{}
	r.frames = append(r.frames, frame("tick", tk))
	r.broadcastLocked()
}

func (r *Run) broadcastLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Run) sent(tenant, replica string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur[tenant].sent++
	r.total[tenant].sent++
	r.replicas[replica]++
}

func (r *Run) dropped(tenant string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur[tenant].dropped++
	r.total[tenant].dropped++
}

// record counts one request's answer, or its lack, by status.
func (r *Run) record(tenant, replica string, a answer, err error, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	code := a.code
	if err != nil {
		code = 0
	}
	r.cur[tenant].add(code, latency)
	r.total[tenant].add(code, latency)
	if err == nil {
		status := int32(a.code) //nolint:gosec // G115: an HTTP status
		r.last[tenant] = Last{Status: status, Replica: replica, RateLimit: a.ratelimit, RateLimitPolicy: a.policy, RetryAfter: a.retryAfter}
	}
}

// Totals are the whole run's counts per tenant so far.
func (r *Run) Totals() map[string]Counts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totalsLocked()
}

func (r *Run) totalsLocked() map[string]Counts {
	out := map[string]Counts{}
	for t, c := range r.total {
		out[t] = c.out()
	}
	return out
}

// Ended reports whether r has ended.
func (r *Run) Ended() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == stateEnded
}
