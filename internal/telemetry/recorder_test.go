package telemetry

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestRequestID(t *testing.T) {
	for _, sent := range [][]string{
		{"a"},
		{"Abc.123_x-Y"},
		{strings.Repeat("z", 128)},
	} {
		if got := RequestID(sent); got != sent[0] {
			t.Errorf("RequestID(%q) = %q, want it kept", sent, got)
		}
	}
	seen := map[string]bool{}
	for _, sent := range [][]string{
		nil,
		{""},
		{strings.Repeat("z", 129)},
		{"a b"},
		{"a,b"},
		{"a/b"},
		{"é"},
		{"a\x00"},
		{"a", "b"},
	} {
		got := RequestID(sent)
		if !validRequestID(got) || len(got) != 26 || slices.Contains(sent, got) || seen[got] {
			t.Errorf("RequestID(%q) = %q, want a new 26-character ID", sent, got)
		}
		seen[got] = true
	}
}

func TestTraceID(t *testing.T) {
	for tp, want := range map[string]string{
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01":     "4bf92f3577b34da6a3ce929d0e0e4736",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00":     "4bf92f3577b34da6a3ce929d0e0e4736", // not sampled upstream: still a trace
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-xyz": "4bf92f3577b34da6a3ce929d0e0e4736", // a later version may append fields
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01":     "",                                 // all-zero trace ID
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01":     "",                                 // all-zero parent ID
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01":     "",                                 // upper case
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01":     "",                                 // forbidden version
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-xyz": "",                                 // version 00 has exactly four fields
		"00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01":      "",                                 // short trace ID
		"": "",
	} {
		if got := TraceID(http.Header{"Traceparent": {tp}}); got != want {
			t.Errorf("TraceID(%q) = %q, want %q", tp, got, want)
		}
	}
}

// newTestRecorder returns a recorder whose access log is JSON in a buffer.
func newTestRecorder(sampleRate float64) (*Recorder, *prometheus.Registry, *bytes.Buffer) {
	reg, buf := prometheus.NewRegistry(), &bytes.Buffer{}
	return NewRecorder(reg, slog.New(slog.NewJSONHandler(buf, nil)), sampleRate), reg, buf
}

// TestAccessLog: one JSON line per request with every OB-03 field, the
// rate limit decisions included (RL-27), and no path, query or client
// address.
func TestAccessLog(t *testing.T) {
	rec, _, buf := newTestRecorder(1)
	rec.Request(t.Context(), &Request{
		ID: "req-1", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Route: "orders", Method: "POST", Status: 201,
		Origin: OriginUpstream, Reason: ReasonNone, Tenant: NoTenant, Upstream: "orders-svc", Attempts: 2,
		Latency: 1500 * time.Microsecond, BytesIn: 5, BytesOut: 42, KeyHash: "64571644321b",
		Decisions: []Decision{{"per-ip", "allow", "enforce"}, {"per-agent", "deny", "shadow"}},
	})
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("%d lines, want 1:\n%s", n, buf)
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"level": "INFO", "msg": "access", "request_id": "req-1", "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
		"route": "orders", "method": "POST", "status": 201.0, "latency_ms": 1.5, "bytes_in": 5.0, "bytes_out": 42.0,
		"upstream": "orders-svc", "attempts": 2.0, "key_hash": "64571644321b",
		"decisions": []any{
			map[string]any{"policy": "per-ip", "decision": "allow", "mode": "enforce"},
			map[string]any{"policy": "per-agent", "decision": "deny", "mode": "shadow"},
		},
	}
	delete(line, "time")
	if got, _ := json.Marshal(line); string(got) != mustJSON(t, want) {
		t.Errorf("access log line\n got %s\nwant %s", got, mustJSON(t, want))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAccessLogSampling: 5xx and the gateway's own 4xx are always logged;
// everything else by a share of request IDs that depends only on the ID.
func TestAccessLogSampling(t *testing.T) {
	rec, _, buf := newTestRecorder(0)
	for _, tt := range []struct {
		status int
		origin string
		logged bool
	}{
		{200, OriginUpstream, false},
		{101, OriginUpstream, false},
		{304, OriginUpstream, false},
		{404, OriginUpstream, false},
		{429, OriginUpstream, false},
		{429, OriginGateway, true},
		{503, OriginUpstream, true},
		{404, OriginGateway, true},
		{413, OriginGateway, true},
		{499, OriginGateway, true},
		{502, OriginGateway, true},
		{504, OriginGateway, true},
	} {
		buf.Reset()
		rec.Request(t.Context(), &Request{ID: "x", Route: NoRoute, Method: "GET", Status: tt.status, Origin: tt.origin, Reason: ReasonNone, Tenant: NoTenant})
		if logged := buf.Len() > 0; logged != tt.logged {
			t.Errorf("rate 0, %s %d: logged = %v, want %v", tt.origin, tt.status, logged, tt.logged)
		}
	}

	ids := make([]string, 20000)
	for i := range ids {
		ids[i] = rand.Text()
	}
	for _, rate := range []float64{0, 0.01, 0.25, 0.5, 1} {
		n := 0
		for _, id := range ids {
			if sampled(id, rate) {
				n++
			}
		}
		if share := float64(n) / float64(len(ids)); share < rate-0.01 || share > rate+0.01 {
			t.Errorf("rate %v sampled %.4f of the IDs", rate, share)
		}
	}
	// A lower rate samples a subset of what a higher one does.
	for _, id := range ids {
		if sampled(id, 0.1) && !sampled(id, 0.2) {
			t.Fatalf("%q sampled at 0.1 but not 0.2", id)
		}
	}

	// Two gateways log the same requests.
	var logged [2][]string
	for i := range logged {
		rec, _, buf := newTestRecorder(0.3)
		for _, id := range ids[:1000] {
			rec.Request(t.Context(), &Request{ID: id, Route: "r", Method: "GET", Status: 200, Origin: OriginUpstream, Reason: ReasonNone, Tenant: NoTenant})
		}
		for l := range strings.Lines(buf.String()) {
			var line map[string]any
			if err := json.Unmarshal([]byte(l), &line); err != nil {
				t.Fatal(err)
			}
			logged[i] = append(logged[i], line["request_id"].(string))
		}
	}
	if len(logged[0]) < 200 || !slices.Equal(logged[0], logged[1]) {
		t.Errorf("two recorders logged %d and %d requests, want the same 300 or so", len(logged[0]), len(logged[1]))
	}
}

func TestMetricLabels(t *testing.T) {
	rec, reg, _ := newTestRecorder(1)
	for _, q := range []Request{
		{Route: "orders", Method: "GET", Status: 200, Origin: OriginUpstream, Reason: ReasonNone, Upstream: "orders"},
		{Route: "orders", Method: "BREW", Status: 418, Origin: OriginUpstream, Reason: ReasonNone, Upstream: "orders"},
		{Route: "orders", Method: "PATCH", Status: 101, Origin: OriginUpstream, Reason: ReasonNone, Upstream: "orders"},
		{Route: "orders", Method: "GET", Status: 799, Origin: OriginUpstream, Reason: ReasonNone, Upstream: "orders"},
		{Route: "orders", Method: "DELETE", Status: 504, Origin: OriginGateway, Reason: ReasonUpstream, Upstream: "orders"},
		{Route: NoRoute, Method: "GET", Status: 404, Origin: OriginGateway, Reason: ReasonNone},
		{Route: "orders", Method: "GET", Status: 429, Origin: OriginGateway, Reason: ReasonRateLimited, Decisions: []Decision{{"per-ip", "deny", "enforce"}, {"per-agent", "deny", "shadow"}}},
		{Route: "orders", Method: "POST", Status: 201, Origin: OriginUpstream, Reason: ReasonNone, Decisions: []Decision{{"per-ip", "allow", "enforce"}, {"per-agent", "deny", "shadow"}}},
		{Route: "orders", Method: "POST", Status: 503, Origin: OriginGateway, Reason: ReasonFailClosed, Decisions: []Decision{{"global", "fail_closed", "enforce"}, {"tenant", "fail_open", "enforce"}}},
	} {
		q.ID, q.Tenant, q.Latency = "x", NoTenant, 3*time.Millisecond
		rec.Request(t.Context(), &q)
	}
	rec.UpstreamAttempt("orders", 1, 2*time.Millisecond)
	rec.UpstreamAttempt("orders", 2, 2*time.Millisecond)
	rec.Retry("orders", true)
	rec.Retry("orders", false)
	rec.Retry("orders", false)
	rec.RateLimitCheck(BackendLocal, 20*time.Microsecond)
	rec.RateLimitCheck(BackendRedis, 6*time.Millisecond)
	rec.RedisError("timeout")
	rec.RedisError("timeout")
	rec.RedisError("conn")
	rec.RefundFailure()

	m := scrape(t, reg)
	for _, s := range []string{
		`gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="orders",tenant="none"} 1`,
		`gw_requests_total{code_class="4xx",method="other",origin="upstream",reason="none",route="orders",tenant="none"} 1`,
		`gw_requests_total{code_class="1xx",method="PATCH",origin="upstream",reason="none",route="orders",tenant="none"} 1`,
		`gw_requests_total{code_class="other",method="GET",origin="upstream",reason="none",route="orders",tenant="none"} 1`,
		`gw_requests_total{code_class="5xx",method="DELETE",origin="gateway",reason="upstream",route="orders",tenant="none"} 1`,
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="none",route="none",tenant="none"} 1`,
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="ratelimited",route="orders",tenant="none"} 1`,
		`gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="per-ip"} 1`,
		`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="per-ip"} 1`,
		`gw_ratelimit_decisions_total{decision="deny",mode="shadow",policy="per-agent"} 2`,
		`gw_ratelimit_check_duration_seconds_bucket{backend="local",le="1e-05"} 0`,
		`gw_ratelimit_check_duration_seconds_bucket{backend="local",le="2.5e-05"} 1`,
		`gw_ratelimit_check_duration_seconds_bucket{backend="redis",le="0.005"} 0`,
		`gw_ratelimit_check_duration_seconds_bucket{backend="redis",le="0.007"} 1`,
		`gw_requests_total{code_class="5xx",method="POST",origin="gateway",reason="fail_closed",route="orders",tenant="none"} 1`,
		`gw_ratelimit_decisions_total{decision="fail_closed",mode="enforce",policy="global"} 1`,
		`gw_ratelimit_decisions_total{decision="fail_open",mode="enforce",policy="tenant"} 1`,
		`gw_redis_errors_total{kind="timeout"} 2`,
		`gw_redis_errors_total{kind="conn"} 1`,
		`gw_redis_errors_total{kind="script"} 0`,
		`gw_ratelimit_refund_failures_total 1`,
		`gw_request_duration_seconds_count{route="orders",tenant="none"} 8`,
		`gw_request_duration_seconds_bucket{route="none",tenant="none",le="0.0025"} 0`,
		`gw_request_duration_seconds_bucket{route="none",tenant="none",le="0.005"} 1`,
		`gw_upstream_duration_seconds_count{attempt="1",upstream="orders"} 1`,
		`gw_upstream_duration_seconds_count{attempt="2",upstream="orders"} 1`,
		`gw_retries_total{outcome="sent",upstream="orders"} 1`,
		`gw_retries_total{outcome="budget_exhausted",upstream="orders"} 2`,
	} {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
}

// TestTenantCapper: the busiest tenants of the last window keep their own
// label; the rest are "other" (OB-01).
func TestTenantCapper(t *testing.T) {
	c := newTenantCapper(2)
	label := func(tenant string, n int) (l string) {
		for range n {
			l = c.label(tenant)
		}
		return l
	}
	// Before the first window closes, free places go to tenants as they arrive.
	if a, b, x := label("a", 1), label("b", 1), label("c", 5); a != "a" || b != "b" || x != otherTenant {
		t.Errorf("first window: a=%s b=%s c=%s, want a, b, other", a, b, x)
	}
	label("b", 2)
	// Window: c 5, b 3, a 1.
	if dropped := c.rotate(); !slices.Equal(dropped, []string{"a"}) {
		t.Errorf("rotate dropped %v, want [a]", dropped)
	}
	if a, b, x := label("a", 9), label("b", 1), label("c", 1); a != otherTenant || b != "b" || x != "c" {
		t.Errorf("second window: a=%s b=%s c=%s, want other, b, c", a, b, x)
	}
	// Window: a 9, b 1, c 1; the tie goes to the first name.
	dropped := c.rotate()
	if !slices.Equal(dropped, []string{"c"}) {
		t.Errorf("rotate dropped %v, want [c]", dropped)
	}
	// A quiet window frees every place.
	c.rotate()
	if x, d := label("c", 1), label("d", 1); x != "c" || d != "d" {
		t.Errorf("after a quiet window: c=%s d=%s, want c, d", x, d)
	}
}

// TestTenantRotation: a tenant that loses its own label loses its series too,
// so tenant churn can't grow the series without bound (OB-01).
func TestTenantRotation(t *testing.T) {
	rec, reg, _ := newTestRecorder(1)
	rec.tenants = newTenantCapper(2)
	for tenant, n := range map[string]int{"acme": 3, "beta": 2, "none": 1} {
		for range n {
			rec.Request(t.Context(), &Request{ID: "x", Route: "r", Method: "GET", Status: 200, Origin: OriginUpstream, Reason: ReasonNone, Tenant: tenant})
		}
	}
	rec.Request(t.Context(), &Request{ID: "x", Route: "r", Method: "GET", Status: 200, Origin: OriginUpstream, Reason: ReasonNone, Tenant: "gamma"})
	m := scrape(t, reg)
	for _, tenant := range []string{"acme", "beta", "none", "other"} {
		if !strings.Contains(m, `tenant="`+tenant+`"`) {
			t.Errorf("no series for tenant %q", tenant)
		}
	}
	if strings.Contains(m, "gamma") {
		t.Error("a third tenant got its own label with two places")
	}

	for range 5 {
		rec.Request(t.Context(), &Request{ID: "x", Route: "r", Method: "GET", Status: 200, Origin: OriginUpstream, Reason: ReasonNone, Tenant: "gamma"})
	}
	rec.rotateTenants() // gamma 6, acme 3, beta 2
	m = scrape(t, reg)
	if strings.Contains(m, `tenant="beta"`) {
		t.Error("beta lost its label but kept its series")
	}
	if !strings.Contains(m, `tenant="acme"`) || !strings.Contains(m, `tenant="none"`) {
		t.Error("a rotation deleted the series of acme or of requests without a tenant")
	}
}

// scrape returns what /metrics serves for g.
func scrape(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	w := httptest.NewRecorder()
	promhttp.HandlerFor(g, promhttp.HandlerOpts{}).ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return "\n" + w.Body.String()
}
