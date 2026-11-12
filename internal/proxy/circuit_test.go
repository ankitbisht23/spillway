package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/resilience"
)

// circuitGauge gathers gw_circuit_state from r, by upstream.
func circuitGauge(t *testing.T, r *Registry) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(r)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "gw_circuit_state" {
			continue
		}
		for _, m := range mf.GetMetric() {
			out[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	return out
}

// answer is a response as the client got it.
type answer struct {
	status     int
	retryAfter string
	attempts   string // Spillway-Attempts, in dev mode
	body       string
}

func (g *gateway) answer(t *testing.T, path string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get(attemptsHeader), string(body)}
}

// held is a test upstream whose requests to /hold wait until release is
// closed, and whose requests answer fail's status when it is set.
type held struct {
	*upstream
	arrived chan struct{}
	release chan struct{}
	fail    atomic.Int32
}

func newHeld(t *testing.T, name string) *held {
	t.Helper()
	h := &held{arrived: make(chan struct{}, 16), release: make(chan struct{})}
	h.upstream = serveUpstream(t, name, func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if r.URL.Path == "/hold" {
			h.arrived <- struct{}{}
			<-h.release
		}
		if code := h.fail.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	return h
}

// hold sends a request to /hold and waits until the upstream has it; the
// returned channel gets its status once it is answered.
func (g *gateway) hold(t *testing.T, u *held) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/hold", nil)
		resp, err := g.Client().Do(req)
		if err != nil {
			done <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case <-u.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("held request never reached the upstream")
	}
	return done
}

// TestCircuitBreaker: through the gateway, an upstream whose attempts fail
// opens its breaker once min_requests attempts are in the window; open, the
// gateway answers 503 circuit_open itself with Retry-After the seconds until
// half-open, and sends nothing upstream; after open_for, half_open_probes
// successful requests close it; a failed probe opens it again (GW-14,
// ADR-0030).
func TestCircuitBreaker(t *testing.T) {
	u := newUpstream(t, "a")
	u.status.Store(http.StatusServiceUnavailable)
	g := newGateway(t, newResolver(u), upstreamOf("circuit: {min_requests: 4, open_for: 10s, half_open_probes: 2}, outlier_detection: {consecutive_failures: 1000}", u))
	g.reg.clock = g.clock
	for i := range 4 {
		if a := g.answer(t, "/"); a.status != http.StatusServiceUnavailable || a.body != "" {
			t.Fatalf("request %d: %+v, want the upstream's 503", i+1, a)
		}
	}
	if s := circuitGauge(t, g.reg)["u"]; s != 1 {
		t.Fatalf("gw_circuit_state %v after 4 failures, want 1 (open)", s)
	}
	open := answer{http.StatusServiceUnavailable, "10", "", `{"error":"upstream circuit open"}`}
	if a := g.answer(t, "/"); a != open {
		t.Errorf("open: %+v, want %+v", a, open)
	}
	g.clock.Advance(2500 * time.Millisecond)
	open.retryAfter = "8" // 7.5 s, rounded up
	if a := g.answer(t, "/"); a != open {
		t.Errorf("open 2.5 s later: %+v, want %+v", a, open)
	}
	if n := u.requests.Load(); n != 4 {
		t.Errorf("upstream got %d requests, want only the 4 before the breaker opened", n)
	}
	hasMetrics(t, g, `gw_requests_total{code_class="5xx",method="GET",origin="gateway",reason="circuit_open",route="r",tenant="none"} 2`,
		`gw_requests_total{code_class="5xx",method="GET",origin="upstream",reason="none",route="r",tenant="none"} 4`)
	if !strings.Contains(g.events.String(), `"msg":"upstream circuit open","upstream":"u","from":"closed","error_rate":1,"requests":4`) {
		t.Errorf("no circuit open log line in %s", g.events.String())
	}

	// Half-open: two successful probes close it.
	g.clock.Advance(7500 * time.Millisecond)
	u.status.Store(0)
	for i := range 2 {
		if a := g.answer(t, "/"); a.status != http.StatusOK {
			t.Fatalf("probe %d: %+v, want 200", i+1, a)
		}
	}
	if s := circuitGauge(t, g.reg)["u"]; s != 0 {
		t.Errorf("gw_circuit_state %v after the probes passed, want 0 (closed)", s)
	}
	for _, msg := range []string{`"msg":"upstream circuit half-open","upstream":"u"`, `"msg":"upstream circuit closed","upstream":"u"`} {
		if !strings.Contains(g.events.String(), msg) {
			t.Errorf("no log line %s", msg)
		}
	}

	// A failed probe opens it again, for another open_for.
	u.status.Store(http.StatusServiceUnavailable)
	for range 4 {
		g.answer(t, "/")
	}
	g.clock.Advance(10 * time.Second)
	if a := g.answer(t, "/"); a.status != http.StatusServiceUnavailable || a.body != "" {
		t.Errorf("failing probe: %+v, want the upstream's 503", a)
	}
	open.retryAfter = "10"
	if a := g.answer(t, "/"); a != open {
		t.Errorf("after a failed probe: %+v, want %+v", a, open)
	}
	if s := circuitGauge(t, g.reg)["u"]; s != 1 {
		t.Errorf("gw_circuit_state %v after a failed probe, want 1 (open)", s)
	}
}

// TestCircuitHalfOpen: half-open lets exactly half_open_probes requests
// through at once; the others get 503 with Retry-After 1 while they run.
func TestCircuitHalfOpen(t *testing.T) {
	u := newHeld(t, "a")
	u.fail.Store(http.StatusBadGateway)
	g := newGateway(t, newResolver(u.upstream), upstreamOf("circuit: {min_requests: 2, open_for: 1s, half_open_probes: 2}", u.upstream))
	g.reg.clock = g.clock
	g.answer(t, "/")
	g.answer(t, "/")
	g.clock.Advance(time.Second)
	u.fail.Store(0)
	probes := []<-chan int{g.hold(t, u), g.hold(t, u)}
	if s := circuitGauge(t, g.reg)["u"]; s != 2 {
		t.Errorf("gw_circuit_state %v with probes out, want 2 (half-open)", s)
	}
	want := answer{http.StatusServiceUnavailable, "1", "", `{"error":"upstream circuit open"}`}
	for range 3 {
		if a := g.answer(t, "/"); a != want {
			t.Errorf("third request while half-open: %+v, want %+v", a, want)
		}
	}
	close(u.release)
	for i, p := range probes {
		if code := <-p; code != http.StatusOK {
			t.Errorf("probe %d: %d, want 200", i+1, code)
		}
	}
	if a := g.answer(t, "/"); a.status != http.StatusOK {
		t.Errorf("after the probes: %+v, want 200", a)
	}
	if s := circuitGauge(t, g.reg)["u"]; s != 0 {
		t.Errorf("gw_circuit_state %v, want 0 (closed)", s)
	}
}

// TestCircuitRetries: a retry needs the breaker closed; when the breaker
// opens mid-request, the next retry is dropped without asking the budget,
// and the client gets the last attempt's answer. A half-open probe gets one
// attempt (ADR-0030).
func TestCircuitRetries(t *testing.T) {
	f := newFlaky(t, "f")
	g := retryGateway(t, retryConfig("circuit: {min_requests: 2, open_for: 1s, half_open_probes: 1}", "retry: {attempts: 3}", f.endpoint()), []*flaky{f})
	g.reg.clock = g.clock
	g.h.jitter = func(time.Duration) time.Duration { return 0 }
	if a := g.answer(t, "/503/9"); a.status != http.StatusServiceUnavailable || a.attempts != "2" || a.body != "" {
		t.Errorf("breaker opened by attempt 2: %+v, want the upstream's 503 after 2 attempts", a)
	}
	hasMetrics(t, g, `gw_retries_total{outcome="sent",upstream="u"} 1`)
	if v := g.metric(t, `gw_retries_total{outcome="budget_exhausted",upstream="u"}`); v != 0 {
		t.Errorf("a retry the breaker dropped counted as budget_exhausted %v times", v)
	}
	g.clock.Advance(time.Second)
	if a := g.answer(t, "/503/9"); a.status != http.StatusServiceUnavailable || a.attempts != "1" {
		t.Errorf("half-open probe: %+v, want the upstream's 503 after 1 attempt", a)
	}
	if calls := f.calls.Load(); calls != 3 {
		t.Errorf("upstream got %d attempts, want 3", calls)
	}
}

// TestCircuitCaps: past max_concurrent, max_connections or max_pending the
// gateway answers 503 at once, reason circuit_open, with Retry-After 1; a
// cap refusal is not retried and is not an upstream failure (GW-14,
// ADR-0030).
func TestCircuitCaps(t *testing.T) {
	overloaded := answer{http.StatusServiceUnavailable, "1", "", `{"error":"upstream overloaded"}`}
	for _, tt := range []struct{ name, circuit string }{
		{"max_concurrent", "max_concurrent: 1"},
		{"max_connections", "max_connections: 1"},
		{"max_pending", "max_pending: 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := newHeld(t, "a")
			cfg := strings.Replace(upstreamOf(fmt.Sprintf("circuit: {%s, min_requests: 1}, outlier_detection: {consecutive_failures: 1}", tt.circuit), u.upstream),
				"match: {path_prefix: /}", "match: {path_prefix: /}, retry: {attempts: 3}", 1)
			g := newGateway(t, newResolver(u.upstream), cfg)
			var first <-chan int
			var release func()
			if tt.name == "max_pending" {
				// Dials wait until released, so the first attempt waits
				// for its connection.
				dialing, unblock := make(chan struct{}, 4), make(chan struct{})
				g.reg.dialer.Control = func(network, address string, c syscall.RawConn) error {
					dialing <- struct{}{}
					<-unblock
					return loopback.control(network, address, c)
				}
				done := make(chan int, 1)
				go func() {
					req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/", nil)
					resp, err := g.Client().Do(req)
					if err != nil {
						done <- 0
						return
					}
					_ = resp.Body.Close()
					done <- resp.StatusCode
				}()
				<-dialing
				first, release = done, sync.OnceFunc(func() { close(unblock) })
			} else {
				first, release = g.hold(t, u), sync.OnceFunc(func() { close(u.release) })
			}
			defer release()
			before := u.requests.Load()
			start := time.Now()
			if a := g.answer(t, "/"); a != overloaded {
				t.Errorf("past the cap: %+v, want %+v", a, overloaded)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("refusal took %v, want it at once", d)
			}
			if n := u.requests.Load() - before; n != 0 {
				t.Errorf("upstream got %d requests past the cap, want 0", n)
			}
			p := (*g.reg.pools.Load())["u"]
			if st := p.breaker.Status(g.reg.clock.Now()); st.State != resilience.Closed || st.Failed != 0 {
				t.Errorf("breaker %+v after a cap refusal, want closed with no failure", st)
			}
			if e := g.endpoint(t, u.addr()); e.outlier.State().Failures != 0 || e.outlier.Ejected(g.reg.clock.Now()) {
				t.Errorf("endpoint %+v after a cap refusal, want no failure counted", e.outlier.State())
			}
			hasMetrics(t, g, `gw_requests_total{code_class="5xx",method="GET",origin="gateway",reason="circuit_open",route="r",tenant="none"} 1`)
			if v := g.metric(t, `gw_retries_total{outcome="sent",upstream="u"}`); v != 0 {
				t.Errorf("%v retries of a cap refusal", v)
			}
			release()
			if code := <-first; code != http.StatusOK {
				t.Errorf("the request holding the place: %d, want 200", code)
			}
		})
	}
}

// TestReloadKeepsOpenBreaker (GW-29): a reload that changes the breaker's
// settings keeps it open until its open_for ends; a changed window empties
// the window and keeps the state (ADR-0030).
func TestReloadKeepsOpenBreaker(t *testing.T) {
	u := newUpstream(t, "a")
	u.status.Store(http.StatusServiceUnavailable)
	cfg := func(circuit string) string {
		return upstreamOf("circuit: {min_requests: 2, open_for: 10s"+circuit+"}", u)
	}
	g := newGateway(t, newResolver(u), cfg(""))
	g.reg.clock = g.clock
	g.answer(t, "/")
	g.answer(t, "/")
	breaker := (*g.reg.pools.Load())["u"].breaker
	before := breaker.Status(g.clock.Now())
	if before.State != resilience.Open || before.Total != 2 {
		t.Fatalf("breaker %+v, want open on 2 attempts", before)
	}
	g.apply(t, cfg(", error_rate: 0.9, half_open_probes: 3"))
	if got := breaker.Status(g.clock.Now()); got != before || (*g.reg.pools.Load())["u"].breaker != breaker {
		t.Errorf("breaker %+v after a reload, want %+v", got, before)
	}
	g.clock.Advance(4 * time.Second)
	if a := g.answer(t, "/"); a.status != http.StatusServiceUnavailable || a.retryAfter != "6" {
		t.Errorf("after a reload: %+v, want refused with Retry-After 6", a)
	}
	g.apply(t, cfg(", window: 20s"))
	if got := breaker.Status(g.clock.Now()); got.State != resilience.Open || got.Total != 0 || got.Until != before.Until {
		t.Errorf("breaker %+v after a changed window, want open until %v with an empty window", got, before.Until)
	}
	if n := u.requests.Load(); n != 2 {
		t.Errorf("upstream got %d requests, want 2", n)
	}
}
