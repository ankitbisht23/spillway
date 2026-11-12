package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// flaky is a test upstream for retries. A request to /<how>/<n> fails the
// first n attempts it sees of each request (by X-Request-Id) as how says,
// and answers the later ones 200 with a retryEcho:
//
//   - a status code: that status;
//   - reset: the connection closed before the headers;
//   - slow: no answer until the attempt ends;
//   - cut: 200 with part of its body, then the connection closed;
//   - early: 103 Early Hints, then 503.
type flaky struct {
	*httptest.Server
	name  string
	calls atomic.Int64
	mu    sync.Mutex
	seen  map[string]int // attempts per request ID
}

type retryEcho struct {
	Upstream string
	Attempt  int // this upstream's attempt of the request
	Method   string
	Deadline []string
	Body     string
}

func newFlaky(t *testing.T, name string) *flaky {
	t.Helper()
	f := &flaky{name: name, seen: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *flaky) serve(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.mu.Lock()
	f.seen[r.Header.Get("X-Request-Id")]++
	n := f.seen[r.Header.Get("X-Request-Id")]
	f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	how, count, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if fails, _ := strconv.Atoi(count); n <= fails {
		switch how {
		case "reset":
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
		case "slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		case "cut":
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "0123456789")
			_ = http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		case "early":
			w.Header().Set("Link", "</a.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			code, _ := strconv.Atoi(how)
			w.WriteHeader(code)
		}
		return
	}
	_ = json.NewEncoder(w).Encode(retryEcho{f.name, n, r.Method, r.Header.Values(DeadlineHeader), string(body)})
}

// maxSeen is the most attempts f saw of any one request.
func (f *flaky) maxSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.seen {
		n = max(n, v)
	}
	return n
}

func (f *flaky) port() string {
	_, port, _ := net.SplitHostPort(f.Listener.Addr().String())
	return port
}

func (f *flaky) addr() string { return "127.0.0.1:" + f.port() }

// retryConfig is a config whose upstream u has the endpoints named in eps
// (name:port) and extra fields, balanced round robin unless fields set lb,
// with one route r to it with extra route fields.
func retryConfig(fields, route string, eps ...string) string {
	var list []string
	for _, e := range eps {
		list = append(list, fmt.Sprintf("{address: '%s'}", e))
	}
	if !strings.Contains(fields, "lb:") {
		fields = strings.TrimPrefix(fields+", lb: round_robin", ", ")
	}
	return fmt.Sprintf("upstreams: [{name: u, endpoints: [%s], %s}]\nroutes: [{name: r, upstream: u, match: {path_prefix: /}, %s}]\n",
		strings.Join(list, ", "), fields, route)
}

func (f *flaky) endpoint() string { return f.name + ":" + f.port() }

// retryGateway is a dev-mode gateway, so that responses carry
// Spillway-Attempts, in front of fs and us.
func retryGateway(t *testing.T, config string, fs []*flaky, us ...*upstream) *gateway {
	t.Helper()
	res := newResolver(us...)
	for _, f := range fs {
		res.set(f.name, "127.0.0.1")
	}
	return newDevGateway(t, res, config, nil, nil, true)
}

// response is what a client got from the gateway.
type retryResponse struct {
	status   int
	header   http.Header
	echo     retryEcho
	attempts string // Spillway-Attempts
	err      error  // reading the body
}

func (g *gateway) do(t *testing.T, method, path string, body io.Reader, header http.Header) retryResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, g.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := retryResponse{status: resp.StatusCode, header: resp.Header, attempts: resp.Header.Get(attemptsHeader)}
	b, err := io.ReadAll(resp.Body)
	out.err = err
	if resp.StatusCode == http.StatusOK && err == nil && method != http.MethodHead {
		if err := json.Unmarshal(b, &out.echo); err != nil {
			t.Fatalf("%s %s: %v in %q", method, path, err, b)
		}
	}
	return out
}

// metric returns a series' value in g's /metrics, 0 if it is absent.
func (g *gateway) metric(t *testing.T, series string) float64 {
	t.Helper()
	for line := range strings.SplitSeq(scrape(t, g.metrics), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

// TestRetryConditions: a route with attempts retries a connect error, a
// reset before the headers, the attempt's own timeout, and 502, 503 and
// 504, and nothing else; when every attempt fails the client gets the last
// one's answer (GW-12, ADR-0030).
func TestRetryConditions(t *testing.T) {
	f := newFlaky(t, "f")
	// The breaker is held off: the cases fail more than min_requests
	// attempts.
	g := retryGateway(t, retryConfig("circuit: {min_requests: 1000}", "retry: {attempts: 2}, timeouts: {per_try: 200ms, total: 5s}", f.endpoint()), []*flaky{f})
	tests := []struct {
		path   string
		status int
		calls  int64
	}{
		{"/502/1", 200, 2},
		{"/503/1", 200, 2},
		{"/504/1", 200, 2},
		{"/reset/1", 200, 2},
		{"/slow/1", 200, 2},
		{"/500/1", 500, 1},
		{"/501/1", 501, 1},
		{"/505/1", 505, 1},
		{"/507/1", 507, 1},
		{"/404/1", 404, 1},
		{"/429/1", 429, 1},
		{"/409/1", 409, 1},
		{"/ok/0", 200, 1},
		{"/503/2", 503, 2},   // the last attempt's answer
		{"/reset/2", 502, 2}, // the gateway's, as without retries
		{"/slow/2", 504, 2},
	}
	for _, tt := range tests {
		// Go's transport sends a request again, once and below the
		// attempt level, when a reused idle connection closes before any
		// response byte: start each case on a new connection.
		(*g.reg.pools.Load())["u"].transport.CloseIdleConnections()
		before := f.calls.Load()
		r := g.do(t, http.MethodGet, tt.path, nil, nil)
		if calls := f.calls.Load() - before; r.status != tt.status || calls != tt.calls || r.attempts != strconv.FormatInt(tt.calls, 10) {
			t.Errorf("GET %s: %d after %d attempts (Spillway-Attempts %q); want %d after %d", tt.path, r.status, calls, r.attempts, tt.status, tt.calls)
		}
	}

	// A connect error: the first endpoint refuses connections.
	refused := newUpstream(t, "refused")
	refused.Close()
	g = retryGateway(t, retryConfig("", "retry: {attempts: 2}", "refused:"+refused.port(), f.endpoint()), []*flaky{f}, refused)
	for i := range 4 { // round robin: every other request starts on the refused endpoint
		want := []string{"2", "1"}[i%2]
		if r := g.do(t, http.MethodGet, "/ok/0", nil, nil); r.status != http.StatusOK || r.attempts != want {
			t.Errorf("request %d with a refusing endpoint: %d after %s attempts, want 200 after %s", i, r.status, r.attempts, want)
		}
	}
	hasMetrics(t, g, `gw_retries_total{outcome="sent",upstream="u"} 2`,
		`gw_upstream_duration_seconds_count{attempt="1",upstream="u"} 4`,
		`gw_upstream_duration_seconds_count{attempt="2",upstream="u"} 2`)
}

// TestRetryMethods: only idempotent methods are retried, unless the route
// sets non_idempotent (GW-12, D4).
func TestRetryMethods(t *testing.T) {
	f := newFlaky(t, "f")
	for _, tt := range []struct {
		route   string
		retried map[string]bool
	}{
		{"retry: {attempts: 2}", map[string]bool{
			"GET": true, "HEAD": true, "OPTIONS": true, "PUT": true, "DELETE": true, "TRACE": true,
			"POST": false, "PATCH": false, "BREW": false,
		}},
		{"retry: {attempts: 2, non_idempotent: true}", map[string]bool{"GET": true, "POST": true, "PATCH": true, "BREW": true}},
		{"retry: {attempts: 1, non_idempotent: true}", map[string]bool{"GET": false, "POST": false}},
		{"", map[string]bool{"GET": false, "PUT": false}},
	} {
		g := retryGateway(t, retryConfig("", tt.route, f.endpoint()), []*flaky{f})
		for method, retried := range tt.retried {
			before := f.calls.Load()
			r := g.do(t, method, "/503/1", nil, nil)
			want, calls := http.StatusServiceUnavailable, int64(1)
			if retried {
				want, calls = http.StatusOK, 2
			}
			if got := f.calls.Load() - before; r.status != want || got != calls {
				t.Errorf("%q, %s: %d after %d attempts, want %d after %d", tt.route, method, r.status, got, want, calls)
			}
		}
	}
}

// TestRetryBody: a body up to 64 KiB is buffered and sent again on a retry;
// a longer one, or one whose Content-Length says so, streams with one
// attempt and arrives whole; the route's body limit still answers 413
// (GW-12, ADR-0030).
func TestRetryBody(t *testing.T) {
	f := newFlaky(t, "f")
	g := retryGateway(t, retryConfig("", "retry: {attempts: 2, non_idempotent: true}", f.endpoint()), []*flaky{f})
	small, limit, over := "hello", strings.Repeat("a", maxRetryBody), strings.Repeat("b", maxRetryBody+1)
	unknown := func(s string) io.Reader { return io.MultiReader(strings.NewReader(s)) } // sent chunked
	for _, tt := range []struct {
		name    string
		body    func() io.Reader
		want    string
		retried bool
	}{
		{"small", func() io.Reader { return strings.NewReader(small) }, small, true},
		{"small, chunked", func() io.Reader { return unknown(small) }, small, true},
		{"empty, chunked", func() io.Reader { return unknown("") }, "", true},
		{"64 KiB", func() io.Reader { return strings.NewReader(limit) }, limit, true},
		{"64 KiB, chunked", func() io.Reader { return unknown(limit) }, limit, true},
		{"over 64 KiB", func() io.Reader { return strings.NewReader(over) }, over, false},
		{"over 64 KiB, chunked", func() io.Reader { return unknown(over) }, over, false},
	} {
		before := f.calls.Load()
		r := g.do(t, http.MethodPost, "/503/1", tt.body(), nil)
		want, calls := http.StatusServiceUnavailable, int64(1)
		if tt.retried {
			want, calls = http.StatusOK, 2
		}
		if got := f.calls.Load() - before; r.status != want || got != calls || tt.retried && r.echo.Body != tt.want {
			t.Errorf("%s: %d after %d attempts with %d bytes, want %d after %d", tt.name, r.status, got, len(r.echo.Body), want, calls)
		}
		if !tt.retried {
			if r := g.do(t, http.MethodPost, "/ok/0", tt.body(), nil); r.status != http.StatusOK || r.echo.Body != tt.want {
				t.Errorf("%s, streamed: %d with %d bytes, want 200 with %d", tt.name, r.status, len(r.echo.Body), len(tt.want))
			}
		}
	}

	g = retryGateway(t, retryConfig("", "retry: {attempts: 2, non_idempotent: true}, max_body_bytes: 10", f.endpoint()), []*flaky{f})
	before := f.calls.Load()
	if r := g.do(t, http.MethodPost, "/ok/0", unknown(strings.Repeat("c", 20)), nil); r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked body over the route's limit: %d, want 413", r.status)
	}
	if r := g.do(t, http.MethodPost, "/503/1", unknown("0123456789"), nil); r.status != http.StatusOK || r.echo.Body != "0123456789" {
		t.Errorf("chunked body at the route's limit: %d with %q, want 200 after a retry", r.status, r.echo.Body)
	}
	if calls := f.calls.Load() - before; calls > 3 {
		t.Errorf("%d upstream attempts, want at most 3", calls)
	}
}

// TestNoRetryAfterResponseBytes: an attempt whose response headers went to
// the client, or a 1xx before them, is never retried (GW-12, ADR-0030).
func TestNoRetryAfterResponseBytes(t *testing.T) {
	f := newFlaky(t, "f")
	g := retryGateway(t, retryConfig("", "retry: {attempts: 3}", f.endpoint()), []*flaky{f})

	// The gateway may still hold the headers in its write buffer when the
	// body fails, so the client sees either a cut body or no response.
	before := f.calls.Load()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/cut/1", nil)
	resp, err := g.Client().Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if calls := f.calls.Load() - before; err == nil || calls != 1 {
		t.Errorf("response cut mid-body: error %v after %d attempts; want an error after 1", err, calls)
	}

	before = f.calls.Load()
	var interim []int
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/early/1", nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		interim = append(interim, code)
		return nil
	}}))
	resp, err = g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls := f.calls.Load() - before; resp.StatusCode != http.StatusServiceUnavailable || calls != 1 || len(interim) != 1 || interim[0] != http.StatusEarlyHints {
		t.Errorf("103 then 503: %d after %d attempts, 1xx seen %v; want 503 after 1, [103]", resp.StatusCode, calls, interim)
	}
}

// TestBackoff: before attempt n + 1 the backoff is drawn from
// [0, min(250 ms, 25 ms × 2^(n−1))) (full jitter, ADR-0030).
func TestBackoff(t *testing.T) {
	top := func(d time.Duration) time.Duration { return d - 1 }
	zero := func(time.Duration) time.Duration { return 0 }
	for n, want := range []time.Duration{25, 50, 100, 200, 250, 250} {
		want *= time.Millisecond
		if got := backoff(n+1, top); got != want-1 {
			t.Errorf("backoff(%d) at most %v, want just under %v", n+1, got, want)
		}
		if got := backoff(n+1, zero); got != 0 {
			t.Errorf("backoff(%d) at least %v, want 0", n+1, got)
		}
	}
	h := NewHandler(nil, nil, nil, nil, nil, nil, false, 4096, nil, discard)
	for range 1000 {
		if d := backoff(2, h.jitter); d < 0 || d >= 50*time.Millisecond {
			t.Fatalf("backoff(2) = %v, want [0, 50ms)", d)
		}
	}
}

// TestRetryDeadline: retries wait their backoff and stop when the backoff
// and 25 ms no longer fit before the total deadline; the client then gets
// the last attempt's answer (GW-12, ADR-0030).
func TestRetryDeadline(t *testing.T) {
	f := newFlaky(t, "f")
	g := retryGateway(t, retryConfig("", "retry: {attempts: 5}, timeouts: {total: 390ms}", f.endpoint()), []*flaky{f})
	g.h.jitter = func(d time.Duration) time.Duration { return d - 1 } // 25, 50, 100, then 200 ms
	start := time.Now()
	r := g.do(t, http.MethodGet, "/503/9", nil, nil)
	// Attempt 4 ends after at least 175 ms of backoff, and 200 + 25 ms
	// more would pass 390 ms.
	if elapsed := time.Since(start); r.status != http.StatusServiceUnavailable || r.attempts != "4" || elapsed < 175*time.Millisecond || elapsed >= 390*time.Millisecond {
		t.Errorf("%d after %s attempts in %v; want 503 after 4, in 175 to 390 ms", r.status, r.attempts, elapsed)
	}
	hasMetrics(t, g, `gw_retries_total{outcome="sent",upstream="u"} 3`)
	if v := g.metric(t, `gw_retries_total{outcome="budget_exhausted",upstream="u"}`); v != 0 {
		t.Errorf("%v retries refused by the budget, want 0: the deadline dropped the retry", v)
	}
}

// TestRetryBudget: retries past the upstream's budget are dropped, not
// queued, and the client gets the attempt's answer; the budget is
// ratio × first attempts + min_per_sec × 10 over a rolling 10 s, and
// gw_retries_total counts both outcomes (GW-13, ADR-0030).
func TestRetryBudget(t *testing.T) {
	f := newFlaky(t, "f")
	// The breaker is held off: the 23 attempts all fail.
	g := retryGateway(t, retryConfig("retry_budget: {ratio: 0.1, min_per_sec: 0.1}, circuit: {min_requests: 1000}", "retry: {attempts: 2}", f.endpoint()), []*flaky{f})
	clock := ratelimit.NewFakeClock(t0)
	g.reg.clock = clock
	g.h.jitter = func(time.Duration) time.Duration { return 0 }
	var attempts []string
	for range 20 {
		r := g.do(t, http.MethodGet, "/503/9", nil, nil)
		if r.status != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", r.status)
		}
		attempts = append(attempts, r.attempts)
	}
	// Request i's retry is granted while R + 1 ≤ 0.1 × i + 1: at i = 1,
	// 10 and 20.
	for i, a := range attempts {
		want := "1"
		if i == 0 || i == 9 || i == 19 {
			want = "2"
		}
		if a != want {
			t.Errorf("request %d: %s attempts, want %s", i+1, a, want)
		}
	}
	if calls := f.calls.Load(); calls != 23 {
		t.Errorf("%d upstream attempts, want 23", calls)
	}
	hasMetrics(t, g,
		`gw_retries_total{outcome="sent",upstream="u"} 3`,
		`gw_retries_total{outcome="budget_exhausted",upstream="u"} 17`,
		`gw_upstream_duration_seconds_count{attempt="1",upstream="u"} 20`,
		`gw_upstream_duration_seconds_count{attempt="2",upstream="u"} 3`)

	clock.Advance(10 * time.Second) // the window rolls past all of it
	if r := g.do(t, http.MethodGet, "/503/1", nil, nil); r.status != http.StatusOK || r.attempts != "2" {
		t.Errorf("10 s later: %d after %s attempts, want 200 after 2", r.status, r.attempts)
	}
}

// TestDeadlineHeader: every attempt carries Spillway-Deadline-Ms, the whole
// milliseconds left of min(total, per_try); a client's copy is never
// forwarded (GW-15, ADR-0030).
func TestDeadlineHeader(t *testing.T) {
	f := newFlaky(t, "f")
	forged := http.Header{DeadlineHeader: {"99999", "1"}}
	for _, tt := range []struct {
		route    string
		path     string
		min, max int
	}{
		{"timeouts: {per_try: 1s, total: 3s}", "/ok/0", 900, 1000},
		{"timeouts: {total: 500ms}", "/ok/0", 400, 500},
		{"", "/ok/0", 14_000, 15_000},
		// The second attempt after a 100 ms per-try timeout.
		{"retry: {attempts: 2}, timeouts: {per_try: 100ms, total: 3s}", "/slow/1", 50, 100},
		// The second attempt after a 1 s per-try timeout, with 300 ms of
		// the total left.
		{"retry: {attempts: 2}, timeouts: {per_try: 1s, total: 1300ms}", "/slow/1", 200, 300},
	} {
		g := retryGateway(t, retryConfig("", tt.route, f.endpoint()), []*flaky{f})
		g.h.jitter = func(time.Duration) time.Duration { return 0 }
		r := g.do(t, http.MethodGet, tt.path, nil, forged)
		if r.status != http.StatusOK || len(r.echo.Deadline) != 1 {
			t.Errorf("%q: %d, upstream got %s %q; want 200 and one value", tt.route, r.status, DeadlineHeader, r.echo.Deadline)
			continue
		}
		if ms, err := strconv.Atoi(r.echo.Deadline[0]); err != nil || ms <= tt.min || ms > tt.max {
			t.Errorf("%q: %s %q, want more than %d and at most %d", tt.route, DeadlineHeader, r.echo.Deadline[0], tt.min, tt.max)
		}
	}
	if got := deadlineMs(time.Now().Add(-time.Second)); got != "1" {
		t.Errorf("deadline passed: %s, want 1", got)
	}
}

// TestRetryPrefersUntried: a retry goes to an endpoint the request has not
// tried while there is one, with every algorithm (GW-07, ADR-0030).
func TestRetryPrefersUntried(t *testing.T) {
	for _, lb := range []string{"round_robin", "p2c", "consistent_hash, hash_on: '{header:X-Key}'"} {
		a, b := newFlaky(t, "a"), newFlaky(t, "b")
		c := newUpstream(t, "c") // answers every path
		// Up to 2 retries per request: more than the default budget. Two
		// of three attempts can fail: more than the breaker's default
		// error rate.
		g := retryGateway(t, retryConfig("lb: "+lb+", outlier_detection: {consecutive_failures: 1000}, retry_budget: {ratio: 1, min_per_sec: 100}, circuit: {min_requests: 1000}",
			"retry: {attempts: 3}", a.endpoint(), b.endpoint(), "c:"+c.port()), []*flaky{a, b}, c)
		for i := range 30 {
			if r := g.do(t, http.MethodGet, "/503/9", nil, http.Header{"X-Key": {strconv.Itoa(i)}}); r.status != http.StatusOK {
				t.Fatalf("%s: request %d got %d after %s attempts, want 200", lb, i, r.status, r.attempts)
			}
		}
		if a.maxSeen() > 1 || b.maxSeen() > 1 || a.calls.Load() == 0 || b.calls.Load() == 0 {
			t.Errorf("%s: a and b saw a request up to %d and %d times (%d and %d attempts); want once, and both used",
				lb, a.maxSeen(), b.maxSeen(), a.calls.Load(), b.calls.Load())
		}
	}
}

// TestRetryAttemptState: each attempt counts in flight on its own endpoint
// until it fails or its response ends, and outlier ejection sees each
// attempt's outcome on the endpoint it went to (GW-07, GW-09, ADR-0030).
func TestRetryAttemptState(t *testing.T) {
	bad := newFlaky(t, "bad")
	hold, held := make(chan struct{}), make(chan struct{}, 1)
	good := newUpstream(t, "good")
	good.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("hold") {
			held <- struct{}{}
			<-hold
		}
		_, _ = io.WriteString(w, `{"Upstream":"good"}`)
	})
	g := retryGateway(t, retryConfig("outlier_detection: {consecutive_failures: 2}", "retry: {attempts: 2}",
		bad.endpoint(), "good:"+good.port()), []*flaky{bad}, good)

	// Round robin starts on bad; its attempt has ended while the retry is
	// in flight on good.
	done := make(chan retryResponse)
	go func() { done <- g.do(t, http.MethodGet, "/503/9?hold", nil, nil) }()
	<-held
	if b, gd := g.endpoint(t, bad.addr()).inFlight.Load(), g.endpoint(t, good.addr()).inFlight.Load(); b != 0 || gd != 1 {
		t.Errorf("retry in flight: %d in flight on bad and %d on good, want 0 and 1", b, gd)
	}
	close(hold)
	if r := <-done; r.status != http.StatusOK || r.attempts != "2" {
		t.Errorf("held request: %d after %s attempts, want 200 after 2", r.status, r.attempts)
	}
	waitFor(t, "in-flight counts back to 0", func() bool {
		return g.endpoint(t, bad.addr()).inFlight.Load() == 0 && g.endpoint(t, good.addr()).inFlight.Load() == 0
	})

	// Requests 2 and 3: good, then bad's second failure in a row, which
	// ejects it although the request succeeded on its retry.
	for range 2 {
		if r := g.do(t, http.MethodGet, "/503/9", nil, nil); r.status != http.StatusOK {
			t.Fatalf("status %d, want 200", r.status)
		}
	}
	if v := endpointGauge(t, g.reg)["u "+bad.addr()]; v != 0 {
		t.Errorf("bad not ejected after 2 failed attempts in a row")
	}
	for range 6 {
		if r := g.do(t, http.MethodGet, "/503/9", nil, nil); r.status != http.StatusOK || r.attempts != "1" {
			t.Errorf("bad ejected: %d after %s attempts, want 200 after 1", r.status, r.attempts)
		}
	}
	if calls := bad.calls.Load(); calls != 2 {
		t.Errorf("bad got %d attempts, want 2", calls)
	}
}
