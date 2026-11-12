package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/resilience"
)

var discard = slog.New(slog.DiscardHandler)

// endpointGauge gathers gw_upstream_endpoint_healthy from r, by
// "upstream endpoint".
func endpointGauge(t *testing.T, r *Registry) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(r)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			out[l["upstream"]+" "+l["endpoint"]] = m.GetGauge().GetValue()
		}
	}
	return out
}

// waitFor polls cond until it holds, for at most 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// addr is u's endpoint as the registry keys it.
func (u *upstream) addr() string { return "127.0.0.1:" + u.port() }

// upstreamOf is a config whose upstream u has the given servers as endpoints,
// by name, and extra fields, with one route to it. u is balanced round robin
// unless fields set lb, so that tests can count where requests go.
func upstreamOf(fields string, us ...*upstream) string {
	var eps []string
	for _, u := range us {
		eps = append(eps, fmt.Sprintf("{address: '%s:%s'}", u.name, u.port()))
	}
	if !strings.Contains(fields, "lb:") {
		fields = strings.TrimPrefix(fields+", lb: round_robin", ", ")
	}
	return fmt.Sprintf("upstreams: [{name: u, endpoints: [%s], %s}]\nroutes: [{name: r, upstream: u, match: {path_prefix: /}}]\n",
		strings.Join(eps, ", "), fields)
}

// served sends n requests and returns the status and serving upstream of
// each, "" for a response without an echo.
func (g *gateway) served(t *testing.T, n int) (codes []int, from []string) {
	t.Helper()
	for range n {
		status, _, e := g.get(t, "/", nil)
		codes = append(codes, status)
		from = append(from, e.Upstream)
	}
	return codes, from
}

func (g *gateway) endpoint(t *testing.T, addr string) *endpoint {
	t.Helper()
	for _, e := range *(*g.reg.pools.Load())["u"].endpoints.Load() {
		if e.addr == addr {
			return e
		}
	}
	return nil
}

// TestHealthThresholds: unhealthy after `unhealthy` failed probes in a row,
// healthy again after `healthy` successes in a row; a result from a stopped
// prober is dropped (GW-08).
func TestHealthThresholds(t *testing.T) {
	hc := &config.HealthCheck{Unhealthy: 3, Healthy: 2}
	e := &endpoint{addr: "127.0.0.1:1"}
	e.healthy.Store(true)
	errDown := errors.New("down")
	steps := []struct {
		err  error
		want bool
	}{
		{errDown, true}, {errDown, true}, {nil, true}, // a success resets the failures
		{errDown, true}, {errDown, true}, {errDown, false},
		{nil, false}, {errDown, false}, {nil, false}, // a failure resets the successes
		{nil, true},
		{errDown, true},
	}
	for i, st := range steps {
		e.observe(t.Context(), st.err, hc, "u", discard)
		if got := e.healthy.Load(); got != st.want {
			t.Fatalf("after result %d: healthy = %v, want %v", i+1, got, st.want)
		}
	}
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	for range 5 {
		e.observe(stopped, errDown, hc, "u", discard)
	}
	if !e.healthy.Load() {
		t.Error("results from a stopped prober counted")
	}
}

// TestProbe: a probe is GET path, query included, to the resolved address
// with the configured address as Host; 2xx and 3xx succeed, anything else,
// a refused connection and the timeout fail (GW-08, ADR-0030).
func TestProbe(t *testing.T) {
	var mu sync.Mutex
	var got *http.Request
	status := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r
		mu.Unlock()
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, strings.Repeat("x", 64<<10))
	}))
	defer srv.Close()
	r := NewRegistry(loopback, newResolver(), discard)
	defer r.Close()
	e := &endpoint{addr: srv.Listener.Addr().String(), host: "orders:8080"}
	hc := &config.HealthCheck{Path: "/status?x=1", Timeout: 200 * time.Millisecond}
	for code, ok := range map[int32]bool{200: true, 204: true, 302: true, 399: true, 404: false, 500: false, 503: false} {
		status.Store(code)
		if err := r.probe(t.Context(), e, hc); (err == nil) != ok {
			t.Errorf("probe answered %d: err = %v, want success %v", code, err, ok)
		}
	}
	mu.Lock()
	if got.Method != http.MethodGet || got.Host != "orders:8080" || got.URL.RequestURI() != "/status?x=1" || got.UserAgent() != "spillway-health-check" {
		t.Errorf("probe = %s %s Host %s User-Agent %q", got.Method, got.URL.RequestURI(), got.Host, got.UserAgent())
	}
	mu.Unlock()
	start := time.Now()
	if err := r.probe(t.Context(), e, &config.HealthCheck{Path: "/slow", Timeout: 100 * time.Millisecond}); err == nil || time.Since(start) > time.Second {
		t.Errorf("slow probe: err = %v after %v, want the 100ms timeout", err, time.Since(start))
	}
	srv.Close()
	if err := r.probe(t.Context(), e, hc); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("probe of a closed port: err = %v, want connection refused", err)
	}
}

// fastHealth probes every 250 ms. A healthy endpoint is marked unhealthy
// only after 3 probes in a row take over 250 ms, which a stall of the test
// machine (ADR-0027 saw up to 370 ms) cannot do alone.
const fastHealth = "health_check: {path: /health, interval: 250ms, timeout: 250ms, unhealthy: 3, healthy: 2}"

// TestHealthChecks: an endpoint whose probes fail is taken out of rotation,
// and put back once they succeed again; the gauge follows (GW-08).
func TestHealthChecks(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	b.probeStatus.Store(http.StatusFound) // 3xx is healthy
	g := newGateway(t, newResolver(a, b), upstreamOf(fastHealth+", outlier_detection: {consecutive_failures: 1000}", a, b))
	gauge := func(u *upstream) float64 { return endpointGauge(t, g.reg)["u "+u.addr()] }
	waitFor(t, "first probes", func() bool { return a.probes.Load() > 0 && b.probes.Load() > 0 })
	if gauge(a) != 1 || gauge(b) != 1 {
		t.Fatalf("gauge = %v, want both 1", endpointGauge(t, g.reg))
	}

	a.probeStatus.Store(http.StatusServiceUnavailable)
	waitFor(t, "a unhealthy", func() bool { return gauge(a) == 0 })
	if _, from := g.served(t, 6); strings.Join(from, ",") != "b,b,b,b,b,b" {
		t.Errorf("with a unhealthy, served by %v, want only b", from)
	}
	if !strings.Contains(g.events.String(), `"msg":"upstream endpoint unhealthy","upstream":"u","endpoint":"`+a.addr()+`","reason":"status 503"`) {
		t.Errorf("no unhealthy log line for a:\n%s", g.events)
	}

	a.probeStatus.Store(0)
	waitFor(t, "a healthy", func() bool { return gauge(a) == 1 })
	if _, from := g.served(t, 4); strings.Count(strings.Join(from, ","), "a") != 2 {
		t.Errorf("with a healthy again, served by %v, want a and b alternating", from)
	}
	if !strings.Contains(g.events.String(), `"msg":"upstream endpoint healthy","upstream":"u","endpoint":"`+a.addr()+`"`) {
		t.Errorf("no healthy log line for a:\n%s", g.events)
	}
}

// TestPanicMode: with every endpoint unhealthy, traffic goes to every
// endpoint that is not ejected, rather than nowhere (ADR-0030).
func TestPanicMode(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	a.probeStatus.Store(http.StatusServiceUnavailable)
	b.probeStatus.Store(http.StatusServiceUnavailable)
	g := newGateway(t, newResolver(a, b), upstreamOf(fastHealth+", outlier_detection: {consecutive_failures: 1000}", a, b))
	waitFor(t, "both unhealthy", func() bool {
		m := endpointGauge(t, g.reg)
		return m["u "+a.addr()] == 0 && m["u "+b.addr()] == 0
	})
	codes, from := g.served(t, 4)
	if fmt.Sprint(codes) != "[200 200 200 200]" || strings.Count(strings.Join(from, ","), "a") != 2 {
		t.Errorf("panic mode: %v from %v, want 200s from a and b alternating", codes, from)
	}
	if !strings.Contains(g.events.String(), `"msg":"upstream has no healthy endpoint; ignoring health checks","upstream":"u"`) {
		t.Errorf("no panic log line:\n%s", g.events)
	}

	// With an endpoint ejected too, panic mode skips it.
	e := g.endpoint(t, b.addr())
	e.outlier.Eject(time.Now(), &config.OutlierDetection{BaseEjection: time.Hour, MaxEjection: time.Hour})
	if _, from := g.served(t, 3); strings.Join(from, ",") != "a,a,a" {
		t.Errorf("panic mode with b ejected: served by %v, want only a", from)
	}

	a.probeStatus.Store(0)
	waitFor(t, "a healthy", func() bool { return endpointGauge(t, g.reg)["u "+a.addr()] == 1 })
	g.served(t, 1)
	if !strings.Contains(g.events.String(), `"msg":"upstream has a healthy endpoint again; health checks apply","upstream":"u"`) {
		t.Errorf("no end-of-panic log line:\n%s", g.events)
	}
}

// TestOutlierEjection: consecutive 5xx eject an endpoint for base × k, at
// most max; it takes no traffic while ejected and returns after (GW-09).
func TestOutlierEjection(t *testing.T) {
	a, b, c := newUpstream(t, "a"), newUpstream(t, "b"), newUpstream(t, "c")
	a.status.Store(http.StatusServiceUnavailable)
	g := newGateway(t, newResolver(a, b, c), upstreamOf("outlier_detection: {consecutive_failures: 2, base_ejection: 10s, max_ejection: 25s}", a, b, c))
	clock := ratelimit.NewFakeClock(t0)
	g.reg.clock = clock
	gauge := func() float64 { return endpointGauge(t, g.reg)["u "+a.addr()] }
	for i, want := range []time.Duration{10 * time.Second, 20 * time.Second, 25 * time.Second} {
		failures := 0
		for range 20 {
			if status, _, _ := g.get(t, "/", nil); status == http.StatusServiceUnavailable {
				if failures++; failures == 2 {
					break
				}
			}
		}
		if failures != 2 || gauge() != 0 {
			t.Fatalf("ejection %d: %d failures from a, gauge %v; want ejected after 2", i+1, failures, gauge())
		}
		if codes, from := g.served(t, 6); strings.Contains(strings.Join(from, ","), "a") || strings.Contains(fmt.Sprint(codes), "503") {
			t.Errorf("ejection %d: %v from %v while a is ejected", i+1, codes, from)
		}
		if line := fmt.Sprintf(`"msg":"upstream endpoint ejected","upstream":"u","endpoint":"%s","for":%d,"times":%d`, a.addr(), want, i+1); !strings.Contains(g.events.String(), line) {
			t.Errorf("ejection %d: no log line %s in\n%s", i+1, line, g.events)
		}
		clock.Advance(want - time.Nanosecond)
		if gauge() != 0 {
			t.Fatalf("ejection %d ended before %v", i+1, want)
		}
		clock.Advance(time.Nanosecond)
		if gauge() != 1 {
			t.Fatalf("ejection %d did not end after %v", i+1, want)
		}
	}
	a.status.Store(0)
	if _, from := g.served(t, 3); strings.Count(strings.Join(from, ","), "a") != 1 {
		t.Errorf("after the last ejection: served by %v, want a back in rotation", from)
	}
}

// TestMaxEjectedPercent: at most floor(n × max_ejected_percent / 100)
// endpoints are ejected at once, so a single endpoint never is; at 100%
// every endpoint can be, and the upstream still gets traffic (GW-09,
// ADR-0030).
func TestMaxEjectedPercent(t *testing.T) {
	tests := []struct {
		endpoints, percent, want int
	}{{1, 50, 0}, {2, 50, 1}, {3, 50, 1}, {4, 50, 2}, {4, 25, 1}, {3, 99, 2}, {2, 100, 2}}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d endpoints at %d%%", tt.endpoints, tt.percent), func(t *testing.T) {
			var us []*upstream
			for i := range tt.endpoints {
				u := newUpstream(t, fmt.Sprintf("e%d", i))
				u.status.Store(http.StatusBadGateway)
				us = append(us, u)
			}
			g := newGateway(t, newResolver(us...), upstreamOf(fmt.Sprintf("outlier_detection: {consecutive_failures: 1, base_ejection: 1h, max_ejected_percent: %d}", tt.percent), us...))
			g.served(t, 4*tt.endpoints)
			ejected := 0
			for _, v := range endpointGauge(t, g.reg) {
				if v == 0 {
					ejected++
				}
			}
			if ejected != tt.want {
				t.Errorf("%d ejected, want %d", ejected, tt.want)
			}
			if tt.want < tt.endpoints && !strings.Contains(g.events.String(), "upstream endpoint ejection skipped: max_ejected_percent reached") {
				t.Error("no log line for a held-back ejection")
			}
			// Every endpoint still answers through the gateway: the
			// upstream's 502, never the gateway's 503.
			before := us[0].requests.Load()
			for _, u := range us[1:] {
				before += u.requests.Load()
			}
			codes, _ := g.served(t, 2)
			after := int64(0)
			for _, u := range us {
				after += u.requests.Load()
			}
			if fmt.Sprint(codes) != "[502 502]" || after-before != 2 {
				t.Errorf("after ejections: %v with %d requests upstream, want the upstream's 502s", codes, after-before)
			}
		})
	}
}

// TestAttemptOutcome pins which attempt outcomes count for outlier ejection
// (ADR-0030, "Attempt outcomes").
func TestAttemptOutcome(t *testing.T) {
	live := t.Context()
	timedOut, cancel := context.WithTimeout(live, 0)
	defer cancel()
	gone, stop := context.WithCancel(live)
	stop()
	idle, stopIdle := context.WithCancelCause(live)
	stopIdle(errStreamIdle)
	broken := &countingBody{}
	broken.broken.Store(true)
	tests := []struct {
		name            string
		ctx             context.Context
		code            int // a response's status; 0 for none
		err             error
		body            *countingBody
		failed, counted bool
	}{
		{"200", live, 200, nil, &countingBody{}, false, true},
		{"404", live, 404, nil, &countingBody{}, false, true},
		{"101", live, 101, nil, &countingBody{}, false, true},
		{"500", live, 500, nil, &countingBody{}, true, true},
		{"503", live, 503, nil, &countingBody{}, true, true},
		{"connect refused", live, 0, syscall.ECONNREFUSED, &countingBody{}, true, true},
		{"reset", live, 0, syscall.ECONNRESET, &countingBody{}, true, true},
		{"EOF before headers", live, 0, io.EOF, &countingBody{}, true, true},
		{"attempt deadline", timedOut, 0, context.DeadlineExceeded, &countingBody{}, true, true},
		{"client went away", gone, 0, context.Canceled, &countingBody{}, false, false},
		{"idle timeout", idle, 0, context.Canceled, &countingBody{}, false, false},
		{"client body failed", live, 0, io.ErrUnexpectedEOF, broken, false, false},
		{"body over the route's limit", live, 0, &http.MaxBytesError{Limit: 1}, &countingBody{}, false, false},
		{"allow-list", live, 0, fmt.Errorf("dial: %w", ErrNotAllowed), &countingBody{}, false, false},
	}
	for _, tt := range tests {
		var resp *http.Response
		if tt.code != 0 {
			resp = &http.Response{StatusCode: tt.code, Body: http.NoBody}
			_ = resp.Body.Close()
		}
		if failed, counted := attemptOutcome(tt.ctx, resp, tt.err, tt.body); failed != tt.failed || counted != tt.counted {
			t.Errorf("%s: failed %v, counted %v; want %v, %v", tt.name, failed, counted, tt.failed, tt.counted)
		}
	}
}

// endpointState is what a reload must keep of an endpoint, by value.
type endpointState struct {
	healthy  bool
	outlier  resilience.OutlierState
	inFlight int64
}

func stateOf(e *endpoint) endpointState {
	return endpointState{e.healthy.Load(), e.outlier.State(), e.inFlight.Load()}
}

// TestReloadKeepsEndpointState (GW-29): under steady load, reloads that
// change settings, the algorithm included, but keep the upstream and its
// endpoints keep every endpoint object with its health, outlier state and
// in-flight count; a reload that replaces one endpoint drops only that one
// and stops its prober.
func TestReloadKeepsEndpointState(t *testing.T) {
	a, b, c, d := newUpstream(t, "a"), newUpstream(t, "b"), newUpstream(t, "c"), newUpstream(t, "d")
	a.probeStatus.Store(http.StatusServiceUnavailable) // unhealthy
	b.status.Store(http.StatusServiceUnavailable)      // ejected
	cfg := func(n int, us ...*upstream) string {
		return fmt.Sprintf("version: %d\n", n) + upstreamOf(fmt.Sprintf(
			"health_check: {path: /health, interval: %dms, timeout: 250ms, unhealthy: 3, healthy: 2}, outlier_detection: {consecutive_failures: %d, base_ejection: 1h}, lb: %s",
			250+n, 3+n%2, []string{"round_robin", "p2c", "consistent_hash, hash_on: '{ip}'"}[n%3]), us...)
	}
	g := newGateway(t, newResolver(a, b, c, d), cfg(0, a, b, c))
	waitFor(t, "a unhealthy", func() bool { return !g.endpoint(t, a.addr()).healthy.Load() })
	for i := 0; endpointGauge(t, g.reg)["u "+b.addr()] == 1; i++ {
		if i == 20 {
			t.Fatal("b not ejected after 20 requests")
		}
		g.get(t, "/", nil)
	}
	pool := (*g.reg.pools.Load())["u"]
	eps := map[string]*endpoint{}
	before := map[string]endpointState{}
	for i, u := range []*upstream{a, b, c} {
		eps[u.name] = g.endpoint(t, u.addr())
		for range i + 1 { // attempts held in flight across the reloads
			defer eps[u.name].begin()()
		}
		before[u.name] = stateOf(eps[u.name])
	}

	var bad atomic.Int64
	var wg sync.WaitGroup
	ctx, stop := context.WithCancel(t.Context())
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/", nil)
				resp, err := g.Client().Do(req)
				if err != nil {
					if ctx.Err() == nil {
						bad.Add(1)
					}
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					bad.Add(1)
				}
			}
		})
	}
	for n := 1; n <= 20; n++ {
		g.apply(t, cfg(n, a, b, c))
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d requests under load failed", bad.Load())
	}
	// A client can read a whole response just before the gateway closes the
	// upstream body and ends the attempt.
	waitFor(t, "the load's attempts to end", func() bool {
		for _, u := range []*upstream{a, b, c} {
			if e := g.endpoint(t, u.addr()); e != nil && e.inFlight.Load() != before[u.name].inFlight {
				return false
			}
		}
		return true
	})
	if (*g.reg.pools.Load())["u"] != pool {
		t.Fatal("a reload replaced the upstream's state")
	}
	for _, u := range []*upstream{a, b, c} {
		e := g.endpoint(t, u.addr())
		if e != eps[u.name] {
			t.Errorf("endpoint %s: replaced by a reload", u.name)
			continue
		}
		if got := stateOf(e); got != before[u.name] {
			t.Errorf("endpoint %s: %+v after the reloads, want %+v", u.name, got, before[u.name])
		}
	}

	// Replace a with d.
	g.apply(t, cfg(21, d, b, c))
	if g.endpoint(t, a.addr()) != nil {
		t.Error("removed endpoint a is still in the registry")
	}
	if _, ok := endpointGauge(t, g.reg)["u "+a.addr()]; ok {
		t.Error("removed endpoint a still has a gauge series")
	}
	if e := g.endpoint(t, d.addr()); e == nil || !e.healthy.Load() || e.outlier.State() != (resilience.OutlierState{}) || e.inFlight.Load() != 0 {
		t.Errorf("new endpoint d = %+v, want healthy with no outlier state and nothing in flight", e)
	}
	for _, u := range []*upstream{b, c} {
		if e := g.endpoint(t, u.addr()); e != eps[u.name] || stateOf(e) != before[u.name] {
			t.Errorf("endpoint %s changed when a was replaced", u.name)
		}
	}
	waitFor(t, "d probed", func() bool { return d.probes.Load() > 0 })
	probes := a.probes.Load()
	time.Sleep(600 * time.Millisecond)
	if n := a.probes.Load() - probes; n > 1 {
		t.Errorf("removed endpoint a was probed %d more times", n)
	}
}

// TestHealthCheckRemoved: removing an upstream's health check stops its
// probers and marks every endpoint healthy; adding it back starts them,
// with endpoints healthy until probes say otherwise (ADR-0030). Close stops
// every prober.
func TestHealthCheckRemoved(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	a.probeStatus.Store(http.StatusServiceUnavailable)
	g := newGateway(t, newResolver(a, b), upstreamOf(fastHealth, a, b))
	waitFor(t, "a unhealthy", func() bool { return endpointGauge(t, g.reg)["u "+a.addr()] == 0 })
	e := g.endpoint(t, a.addr())

	g.apply(t, upstreamOf("outlier_detection: {}", a, b))
	if g.endpoint(t, a.addr()) != e || endpointGauge(t, g.reg)["u "+a.addr()] != 1 {
		t.Error("without a health check, a is not the same endpoint, healthy")
	}
	probes := a.probes.Load()
	time.Sleep(600 * time.Millisecond)
	if n := a.probes.Load() - probes; n > 1 {
		t.Errorf("a was probed %d times after the health check was removed", n)
	}

	g.apply(t, upstreamOf(fastHealth, a, b))
	if endpointGauge(t, g.reg)["u "+a.addr()] != 1 {
		t.Error("a was not healthy right after the health check came back")
	}
	waitFor(t, "a unhealthy again", func() bool { return endpointGauge(t, g.reg)["u "+a.addr()] == 0 })

	g.reg.Close()
	time.Sleep(50 * time.Millisecond)
	probes = a.probes.Load() + b.probes.Load()
	time.Sleep(600 * time.Millisecond)
	if n := a.probes.Load() + b.probes.Load() - probes; n != 0 {
		t.Errorf("%d probes after Close", n)
	}
}

// TestRemovedUpstreamStopsProbing: a reload that removes an upstream stops
// its probers, and a DNS refresh that still holds the removed pool does not
// start them again.
func TestRemovedUpstreamStopsProbing(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	g := newGateway(t, newResolver(a, b), upstreamOf(fastHealth, a))
	waitFor(t, "a probed", func() bool { return a.probes.Load() > 0 })
	removed := (*g.reg.pools.Load())["u"]
	g.apply(t, oneUpstream(b))
	removed.mu.Lock()
	g.reg.resolve(removed) // a refresh that loaded the pools before the reload
	removed.mu.Unlock()
	for _, e := range *removed.endpoints.Load() {
		if e.stop != nil {
			t.Errorf("endpoint %s of the removed upstream has a prober again", e.addr)
		}
	}
	time.Sleep(50 * time.Millisecond)
	probes := a.probes.Load()
	time.Sleep(600 * time.Millisecond)
	if n := a.probes.Load() - probes; n != 0 {
		t.Errorf("a was probed %d times after its upstream was removed", n)
	}
}
