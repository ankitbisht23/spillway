package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/proxy"
)

// shutdownUpstream counts health check probes, and holds requests to /slow
// until free is called.
type shutdownUpstream struct {
	*httptest.Server
	probes  atomic.Int64
	arrived chan struct{}
	release chan struct{}
	free    func()
}

func newShutdownUpstream(t *testing.T) *shutdownUpstream {
	u := &shutdownUpstream{arrived: make(chan struct{}, 4), release: make(chan struct{})}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			u.probes.Add(1)
			return
		case "/slow":
			u.arrived <- struct{}{}
			select {
			case <-u.release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "upstream")
	}))
	u.free = sync.OnceFunc(func() { close(u.release) })
	t.Cleanup(u.Close)
	t.Cleanup(u.free)
	return u
}

// lockedBuffer is a log destination safe for the gateway and the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// shutdownGateway runs serve in front of u with health checks every 50 ms
// and the given drain and grace. The returned function cancels serve's
// context, as SIGTERM does.
func shutdownGateway(t *testing.T, u *shutdownUpstream, drain, grace time.Duration, flush func(context.Context)) (s settings, logs *lockedBuffer, signal func(), done <-chan error) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: 1\nupstreams: [{ name: up, endpoints: [{ address: '" + u.Listener.Addr().String() + "' }], health_check: { path: /health, interval: 100ms, timeout: 100ms } }]\n" +
		"routes: [{ name: all, match: { path_prefix: / }, upstream: up }]\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s = settings{
		configPath:    cfg,
		httpAddr:      freeAddr(t),
		adminAddr:     freeAddr(t),
		allow:         proxy.Allowlist{netip.MustParsePrefix("127.0.0.0/8")},
		dnsRefresh:    time.Minute,
		drain:         drain,
		shutdownGrace: grace,

		maxConnections: defaultMaxConnections,
		maxInFlight:    defaultMaxInFlight,
	}
	logs = &lockedBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- serve(ctx, s, slog.New(slog.NewJSONHandler(logs, nil)), flush) }()
	waitFor(t, "/readyz 200", func() bool { code, _ := get(t, "http://"+s.adminAddr+"/readyz"); return code == http.StatusOK })
	waitFor(t, "a health check probe", func() bool { return u.probes.Load() > 0 })
	return s, logs, cancel, errc
}

// slowRequest sends GET /slow on its own connection, waits until the
// upstream has it, and returns a channel that gets its status, 0 if it
// failed.
func slowRequest(t *testing.T, s settings, u *shutdownUpstream) <-chan int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	code := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+s.httpAddr+"/slow", nil)
		resp, err := client.Do(req)
		if err != nil {
			code <- 0
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.ReadAll(resp.Body); err != nil {
			code <- 0
			return
		}
		code <- resp.StatusCode
	}()
	select {
	case <-u.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request never reached the upstream")
	}
	return code
}

// refused reports whether a new connection to addr is refused.
func refused(t *testing.T, addr string) bool {
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// TestGracefulShutdown (GW-23, ADR-0030): after the signal, /readyz fails at
// once; during the drain (2 s, so that a 370 ms host stall cannot end it
// before the test's checks, ADR-0027) a new connection is served, with Connection:
// close, and health checks go on; after the drain new connections are
// refused while a request in flight finishes within the grace; then the
// probers stop, the telemetry is flushed, and serve returns.
func TestGracefulShutdown(t *testing.T) {
	u := newShutdownUpstream(t)
	var flushedAt atomic.Int64
	var flushProbes atomic.Int64
	s, logs, signal, done := shutdownGateway(t, u, 2*time.Second, 5*time.Second, func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("flush without a deadline")
		}
		flushedAt.Store(time.Now().UnixNano())
		flushProbes.Store(u.probes.Load())
	})
	traffic, admin := "http://"+s.httpAddr, "http://"+s.adminAddr
	slow := slowRequest(t, s, u)

	start := time.Now()
	signal()
	waitFor(t, "/readyz 503", func() bool { code, _ := get(t, admin+"/readyz"); return code == http.StatusServiceUnavailable })
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("/readyz failed %v after the signal, want at once", d)
	}

	// The drain: a new connection is still served, and told to close.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, traffic+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatalf("request during the drain: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "upstream" || !resp.Close {
		t.Errorf("request during the drain: %d %q, Connection: close %v; want 200 from the upstream with Connection: close", resp.StatusCode, body, resp.Close)
	}
	if refused(t, s.httpAddr) {
		t.Fatalf("connection refused %v after the signal, within the 2 s drain", time.Since(start))
	}

	// After the drain: new connections are refused; probes go on while the
	// request in flight runs.
	waitFor(t, "connections refused", func() bool { return refused(t, s.httpAddr) })
	if d := time.Since(start); d < 2*time.Second {
		t.Errorf("connections refused %v after the signal, within the 2 s drain", d)
	}
	probes := u.probes.Load()
	waitFor(t, "a probe during the grace", func() bool { return u.probes.Load() > probes })
	select {
	case err := <-done:
		t.Fatalf("serve returned during the grace: %v", err)
	default:
	}
	u.free()
	if code := <-slow; code != http.StatusOK {
		t.Errorf("request in flight across the drain = %d, want 200", code)
	}
	finished := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the last request")
	}
	if at := flushedAt.Load(); at == 0 || time.Unix(0, at).Before(finished.Add(-100*time.Millisecond)) {
		t.Error("telemetry not flushed after the servers shut down")
	}
	if code, _ := get(t, admin+"/readyz"); code != 0 {
		t.Errorf("admin port answered %d after serve returned", code)
	}
	// Probes stopped before the flush. A probe sent just before they
	// stopped may reach the upstream after it; a prober still running
	// would probe 3 times in 300 ms.
	time.Sleep(300 * time.Millisecond)
	if n := u.probes.Load() - flushProbes.Load(); n > 1 {
		t.Errorf("%d probes after the flush", n)
	}
	for _, msg := range []string{`"msg":"shutting down: draining","drain":"2s"`, `"msg":"shutting down: closing listeners","grace":"5s","requests":1`} {
		if !strings.Contains(logs.String(), msg) {
			t.Errorf("no log line %s in\n%s", msg, logs.String())
		}
	}
}

// TestShutdownGraceCuts: a request that outlasts the grace is cut, the log
// says how many were, and serve returns.
func TestShutdownGraceCuts(t *testing.T) {
	u := newShutdownUpstream(t)
	var flushed atomic.Bool
	s, logs, signal, done := shutdownGateway(t, u, 0, 300*time.Millisecond, func(context.Context) { flushed.Store(true) })
	slow := slowRequest(t, s, u)
	start := time.Now()
	signal()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the grace")
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("serve returned %v after the signal, before the 300 ms grace", d)
	}
	if code := <-slow; code == http.StatusOK {
		t.Error("a request outlasting the grace finished with 200, want it cut")
	}
	if !flushed.Load() {
		t.Error("telemetry not flushed")
	}
	if !strings.Contains(logs.String(), `"msg":"shutdown grace ended; cutting requests","requests":1`) {
		t.Errorf("no log line for the cut request in\n%s", logs.String())
	}
}
