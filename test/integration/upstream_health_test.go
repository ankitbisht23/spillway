//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// TestGatewayUpstreamHealth runs a gateway on the host in front of the
// compose mock upstreams upstream-1 … upstream-3 (published on 9101–9103),
// with upstream-3 reached through a proxy in this test that can fail it
// (GW-08, GW-09, ADR-0030):
//
//   - with active health checks, upstream-3 closing every connection is
//     marked unhealthy and gets no traffic, and once it answers again it is
//     marked healthy and gets traffic again;
//   - without health checks, upstream-3 answering 503 is ejected after 2
//     in a row, gets no traffic while ejected, and returns after the
//     ejection.
//
// The fault lives in this test's proxy, so the compose stack is never
// changed and nothing needs restoring. Both upstreams are balanced round
// robin, so that the test can count the requests that reach upstream-3.
func TestGatewayUpstreamHealth(t *testing.T) {
	fault := newFaultProxy(t, "127.0.0.1:9103")
	g := startGateway(t, os.Getenv(redis.AddrMapEnv), fmt.Sprintf(`
version: 1
upstreams:
  - name: active
    lb: round_robin
    endpoints: [{address: '127.0.0.1:9101'}, {address: '127.0.0.1:9102'}, {address: '%[1]s'}]
    health_check: {path: /status/200, interval: 500ms, timeout: 500ms, unhealthy: 2, healthy: 2}
    outlier_detection: {consecutive_failures: 1000}
  - name: passive
    lb: round_robin
    endpoints: [{address: '127.0.0.1:9101'}, {address: '127.0.0.1:9102'}, {address: '%[1]s'}]
    outlier_detection: {consecutive_failures: 2, base_ejection: 3s}
routes:
  - {name: active, upstream: active, match: {path_prefix: /active}, strip_prefix: /active, timeouts: {per_try: 1s}}
  - {name: passive, upstream: passive, match: {path_prefix: /passive}, strip_prefix: /passive, timeouts: {per_try: 1s}}
`, fault.addr))
	gauge := func(upstream string) float64 {
		v, ok := g.metrics(t)[fmt.Sprintf(`gw_upstream_endpoint_healthy{endpoint=%q,upstream=%q}`, fault.addr, upstream)]
		if !ok {
			t.Fatalf("no gw_upstream_endpoint_healthy series for %s in %s", fault.addr, upstream)
		}
		return v
	}
	waitGauge := func(upstream string, want float64) time.Duration {
		t.Helper()
		start := time.Now()
		for deadline := start.Add(10 * time.Second); gauge(upstream) != want; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: upstream-3's gauge did not reach %v within 10 s", upstream, want)
			}
		}
		return time.Since(start)
	}
	// send sends n requests and returns how many were not 200 and how many
	// reached upstream-3.
	send := func(route string, n int) (failed int, faulty int64) {
		t.Helper()
		before := fault.traffic.Load()
		for range n {
			if code, _, _ := g.get(t, "/"+route+"/status/200"); code != http.StatusOK {
				failed++
			}
		}
		return failed, fault.traffic.Load() - before
	}

	// Active health checks.
	if failed, faulty := send("active", 9); failed != 0 || faulty != 3 {
		t.Fatalf("active, all up: %d of 9 failed, %d reached upstream-3; want 0 and 3", failed, faulty)
	}
	fault.mode.Store(faultClose)
	t.Logf("active: upstream-3 marked unhealthy %v after it went down", waitGauge("active", 0))
	if failed, faulty := send("active", 30); failed != 0 || faulty != 0 {
		t.Errorf("active, upstream-3 unhealthy: %d of 30 failed, %d reached upstream-3; want 0 and 0", failed, faulty)
	}
	g.waitLog(t, `"msg":"upstream endpoint unhealthy","upstream":"active","endpoint":"`+fault.addr+`"`)
	fault.mode.Store(faultNone)
	t.Logf("active: upstream-3 marked healthy %v after it came back", waitGauge("active", 1))
	if failed, faulty := send("active", 9); failed != 0 || faulty != 3 {
		t.Errorf("active, upstream-3 back: %d of 9 failed, %d reached upstream-3; want 0 and 3", failed, faulty)
	}
	g.waitLog(t, `"msg":"upstream endpoint healthy","upstream":"active","endpoint":"`+fault.addr+`"`)

	// Passive outlier ejection.
	fault.mode.Store(fault503)
	if failed, faulty := send("passive", 6); failed != 2 || faulty != 2 {
		t.Fatalf("passive, upstream-3 answering 503: %d of 6 failed, %d reached upstream-3; want 2 and 2", failed, faulty)
	}
	if gauge("passive") != 0 {
		t.Fatal("passive: upstream-3 not ejected after 2 503s in a row")
	}
	g.waitLog(t, `"msg":"upstream endpoint ejected","upstream":"passive","endpoint":"`+fault.addr+`","for":3000000000,"times":1`)
	if failed, faulty := send("passive", 20); failed != 0 || faulty != 0 {
		t.Errorf("passive, upstream-3 ejected: %d of 20 failed, %d reached upstream-3; want 0 and 0", failed, faulty)
	}
	fault.mode.Store(faultNone)
	t.Logf("passive: upstream-3 back %v after the last check", waitGauge("passive", 1))
	if failed, faulty := send("passive", 9); failed != 0 || faulty != 3 {
		t.Errorf("passive, upstream-3 back: %d of 9 failed, %d reached upstream-3; want 0 and 3", failed, faulty)
	}
}

// Fault modes of a faultProxy.
const (
	faultNone  = iota
	faultClose // close every request's connection without an answer
	fault503   // answer every request 503
)

// faultProxy is an HTTP proxy to one upstream that can fail it, and counts
// the requests other than health checks that reach it.
type faultProxy struct {
	addr    string
	mode    atomic.Int32
	traffic atomic.Int64
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{addr: ln.Addr().String()}
	rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: target})
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "spillway-health-check" {
			p.traffic.Add(1)
		}
		switch p.mode.Load() {
		case faultClose:
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		case fault503:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rp.ServeHTTP(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}
