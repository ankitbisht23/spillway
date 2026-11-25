package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// The policies of BenchmarkServeHTTP's cases (ADR-0073 §D7). Limits are high
// enough that no request is denied.
const (
	benchLocal = `
policies:
  - {name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000000000, period: 1s}], local: true}
  - {name: per-route, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 1000000000, period: 1s}], local: true}
`
	benchRedis = `
policies:
  - {name: per-ip, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000000, period: 1s}], failure_mode: fail_closed}
`
	benchTenant = `
policies:
  - {id: 1, name: per-tenant, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 1000000, period: 1s}], failure_mode: fail_closed}
`
	benchLease = `
policies:
  - {name: batched, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000000, period: 1s}], lease_k: 1000, failure_mode: fail_closed}
`
)

// kilobyte is the benchmark upstream's response body: NFR-01's 1 KB.
var kilobyte = bytes.Repeat([]byte("x"), 1024)

// serveBench is a handler as the gateway builds it for GET / on one route to
// a loopback upstream answering 1 KB, with the given policies, Redis and
// identity (managed: compiled as a managed gateway does). It access-logs 1%
// of requests, as the benchmark gateway does (ADR-0015).
func serveBench(tb testing.TB, policies string, managed bool, r Redis, id *config.Identity) *Handler {
	tb.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(kilobyte) }))
	tb.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	res := &fakeResolver{hosts: map[string][]netip.Addr{}}
	res.set("up", "127.0.0.1")
	c, err := config.Load([]byte(`
ratelimit: {redis_budget: 1s} # a slow benchmark machine must not time out on scheduling
upstreams: [{name: up, endpoints: [{address: 'up:` + port + `'}]}]
routes: [{name: r, upstream: up, match: {path_prefix: /}}]
` + policies))
	if err != nil {
		tb.Fatal(err)
	}
	compile := config.Compile
	if managed {
		compile = config.CompileManaged
	}
	s, err := compile(c)
	if err != nil {
		tb.Fatal(err)
	}
	discard := slog.New(slog.DiscardHandler)
	reg := NewRegistry(loopback, res, discard)
	tb.Cleanup(reg.Close)
	if err := reg.Apply(s); err != nil {
		tb.Fatal(err)
	}
	limiters := local.NewRegistry(ratelimit.SystemClock)
	limiters.Apply(s)
	var current atomic.Pointer[config.Snapshot]
	current.Store(s)
	var ids atomic.Pointer[config.Identity]
	ids.Store(id)
	rec := telemetry.NewRecorder(prometheus.NewRegistry(), slog.New(slog.NewJSONHandler(io.Discard, nil)), 0.01)
	return NewHandler(current.Load, ids.Load, nil, reg, limiters, r, false, 4096, nil, rec, discard)
}

// BenchmarkServeHTTP is one GET through the real handler to a loopback
// upstream answering 1 KB (ADR-0073 §D7): plain (no policy), local (two
// local GCRA policies), redis (a Redis policy that a zero-latency Redis
// allows), apikey (a managed key resolved, and a tenant-level Redis policy)
// and lease (a batched policy served from its lease). Allocations per
// request are its allocs/op.
func BenchmarkServeHTTP(b *testing.B) {
	key, entry := newManagedKey(b, 1, time.Time{})
	for _, bc := range []struct {
		name     string
		policies string
		managed  bool
		redis    bool
		id       *config.Identity
		header   http.Header
	}{
		{name: "plain"},
		{name: "local", policies: benchLocal},
		{name: "redis", policies: benchRedis, redis: true},
		{name: "apikey", policies: benchTenant, managed: true, redis: true, id: managedIdentity(b, entry), header: http.Header{"X-Api-Key": {key.Secret.Reveal()}}},
		{name: "lease", policies: benchLease, redis: true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			var r Redis
			stub := &stubRedis{}
			if bc.redis {
				r = stub
			}
			h := serveBench(b, bc.policies, bc.managed, r, bc.id)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if bc.header != nil {
				req.Header = bc.header
			}
			serve := func() {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req.Clone(req.Context()))
				if w.Code != http.StatusOK || w.Body.Len() != len(kilobyte) {
					b.Fatalf("status %d, %d bytes", w.Code, w.Body.Len())
				}
			}
			serve() // the upstream connection and the limiter keys, once
			stub.calls.Store(0)
			b.ReportAllocs()
			for b.Loop() {
				serve()
			}
			if bc.redis {
				b.ReportMetric(float64(stub.calls.Load())/float64(b.N), "redis-calls/op")
			}
		})
	}
}
