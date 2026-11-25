package proxy

import (
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
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// stubRedis answers every call at once with the same decision: an allow,
// granting a batched limit its lease_k, or a deny by the call's first limit
// with a 60 s retry-after. It counts the calls it answers.
type stubRedis struct {
	deny  bool
	calls atomic.Int64
}

func (s *stubRedis) Call(_ context.Context, _ int64, limits []rlredis.Limit) (rlredis.Result, error) {
	s.calls.Add(1)
	res := rlredis.Result{Allowed: !s.deny, DenyingLimit: -1, NowUS: 1, Limits: make([]ratelimit.Decision, len(limits)), Granted: make([]int64, len(limits))}
	for i := range res.Limits {
		res.Limits[i] = ratelimit.Decision{Allowed: !s.deny, Remaining: 5, ResetAfter: time.Second}
		if !s.deny {
			res.Granted[i] = max(limits[i].LeaseK, 1)
		}
	}
	if s.deny {
		res.DenyingLimit, res.RetryAfter = 0, time.Minute
		res.Limits[0] = ratelimit.Decision{ResetAfter: time.Minute, RetryAfter: time.Minute}
	}
	return res, nil
}

func (s *stubRedis) Refund(context.Context, int64, int64, []rlredis.Limit) (rlredis.Result, error) {
	return rlredis.Result{Allowed: true, DenyingLimit: -1}, nil
}

// redisBenchHandler is a handler for GET / with one enforce Redis policy
// per client IP, decided by r, and a loopback upstream.
func redisBenchHandler(tb testing.TB, r Redis) *Handler {
	tb.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	tb.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	res := &fakeResolver{hosts: map[string][]netip.Addr{}}
	res.set("up", "127.0.0.1")
	c, err := config.Load([]byte(`
ratelimit: {redis_budget: 1s} # a parallel run must not time out on scheduling
upstreams: [{name: up, endpoints: [{address: 'up:` + port + `'}]}]
routes: [{name: r, upstream: up, match: {path_prefix: /}}]
policies:
  - {name: per-ip, level: route, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 10, period: 1s}], failure_mode: fail_closed}
`))
	if err != nil {
		tb.Fatal(err)
	}
	s, err := config.Compile(c)
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
	return NewHandler(current.Load, ids.Load, nil, reg, limiters, r, false, 4096, nil, telemetry.NewRecorder(prometheus.NewRegistry(), discard, 0), discard)
}

func serveStatus(tb testing.TB, h *Handler, want int) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if w.Code != want {
		tb.Fatalf("status %d, want %d", w.Code, want)
	}
}

// BenchmarkRedisCheck is one request through the handler with a Redis
// policy that a zero-latency Redis allows, or denies: the Redis check's
// own cost on the hot path, before and after the deny cache (RL-19).
func BenchmarkRedisCheck(b *testing.B) {
	for _, bc := range []struct {
		name   string
		deny   bool
		status int
	}{{"allowed", false, http.StatusOK}, {"denied", true, http.StatusTooManyRequests}} {
		b.Run(bc.name, func(b *testing.B) {
			r := &stubRedis{deny: bc.deny}
			h := redisBenchHandler(b, r)
			serveStatus(b, h, bc.status) // the upstream connection, once
			r.calls.Store(0)
			b.ReportAllocs()
			for b.Loop() {
				serveStatus(b, h, bc.status)
			}
			b.ReportMetric(float64(r.calls.Load())/float64(b.N), "redis-calls/op")
		})
	}
}

// BenchmarkCheckRedisAllowed is the Redis phase alone, newLimits included,
// for a request that a zero-latency Redis allows: what the deny cache adds
// to every allowed request (RL-19). The parallel case sends every request
// for one key, so all of them meet in one shard of the cache.
func BenchmarkCheckRedisAllowed(b *testing.B) {
	r := &stubRedis{}
	h := redisBenchHandler(b, r)
	s := h.snapshot()
	route := &s.Routes[0]
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	ip := netip.MustParseAddr("192.0.2.1")
	check := func(b *testing.B) {
		var q telemetry.Request
		l := newLimits(s, route, h.limiters, h.redis, nil, nil, h.rec, h.log, req, &q, ip)
		l.useDenyCache(h.deny, s, h.identity())
		if !l.checkRedis() {
			b.Fatal("denied")
		}
	}
	b.Run("serial", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			check(b)
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				check(b)
			}
		})
	})
}
