package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
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
	g.h = NewHandler(g.current.Load, g.identity.Load, nil, g.reg, g.limiters, nil, false, maxInFlight, nil, rec, slog.New(slog.DiscardHandler))
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
// connections and no cap per address that matters, and returns the cap,
// the address, the metrics, and the log. A request for /hold gets 200 and
// its headers at once, and its body only when its client goes away or the
// test ends.
func connCapServer(t *testing.T, max int64, tlsConfig *tls.Config, h http.Handler) (*ConnLimit, string, *prometheus.Registry, *logBuffer) {
	t.Helper()
	return capServer(t, "127.0.0.1:0", max, math.MaxInt64, nil, tlsConfig, h)
}

// capServer is connCapServer listening on listen, with a cap of maxPerIP
// connections per client address outside trusted.
func capServer(t *testing.T, listen string, max, maxPerIP int64, trusted TrustedProxies, tlsConfig *tls.Config, h http.Handler) (*ConnLimit, string, *prometheus.Registry, *logBuffer) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	inner := h
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hold" {
			inner.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	metrics, logs := prometheus.NewRegistry(), &logBuffer{}
	rec := telemetry.NewRecorder(metrics, slog.New(slog.DiscardHandler), 1)
	l := NewConnLimit(max, maxPerIP, trusted, rec, slog.New(slog.NewJSONHandler(logs, nil)))
	srv := NewServer(h, slog.New(slog.DiscardHandler))
	srv.ConnState = l.ConnState
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", listen)
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

// holdOpen sends a request for /hold on a new connection, which then has a
// request in progress until it is closed; it returns the connection.
func holdOpen(t *testing.T, addr string) net.Conn {
	t.Helper()
	c := dial(t, addr)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "GET /hold HTTP/1.1\r\nHost: x\r\n\r\n")
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("held connection: %q, %v", line, err)
	}
	_ = c.SetReadDeadline(time.Time{})
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

// TestConnectionCap: past the cap, with every connection's request in
// progress, a connection gets the fixed 503 as soon as its request starts,
// without the gateway reading the rest, and is closed; one that sends
// nothing gets nothing within the shed deadline. Each is counted as a shed
// request, and shedding is logged once a second. Closing a counted
// connection, hijacked ones included, frees its place (GW-26, ADR-0030).
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
	open := holdOpen(t, addr)
	if n := l.Open(); n != 1 {
		t.Fatalf("%d open, want 1", n)
	}

	// A raw connection gets the answer after its request's first bytes.
	start := time.Now()
	raw := dial(t, addr)
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(raw, "GET / HT")
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
	if v := metricOf(t, metrics, cappedSeries(telemetry.CapGlobal, telemetry.ActionShed)); v != "2" {
		t.Errorf("global shed connections: %q, want 2", v)
	}
	if n := strings.Count(logs.String(), `"msg":"shedding connections"`); n != 1 || !strings.Contains(logs.String(), `"connections":1,"max_connections":1`) {
		t.Errorf("want one log line for the first shed connection within a second, got\n%s", logs.String())
	}
	if n := l.Open(); n != 1 {
		t.Errorf("%d open after shedding, want 1", n)
	}

	// A connection that sends nothing is closed at the shed deadline,
	// unanswered.
	silent := dial(t, addr)
	_ = silent.SetDeadline(time.Now().Add(5 * time.Second))
	if got, err := io.ReadAll(silent); len(got) != 0 || err != nil {
		t.Errorf("silent connection got %q, %v; want nothing, then EOF", got, err)
	}
	if v := metricOf(t, metrics, shedSeries); v != "3" {
		t.Errorf("%s = %q, want 3", shedSeries, v)
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

// TestConnectionCapWaiting: at the cap, a new connection takes the place of
// the one that has waited longest without a request in progress, idle
// after a response or silent since it connected, which is closed; a
// connection with a request in progress is never closed for it (GW-26,
// GW-28, ADR-0033).
func TestConnectionCapWaiting(t *testing.T) {
	l, addr, metrics, logs := connCapServer(t, 3, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // a refusal keeps the connection open
	}))
	held := holdOpen(t, addr)
	idle := keepOpenStatus(t, addr, http.StatusTooManyRequests)
	silent := dial(t, addr)
	waitFor(t, "3 open", func() bool { return l.Open() == 3 })
	closed := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
	}

	// The idle connection has waited longest: it gives way.
	next := keepOpenStatus(t, addr, http.StatusTooManyRequests)
	if !closed(idle) || closed(silent) || closed(held) {
		t.Errorf("after a fourth connection: idle closed %v, silent %v, held %v; want only the idle one", closed(idle), closed(silent), closed(held))
	}
	// Then the silent one, which waited before next went idle.
	_ = keepOpenStatus(t, addr, http.StatusTooManyRequests)
	if !closed(silent) || closed(next) || closed(held) {
		t.Errorf("after a fifth connection: silent closed %v, next %v, held %v; want only the silent one", closed(silent), closed(next), closed(held))
	}
	if n := l.Open(); n != 3 {
		t.Errorf("%d open, want 3", n)
	}
	if v := metricOf(t, metrics, shedSeries); v != "" {
		t.Errorf("%s = %q, want none shed", shedSeries, v)
	}
	if v := metricOf(t, metrics, cappedSeries(telemetry.CapGlobal, telemetry.ActionClosedWaiting)); v != "2" {
		t.Errorf("waiting connections closed at the global cap: %q, want 2", v)
	}
	if !strings.Contains(logs.String(), `"msg":"closing waiting connections at the connection cap","connections":1,"max_connections":3`) {
		t.Errorf("no log line for the closed connection in\n%s", logs.String())
	}
}

// keepOpenStatus sends one request on a new connection, checks its status,
// and leaves the connection open and idle; it returns the connection.
func keepOpenStatus(t *testing.T, addr string, status int) net.Conn {
	t.Helper()
	c := dial(t, addr)
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("status %d, want %d", resp.StatusCode, status)
	}
	return c
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
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/hold", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client().Do(req) // its stream stays open
	if err != nil || resp.Proto != "HTTP/2.0" {
		t.Fatalf("first connection: %v, %v; want HTTP/2", resp, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
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
	holdOpen(t, addr)
	start := time.Now()
	for i := range maxShedding {
		c := dial(t, addr)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, "GET / HTTP/1.1\r\n")
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
	// A write after the reset arrived fails with EPIPE instead.
	if got, err := firstLine(t, addr); got != "" || !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE) {
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

// firstLine opens a connection to addr, starts a request, and returns the
// first line it reads. A reset can come while connecting, writing or
// reading.
func firstLine(t *testing.T, addr string) (string, error) {
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\n"); err != nil {
		return "", err
	}
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
	// At most 20 connections are dialled at once: a burst of 200 connects
	// can overflow macOS's listen backlog of 128, and the kernel's resets
	// would fail the test, not shedding. Every dial counts, not only each
	// client's first: a shed answer closes its connection, so the client
	// dials again.
	connecting := make(chan struct{}, 20)
	var dialer net.Dialer
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 200, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case connecting <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-connecting }()
		return dialer.DialContext(ctx, network, addr)
	}}}
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

// cappedSeries is gw_connections_capped_total's series for which and action.
func cappedSeries(which, action string) string {
	return `gw_connections_capped_total{action="` + action + `",cap="` + which + `"}`
}

// openFrom returns how many open connections l counts for the address ip.
func openFrom(l *ConnLimit, ip string) int64 {
	key, _ := addrKey(netip.MustParseAddr(ip), nil)
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.addrs[key]; a != nil {
		return a.open
	}
	return 0
}

// TestConnectionCapPerIP: at its address's cap, a new connection takes the
// place of that address's longest-waiting connection, which is closed; with
// every one of them busy, it gets the shed path's 503, while the replica's
// cap has room. Each is counted with cap per_ip and logged without the
// address; the address's count follows closes and never counts a shed
// connection (GW-26, ADR-0071).
func TestConnectionCapPerIP(t *testing.T) {
	l, addr, metrics, logs := capServer(t, "127.0.0.1:0", 100, 3, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // a refusal keeps the connection open
	}))
	held := holdOpen(t, addr)
	idle := keepOpenStatus(t, addr, http.StatusTooManyRequests)
	silent := dial(t, addr)
	waitFor(t, "3 open", func() bool { return l.Open() == 3 })
	closed := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
	}

	// The idle connection has waited longest: it gives way, then the
	// silent one.
	next := keepOpenStatus(t, addr, http.StatusTooManyRequests)
	if !closed(idle) || closed(silent) || closed(held) {
		t.Errorf("after a fourth connection: idle closed %v, silent %v, held %v; want only the idle one", closed(idle), closed(silent), closed(held))
	}
	fifth := keepOpenStatus(t, addr, http.StatusTooManyRequests)
	if !closed(silent) || closed(next) || closed(held) {
		t.Errorf("after a fifth connection: silent closed %v, next %v, held %v; want only the silent one", closed(silent), closed(next), closed(held))
	}
	if n, m := l.Open(), openFrom(l, "127.0.0.1"); n != 3 || m != 3 {
		t.Errorf("%d open, %d from 127.0.0.1; want 3 and 3", n, m)
	}
	if v := metricOf(t, metrics, cappedSeries(telemetry.CapPerIP, telemetry.ActionClosedWaiting)); v != "2" {
		t.Errorf("waiting connections closed at the per-address cap: %q, want 2", v)
	}
	if v := metricOf(t, metrics, cappedSeries(telemetry.CapGlobal, telemetry.ActionClosedWaiting)); v != "0" {
		t.Errorf("waiting connections closed at the global cap: %q, want 0", v)
	}

	// All three busy: the next connection is shed.
	_, _ = next.Close(), fifth.Close()
	waitFor(t, "1 open", func() bool { return l.Open() == 1 })
	holdOpen(t, addr)
	holdOpen(t, addr)
	raw := dial(t, addr)
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(raw, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if got, err := io.ReadAll(raw); err != nil || string(got) != shedResponse {
		t.Errorf("past the per-address cap: %q, %v; want %q", got, err, shedResponse)
	}
	if n, m := l.Open(), openFrom(l, "127.0.0.1"); n != 3 || m != 3 {
		t.Errorf("after a shed connection: %d open, %d from 127.0.0.1; want 3 and 3", n, m)
	}
	for _, s := range []string{shedSeries, cappedSeries(telemetry.CapPerIP, telemetry.ActionShed)} {
		if v := metricOf(t, metrics, s); v != "1" {
			t.Errorf("%s = %q, want 1", s, v)
		}
	}
	if v := metricOf(t, metrics, cappedSeries(telemetry.CapGlobal, telemetry.ActionShed)); v != "0" {
		t.Errorf("global shed connections: %q, want 0", v)
	}
	out := logs.String()
	for _, want := range []string{
		`"msg":"closing waiting connections at the per-address connection cap","connections":1,"max_connections_per_ip":3`,
		`"msg":"shedding connections at the per-address connection cap","connections":1,"max_connections_per_ip":3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "127.0.0.1") {
		t.Errorf("the log names the client address:\n%s", out)
	}

	// Closing them frees the address's places, and its entry goes.
	_ = held.Close()
	waitFor(t, "a place freed", func() bool { return openFrom(l, "127.0.0.1") == 2 })
	l.mu.Lock()
	n := len(l.addrs)
	l.mu.Unlock()
	if n != 1 {
		t.Errorf("%d addresses tracked, want 1", n)
	}
}

// TestConnectionCapPerIPGlobalShed: a connection shed at the replica's cap
// gives back the place its address had taken for it (ADR-0071).
func TestConnectionCapPerIPGlobalShed(t *testing.T) {
	l, addr, metrics, _ := capServer(t, "127.0.0.1:0", 2, 2, nil, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	a, b := holdOpen(t, addr), holdOpen(t, addr)
	// Both caps are reached; the per-address one is checked first.
	if got, err := firstLine(t, addr); err != nil || !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Fatalf("third connection: %q, %v; want the shed 503", got, err)
	}
	if v := metricOf(t, metrics, cappedSeries(telemetry.CapPerIP, telemetry.ActionShed)); v != "1" {
		t.Errorf("per-address shed connections: %q, want 1", v)
	}
	_, _ = a.Close(), b.Close()
	waitFor(t, "every place freed", func() bool { return l.Open() == 0 && openFrom(l, "127.0.0.1") == 0 })

	// Only the replica's cap reached.
	l, addr, metrics, _ = capServer(t, "127.0.0.1:0", 2, 3, nil, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	holdOpen(t, addr)
	holdOpen(t, addr)
	if got, err := firstLine(t, addr); err != nil || !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Fatalf("third connection: %q, %v; want the shed 503", got, err)
	}
	if v := metricOf(t, metrics, cappedSeries(telemetry.CapGlobal, telemetry.ActionShed)); v != "1" {
		t.Errorf("global shed connections: %q, want 1", v)
	}
	if n := openFrom(l, "127.0.0.1"); n != 2 {
		t.Errorf("%d counted for 127.0.0.1 after a global shed, want 2", n)
	}
}

// TestConnectionCapPerIPTrusted: a trusted proxy's connections have no cap
// of their own, only the replica's (SEC-05, ADR-0071).
func TestConnectionCapPerIPTrusted(t *testing.T) {
	trusted, err := ParseTrustedProxies("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	l, addr, metrics, _ := capServer(t, "127.0.0.1:0", 10, 1, trusted, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for range 3 {
		holdOpen(t, addr)
	}
	l.mu.Lock()
	n := len(l.addrs)
	l.mu.Unlock()
	if o := l.Open(); o != 3 || n != 0 {
		t.Errorf("%d open, %d addresses tracked; want 3 and 0", o, n)
	}
	for _, a := range []string{telemetry.ActionShed, telemetry.ActionClosedWaiting} {
		if v := metricOf(t, metrics, cappedSeries(telemetry.CapPerIP, a)); v != "0" {
			t.Errorf("per-address %s: %q, want 0", a, v)
		}
	}
}

// TestConnectionCapPerIPOtherAddress: one address at its cap with every
// connection busy leaves another address's connections alone (ADR-0071).
func TestConnectionCapPerIPOtherAddress(t *testing.T) {
	l, addr, _, _ := capServer(t, ":0", 10, 1, nil, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	_, port, _ := net.SplitHostPort(addr)
	v6 := net.JoinHostPort("::1", port)
	if c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", v6); err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	} else {
		_ = c.Close()
		waitFor(t, "the probe closed", func() bool { return l.Open() == 0 })
	}
	holdOpen(t, net.JoinHostPort("127.0.0.1", port))
	if got, err := firstLine(t, net.JoinHostPort("127.0.0.1", port)); err != nil || !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Errorf("second connection from 127.0.0.1: %q, %v; want the shed 503", got, err)
	}
	keepOpen(t, v6)
	if n, m := openFrom(l, "127.0.0.1"), openFrom(l, "::1"); n != 1 || m != 1 {
		t.Errorf("%d from 127.0.0.1 and %d from ::1, want 1 and 1", n, m)
	}
}

// TestAddrKey: a connection counts against its IPv4 address, IPv4-mapped
// ones included, or its IPv6 address's /64; a trusted proxy's has none
// (ADR-0071).
func TestAddrKey(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.0.0.0/8, 2001:db8:ffff::/48")
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]string{
		"192.0.2.1":           "192.0.2.1/32",
		"::ffff:192.0.2.1":    "192.0.2.1/32",
		"2001:db8:1:2:3::1":   "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":     "2001:db8:1:3::/64",
		"fe80::1%en0":         "fe80::/64",
		"::1":                 "::/64",
		"10.1.2.3":            "",
		"::ffff:10.1.2.3":     "",
		"2001:db8:ffff:1::1":  "",
	} {
		key, ok := addrKey(netip.MustParseAddr(ip), trusted)
		got := ""
		if ok {
			got = key.String()
		}
		if got != want {
			t.Errorf("%s: key %q, want %q", ip, got, want)
		}
	}
}

// pipeListener hands out one end of net.Pipe per Dial, with the given
// remote address, so tests can connect from any address.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
}

type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c *addrConn) RemoteAddr() net.Addr { return c.remote }

func (p *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-p.conns:
		return c, nil
	case <-p.done:
		return nil, net.ErrClosed
	}
}

func (p *pipeListener) Close() error   { return nil }
func (p *pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

// dialFrom returns the client end of a connection from ip.
func (p *pipeListener) dialFrom(t *testing.T, ip string) net.Conn {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _, _ = server.Close(), client.Close() })
	p.conns <- &addrConn{Conn: server, remote: net.TCPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(ip), 50000))}
	return client
}

// TestConnectionCapPerIPv6: addresses in one IPv6 /64 share its cap;
// another /64 has its own (ADR-0071).
func TestConnectionCapPerIPv6(t *testing.T) {
	rec := telemetry.NewRecorder(prometheus.NewRegistry(), slog.New(slog.DiscardHandler), 1)
	l := NewConnLimit(10, 1, nil, rec, slog.New(slog.DiscardHandler))
	p := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	t.Cleanup(func() { close(p.done) })
	ln := l.Listener(p, nil)
	accepted := make(chan net.Conn, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.ConnState(c, http.StateActive) // busy: never closed for another
			accepted <- c
		}
	}()
	p.dialFrom(t, "2001:db8:1:2::1")
	<-accepted
	// Same /64: shed. The shed path answers once the request starts.
	c := p.dialFrom(t, "2001:db8:1:2:ffff::2")
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go func() { _, _ = io.WriteString(c, "G") }()
	if got, err := bufio.NewReader(c).ReadString('\n'); err != nil || !strings.HasPrefix(got, "HTTP/1.1 503") {
		t.Errorf("second connection in the /64: %q, %v; want the shed 503", got, err)
	}
	// Another /64: accepted.
	p.dialFrom(t, "2001:db8:1:3::1")
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("a connection from another /64 was not accepted")
	}
	if n := l.Open(); n != 2 {
		t.Errorf("%d open, want 2", n)
	}
}
