package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// newTracer returns a tracer whose spans, once ended, sr keeps.
func newTracer(t *testing.T, sampler sdktrace.Sampler) (*tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler), sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return sr, tp.Tracer(TracerName)
}

// traceUpstream answers a request's first n attempts 503 for a path
// /fail/n and 200 otherwise, and keeps each attempt's headers by request ID.
type traceUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen map[string][]http.Header
}

func newTraceUpstream(t *testing.T) *traceUpstream {
	t.Helper()
	u := &traceUpstream{seen: map[string][]http.Header{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		id := r.Header.Get("X-Request-Id")
		u.mu.Lock()
		u.seen[id] = append(u.seen[id], r.Header.Clone())
		n := len(u.seen[id])
		u.mu.Unlock()
		if fails, ok := strings.CutPrefix(r.URL.Path, "/fail/"); ok {
			if f, _ := strconv.Atoi(fails); n <= f {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *traceUpstream) headers(id string) []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen[id]
}

func (u *traceUpstream) port() string {
	_, port, _ := net.SplitHostPort(u.Listener.Addr().String())
	return port
}

// traceConfig sends every path to the upstream tu, round robin over eps
// (tu's port by default), with 3 attempts, the breaker held off, two local
// policies (one per phase) and two Redis ones (alice's tenant slot, and a
// global one that allows one request an hour).
func traceConfig(fields string, eps ...string) string {
	var list []string
	for _, e := range eps {
		list = append(list, fmt.Sprintf("{address: '%s'}", e))
	}
	return fmt.Sprintf(`
upstreams: [{name: u, lb: round_robin, endpoints: [%s], circuit: {min_requests: 1000}%s}]
routes: [{name: r, upstream: u, match: {path_prefix: /}, retry: {attempts: 3}}]
policies:
  - {name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
  - {name: per-consumer, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
  - {name: per-tenant, level: tenant, match: {routes: ['*']}, key_template: '{tenant}', algorithm: gcra, limits: [{rate: 1000, period: 1s}]}
  - {name: hourly, level: global, match: {routes: ['*']}, key_template: '{header:X-Hourly}', algorithm: sliding_window, limits: [{rate: 1, period: 1h}]}
`, strings.Join(list, ", "), fields)
}

// traceGateway is a traced dev-mode gateway in front of u, with alice's
// and bob's keys and a fake Redis.
func traceGateway(t *testing.T, tracer trace.Tracer, u *traceUpstream, config string) (*gateway, *fakeRedis) {
	t.Helper()
	res := &fakeResolver{hosts: map[string][]netip.Addr{}}
	res.set("tu", "127.0.0.1")
	f := newFakeRedis()
	g := newTracedGateway(t, res, config, nil, f, true, tracer)
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey, "bob": bobKey}))
	return g, f
}

// spanTree is the spans of one trace.
type spanTree []sdktrace.ReadOnlySpan

// waitTrace waits until sr holds the server span of traceID and returns
// every span of it ended by then.
func waitTrace(t *testing.T, sr *tracetest.SpanRecorder, traceID trace.TraceID) spanTree {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var tree spanTree
		for _, s := range sr.Ended() {
			if s.SpanContext().TraceID() == traceID {
				tree = append(tree, s)
			}
		}
		if tree.server() != nil {
			return tree
		}
		if time.Now().After(deadline) {
			t.Fatalf("no server span for trace %s", traceID)
		}
	}
}

func (tr spanTree) server() sdktrace.ReadOnlySpan {
	for _, s := range tr {
		if s.SpanKind() == trace.SpanKindServer {
			return s
		}
	}
	return nil
}

// named returns the spans named name, in the order they started.
func (tr spanTree) named(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range tr {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b sdktrace.ReadOnlySpan) int { return a.StartTime().Compare(b.StartTime()) })
	return out
}

// attrs returns a span's attributes as strings.
func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := map[string]string{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}

// wantAttrs reports each attribute of s that is not as in want; "" wants it
// absent.
func wantAttrs(t *testing.T, s sdktrace.ReadOnlySpan, want map[string]string) {
	t.Helper()
	got := attrs(s)
	for k, v := range want {
		if g, ok := got[k]; v == "" && ok || v != "" && g != v {
			t.Errorf("span %q: %s = %q, want %q (all: %v)", s.Name(), k, g, v, got)
		}
	}
}

const (
	clientTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	clientSpan  = "00f067aa0ba902b7"
)

// TestTraceSpans: a sampled request that is retried twice makes a server
// span continuing the client's trace, with children for API key
// resolution, each local rate limit phase, each Redis call and each
// upstream attempt, each retry naming the failure before it. Each
// attempt sends the upstream its own span as the parent, with the client's
// tracestate, and the access log carries the trace ID. No attribute holds
// the key, the consumer, the client's address or the path (OB-02, GW-22).
func TestTraceSpans(t *testing.T) {
	sr, tracer := newTracer(t, sdktrace.ParentBased(sdktrace.NeverSample()))
	u := newTraceUpstream(t)
	g, _ := traceGateway(t, tracer, u, traceConfig("", "tu:"+u.port()))

	resp := g.do(t, http.MethodGet, "/fail/2", nil, http.Header{
		"Traceparent":  {"00-" + clientTrace + "-" + clientSpan + "-01"},
		"Tracestate":   {"vendor=x"},
		"X-Request-Id": {"traced-1"},
		"X-Api-Key":    {aliceKey},
		"X-Hourly":     {"a"},
	})
	if resp.status != http.StatusOK || resp.attempts != "3" {
		t.Fatalf("status %d after %s attempts, want 200 after 3", resp.status, resp.attempts)
	}
	tid, _ := trace.TraceIDFromHex(clientTrace)
	tree := waitTrace(t, sr, tid)

	server := tree.server()
	if server.Name() != "GET r" || server.Parent().SpanID().String() != clientSpan || !server.Parent().IsRemote() {
		t.Errorf("server span %q with parent %v, want GET r under the client's %s", server.Name(), server.Parent().SpanID(), clientSpan)
	}
	if ts := server.SpanContext().TraceState().String(); ts != "vendor=x" {
		t.Errorf("server span tracestate %q, want vendor=x", ts)
	}
	wantAttrs(t, server, map[string]string{
		"http.request.method": "GET", "http.route": "r", "http.response.status_code": "200",
		"network.protocol.version": "1.1", "spillway.request_id": "traced-1", "spillway.upstream": "u",
		"spillway.origin": "upstream", "spillway.reason": "none", "spillway.attempts": "3",
		"spillway.retry_dropped": "", "error.type": "",
	})
	if server.Status().Code != codes.Unset {
		t.Errorf("server span status %v, want unset", server.Status())
	}

	// Every other span is a child of the server span.
	for _, s := range tree {
		if s != server && s.Parent().SpanID() != server.SpanContext().SpanID() {
			t.Errorf("span %q has parent %v, want the server span", s.Name(), s.Parent().SpanID())
		}
	}
	if a := tree.named("authn"); len(a) != 1 {
		t.Errorf("%d authn spans, want 1", len(a))
	} else {
		wantAttrs(t, a[0], map[string]string{"spillway.authn.result": "consumer"})
	}
	local := tree.named("ratelimit local")
	if len(local) != 2 {
		t.Fatalf("%d ratelimit local spans, want 2", len(local))
	}
	wantAttrs(t, local[0], map[string]string{"spillway.ratelimit.phase": "pre_auth", "spillway.ratelimit.policies": "1", "spillway.ratelimit.decision": "allow"})
	wantAttrs(t, local[1], map[string]string{"spillway.ratelimit.phase": "post_auth", "spillway.ratelimit.policies": "1", "spillway.ratelimit.decision": "allow"})
	redis := tree.named("ratelimit redis")
	if len(redis) != 2 {
		t.Fatalf("%d ratelimit redis spans, want 2", len(redis))
	}
	wantAttrs(t, redis[0], map[string]string{"spillway.ratelimit.slot": "tenant", "spillway.ratelimit.refund": "false", "spillway.ratelimit.result": "allow"})
	wantAttrs(t, redis[1], map[string]string{"spillway.ratelimit.slot": "global", "spillway.ratelimit.refund": "false", "spillway.ratelimit.result": "allow"})
	if redis[0].SpanKind() != trace.SpanKindClient {
		t.Errorf("ratelimit redis span kind %v, want client", redis[0].SpanKind())
	}

	att := tree.named("upstream attempt")
	if len(att) != 3 {
		t.Fatalf("%d upstream attempt spans, want 3", len(att))
	}
	common := map[string]string{
		"spillway.upstream": "u", "spillway.endpoint": "127.0.0.1:" + u.port(),
		"server.address": "tu", "server.port": u.port(), "http.request.method": "GET",
	}
	wantAttrs(t, att[0], map[string]string{"spillway.retry_reason": "", "spillway.backoff_ms": ""})
	for i, s := range att {
		wantAttrs(t, s, common)
		wantAttrs(t, s, map[string]string{"spillway.attempt": strconv.Itoa(i + 1)})
		if s.SpanKind() != trace.SpanKindClient {
			t.Errorf("attempt %d: span kind %v, want client", i+1, s.SpanKind())
		}
		if i == 0 {
			continue
		}
		wantAttrs(t, s, map[string]string{"spillway.retry_reason": "503"})
		// Full jitter below 25 ms × 2^(i−1) (ADR-0030).
		limit := 25 << (i - 1)
		if b, err := strconv.ParseFloat(attrs(s)["spillway.backoff_ms"], 64); err != nil || b < 0 || b >= float64(limit) {
			t.Errorf("attempt %d: backoff_ms %q, want in [0, %d)", i+1, attrs(s)["spillway.backoff_ms"], limit)
		}
	}
	for i, s := range att[:2] {
		wantAttrs(t, s, map[string]string{"http.response.status_code": "503", "error.type": "503"})
		if s.Status().Code != codes.Error {
			t.Errorf("attempt %d: status %v, want error", i+1, s.Status())
		}
	}
	wantAttrs(t, att[2], map[string]string{"http.response.status_code": "200", "error.type": ""})
	if att[2].Status().Code != codes.Unset {
		t.Errorf("attempt 3: status %v, want unset", att[2].Status())
	}

	// The upstream saw each attempt's own span as the parent.
	seen := u.headers("traced-1")
	if len(seen) != 3 {
		t.Fatalf("upstream saw %d attempts, want 3", len(seen))
	}
	for i, h := range seen {
		want := "00-" + clientTrace + "-" + att[i].SpanContext().SpanID().String() + "-01"
		if got := h.Values("Traceparent"); len(got) != 1 || got[0] != want {
			t.Errorf("attempt %d: upstream got traceparent %q, want %q", i+1, got, want)
		}
		if got := h.Values("Tracestate"); len(got) != 1 || got[0] != "vendor=x" {
			t.Errorf("attempt %d: upstream got tracestate %q, want vendor=x", i+1, got)
		}
	}
	if line := g.logs.access(t, "traced-1"); line["trace_id"] != clientTrace {
		t.Errorf("access log trace_id %v, want %s", line["trace_id"], clientTrace)
	}

	for _, s := range tree {
		for k, v := range attrs(s) {
			for _, secret := range []string{aliceKey, "alice", "acme", "/fail", "X-Hourly"} {
				if strings.Contains(v, secret) {
					t.Errorf("span %q: %s = %q holds %q", s.Name(), k, v, secret)
				}
			}
		}
	}
}

// TestTraceOutcomes: what the spans say when a request is denied, refused
// or fails: an invalid key, a Redis deny and the refund it causes, a
// connect error, and an upstream 5xx whose retry the budget drops (OB-02).
func TestTraceOutcomes(t *testing.T) {
	sr, tracer := newTracer(t, sdktrace.AlwaysSample())
	u := newTraceUpstream(t)
	refused := freePort(t)
	// send sends a request through g and returns the client's answer and
	// the request's spans.
	send := func(t *testing.T, g *gateway, id, path string, header http.Header) (retryResponse, spanTree) {
		t.Helper()
		h := http.Header{"X-Request-Id": {id}}
		for k, v := range header {
			h[k] = v
		}
		resp := g.do(t, http.MethodGet, path, nil, h)
		line := g.logs.access(t, id)
		tid, err := trace.TraceIDFromHex(fmt.Sprint(line["trace_id"]))
		if err != nil {
			t.Fatalf("%s: access log trace_id %v: %v", id, line["trace_id"], err)
		}
		return resp, waitTrace(t, sr, tid)
	}

	g, _ := traceGateway(t, tracer, u, traceConfig("", "tu:"+refused, "tu:"+u.port()))
	t.Run("invalid key", func(t *testing.T) {
		resp, tree := send(t, g, "bad-key", "/", http.Header{"X-Api-Key": {"not-a-key-at-all"}})
		if resp.status != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", resp.status)
		}
		wantAttrs(t, tree.server(), map[string]string{"http.response.status_code": "401", "spillway.origin": "gateway", "spillway.attempts": "0", "spillway.upstream": ""})
		if tree.server().Status().Code != codes.Unset {
			t.Errorf("server span status %v, want unset for a 4xx", tree.server().Status())
		}
		if a := tree.named("authn"); len(a) != 1 || attrs(a[0])["spillway.authn.result"] != "invalid" {
			t.Errorf("authn spans %v, want one with result invalid", a)
		}
		if n := len(tree.named("upstream attempt")) + len(tree.named("ratelimit redis")); n != 0 {
			t.Errorf("%d attempt or Redis spans, want none", n)
		}
	})

	t.Run("redis deny and refund", func(t *testing.T) {
		// The hourly limit allows one request per X-Hourly value.
		key := http.Header{"X-Api-Key": {aliceKey}, "X-Hourly": {"b"}}
		if resp, _ := send(t, g, "hourly-1", "/", key); resp.status != http.StatusOK {
			t.Fatalf("first request: status %d, want 200", resp.status)
		}
		resp, tree := send(t, g, "hourly-2", "/", key)
		if resp.status != http.StatusTooManyRequests {
			t.Fatalf("status %d, want 429", resp.status)
		}
		wantAttrs(t, tree.server(), map[string]string{"spillway.reason": "ratelimited", "spillway.attempts": "0"})
		// The refund of the tenant call runs after the answer.
		for deadline := time.Now().Add(5 * time.Second); len(tree.named("ratelimit redis")) < 3 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			tree = waitTrace(t, sr, tree.server().SpanContext().TraceID())
		}
		var got []string
		for _, s := range tree.named("ratelimit redis") {
			a := attrs(s)
			got = append(got, a["spillway.ratelimit.slot"]+" "+a["spillway.ratelimit.refund"]+" "+a["spillway.ratelimit.result"])
		}
		slices.Sort(got)
		if want := []string{"global false deny", "tenant false allow", "tenant true allow"}; !slices.Equal(got, want) {
			t.Errorf("Redis spans %q, want %q", got, want)
		}
		if l := tree.named("ratelimit local"); len(l) != 2 {
			t.Errorf("%d local spans, want 2", len(l))
		}
	})

	t.Run("connect error", func(t *testing.T) {
		// Round robin: of two requests, one starts on the refusing
		// endpoint and is retried on the other.
		var retried spanTree
		for i := range 2 {
			resp, tree := send(t, g, "refused-"+strconv.Itoa(i), "/", nil)
			if resp.status != http.StatusOK {
				t.Fatalf("status %d, want 200", resp.status)
			}
			if att := tree.named("upstream attempt"); len(att) == 2 {
				retried = tree
			}
		}
		if retried == nil {
			t.Fatal("no request was retried")
		}
		att := retried.named("upstream attempt")
		wantAttrs(t, att[0], map[string]string{"spillway.endpoint": "127.0.0.1:" + refused, "error.type": "connect_error", "http.response.status_code": ""})
		wantAttrs(t, att[1], map[string]string{"spillway.retry_reason": "connect_error", "http.response.status_code": "200"})
	})

	t.Run("budget", func(t *testing.T) {
		// The budget, 0.1 × requests + 0.1/s × 10 s, grants the first
		// request's first retry and refuses its second.
		g, _ := traceGateway(t, tracer, u, traceConfig(", retry_budget: {ratio: 0.1, min_per_sec: 0.1}", "tu:"+u.port()))
		resp, tree := send(t, g, "all-fail", "/fail/9", nil)
		if resp.status != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", resp.status)
		}
		server := tree.server()
		wantAttrs(t, server, map[string]string{"spillway.attempts": "2", "spillway.retry_dropped": "budget_exhausted", "error.type": "503", "spillway.origin": "upstream"})
		if server.Status().Code != codes.Error {
			t.Errorf("server span status %v, want error", server.Status())
		}
		if att := tree.named("upstream attempt"); len(att) != 2 || att[1].Status().Code != codes.Error {
			t.Errorf("attempt spans %d, want 2, both errors", len(att))
		}
	})
}

// TestTraceUnsampled: an unsampled request records nothing, yet keeps its
// trace ID in the access log and sends the upstream a traceparent of its
// own, unsampled; a request whose client asked for sampling is sampled
// whatever the ratio (OB-02).
func TestTraceUnsampled(t *testing.T) {
	sr, tracer := newTracer(t, sdktrace.ParentBased(sdktrace.NeverSample()))
	u := newTraceUpstream(t)
	g, _ := traceGateway(t, tracer, u, traceConfig("", "tu:"+u.port()))

	for _, tt := range []struct {
		name, traceparent string
	}{
		{"no traceparent", ""},
		{"invalid traceparent", "00-zz-00f067aa0ba902b7-01"},
		{"unsampled parent", "00-" + clientTrace + "-" + clientSpan + "-00"},
	} {
		id := strings.ReplaceAll(tt.name, " ", "-")
		h := http.Header{"X-Request-Id": {id}, "Tracestate": {"vendor=x"}}
		if tt.traceparent != "" {
			h.Set("Traceparent", tt.traceparent)
		}
		if resp := g.do(t, http.MethodGet, "/", nil, h); resp.status != http.StatusOK {
			t.Fatalf("%s: status %d", tt.name, resp.status)
		}
		traceID := fmt.Sprint(g.logs.access(t, id)["trace_id"])
		if tid, err := trace.TraceIDFromHex(traceID); err != nil || !tid.IsValid() {
			t.Errorf("%s: access log trace_id %q, want a trace ID", tt.name, traceID)
		}
		if tt.name == "unsampled parent" && traceID != clientTrace {
			t.Errorf("%s: trace ID %s, want the client's %s", tt.name, traceID, clientTrace)
		}
		seen := u.headers(id)
		if len(seen) != 1 {
			t.Fatalf("%s: upstream saw %d attempts", tt.name, len(seen))
		}
		tp := strings.Split(seen[0].Get("Traceparent"), "-")
		if len(tp) != 4 || tp[1] != traceID || tp[3] != "00" || tp[2] == clientSpan {
			t.Errorf("%s: upstream got traceparent %q, want trace %s, a span of the gateway's, unsampled", tt.name, seen[0].Get("Traceparent"), traceID)
		}
		if ts := seen[0].Values("Tracestate"); (tt.name == "unsampled parent") != (len(ts) == 1 && ts[0] == "vendor=x") {
			t.Errorf("%s: upstream got tracestate %q", tt.name, ts)
		}
	}
	if n := len(sr.Ended()); n != 0 {
		t.Errorf("%d spans recorded, want none", n)
	}

	resp := g.do(t, http.MethodGet, "/", nil, http.Header{"X-Request-Id": {"forced"}, "Traceparent": {"00-" + clientTrace + "-" + clientSpan + "-01"}})
	if resp.status != http.StatusOK {
		t.Fatalf("status %d", resp.status)
	}
	tid, _ := trace.TraceIDFromHex(clientTrace)
	if tree := waitTrace(t, sr, tid); len(tree.named("upstream attempt")) != 1 {
		t.Errorf("sampled parent: spans %d, want a server span and one attempt", len(tree))
	}
}

// TestTraceShed: a shed request gets no span, and its access log line has a
// trace ID with tracing on: the client's, or a new one (ADR-0030).
func TestTraceShed(t *testing.T) {
	sr, tracer := newTracer(t, sdktrace.AlwaysSample())
	u := newTraceUpstream(t)
	g, _ := traceGateway(t, tracer, u, traceConfig("", "tu:"+u.port()))
	g.Close()
	g.logs = &logBuffer{}
	rec := telemetry.NewRecorder(prometheus.NewRegistry(), slog.New(slog.NewJSONHandler(g.logs, nil)), 1)
	g.h = NewHandler(g.current.Load, g.identity.Load, nil, g.reg, g.limiters, nil, false, 0, tracer, rec, slog.New(slog.DiscardHandler))
	g.Server = httptest.NewServer(g.h)
	t.Cleanup(g.Close)

	for _, tt := range []struct{ id, traceparent string }{
		{"shed-new", ""},
		{"shed-client", "00-" + clientTrace + "-" + clientSpan + "-01"},
	} {
		h := http.Header{"X-Request-Id": {tt.id}}
		if tt.traceparent != "" {
			h.Set("Traceparent", tt.traceparent)
		}
		if r := g.do(t, http.MethodGet, "/", nil, h); r.status != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d, want 503", tt.id, r.status)
		}
		traceID := fmt.Sprint(g.logs.access(t, tt.id)["trace_id"])
		if tid, err := trace.TraceIDFromHex(traceID); err != nil || !tid.IsValid() {
			t.Errorf("%s: access log trace_id %q, want a trace ID", tt.id, traceID)
		}
		if tt.traceparent != "" && traceID != clientTrace {
			t.Errorf("%s: trace ID %s, want the client's %s", tt.id, traceID, clientTrace)
		}
	}
	if n := len(sr.Ended()); n != 0 {
		t.Errorf("%d spans recorded, want none", n)
	}
}

// TestFailureType: an attempt's failure as spans name it (ADR-0030,
// "Attempt outcomes").
func TestFailureType(t *testing.T) {
	live := context.Background()
	timedOut, cancel := context.WithDeadline(live, time.Now().Add(-time.Second))
	defer cancel()
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, tt := range []struct {
		name string
		ctx  context.Context
		code int
		err  error
		want string
	}{
		{"503", live, 503, nil, "503"},
		{"500", live, 500, nil, "500"},
		{"refused", live, 0, dial, "connect_error"},
		{"connect timeout", live, 0, fmt.Errorf("wrapped: %w", &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}), "connect_error"},
		{"reset", live, 0, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, "reset"},
		{"EOF", live, 0, io.EOF, "reset"},
		{"attempt deadline", timedOut, 0, context.DeadlineExceeded, "timeout"},
		{"attempt deadline while dialling", timedOut, 0, dial, "timeout"},
	} {
		var resp *http.Response
		if tt.code != 0 {
			resp = &http.Response{StatusCode: tt.code}
		}
		if got := failureType(tt.ctx, resp, tt.err); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

// freePort returns a loopback port nothing listens on just now.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return port
}

// benchHandler returns a handler that serves GET /bytes from a loopback
// upstream, with a local rate limit policy, and traces with the tracer
// tracer(ratio) returns: nil for none, or one whose spans go through the
// gateway's batch processor to an exporter that drops them.
func benchHandler(tb testing.TB) (handler func(trace.Tracer) *Handler, tracer func(ratio float64) trace.Tracer) {
	tb.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	tb.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	res := &fakeResolver{hosts: map[string][]netip.Addr{}}
	res.set("up", "127.0.0.1")
	c, err := config.Load(fmt.Appendf(nil, `
upstreams: [{name: up, endpoints: [{address: 'up:%s'}]}]
routes: [{name: r, upstream: up, match: {path_prefix: /}}]
policies:
  - {name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000000000, period: 1s}], local: true}
`, port))
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
	handler = func(tracer trace.Tracer) *Handler {
		return NewHandler(current.Load, ids.Load, nil, reg, limiters, nil, false, 4096, tracer, telemetry.NewRecorder(prometheus.NewRegistry(), discard, 0), discard)
	}
	tracer = func(ratio float64) trace.Tracer {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
			sdktrace.WithBatcher(tracetest.NewNoopExporter(), sdktrace.WithBatchTimeout(time.Second), sdktrace.WithMaxExportBatchSize(512), sdktrace.WithMaxQueueSize(2048)),
		)
		tb.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		return tp.Tracer(TracerName)
	}
	return handler, tracer
}

func serveBytes(tb testing.TB, h *Handler) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bytes", nil))
	if w.Code != http.StatusOK {
		tb.Fatalf("status %d", w.Code)
	}
}

// BenchmarkHandler is one GET through the handler, with tracing off and at
// head-sampling ratios 0, 0.01 (benchmarks, D6) and 1 (dev) (OB-02,
// ADR-0030).
func BenchmarkHandler(b *testing.B) {
	handler, tracer := benchHandler(b)
	for _, bc := range []struct {
		name   string
		tracer trace.Tracer
	}{{"off", nil}, {"ratio=0", tracer(0)}, {"ratio=0.01", tracer(0.01)}, {"ratio=1", tracer(1)}} {
		b.Run(bc.name, func(b *testing.B) {
			h := handler(bc.tracer)
			b.ReportAllocs()
			for b.Loop() {
				serveBytes(b, h)
			}
		})
	}
}

// TestUnsampledAllocations: an unsampled request costs at most 24
// allocations more than one with tracing off: its server span, one span
// per attempt to carry the trace context upstream, and their contexts,
// and nothing for the other stages (ADR-0030). BenchmarkHandler measured
// 15 (Go 1.27, OTel 1.47).
func TestUnsampledAllocations(t *testing.T) {
	handler, tracer := benchHandler(t)
	off, unsampled := handler(nil), handler(tracer(0))
	serveBytes(t, off) // connections and limiter keys, once
	serveBytes(t, unsampled)
	a := testing.AllocsPerRun(200, func() { serveBytes(t, off) })
	b := testing.AllocsPerRun(200, func() { serveBytes(t, unsampled) })
	if b-a > 24 {
		t.Errorf("unsampled request: %.0f allocations, tracing off: %.0f; want at most 24 more", b, a)
	}
}
