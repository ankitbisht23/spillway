package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

var loopback = Allowlist{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// fakeResolver answers from a map that tests change.
type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string][]netip.Addr
	err   error
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ips, ok := f.hosts[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ips, nil
}

func (f *fakeResolver) set(host string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[host] = nil
	for _, ip := range ips {
		f.hosts[host] = append(f.hosts[host], netip.MustParseAddr(ip))
	}
}

// upstream is a test server that echoes what it received and counts the
// requests it served and the connections it accepted and closed. Health
// check probes are counted apart and answered probeStatus, if set; other
// requests are answered status, if set (GW-08, GW-09).
type upstream struct {
	*httptest.Server
	name        string
	requests    atomic.Int64
	conns       atomic.Int64
	closed      atomic.Int64
	probes      atomic.Int64
	probeStatus atomic.Int32
	status      atomic.Int32
}

type echo struct {
	Upstream string
	Path     string
	RawQuery string
	Host     string
	Header   http.Header
	Body     string
	Length   int64
}

func newUpstream(t *testing.T, name string) *upstream {
	t.Helper()
	u := &upstream{name: name}
	u.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "spillway-health-check" {
			u.probes.Add(1)
			if code := u.probeStatus.Load(); code != 0 {
				w.WriteHeader(int(code))
			}
			return
		}
		u.requests.Add(1)
		if code := u.status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Connection", "X-Upstream-Secret")
		w.Header().Set("X-Upstream-Secret", "1")
		w.Header().Set("Keep-Alive", "timeout=5")
		_ = json.NewEncoder(w).Encode(echo{name, r.URL.Path, r.URL.RawQuery, r.Host, r.Header, string(body), r.ContentLength})
	}))
	u.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			u.conns.Add(1)
		case http.StateClosed:
			u.closed.Add(1)
		}
	}
	u.Start()
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) port() string {
	_, port, _ := net.SplitHostPort(u.Listener.Addr().String())
	return port
}

func snapshot(t *testing.T, yaml string) *config.Snapshot {
	t.Helper()
	c, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// t0 is where every test gateway's limiter clock starts: ADR-0018's test
// vectors' instant.
var t0 = time.Date(2026, 11, 10, 12, 0, 0, 250_000_000, time.UTC)

// gateway serves h through a real HTTP server; current is the snapshot and
// identity the identity, nil until a test sets one. Its rate limiters read
// clock, which moves only when a test moves it. It access-logs every request
// to logs and keeps its metrics in metrics.
type gateway struct {
	*httptest.Server
	h        *Handler
	reg      *Registry
	events   *logBuffer // the registry's log
	limiters *local.Registry
	clock    *ratelimit.FakeClock
	current  atomic.Pointer[config.Snapshot]
	identity atomic.Pointer[config.Identity]
	metrics  *prometheus.Registry
	logs     *logBuffer
}

func newGateway(t *testing.T, res Resolver, yaml string) *gateway {
	t.Helper()
	return newGatewayBehind(t, res, yaml, nil)
}

// newGatewayBehind is newGateway with trusted proxies (SEC-05).
func newGatewayBehind(t *testing.T, res Resolver, yaml string, trusted TrustedProxies) *gateway {
	t.Helper()
	return newGatewayWith(t, res, yaml, trusted, nil)
}

// newGatewayWith is newGateway with trusted proxies and a Redis backend.
func newGatewayWith(t *testing.T, res Resolver, yaml string, trusted TrustedProxies, redis Redis) *gateway {
	t.Helper()
	return newDevGateway(t, res, yaml, trusted, redis, false)
}

// newDevGateway is newGatewayWith with dev mode on or off (ADR-0021).
func newDevGateway(t *testing.T, res Resolver, yaml string, trusted TrustedProxies, redis Redis, devMode bool) *gateway {
	t.Helper()
	return newTracedGateway(t, res, yaml, trusted, redis, devMode, nil)
}

// newTracedGateway is newDevGateway that traces requests with tracer, if
// not nil (OB-02).
func newTracedGateway(t *testing.T, res Resolver, yaml string, trusted TrustedProxies, redis Redis, devMode bool, tracer trace.Tracer) *gateway {
	t.Helper()
	g := &gateway{events: &logBuffer{}, clock: ratelimit.NewFakeClock(t0), metrics: prometheus.NewRegistry(), logs: &logBuffer{}}
	g.reg = NewRegistry(loopback, res, slog.New(slog.NewJSONHandler(g.events, nil)))
	t.Cleanup(g.reg.Close)
	g.limiters = local.NewRegistry(g.clock)
	g.apply(t, yaml)
	rec := telemetry.NewRecorder(g.metrics, slog.New(slog.NewJSONHandler(g.logs, nil)), 1)
	g.h = NewHandler(g.current.Load, g.identity.Load, trusted, g.reg, g.limiters, redis, devMode, 4096, tracer, rec, slog.New(slog.DiscardHandler))
	g.Server = httptest.NewServer(g.h)
	t.Cleanup(g.Close)
	return g
}

func (g *gateway) apply(t *testing.T, yaml string) {
	t.Helper()
	g.swap(t, snapshot(t, yaml))
}

// swap reconciles the registries with s and makes it current, as the config
// watcher does.
func (g *gateway) swap(t *testing.T, s *config.Snapshot) {
	if err := g.reg.Apply(s); err != nil {
		t.Error(err)
		return
	}
	g.limiters.Apply(s)
	g.current.Store(s)
}

// get returns the response's status and headers, and the upstream's echo for
// a 200.
func (g *gateway) get(t *testing.T, path string, header http.Header) (int, http.Header, echo) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+path, nil)
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
	var e echo
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, resp.Header, e
}

// dial opens a raw connection, closed when the test ends.
func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func oneUpstream(u *upstream) string {
	return fmt.Sprintf(`
upstreams: [{name: %[1]s, endpoints: [{address: '%[1]s:%[2]s'}]}]
routes: [{name: r, upstream: %[1]s, match: {path_prefix: /}}]
`, u.name, u.port())
}

func newResolver(u ...*upstream) *fakeResolver {
	f := &fakeResolver{hosts: map[string][]netip.Addr{}}
	for _, x := range u {
		f.set(x.name, "127.0.0.1")
	}
	return f
}

func TestForwarding(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))

	status, respHeader, e := g.get(t, "/v1//orders/./a/../42?x=1&y=2", http.Header{
		"X-Forwarded-For":    {"6.6.6.6"},
		"X-Forwarded-Host":   {"evil.example"},
		"X-Forwarded-Proto":  {"https"},
		"X-Forwarded-Port":   {"443"},
		"X-Forwarded-Prefix": {"/admin"},
		"Forwarded":          {"for=6.6.6.6"},
		"Connection":         {"X-Client-Secret"},
		"X-Client-Secret":    {"1"},
		"Te":                 {"gzip, trailers"},
		"X-Kept":             {"yes"},
	})
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	host := strings.TrimPrefix(g.URL, "http://")
	for _, c := range []struct{ what, got, want string }{
		{"path (cleaned)", e.Path, "/v1/orders/42"},
		{"query", e.RawQuery, "x=1&y=2"},
		{"Host (kept from the client)", e.Host, host},
		{"X-Forwarded-For (set by the gateway)", e.Header.Get("X-Forwarded-For"), "127.0.0.1"},
		{"X-Forwarded-Host", e.Header.Get("X-Forwarded-Host"), host},
		{"X-Forwarded-Proto", e.Header.Get("X-Forwarded-Proto"), "http"},
		{"X-Forwarded-Port (any other X-Forwarded-* is removed)", e.Header.Get("X-Forwarded-Port"), ""},
		{"X-Forwarded-Prefix", e.Header.Get("X-Forwarded-Prefix"), ""},
		{"Forwarded", e.Header.Get("Forwarded"), ""},
		{"header named in Connection", e.Header.Get("X-Client-Secret"), ""},
		{"Te (only trailers passes)", e.Header.Get("Te"), "trailers"},
		{"ordinary header", e.Header.Get("X-Kept"), "yes"},
		{"response header named in Connection", respHeader.Get("X-Upstream-Secret"), ""},
		{"response Keep-Alive", respHeader.Get("Keep-Alive"), ""},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.what, c.got, c.want)
		}
	}
}

func TestUserinfoNotForwarded(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	conn := dial(t, g.Listener.Addr().String())
	_, _ = fmt.Fprintf(conn, "GET http://user:secret@%s/a HTTP/1.1\r\nHost: %[1]s\r\nConnection: close\r\n\r\n", g.Listener.Addr())
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var e echo
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if got := e.Header.Get("Authorization"); got != "" {
		t.Errorf("upstream got Authorization %q from the URL's userinfo", got)
	}
}

// TestH2CUpgradeIgnored: an h2c upgrade reaches the upstream as a plain
// HTTP/1.1 request, so no HTTP/2 tunnel can carry requests past routing
// (ADR-0008: upstreams speak HTTP/1.1 only).
func TestH2CUpgradeIgnored(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	for _, upgrade := range []string{"h2c", "foo, H2C"} {
		status, _, e := g.get(t, "/", http.Header{
			"Connection":     {"Upgrade, HTTP2-Settings"},
			"Upgrade":        {upgrade},
			"Http2-Settings": {"AAMAAABkAARAAAAAAAIAAAAA"},
		})
		if status != http.StatusOK || e.Header.Get("Upgrade") != "" || e.Header.Get("Connection") != "" {
			t.Errorf("Upgrade %q: status %d, upstream got Upgrade %q and Connection %q; want 200 and neither",
				upgrade, status, e.Header.Get("Upgrade"), e.Header.Get("Connection"))
		}
	}
}

// TestHTTP2QueryWithSpace: a raw space in an HTTP/2 query, which would split
// the upstream's HTTP/1.1 request line, gets the 400 that HTTP/1.1 gets.
func TestHTTP2QueryWithSpace(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	tg := &gateway{Server: httptest.NewUnstartedServer(g.Config.Handler)}
	tg.EnableHTTP2 = true
	tg.StartTLS()
	t.Cleanup(tg.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tg.URL+"/a", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.RawQuery = "x=1 HTTP/1.0"
	resp, err := tg.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusBadRequest || u.conns.Load() != 0 {
		t.Errorf("%s answered %d and the upstream got %d connections; want HTTP/2, 400 and none", resp.Proto, resp.StatusCode, u.conns.Load())
	}
	if status, _, e := tg.get(t, "/a?x=%20&y=2", nil); status != http.StatusOK || e.RawQuery != "x=%20&y=2" {
		t.Errorf("encoded space: status %d, upstream query %q; want 200 and the query unchanged", status, e.RawQuery)
	}
}

func TestGatewayErrors(t *testing.T) {
	u := newUpstream(t, "orders")
	res := newResolver(u)
	res.set("metadata", "169.254.169.254", "10.1.2.3")
	down := newUpstream(t, "down")
	downPort := down.port()
	down.Close()
	res.set("down", "127.0.0.1")
	g := newGateway(t, res, fmt.Sprintf(`
upstreams:
  - {name: orders, endpoints: [{address: 'orders:%s'}]}
  - {name: metadata, endpoints: [{address: 'metadata:80'}]}
  - {name: down, endpoints: [{address: 'down:%s'}]}
routes:
  - {name: orders, upstream: orders, match: {path_prefix: /orders, methods: [GET]}}
  - {name: metadata, upstream: metadata, match: {path_prefix: /metadata}}
  - {name: down, upstream: down, match: {path_prefix: /down}}
`, u.port(), downPort))

	for _, tt := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/nowhere", http.StatusNotFound},
		{"POST", "/orders", http.StatusNotFound},
		{"GET", "/metadata", http.StatusServiceUnavailable}, // every address resolved outside the allow-list
		{"GET", "/down", http.StatusBadGateway},
	} {
		req, _ := http.NewRequestWithContext(t.Context(), tt.method, g.URL+tt.path, nil)
		resp, err := g.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
		}
	}

	conn := dial(t, g.Listener.Addr().String())
	_, _ = fmt.Fprintf(conn, "CONNECT orders:%s HTTP/1.1\r\nHost: orders\r\n\r\n", u.port())
	if line, _ := bufio.NewReader(conn).ReadString('\n'); !strings.Contains(line, "405") {
		t.Errorf("CONNECT answered %q, want 405", line)
	}
}

// TestAmbiguousPaths: a path with a backslash or a "." or ".." segment
// followed by ";", literal or percent-encoded in either case, is answered 400
// before routing; nothing is decoded twice (ADR-0021, U3).
func TestAmbiguousPaths(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	for _, tt := range []struct {
		target string
		want   int
	}{
		{"/public/..;/admin", 400},
		{"/public/.;/admin", 400},
		{"/public/..;jsessionid=x/admin", 400},
		{"/..;", 400},
		{"/public/%2e%2e;/admin", 400},
		{"/public/%2E%2E%3B/admin", 400},
		{"/public/.%3b/admin", 400},
		{`/public\admin`, 400},
		{"/public%5cadmin", 400},
		{"/public%5Cadmin", 400},
		{"/public/%252e%252e;/admin", 200}, // the literal %2e%2e;
		{"/public/...;/admin", 200},
		{"/public/a..;/admin", 200},
		{"/public/x;/admin", 200},
		{"/public/../admin", 200}, // cleaned (ADR-0007)
	} {
		if got := rawStatus(t, g, "GET "+tt.target+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); got != tt.want {
			t.Errorf("GET %s = %d, want %d", tt.target, got, tt.want)
		}
	}
	if n := u.requests.Load(); n != 5 {
		t.Errorf("upstream got %d requests, want the 5 allowed ones", n)
	}
}

// TestMethodCase: a method that is not all upper case is answered 400 before
// routing, so "post" cannot skip a POST route and its limits for a catch-all
// route (ADR-0029).
func TestMethodCase(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), fmt.Sprintf(`
upstreams: [{name: orders, endpoints: [{address: 'orders:%s'}]}]
routes:
  - {name: login, upstream: orders, match: {path: /login, methods: [POST]}}
  - {name: default, upstream: orders, match: {path_prefix: /}}
policies: [{name: login-once, level: route, match: {routes: [login]}, key_template: '{route}', algorithm: gcra, limits: [{rate: 1, period: 1m, burst: 1}], local: true}]
`, u.port()))
	for i, want := range []struct {
		method string
		status int
	}{{"POST", 200}, {"POST", 429}, {"post", 400}, {"Post", 400}, {"pOST", 400}, {"GET", 200}} {
		if got := g.send(t, want.method, "/login", nil); got.status != want.status {
			t.Errorf("request %d (%s /login): %d, want %d", i+1, want.method, got.status, want.status)
		}
	}
}

func TestProxyErrorStatus(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, false, 4096, nil, nil, slog.New(slog.DiscardHandler))
	for _, tt := range []struct {
		err    error
		want   int
		reason string
	}{
		{&net.OpError{Op: "dial", Err: fmt.Errorf("dial x: %w", ErrNotAllowed)}, http.StatusBadGateway, telemetry.ReasonUpstream},
		{&net.OpError{Op: "dial", Err: &net.DNSError{IsTimeout: true}}, http.StatusGatewayTimeout, telemetry.ReasonUpstream},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, telemetry.ReasonUpstream},
		{errors.New("connection reset"), http.StatusBadGateway, telemetry.ReasonUpstream},
		{fmt.Errorf("write error: %w", &http.MaxBytesError{Limit: 10}), http.StatusRequestEntityTooLarge, telemetry.ReasonNone},
	} {
		w := httptest.NewRecorder()
		q := telemetry.Request{Origin: telemetry.OriginUpstream}
		h.proxyError(w, httptest.NewRequestWithContext(t.Context(), "GET", "/", nil), tt.err, &q)
		if w.Code != tt.want || q.Origin != telemetry.OriginGateway || q.Reason != tt.reason {
			t.Errorf("proxyError(%v) = %d, origin %s, reason %s; want %d, gateway, %s", tt.err, w.Code, q.Origin, q.Reason, tt.want, tt.reason)
		}
	}

	// The idle timeout cancels the request context: 504. A client that went
	// away also cancels it, and gets nothing.
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errStreamIdle)
	w := httptest.NewRecorder()
	h.proxyError(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), context.Canceled, &telemetry.Request{})
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("idle timeout: %d, want 504", w.Code)
	}
	ctx, stop := context.WithCancel(t.Context())
	stop()
	w = httptest.NewRecorder()
	h.proxyError(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), context.Canceled, &telemetry.Request{})
	if w.Body.Len() != 0 {
		t.Errorf("client gone: answered %d %q", w.Code, w.Body)
	}
}

func TestDialChecksAllowlist(t *testing.T) {
	r := NewRegistry(loopback, newResolver(), slog.New(slog.DiscardHandler))
	for _, addr := range []string{"169.254.169.254:80", "10.0.0.1:80", "[fe80::1%lo0]:80", "[::ffff:169.254.169.254]:80"} {
		conn, err := r.dialer.DialContext(context.Background(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("dial %s: err = %v, want ErrNotAllowed", addr, err)
		}
	}
}

func TestAllowlist(t *testing.T) {
	a, err := ParseAllowlist(" 172.29.0.0/16, 10.0.0.0/8 ,::/0")
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"172.29.0.5":             true,
		"172.30.0.5":             false,
		"10.9.9.9":               true,
		"::ffff:10.9.9.9":        true,
		"2001:db8::1":            true,
		"169.254.169.254":        false, // always denied, even inside an allowed network
		"fe80::1":                false,
		"fd00:ec2::254":          false,
		"::ffff:169.254.169.254": false,
	} {
		if got := a.Allows(netip.MustParseAddr(ip)); got != want {
			t.Errorf("Allows(%s) = %v, want %v", ip, got, want)
		}
	}
	for _, bad := range []string{"", " , ", "10.0.0.1", "10.0.0.0/33", "example.com/8"} {
		if _, err := ParseAllowlist(bad); err == nil {
			t.Errorf("ParseAllowlist(%q) succeeded", bad)
		}
	}
}

func TestApplyRejectsDisallowedEndpointIP(t *testing.T) {
	r := NewRegistry(loopback, newResolver(), slog.New(slog.DiscardHandler))
	s := snapshot(t, `
upstreams: [{name: u, endpoints: [{address: '127.0.0.1:80'}, {address: '10.0.0.7:80'}]}]
routes: [{name: r, upstream: u, match: {path: /}}]
`)
	if err := r.Apply(s); !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), `endpoint "10.0.0.7:80"`) {
		t.Errorf("Apply error = %v, want ErrNotAllowed naming the endpoint", err)
	}
	if len(*r.pools.Load()) != 0 {
		t.Error("a rejected snapshot changed the registry")
	}
}

// TestReloadKeepsPools: a reload keeps the pool, and so the open connections,
// of an upstream it does not change, and closes the idle connections of one
// it removes (GW-29).
func TestReloadKeepsPools(t *testing.T) {
	a, b, c := newUpstream(t, "a"), newUpstream(t, "b"), newUpstream(t, "c")
	twoUpstreams := func(x, y *upstream) string {
		return fmt.Sprintf(`
upstreams:
  - {name: %[1]s, endpoints: [{address: '%[1]s:%[2]s'}]}
  - {name: %[3]s, endpoints: [{address: '%[3]s:%[4]s'}]}
routes:
  - {name: %[1]s, upstream: %[1]s, match: {path_prefix: /%[1]s}}
  - {name: %[3]s, upstream: %[3]s, match: {path_prefix: /%[3]s}}
`, x.name, x.port(), y.name, y.port())
	}
	g := newGateway(t, newResolver(a, b, c), twoUpstreams(a, b))
	poolA := (*g.reg.pools.Load())["a"]

	for range 3 {
		g.get(t, "/a", nil)
	}
	g.get(t, "/b", nil)
	g.apply(t, twoUpstreams(a, c))
	for range 3 {
		if status, _, e := g.get(t, "/a", nil); status != http.StatusOK || e.Upstream != "a" {
			t.Fatalf("after reload: %d from %q", status, e.Upstream)
		}
	}
	if status, _, e := g.get(t, "/c", nil); status != http.StatusOK || e.Upstream != "c" {
		t.Errorf("new upstream c: %d from %q", status, e.Upstream)
	}

	if (*g.reg.pools.Load())["a"] != poolA {
		t.Error("the reload replaced upstream a's pool")
	}
	if n := a.conns.Load(); n != 1 {
		t.Errorf("upstream a accepted %d connections over 6 sequential requests and a reload, want 1", n)
	}
	if _, ok := (*g.reg.pools.Load())["b"]; ok {
		t.Error("removed upstream b is still in the registry")
	}
	deadline := time.Now().Add(5 * time.Second)
	for b.closed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("upstream b's idle connection was not closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A request still on the old snapshot can reach an upstream that a reload
// removed.
func TestRemovedUpstreamStillServesOldSnapshot(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	g := newGateway(t, newResolver(a, b), oneUpstream(a))
	old := g.current.Load()
	if err := g.reg.Apply(snapshot(t, oneUpstream(b))); err != nil {
		t.Fatal(err)
	}
	route, _, _ := old.Match("", "GET", "/", nil)
	ep, err := g.reg.pool(route.Upstream).pick(time.Now(), "", nil)
	if err != nil || !strings.HasSuffix(ep.addr, ":"+a.port()) {
		t.Errorf("pick = %v, %v; want upstream a's address", ep, err)
	}
}

// heldResolver resolves every name to 127.0.0.1, after closing looking and
// waiting for release.
type heldResolver struct{ looking, release chan struct{} }

func (h heldResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	close(h.looking)
	<-h.release
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

// TestRebuiltPoolDoesNotHoldUpReload: while a request on an old snapshot
// resolves the names of an upstream that a reload removed, another reload
// goes ahead (GW-29).
func TestRebuiltPoolDoesNotHoldUpReload(t *testing.T) {
	res := heldResolver{make(chan struct{}), make(chan struct{})}
	reg := NewRegistry(loopback, res, slog.New(slog.DiscardHandler))
	old := snapshot(t, "upstreams: [{name: gone, endpoints: [{address: 'gone:80'}]}]\nroutes: [{name: r, upstream: gone, match: {path_prefix: /}}]")
	next := snapshot(t, "upstreams: [{name: u, endpoints: [{address: '127.0.0.1:80'}]}]\nroutes: [{name: r, upstream: u, match: {path_prefix: /}}]")

	picked := make(chan error, 1)
	go func() {
		_, err := reg.pool(old.Upstreams["gone"]).pick(time.Now(), "", nil)
		picked <- err
	}()
	<-res.looking
	applied := make(chan error, 1)
	go func() { applied <- reg.Apply(next) }()
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("the reload waited for the request's DNS lookup")
	}
	close(res.release)
	if err := <-picked; err != nil {
		t.Errorf("rebuilt pool: %v", err)
	}
}

func TestRoundRobin(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	g := newGateway(t, newResolver(a, b), fmt.Sprintf(`
upstreams: [{name: u, lb: round_robin, endpoints: [{address: 'a:%s'}, {address: '127.0.0.1:%s'}]}]
routes: [{name: r, upstream: u, match: {path_prefix: /}}]
`, a.port(), b.port()))
	var got []string
	for range 4 {
		_, _, e := g.get(t, "/", nil)
		got = append(got, e.Upstream)
	}
	if strings.Join(got, ",") != "a,b,a,b" {
		t.Errorf("upstreams = %v, want alternating a and b", got)
	}
}

// TestDNSReresolution: a refresh picks up new addresses, skips disallowed
// ones, and keeps the last addresses when a lookup fails (GW-06, SEC-06).
func TestDNSReresolution(t *testing.T) {
	a := newUpstream(t, "orders")
	b := newUpstream(t, "b")
	res := newResolver(a)
	// One name and port: it resolves to 127.0.0.1, where a listens, then
	// moves to ::1, where b listens on the same port.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort("::1", a.port()))
	if err != nil {
		t.Skipf("cannot listen on [::1]:%s: %v", a.port(), err)
	}
	b2 := &http.Server{Handler: b.Config.Handler, ReadHeaderTimeout: time.Second}
	go func() { _ = b2.Serve(ln) }()
	t.Cleanup(func() { _ = b2.Close() })

	g := newGateway(t, res, oneUpstream(a))
	if _, _, e := g.get(t, "/", nil); e.Upstream != "orders" {
		t.Fatalf("before: served by %q", e.Upstream)
	}

	res.set("orders", "::1", "169.254.169.254")
	g.reg.refresh()
	if _, _, e := g.get(t, "/", nil); e.Upstream != "b" {
		t.Errorf("after re-resolution: served by %q, want b", e.Upstream)
	}
	if eps := *(*g.reg.pools.Load())["orders"].endpoints.Load(); len(eps) != 1 {
		t.Errorf("%d endpoints, want only the allowed address", len(eps))
	}

	res.mu.Lock()
	res.err = errors.New("DNS down")
	res.mu.Unlock()
	g.reg.refresh()
	if _, _, e := g.get(t, "/", nil); e.Upstream != "b" {
		t.Errorf("after a failed lookup: served by %q, want b (last addresses kept)", e.Upstream)
	}
}

// TestRequestFraming pins how conflicting framing reaches the upstream
// (SEC-07, ADR-0009): Transfer-Encoding wins over Content-Length and the body
// is re-framed, so the upstream never sees both; other ambiguities are refused.
func TestRequestFraming(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	send := func(raw string) (int, echo) {
		conn := dial(t, g.Listener.Addr().String())
		_, _ = io.WriteString(conn, raw)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var e echo
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(resp.Body).Decode(&e)
		}
		return resp.StatusCode, e
	}

	status, e := send("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n")
	if status != http.StatusOK || e.Body != "hello" {
		t.Errorf("CL + TE: status %d, upstream body %q; want 200 and the chunked body", status, e.Body)
	}
	if cl, te := e.Header.Get("Content-Length"), e.Header.Get("Transfer-Encoding"); cl != "" && cl != "5" || te != "" && cl != "" {
		t.Errorf("CL + TE: upstream saw Content-Length %q and Transfer-Encoding %q", cl, te)
	}

	for raw, want := range map[string]int{
		"POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\nContent-Length: 5\r\n\r\nhello":                       http.StatusBadRequest,
		"POST / HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: gzip, chunked\r\n\r\n0\r\n\r\n":                         http.StatusNotImplemented,
		"POST / HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n": http.StatusNotImplemented,
	} {
		if status, _ := send(raw); status != want {
			t.Errorf("%q = %d, want %d", strings.SplitN(raw, "\r\n\r\n", 2)[0], status, want)
		}
	}
}
