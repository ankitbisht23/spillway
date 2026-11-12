//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// TestGatewayRedisFailure runs a gateway on the host whose Redis nodes are
// reached through proxies that the test can cut, so Redis becomes
// unreachable for this gateway alone (RL-22, RL-23, ADR-0021, ADR-0024,
// ADR-0025):
//
//   - once warmed up, its first request, with a fail_closed policy and the
//     default 5 ms budget, is decided by Redis, not by its failure mode;
//   - with every node cut off, a fail_open policy allows, a fail_closed one
//     answers 503, and a degraded one admits limit ÷ replicas, each decided
//     within one budget; the health checks open the breakers;
//   - once the nodes are back, the first answered health check PINGs close
//     the breakers within a second, and Redis decides again from the first
//     request to each node;
//   - in a second outage the degraded limiter carries on from the first.
func TestGatewayRedisFailure(t *testing.T) {
	run := scriptName("failure")
	nodes, err := redis.ParseAddrMap(os.Getenv(redis.AddrMapEnv))
	if err != nil || nodes == nil {
		t.Fatalf("%s: %v; run make test-integration", redis.AddrMapEnv, err)
	}
	var proxies []*cutProxy
	var addrMap []string
	for i := range 6 {
		node := fmt.Sprintf("redis-%d:6379", i)
		p := newCutProxy(t, nodes[node])
		proxies = append(proxies, p)
		addrMap = append(addrMap, node+"="+p.addr)
	}
	cut := func(down bool) {
		for _, p := range proxies {
			p.down.Store(down)
		}
	}
	t.Cleanup(func() { cut(false) })

	g := startGateway(t, strings.Join(addrMap, ","), `
version: 1
ratelimit: { replicas: 2 }
upstreams: [{ name: httpbin, endpoints: [{ address: '127.0.0.1:9101' }] }]
routes:
  - { name: open, match: { path_prefix: /anything/open }, upstream: httpbin }
  - { name: closed, match: { path_prefix: /anything/closed }, upstream: httpbin }
  - { name: degraded, match: { path_prefix: /anything/degraded }, upstream: httpbin }
policies:
  - { name: `+run+`-open, level: route, match: { routes: [open] }, key_template: '{route}', algorithm: gcra, limits: [{ rate: 1000, period: 1s }], failure_mode: fail_open }
  - { name: `+run+`-closed, level: route, match: { routes: [closed] }, key_template: '{route}', algorithm: gcra, limits: [{ rate: 1000, period: 1s }], failure_mode: fail_closed }
  - { name: `+run+`-degraded, level: route, match: { routes: [degraded] }, key_template: '{route}', algorithm: gcra, limits: [{ rate: 4, period: 1h, burst: 4 }], failure_mode: degraded }
`)
	g.waitLog(t, `"msg":"redis limiter warmed","nodes":6`)

	open, closed := `"`+run+`-open";q=1000;w=1`, `"`+run+`-closed";q=1000;w=1`
	degraded := func(q int) string { return `"` + run + `-degraded";q=` + strconv.Itoa(q) + `;w=3600` }
	expect := func(step, route string, status int, policy string) {
		t.Helper()
		got, h, body := g.get(t, "/anything/"+route)
		if got != status || h.Get("RateLimit-Policy") != policy {
			t.Fatalf("%s: GET /anything/%s: %d with RateLimit-Policy %q (%s); want %d with %q", step, route, got, h.Get("RateLimit-Policy"), body, status, policy)
		}
		if status == http.StatusServiceUnavailable && (body != `{"error":"rate limit unavailable"}` || h.Get("Retry-After") != "1") {
			t.Fatalf("%s: 503 with body %q and Retry-After %q", step, body, h.Get("Retry-After"))
		}
	}

	// Warmed: the very first request is decided by Redis; a failure would
	// be a 503.
	expect("first request", "closed", http.StatusOK, closed)
	expect("Redis up", "open", http.StatusOK, open)
	expect("Redis up", "degraded", http.StatusOK, degraded(4))

	cut(true)
	before := g.metrics(t)
	for i := range 6 { // timeouts at first, then refused by the breaker the health checks open
		expect(fmt.Sprint("outage, request ", i+1), "closed", http.StatusServiceUnavailable, "")
	}
	expect("outage", "open", http.StatusOK, "") // fail_open policies are left out of the fields
	expect("outage", "degraded", http.StatusOK, degraded(2))
	expect("outage", "degraded", http.StatusOK, degraded(2))
	expect("outage", "degraded", http.StatusTooManyRequests, degraded(2))
	after := g.metrics(t)
	g.waitLog(t, `"msg":"redis limiter: circuit breaker open"`)

	// Every Redis check took at most the 5 ms budget plus ADR-0021's 2 ms
	// slack.
	checks := after.value(`gw_ratelimit_check_duration_seconds_count{backend="redis"}`) - before.value(`gw_ratelimit_check_duration_seconds_count{backend="redis"}`)
	inBudget := after.value(`gw_ratelimit_check_duration_seconds_bucket{backend="redis",le="0.007"}`) - before.value(`gw_ratelimit_check_duration_seconds_bucket{backend="redis",le="0.007"}`)
	t.Logf("measure: %v of %v Redis checks during the outage within 7 ms", inBudget, checks)
	if checks != 10 || inBudget != checks {
		t.Errorf("%v of %v Redis checks within 7 ms; want all of 10", inBudget, checks)
	}
	errs := after.value(`gw_redis_errors_total{kind="timeout"}`) + after.value(`gw_redis_errors_total{kind="conn"}`) -
		before.value(`gw_redis_errors_total{kind="timeout"}`) - before.value(`gw_redis_errors_total{kind="conn"}`)
	if errs != 10 {
		t.Errorf("%v timeout and conn errors during the outage, want 10", errs)
	}
	for _, m := range []string{
		`gw_ratelimit_decisions_total{decision="fail_closed",mode="enforce",policy="` + run + `-closed"}`,
		`gw_ratelimit_decisions_total{decision="fail_open",mode="enforce",policy="` + run + `-open"}`,
		`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="` + run + `-degraded"}`,
		`gw_requests_total{code_class="5xx",method="GET",origin="gateway",reason="fail_closed",route="closed",tenant="none"}`,
	} {
		if after.value(m) < 1 {
			t.Errorf("/metrics: %s is %v, want at least 1", m, after.value(m))
		}
	}

	// Back up: the health checks' first answered PING closes the breaker,
	// with no request sent as a probe, and Redis decides again.
	cut(false)
	back := time.Now()
	for deadline := back.Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		status, h, _ := g.get(t, "/anything/closed")
		if status == http.StatusOK && h.Get("RateLimit-Policy") == closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the outage: GET /anything/closed is still %d", status)
		}
	}
	t.Logf("measure: Redis decides again %v after the outage", time.Since(back))
	if d := time.Since(back); d > time.Second {
		t.Errorf("Redis decides again %v after the outage, want within a second", d)
	}
	g.waitLog(t, `"msg":"redis limiter: circuit breaker closed"`)
	expect("Redis back", "degraded", http.StatusOK, degraded(4))

	// A second outage: the degraded limiter still holds the first's two.
	cut(true)
	expect("second outage", "degraded", http.StatusTooManyRequests, degraded(2))
}

// cutProxy forwards a local port to a Redis node until it is cut: then it
// forwards nothing either way and leaves new connections unanswered, as a
// paused node does.
type cutProxy struct {
	addr string
	down atomic.Bool
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{addr: ln.Addr().String()}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, client)
			mu.Unlock()
			if p.down.Load() {
				go func() { _, _ = io.Copy(io.Discard, client) }()
				continue
			}
			var d net.Dialer
			server, err := d.DialContext(context.Background(), "tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			go p.pipe(server, client)
			go p.pipe(client, server)
		}
	}()
	return p
}

// pipe copies src to dst, dropping what arrives while the proxy is cut.
func (p *cutProxy) pipe(dst, src net.Conn) {
	defer func() { _ = dst.Close(); _ = src.Close() }()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.down.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// hostGateway is a gateway process run on the host.
type hostGateway struct {
	url, admin string
	out        *syncBuffer
}

// startGateway builds the gateway and runs it on the host with config,
// reaching Redis Cluster through addrMap (ADR-0004) and the upstreams on
// their published ports. It stops when the test ends.
func startGateway(t *testing.T, addrMap, config string) *hostGateway {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gateway")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "github.com/ankitbisht23/spillway/cmd/gateway") //nolint:gosec // G204: a path in the test's temporary directory
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &hostGateway{url: "http://" + freeAddr(t), admin: "http://" + freeAddr(t), out: &syncBuffer{}}
	// Not t.Context(): it ends before the cleanup below can stop the
	// gateway with SIGTERM.
	cmd := exec.CommandContext(context.Background(), bin) //nolint:gosec // G204: the binary built above
	cmd.Env = []string{
		"SPILLWAY_GATEWAY_CONFIG=" + cfg,
		"SPILLWAY_GATEWAY_HTTP_ADDR=" + strings.TrimPrefix(g.url, "http://"),
		"SPILLWAY_GATEWAY_ADMIN_ADDR=" + strings.TrimPrefix(g.admin, "http://"),
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS=127.0.0.0/8",
		"SPILLWAY_GATEWAY_REDIS_ADDRS=redis-0:6379,redis-1:6379,redis-2:6379,redis-3:6379,redis-4:6379,redis-5:6379",
		"SPILLWAY_REDIS_ADDR_MAP=" + addrMap,
		"SPILLWAY_GATEWAY_DRAIN=0s", // tests stop it at once (GW-23)
	}
	cmd.Stdout, cmd.Stderr = g.out, g.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("gateway output:\n%s", g.out.String())
		}
	})
	ready := func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.admin+"/readyz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false // not listening yet
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if ready() {
			return g
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway not ready:\n%s", g.out.String())
		}
	}
}

// freeAddr returns a loopback address with a port that was free just now.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func (g *hostGateway) get(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// waitLog waits until the gateway's output contains s.
func (g *hostGateway) waitLog(t *testing.T, s string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(g.out.String(), s); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("gateway never logged %s", s)
		}
	}
}

// metricValues are /metrics samples by series.
type metricValues map[string]float64

func (m metricValues) value(series string) float64 { return m[series] }

func (g *hostGateway) metrics(t *testing.T) metricValues {
	t.Helper()
	_, body := get(t, g.admin+"/metrics")
	m := metricValues{}
	for _, line := range strings.Split(body, "\n") {
		if i := strings.LastIndexByte(line, ' '); i > 0 && !strings.HasPrefix(line, "#") {
			if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
				m[line[:i]] = v
			}
		}
	}
	return m
}

// syncBuffer is a bytes.Buffer safe for a process's output and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
