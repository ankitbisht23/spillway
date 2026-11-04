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

	"github.com/ankitbisht23/spillway/internal/config"
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
// requests it served and the connections it accepted and closed.
type upstream struct {
	*httptest.Server
	name     string
	requests atomic.Int64
	conns    atomic.Int64
	closed   atomic.Int64
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
		u.requests.Add(1)
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

// gateway serves h through a real HTTP server; current is the snapshot. It
// access-logs every request to logs and keeps its metrics in metrics.
type gateway struct {
	*httptest.Server
	reg     *Registry
	current atomic.Pointer[config.Snapshot]
	metrics *prometheus.Registry
	logs    *logBuffer
}

func newGateway(t *testing.T, res Resolver, yaml string) *gateway {
	t.Helper()
	g := &gateway{reg: NewRegistry(loopback, res, slog.New(slog.DiscardHandler)), metrics: prometheus.NewRegistry(), logs: &logBuffer{}}
	g.apply(t, yaml)
	rec := telemetry.NewRecorder(g.metrics, slog.New(slog.NewJSONHandler(g.logs, nil)), 1)
	g.Server = httptest.NewServer(NewHandler(g.current.Load, g.reg, rec, slog.New(slog.DiscardHandler)))
	t.Cleanup(g.Close)
	return g
}

func (g *gateway) apply(t *testing.T, yaml string) {
	t.Helper()
	s := snapshot(t, yaml)
	if err := g.reg.Apply(s); err != nil {
		t.Fatal(err)
	}
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

func TestProxyErrorStatus(t *testing.T) {
	h := NewHandler(nil, nil, nil, slog.New(slog.DiscardHandler))
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
	addr, err := g.reg.pool(route.Upstream).pick()
	if err != nil || !strings.HasSuffix(addr, ":"+a.port()) {
		t.Errorf("pick = %q, %v; want upstream a's address", addr, err)
	}
}

func TestRoundRobin(t *testing.T) {
	a, b := newUpstream(t, "a"), newUpstream(t, "b")
	g := newGateway(t, newResolver(a, b), fmt.Sprintf(`
upstreams: [{name: u, endpoints: [{address: 'a:%s'}, {address: '127.0.0.1:%s'}]}]
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
	if addrs := *(*g.reg.pools.Load())["orders"].addrs.Load(); len(addrs) != 1 {
		t.Errorf("addresses = %v, want only the allowed one", addrs)
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
