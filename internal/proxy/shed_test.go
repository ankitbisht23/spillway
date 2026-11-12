package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// shedAnswer is what a client gets past either cap (D5, ADR-0030).
var shedAnswer = answer{http.StatusServiceUnavailable, "1", "", `{"error":"overloaded"}`}

// shedSeries is gw_requests_total's series for shed connections, and for
// shed GET requests with method="GET".
const shedSeries = `gw_requests_total{code_class="5xx",method="other",origin="gateway",reason="shed",route="none",tenant="none"}`

// capped replaces g's handler and server with ones that shed past
// maxInFlight requests, with fresh metrics and access logs.
func (g *gateway) capped(t *testing.T, maxInFlight int64) {
	t.Helper()
	g.Close()
	g.metrics, g.logs = prometheus.NewRegistry(), &logBuffer{}
	rec := telemetry.NewRecorder(g.metrics, slog.New(slog.NewJSONHandler(g.logs, nil)), 1)
	g.h = NewHandler(g.current.Load, g.identity.Load, nil, g.reg, g.limiters, nil, false, maxInFlight, rec, slog.New(slog.DiscardHandler))
	g.Server = httptest.NewServer(g.h)
	t.Cleanup(g.Close)
}

// TestInFlightCap: with maxInFlight requests held at the upstream, the next
// request gets 503 with Retry-After 1 at once, reason shed, route none, and
// nothing goes upstream; it is counted and access-logged. Once the held
// requests finish, or the gateway answers itself, their places are free
// again (GW-26, ADR-0030).
func TestInFlightCap(t *testing.T) {
	u := newHeld(t, "a")
	g := newGateway(t, newResolver(u.upstream), upstreamOf("", u.upstream))
	g.capped(t, 2)
	first, second := g.hold(t, u), g.hold(t, u)
	if n := g.h.InFlight(); n != 2 {
		t.Fatalf("%d in flight, want 2", n)
	}
	before := u.requests.Load()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "shed-1")
	start := time.Now()
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// A refusal takes microseconds; 1 s leaves room for a host stall
	// (ADR-0027).
	if d := time.Since(start); d > time.Second {
		t.Errorf("shed answer took %v, want it at once", d)
	}
	if a := (answer{resp.StatusCode, resp.Header.Get("Retry-After"), "", string(body)}); a != shedAnswer {
		t.Errorf("past the cap: %+v, want %+v", a, shedAnswer)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	if n := u.requests.Load() - before; n != 0 {
		t.Errorf("upstream got %d requests past the cap, want 0", n)
	}
	hasMetrics(t, g, strings.Replace(shedSeries, `method="other"`, `method="GET"`, 1)+" 1")
	if line := g.logs.access(t, "shed-1"); line["status"] != 503.0 || line["route"] != "none" || line["upstream"] != "" || line["attempts"] != 0.0 {
		t.Errorf("access log line of the shed request: %v", line)
	}

	close(u.release)
	for _, done := range []<-chan int{first, second} {
		if code := <-done; code != http.StatusOK {
			t.Errorf("held request: %d, want 200", code)
		}
	}
	waitFor(t, "in-flight count back to 0", func() bool { return g.h.InFlight() == 0 })
	for range 3 {
		if a := g.answer(t, "/"); a.status != http.StatusOK {
			t.Errorf("after the held requests: %+v, want 200", a)
		}
	}
	// Answers the gateway gives itself free their places too.
	if got := g.send(t, "get", "/", nil); got.status != http.StatusBadRequest {
		t.Errorf("lower-case method: %d, want 400", got.status)
	}
	waitFor(t, "in-flight count back to 0", func() bool { return g.h.InFlight() == 0 })
}

// TestInFlightCapHTTP2: HTTP/2 streams count one each, so a single
// connection's streams past the cap are shed (ADR-0030).
func TestInFlightCapHTTP2(t *testing.T) {
	u := newHeld(t, "a")
	g := newGateway(t, newResolver(u.upstream), upstreamOf("", u.upstream))
	g.capped(t, 1)
	g.Close()
	g.Server = httptest.NewUnstartedServer(g.h)
	g.EnableHTTP2 = true
	g.StartTLS()
	t.Cleanup(g.Close)
	held := g.hold(t, u)
	if f := fetch(t, g.Client(), g.URL+"/"); f.proto != "HTTP/2.0" || f.answer != shedAnswer {
		t.Errorf("second stream: %s %+v, want HTTP/2 and %+v", f.proto, f.answer, shedAnswer)
	}
	close(u.release)
	if code := <-held; code != http.StatusOK {
		t.Errorf("held stream: %d, want 200", code)
	}
}

// connCapServer serves h on a plain or TLS listener with a cap of max
// connections, and returns the cap, the address, the metrics, and the log.
func connCapServer(t *testing.T, max int64, tlsConfig *tls.Config, h http.Handler) (*ConnLimit, string, *prometheus.Registry, *logBuffer) {
	t.Helper()
	metrics, logs := prometheus.NewRegistry(), &logBuffer{}
	rec := telemetry.NewRecorder(metrics, slog.New(slog.DiscardHandler), 1)
	l := NewConnLimit(max, rec, slog.New(slog.NewJSONHandler(logs, nil)))
	srv := NewServer(h, slog.New(slog.DiscardHandler))
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig != nil {
		srv.TLSConfig = tlsConfig
		go func() { _ = srv.ServeTLS(l.Listener(ln, tlsConfig), "", "") }()
	} else {
		go func() { _ = srv.Serve(l.Listener(ln, nil)) }()
	}
	t.Cleanup(func() { _ = srv.Close() })
	return l, ln.Addr().String(), metrics, logs
}

// keepOpen sends one request on a new connection and leaves it open and
// idle; it returns the connection.
func keepOpen(t *testing.T, addr string) net.Conn {
	t.Helper()
	c := dial(t, addr)
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first connection: %d, want 200", resp.StatusCode)
	}
	return c
}

// fetched is a response as fetch returns it.
type fetched struct {
	answer
	proto string
	close bool // Connection: close
	tls   bool
}

// fetch sends a GET with client and returns what it got.
func fetch(t *testing.T, client *http.Client, url string) fetched {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return fetched{answer{resp.StatusCode, resp.Header.Get("Retry-After"), "", string(body)}, resp.Proto, resp.Close, resp.TLS != nil}
}

// metricOf returns series' value in g's metrics, "" if it has none.
func metricOf(t *testing.T, g prometheus.Gatherer, series string) string {
	t.Helper()
	for line := range strings.SplitSeq(scrape(t, g), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

// TestConnectionCap: past the cap, a connection gets the fixed 503 at once,
// without the gateway reading its request, and is closed; it is counted as
// a shed request and logged once a second. Closing a counted connection,
// hijacked ones included, frees its place (GW-26, ADR-0030).
func TestConnectionCap(t *testing.T) {
	hijacked := make(chan struct{})
	l, addr, metrics, logs := connCapServer(t, 1, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hijack" {
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		close(hijacked)
		_, _ = brw.ReadString('\n') // until the client closes
	}))
	open := keepOpen(t, addr)
	if n := l.Open(); n != 1 {
		t.Fatalf("%d open, want 1", n)
	}

	// A raw connection that sends nothing gets the answer.
	start := time.Now()
	raw := dial(t, addr)
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(raw)
	if d := time.Since(start); d > time.Second {
		t.Errorf("shed connection answered in %v, want at once", d)
	}
	if err != nil || string(got) != shedResponse {
		t.Errorf("shed connection got %q, %v; want %q then EOF", got, err, shedResponse)
	}

	// An HTTP client's request gets it too.
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	if f := fetch(t, client, "http://"+addr+"/"); f.answer != shedAnswer || !f.close {
		t.Errorf("HTTP client past the cap: %+v, close %v; want %+v and Connection: close", f.answer, f.close, shedAnswer)
	}
	if v := metricOf(t, metrics, shedSeries); v != "2" {
		t.Errorf("%s = %q, want 2", shedSeries, v)
	}
	if n := strings.Count(logs.String(), `"msg":"shedding connections"`); n != 1 || !strings.Contains(logs.String(), `"connections":1,"max_connections":1`) {
		t.Errorf("want one log line for the first shed connection within a second, got\n%s", logs.String())
	}
	if n := l.Open(); n != 1 {
		t.Errorf("%d open after shedding, want 1", n)
	}

	// Closing the open connection frees its place; a hijacked connection
	// holds it until it closes.
	_ = open.Close()
	waitFor(t, "the place freed", func() bool { return l.Open() == 0 })
	h := dial(t, addr)
	_, _ = io.WriteString(h, "GET /hijack HTTP/1.1\r\nHost: x\r\n\r\n")
	<-hijacked
	if n := l.Open(); n != 1 {
		t.Errorf("%d open with a hijacked connection, want 1", n)
	}
	_ = h.Close()
	waitFor(t, "the hijacked connection's place freed", func() bool { return l.Open() == 0 })
	_ = keepOpen(t, addr)
}

// TestConnectionCapTLS: on the TLS listener a shed connection gets a
// handshake whose ALPN offers only HTTP/1.1, then the same 503, even for a
// client that prefers HTTP/2 (ADR-0030).
func TestConnectionCapTLS(t *testing.T) {
	c, roots, _, _ := newCerts(t, 1)
	l, addr, metrics, _ := connCapServer(t, 1, c.TLSConfig(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := func() *http.Client {
		cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}}
		t.Cleanup(cl.CloseIdleConnections)
		return cl
	}
	if f := fetch(t, client(), "https://"+addr+"/"); f.proto != "HTTP/2.0" { // stays open, idle
		t.Errorf("first connection: %s, want HTTP/2", f.proto)
	}
	if n := l.Open(); n != 1 {
		t.Fatalf("%d open, want 1", n)
	}
	start := time.Now()
	f := fetch(t, client(), "https://"+addr+"/")
	if d := time.Since(start); d > time.Second {
		t.Errorf("shed TLS connection answered in %v, want at once", d)
	}
	if f.answer != shedAnswer || f.proto != "HTTP/1.1" || !f.tls {
		t.Errorf("past the cap over TLS: %s %+v, TLS %v; want HTTP/1.1 over TLS and %+v", f.proto, f.answer, f.tls, shedAnswer)
	}
	if v := metricOf(t, metrics, shedSeries); v != "1" {
		t.Errorf("%s = %q, want 1", shedSeries, v)
	}
}

// TestShedPathFull: with maxShedding connections being shed (each held
// until the client closes or the deadline), the next is reset at once; once
// the deadline frees the shed path, connections get the 503 again
// (ADR-0030).
func TestShedPathFull(t *testing.T) {
	_, addr, metrics, _ := connCapServer(t, 1, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	keepOpen(t, addr)
	start := time.Now()
	for i := range maxShedding {
		c := dial(t, addr)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "HTTP/1.1 503") {
			t.Fatalf("shed connection %d: %q, %v", i+1, line, err)
		}
	}
	// The 256 connections stay in the shed path until shedDeadline; the
	// test needs them all there, so it gives up if a host stall made
	// opening them take a large part of it (ADR-0027).
	if d := time.Since(start); d > shedDeadline/2 {
		t.Skipf("opening %d connections took %v", maxShedding, d)
	}
	if got, err := firstLine(t, addr); got != "" || !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("past the shed path: got %q, %v; want a reset", got, err)
	}
	if v := metricOf(t, metrics, shedSeries); v != "257" {
		t.Errorf("%s = %q, want 257", shedSeries, v)
	}
	waitFor(t, "the shed path freed", func() bool {
		line, _ := firstLine(t, addr)
		return strings.HasPrefix(line, "HTTP/1.1 503")
	})
}

// firstLine opens a connection to addr and returns the first line it reads.
// A reset can come while connecting or while reading.
func firstLine(t *testing.T, addr string) (string, error) {
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return bufio.NewReader(c).ReadString('\n')
}

// TestShedLatencyUnderOverload: 200 clients loop on a gateway that takes
// 20 requests at once to an upstream answering in 50 ms, so most requests
// are shed. Shed answers stay fast: the test logs their latency
// percentiles and fails only on one above 1 s, which no host stall reaches
// by itself (ADR-0027).
func TestShedLatencyUnderOverload(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	u := serveUpstream(t, "slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	})
	g := newGateway(t, newResolver(u), upstreamOf("", u))
	g.capped(t, 20)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200}}
	t.Cleanup(client.CloseIdleConnections)
	var mu sync.Mutex
	var shed []time.Duration
	ok := 0
	var wg sync.WaitGroup
	end := time.Now().Add(2 * time.Second)
	for range 200 {
		wg.Go(func() {
			for time.Now().Before(end) {
				start := time.Now()
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/", nil)
				resp, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				d := time.Since(start)
				mu.Lock()
				if resp.StatusCode == http.StatusServiceUnavailable {
					shed = append(shed, d)
				} else {
					ok++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(shed) == 0 || ok == 0 {
		t.Fatalf("%d shed and %d served, want both", len(shed), ok)
	}
	slices.Sort(shed)
	q := func(p float64) time.Duration { return shed[int(p*float64(len(shed)-1))] }
	t.Logf("%d served, %d shed; shed latency p50 %v, p99 %v, p99.9 %v, max %v", ok, len(shed), q(0.5), q(0.99), q(0.999), shed[len(shed)-1])
	if m := shed[len(shed)-1]; m > time.Second {
		t.Errorf("slowest shed answer took %v, want under 1 s", m)
	}
	if g.h.InFlight() != 0 {
		waitFor(t, "in-flight count back to 0", func() bool { return g.h.InFlight() == 0 })
	}
}
