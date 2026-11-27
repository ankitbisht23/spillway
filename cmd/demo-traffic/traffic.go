package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"

	"github.com/ankitbisht23/spillway/internal/demo"
	"github.com/ankitbisht23/spillway/internal/loadgen"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// The load engine of "demo-traffic run" (SC-02, ADR-0096 §1): open-model
// streams (internal/loadgen), one per consumer and route, each with its own
// transport, a 15 s client timeout and at most 1,000 requests in flight,
// round robin over the demo gateways unless a stream names one replica.
// Keys are issued at run time and live in memory only; output names a key
// by its ID.

const (
	clientTimeout = 15 * time.Second // above every route's 6 s total plus the worst stall
	maxInFlight   = 1000             // per stream; a request due while 1,000 are in flight is dropped
	keyRenewBelow = 10 * time.Minute // a key with less left is replaced
	applyTimeout  = 10 * time.Second // a new key must work on every replica within this
)

// routes are the demo routes' requests (ADR-0094 §3).
var routes = map[string]struct{ method, path string }{
	"orders":   {http.MethodGet, "/v1/orders"},
	"catalog":  {http.MethodGet, "/v1/catalog"},
	"payments": {http.MethodPost, "/v1/payments"},
}

// gateway is one demo replica: its name, its proxy URL and its admin URL.
type gateway struct{ name, url, admin string }

// gatewaysFrom reads the replicas from comma-separated proxy and admin URLs
// of the same length; a replica's name is its URL's host name.
func gatewaysFrom(urls, admins string) ([]gateway, error) {
	us, as := strings.Split(urls, ","), strings.Split(admins, ",")
	if len(us) != len(as) {
		return nil, fmt.Errorf("SPILLWAY_DEMO_GATEWAYS has %d URLs and SPILLWAY_DEMO_ADMIN_URLS %d; want one admin URL per gateway", len(us), len(as))
	}
	var gs []gateway
	for i, u := range us {
		p, err := url.Parse(strings.TrimSpace(u))
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Hostname() == "" {
			return nil, fmt.Errorf("SPILLWAY_DEMO_GATEWAYS: %q is not an http:// URL", u)
		}
		a, err := url.Parse(strings.TrimSpace(as[i]))
		if err != nil || (a.Scheme != "http" && a.Scheme != "https") || a.Hostname() == "" {
			return nil, fmt.Errorf("SPILLWAY_DEMO_ADMIN_URLS: %q is not an http:// URL", as[i])
		}
		gs = append(gs, gateway{name: p.Hostname(), url: strings.TrimSuffix(p.String(), "/"), admin: strings.TrimSuffix(a.String(), "/")})
	}
	// Replicas on one host (tests) are named by host and port.
	hosts := map[string]int{}
	for _, g := range gs {
		hosts[g.name]++
	}
	for i := range gs {
		if hosts[gs[i].name] > 1 {
			u, _ := url.Parse(gs[i].url)
			gs[i].name = u.Host
		}
	}
	return gs, nil
}

// apiKey is a demo key. Its secret prints as [redacted]; output names it
// by ID (key #412).
type apiKey struct {
	id         int64
	consumerID int64
	consumer   string
	secret     secret.Value
	expires    time.Time
}

func (k *apiKey) String() string { return "key #" + strconv.FormatInt(k.id, 10) }

// keyring issues and holds the run's keys, one current key per consumer
// (ADR-0095 §3: no expires_at, so 30 minutes in demo mode).
type keyring struct {
	cp  *demo.Client
	mu  sync.Mutex
	ids map[string]int64 // consumer name → ID
	cur map[string]*atomic.Pointer[apiKey]
}

func newKeyring(cp *demo.Client) *keyring {
	return &keyring{cp: cp, ids: map[string]int64{}, cur: map[string]*atomic.Pointer[apiKey]{}}
}

// consumerID resolves a consumer by name.
func (k *keyring) consumerID(ctx context.Context, name string) (int64, error) {
	k.mu.Lock()
	id, ok := k.ids[name]
	k.mu.Unlock()
	if ok {
		return id, nil
	}
	var list struct {
		Items []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if _, err := k.cp.Must(ctx, http.StatusOK, http.MethodGet, "/consumers?"+url.Values{"name": {name}}.Encode(), nil, &list); err != nil {
		return 0, err
	}
	for _, c := range list.Items {
		if c.Name == name {
			k.mu.Lock()
			k.ids[name] = c.ID
			k.mu.Unlock()
			return c.ID, nil
		}
	}
	return 0, fmt.Errorf("no consumer %s in the demo database (make demo-reset seeds it)", name)
}

// newKey issues a new key for consumer; use makes it the consumer's
// current key.
func (k *keyring) newKey(ctx context.Context, consumer string) (*apiKey, error) {
	id, err := k.consumerID(ctx, consumer)
	if err != nil {
		return nil, err
	}
	var out struct {
		ID        int64      `json:"id"`
		Key       string     `json:"key"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if _, err := k.cp.Must(ctx, http.StatusCreated, http.MethodPost, "/consumers/"+strconv.FormatInt(id, 10)+"/keys", map[string]any{}, &out); err != nil {
		return nil, err
	}
	key := &apiKey{id: out.ID, consumerID: id, consumer: consumer, secret: secret.New(out.Key), expires: time.Now().Add(30 * time.Minute)}
	if out.ExpiresAt != nil {
		key.expires = *out.ExpiresAt
	}
	return key, nil
}

// use makes key its consumer's current key.
func (k *keyring) use(key *apiKey) {
	k.mu.Lock()
	p := k.cur[key.consumer]
	if p == nil {
		p = &atomic.Pointer[apiKey]{}
		k.cur[key.consumer] = p
	}
	k.mu.Unlock()
	p.Store(key)
}

// current is consumer's current key, nil before the first use.
func (k *keyring) current(consumer string) *apiKey {
	k.mu.Lock()
	p := k.cur[consumer]
	k.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.Load()
}

// due lists the consumers whose current key has under keyRenewBelow left.
func (k *keyring) due(now time.Time) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for c, p := range k.cur {
		if key := p.Load(); key != nil && key.expires.Sub(now) < keyRenewBelow {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

// answer is one request's outcome, its times from the run's time 0.
type answer struct {
	due, done  time.Duration
	status     int    // 0: no answer
	errKind    string // without an answer: timeout, dial or other (loadgen.ErrorKind)
	replica    int    // index into the gateways
	keyID      int64
	decisionUS int64  // Spillway-Decision-Time: Redis decided it (ADR-0021); 0 for none
	redisErr   string // Spillway-Redis-Error (ADR-0034)
	// The RateLimit fields (ADR-0018), kept only for streams that ask.
	rateLimit, policy, retryAfter string
}

func (a answer) latency() time.Duration { return a.done - a.due }
func (a answer) ok() bool               { return a.status >= 200 && a.status < 300 }

// streamSpec is one stream of a scenario: rate requests per per (one
// second by default) from consumer on route, sent from from to to (from
// the run's time 0), round robin or to one replica.
type streamSpec struct {
	consumer, route string
	rate            int
	per             time.Duration
	from, to        time.Duration // to 0: the run's end
	replica         int           // 0: round robin; n: gateway n only
	// D2's slots (ADR-0074, ADR-0083): warm-up, slot length and odd slots
	// only; the slots' histograms leave out each slot's first second.
	warmup, slot time.Duration
	toggle       bool
	headers      bool // keep each answer's RateLimit fields
}

// name is the stream's label: consumer and route, and the replica when it
// has one.
func (s streamSpec) name(gs []gateway) string {
	n := s.consumer + " " + s.route
	if s.replica > 0 && s.replica <= len(gs) {
		n += "@" + gs[s.replica-1].name
	}
	return n
}

// tenantOf is a demo consumer's tenant: its name without the last part
// (pro-co-mobile → pro-co).
func tenantOf(consumer string) string {
	if i := strings.LastIndexByte(consumer, '-'); i > 0 {
		return consumer[:i]
	}
	return consumer
}

// perSecond is the spec's rate per second.
func (s streamSpec) perSecond() float64 {
	per := s.per
	if per <= 0 {
		per = time.Second
	}
	return float64(s.rate) / per.Seconds()
}

// stream is a running streamSpec.
type stream struct {
	spec    streamSpec
	name    string
	e       *engine
	client  *http.Client
	next    atomic.Uint64
	keep    bool
	stop    context.CancelFunc
	done    chan struct{}
	result  loadgen.Result
	mu      sync.Mutex
	answers []answer
	total   *tally // whole run
	live    *tally // since the printer last took it
}

// tally counts answers by kind with their latency.
type tally struct {
	n, ok, limited, forbidden, unavailable, other, failed int64
	lat                                                   *hdrhistogram.Histogram // µs
	last                                                  answer                  // the latest answer, for its headers
	okBy                                                  []int64                 // 2xx by replica
}

func newTally() *tally { return &tally{lat: hdrhistogram.New(1, loadgen.MaxLatency.Microseconds(), 3)} }

func (t *tally) add(a answer) {
	t.n++
	switch {
	case a.status == 0:
		t.failed++
	case a.ok():
		t.ok++
		for len(t.okBy) <= a.replica {
			t.okBy = append(t.okBy, 0)
		}
		t.okBy[a.replica]++
	case a.status == http.StatusTooManyRequests:
		t.limited++
	case a.status == http.StatusUnauthorized || a.status == http.StatusForbidden:
		t.forbidden++
	case a.status == http.StatusServiceUnavailable:
		t.unavailable++
	default:
		t.other++
	}
	if a.status != 0 {
		_ = t.lat.RecordValue(min(max(a.latency(), time.Microsecond), loadgen.MaxLatency).Microseconds()) // in range
		t.last = a
	}
}

func (t *tally) merge(o *tally) {
	t.n += o.n
	t.ok += o.ok
	t.limited += o.limited
	t.forbidden += o.forbidden
	t.unavailable += o.unavailable
	t.other += o.other
	t.failed += o.failed
	t.lat.Merge(o.lat)
	for i, n := range o.okBy {
		for len(t.okBy) <= i {
			t.okBy = append(t.okBy, 0)
		}
		t.okBy[i] += n
	}
	if o.last.done > t.last.done {
		t.last = o.last
	}
}

// p99 is the tally's p99 latency, 0 without answers.
func (t *tally) p99() time.Duration {
	if t.lat.TotalCount() == 0 {
		return 0
	}
	return time.Duration(t.lat.ValueAtQuantile(99)) * time.Microsecond
}

func (t *tally) p50() time.Duration {
	if t.lat.TotalCount() == 0 {
		return 0
	}
	return time.Duration(t.lat.ValueAtQuantile(50)) * time.Microsecond
}

// engine runs one scenario's streams against the gateways.
type engine struct {
	gws    []gateway
	keys   *keyring
	t0     time.Time       // the first scheduled request
	reqCtx context.Context // every request's; ends on SIGINT and SIGTERM
	keep   bool            // keep every answer (scenarios, not base)
	end    time.Duration   // the run's length; streams without to stop here

	mu      sync.Mutex
	streams []*stream
}

// since is the time from time 0.
func (e *engine) since() time.Duration { return time.Since(e.t0) }

// sleepUntil waits until d after time 0, or until ctx ends; false if it
// ended.
func (e *engine) sleepUntil(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(time.Until(e.t0.Add(d)))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// add creates the streams of specs; start starts them.
func (e *engine) add(specs ...streamSpec) []*stream {
	var out []*stream
	for _, sp := range specs {
		s := &stream{spec: sp, name: sp.name(e.gws), e: e, keep: e.keep, done: make(chan struct{}), total: newTally(), live: newTally(),
			client: &http.Client{
				Timeout: clientTimeout,
				Transport: &http.Transport{
					Proxy: nil, MaxIdleConns: 0, MaxIdleConnsPerHost: maxInFlight, IdleConnTimeout: 90 * time.Second,
					DisableCompression: true,
				},
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}}
		out = append(out, s)
	}
	e.mu.Lock()
	e.streams = append(e.streams, out...)
	e.mu.Unlock()
	return out
}

// start runs s on its schedule until its end or its stop; ctx bounds the
// schedule only (requests run on the engine's request context).
func (s *stream) start(ctx context.Context) {
	ctx, s.stop = context.WithCancel(ctx)
	sp := s.spec
	to := sp.to
	if to <= 0 {
		to = s.e.end
	}
	g := loadgen.Generator{
		Clock: loadgen.RealClock{}, Rate: sp.rate, Per: sp.per, Warmup: sp.warmup, Duration: to - sp.from - sp.warmup,
		MaxInFlight: maxInFlight, Do: s.do, Start: s.e.t0.Add(sp.from),
		Slot: sp.slot, Settle: min(time.Second, sp.slot/2), Toggle: sp.toggle,
	}
	go func() {
		defer close(s.done)
		r := g.Run(ctx)
		s.mu.Lock()
		s.result = r
		s.mu.Unlock()
	}()
}

// wait waits for s's last answer.
func (s *stream) wait() { <-s.done }

// do sends one request of s, due at due.
func (s *stream) do(_ context.Context, due time.Time) (int, string, error) {
	sp := s.spec
	r := sp.replica - 1
	if sp.replica <= 0 || sp.replica > len(s.e.gws) {
		r = int((s.next.Add(1) - 1) % uint64(len(s.e.gws))) //nolint:gosec // G115: below len(gws)
	}
	a := answer{due: due.Sub(s.e.t0), replica: r}
	key := s.e.keys.current(sp.consumer)
	rt := routes[sp.route]
	req, err := http.NewRequestWithContext(s.e.reqCtx, rt.method, s.e.gws[r].url+rt.path, nil) //nolint:gosec // G704: the demo gateways in the compose network are the point
	if err != nil || key == nil {
		a.done, a.errKind = time.Since(s.e.t0), "other"
		s.record(a)
		return 0, "", errors.Join(err, errors.New("no key"))
	}
	a.keyID = key.id
	req.Header.Set(apiKeyHeader, key.secret.Reveal())
	resp, err := s.client.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		a.done, a.errKind = time.Since(s.e.t0), loadgen.ErrorKind(err)
		s.record(a)
		return 0, "", err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	a.done = time.Since(s.e.t0)
	if err != nil {
		a.errKind = loadgen.ErrorKind(err)
		s.record(a)
		return 0, "", err
	}
	a.status = resp.StatusCode
	a.decisionUS, _ = strconv.ParseInt(resp.Header.Get("Spillway-Decision-Time"), 10, 64)
	a.redisErr = resp.Header.Get("Spillway-Redis-Error")
	if sp.headers {
		a.rateLimit, a.policy, a.retryAfter = resp.Header.Get("RateLimit"), resp.Header.Get("RateLimit-Policy"), resp.Header.Get("Retry-After")
	}
	s.record(a)
	return resp.StatusCode, resp.Proto, nil
}

// apiKeyHeader carries a consumer's API key (GW-17).
const apiKeyHeader = "X-API-Key" //nolint:gosec // G101: the header's name, not a credential

func (s *stream) record(a answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total.add(a)
	s.live.add(a)
	if s.keep {
		if !s.spec.headers {
			a.rateLimit, a.policy, a.retryAfter = "", "", ""
		}
		s.answers = append(s.answers, a)
	}
}

// takeLive returns s's tally since the last call and starts a new one.
func (s *stream) takeLive() *tally {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.live
	s.live = newTally()
	return t
}

// latest is s's latest answer.
func (s *stream) latest() answer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total.last
}

// snapshot returns s's answers so far and its total tally.
func (s *stream) snapshot() ([]answer, *tally, loadgen.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := newTally()
	t.merge(s.total)
	return append([]answer(nil), s.answers...), t, s.result
}

// waitApplied sends a probe with key to every gateway every 100 ms until
// each answers something other than 401 (the key is in the replica's
// identity, CP-20), for at most applyTimeout.
func waitApplied(ctx context.Context, gws []gateway, key *apiKey) error {
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(applyTimeout)
	for _, g := range gws {
		for {
			st, err := probeOnce(ctx, client, g, key, "catalog")
			if err == nil && st != http.StatusUnauthorized {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return fmt.Errorf("%s does not work on %s within %v (status %d)", key, g.name, applyTimeout, st)
			}
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return nil
}

// probeOnce sends one request on route with key to g and returns its
// status.
func probeOnce(ctx context.Context, client *http.Client, g gateway, key *apiKey, route string) (int, error) {
	status, _, err := probeAnswer(ctx, client, g, key, route)
	return status, err
}

// probeAnswer sends one request on route with key to g and returns the
// answer's status and header, its body read and closed.
func probeAnswer(ctx context.Context, client *http.Client, g gateway, key *apiKey, route string) (int, http.Header, error) {
	rt := routes[route]
	req, err := http.NewRequestWithContext(ctx, rt.method, g.url+rt.path, nil) //nolint:gosec // G704: the demo gateways are the point
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set(apiKeyHeader, key.secret.Reveal())
	resp, err := client.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return 0, nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}
