package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// logBuffer collects JSON log lines written from any goroutine.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// access waits for the access log line of a request ID and returns it. A
// request is logged after its response is written, so the client can see the
// response first.
func (b *logBuffer) access(t *testing.T, requestID string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b.mu.Lock()
		lines := bytes.Split(b.buf.Bytes(), []byte("\n"))
		b.mu.Unlock()
		for _, l := range lines {
			var line map[string]any
			if json.Unmarshal(l, &line) == nil && line["msg"] == "access" && line["request_id"] == requestID {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no access log line for request %q", requestID)
		}
	}
}

// scrape returns what /metrics serves for g.
func scrape(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	w := httptest.NewRecorder()
	promhttp.HandlerFor(g, promhttp.HandlerOpts{}).ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return "\n" + w.Body.String()
}

// TestRequestID: a valid request ID from the client goes upstream and back
// unchanged; any other is replaced by a new one, which the upstream, the
// response and the access log all carry. It replaces the upstream's own, and
// the gateway's own answers carry one too (GW-22).
func TestRequestID(t *testing.T) {
	u := serveUpstream(t, "orders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "upstream-own")
		_ = json.NewEncoder(w).Encode(echo{Header: r.Header})
	})
	g := newGateway(t, newResolver(u), routeTo(u, "priority: 0"))

	seen := map[string]bool{}
	for _, tt := range []struct {
		sent []string
		keep bool
	}{
		{[]string{"Abc.123_x-Y"}, true},
		{[]string{strings.Repeat("a", 128)}, true},
		{nil, false},
		{[]string{strings.Repeat("a", 129)}, false},
		{[]string{"a b"}, false},
		{[]string{"a", "b"}, false},
	} {
		status, h, e := g.get(t, "/v1/orders", http.Header{"X-Request-Id": tt.sent})
		got, upstreamGot := h.Values("X-Request-Id"), e.Header.Values("X-Request-Id")
		if status != http.StatusOK || len(got) != 1 || !slices.Equal(upstreamGot, got) {
			t.Errorf("sent %q: status %d, response %q, upstream got %q; want 200 and one ID, the same", tt.sent, status, got, upstreamGot)
			continue
		}
		if keep := len(tt.sent) == 1 && got[0] == tt.sent[0]; keep != tt.keep || !tt.keep && (len(got[0]) != 26 || seen[got[0]]) {
			t.Errorf("sent %q: request ID %q, want it kept: %v", tt.sent, got[0], tt.keep)
		}
		seen[got[0]] = true
		if line := g.logs.access(t, got[0]); line["route"] != "r" {
			t.Errorf("access log line for %s: %v", got[0], line)
		}
	}

	status, h, _ := g.get(t, "/nowhere", nil)
	if id := h.Get("X-Request-Id"); status != http.StatusNotFound || len(id) != 26 {
		t.Errorf("no route: %d with request ID %q, want 404 with a new one", status, id)
	} else if line := g.logs.access(t, id); line["route"] != "none" || line["status"] != 404.0 {
		t.Errorf("access log line for the 404: %v", line)
	}
}

// TestTraceparent: traceparent goes upstream unchanged, valid or not, and a
// valid one's trace ID goes in the access log (GW-22).
func TestTraceparent(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	for i, tt := range []struct{ traceparent, traceID string }{
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""},
		{"not a traceparent", ""},
	} {
		id := "trace-" + strconv.Itoa(i)
		_, _, e := g.get(t, "/", http.Header{"Traceparent": {tt.traceparent}, "X-Request-Id": {id}})
		if got := e.Header.Values("Traceparent"); len(got) != 1 || got[0] != tt.traceparent {
			t.Errorf("upstream got traceparent %q, want %q unchanged", got, tt.traceparent)
		}
		if line := g.logs.access(t, id); line["trace_id"] != tt.traceID {
			t.Errorf("traceparent %q: access log trace_id %q, want %q", tt.traceparent, line["trace_id"], tt.traceID)
		}
	}
}

// TestAccessLogAndMetrics: each request's access log line and RED metric
// labels, for upstream answers and the gateway's own (OB-01, OB-03).
func TestAccessLogAndMetrics(t *testing.T) {
	u := serveUpstream(t, "orders", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if code, err := strconv.Atoi(r.URL.Query().Get("status")); err == nil {
			w.WriteHeader(code)
		}
		_, _ = w.Write(body)
	})
	down := newUpstream(t, "down")
	downPort := down.port()
	down.Close()
	res := newResolver(u)
	res.set("down", "127.0.0.1")
	res.set("metadata", "169.254.169.254") // outside the allow-list: no usable address
	g := newGateway(t, res, fmt.Sprintf(`
upstreams:
  - {name: orders, endpoints: [{address: 'orders:%s'}]}
  - {name: down, endpoints: [{address: 'down:%s'}]}
  - {name: metadata, endpoints: [{address: 'metadata:80'}]}
routes:
  - {name: orders, upstream: orders, match: {path_prefix: /orders}}
  - {name: down, upstream: down, match: {path_prefix: /down}}
  - {name: metadata, upstream: metadata, match: {path_prefix: /metadata}}
`, u.port(), downPort))

	for i, tt := range []struct {
		method, path, body string
		status             int
		log                map[string]any // fields of the access log line
		labels             string         // of gw_requests_total
	}{
		{
			"POST", "/orders/raw-path-1", "hello", 200,
			map[string]any{"route": "orders", "method": "POST", "bytes_in": 5.0, "bytes_out": 5.0, "upstream": "orders"},
			`code_class="2xx",method="POST",origin="upstream",reason="none",route="orders",tenant="none"`,
		},
		{
			"BREW", "/orders/raw-path-2?status=418", "", 418,
			map[string]any{"route": "orders", "method": "BREW", "bytes_out": 0.0, "upstream": "orders"},
			`code_class="4xx",method="other",origin="upstream",reason="none",route="orders",tenant="none"`,
		},
		{
			"GET", "/orders/raw-path-3?status=503", "", 503,
			map[string]any{"route": "orders", "upstream": "orders"},
			`code_class="5xx",method="GET",origin="upstream",reason="none",route="orders",tenant="none"`,
		},
		{
			"GET", "/raw-path-4", "", 404,
			map[string]any{"route": "none", "upstream": "", "bytes_out": float64(len("no route\n"))},
			`code_class="4xx",method="GET",origin="gateway",reason="none",route="none",tenant="none"`,
		},
		{
			"GET", "/down/raw-path-5", "", 502,
			map[string]any{"route": "down", "upstream": "down"},
			`code_class="5xx",method="GET",origin="gateway",reason="upstream",route="down",tenant="none"`,
		},
		{
			"GET", "/metadata/raw-path-6", "", 503,
			map[string]any{"route": "metadata", "upstream": "metadata"},
			`code_class="5xx",method="GET",origin="gateway",reason="upstream",route="metadata",tenant="none"`,
		},
	} {
		id := "req-" + strconv.Itoa(i)
		req, err := http.NewRequestWithContext(t.Context(), tt.method, g.URL+tt.path, strings.NewReader(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Request-Id", id)
		resp, err := g.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tt.status {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.status)
		}

		line := g.logs.access(t, id)
		want := map[string]any{"status": float64(tt.status), "trace_id": "", "key_hash": ""}
		for k, v := range tt.log {
			want[k] = v
		}
		for k, v := range want {
			if line[k] != v {
				t.Errorf("%s %s: access log %s = %v, want %v", tt.method, tt.path, k, line[k], v)
			}
		}
		if d, ok := line["decisions"].([]any); !ok || len(d) != 0 {
			t.Errorf("%s %s: decisions = %v, want []", tt.method, tt.path, line["decisions"])
		}
		if ms, ok := line["latency_ms"].(float64); !ok || ms < 0 {
			t.Errorf("%s %s: latency_ms = %v", tt.method, tt.path, line["latency_ms"])
		}
		if m := scrape(t, g.metrics); !strings.Contains(m, "\ngw_requests_total{"+tt.labels+"} 1\n") {
			t.Errorf("%s %s: no gw_requests_total{%s} 1 in\n%s", tt.method, tt.path, tt.labels, m)
		}
	}

	m := scrape(t, g.metrics)
	for _, s := range []string{
		`gw_request_duration_seconds_count{route="orders",tenant="none"} 3`,
		`gw_request_duration_seconds_count{route="none",tenant="none"} 1`,
		`gw_request_duration_seconds_count{route="down",tenant="none"} 1`,
		`gw_request_duration_seconds_count{route="metadata",tenant="none"} 1`,
		`gw_upstream_duration_seconds_count{attempt="1",upstream="orders"} 3`,
		`gw_upstream_duration_seconds_count{attempt="1",upstream="down"} 1`,
	} {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
	if strings.Contains(m, `upstream="metadata"`) {
		t.Error("an upstream without a usable address has an attempt duration")
	}
	if strings.Contains(m, "raw-path") {
		t.Error("a raw path became a label value")
	}
}

// TestClientGone: a request whose client leaves before the answer is recorded
// as 499.
func TestClientGone(t *testing.T) {
	arrived := make(chan struct{})
	u := serveUpstream(t, "orders", func(_ http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	})
	g := newGateway(t, newResolver(u), oneUpstream(u))
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-arrived; cancel() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "gone")
	if resp, err := g.Client().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the request was answered")
	}
	if line := g.logs.access(t, "gone"); line["status"] != 499.0 {
		t.Errorf("access log status = %v, want 499", line["status"])
	}
	labels := `code_class="4xx",method="GET",origin="gateway",reason="none",route="r",tenant="none"`
	if !strings.Contains(scrape(t, g.metrics), "\ngw_requests_total{"+labels+"} 1\n") {
		t.Errorf("no gw_requests_total{%s} 1", labels)
	}
}

// TestCutResponse: a response cut off mid-body, which ends the handler with a
// panic, is still recorded, with the bytes written before the cut.
func TestCutResponse(t *testing.T) {
	u := serveUpstream(t, "orders", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, "abcd")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	g := newGateway(t, newResolver(u), oneUpstream(u))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "cut")
	if resp, err := g.Client().Do(req); err == nil { // the cut can come before anything reaches the client
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Error("the cut body read without error")
		}
		_ = resp.Body.Close()
	}
	if line := g.logs.access(t, "cut"); line["status"] != 200.0 || line["bytes_out"] != 4.0 {
		t.Errorf("access log status %v, bytes_out %v; want 200 and 4", line["status"], line["bytes_out"])
	}
}

// TestUpgrade: a 101, which the reverse proxy writes on the hijacked
// connection, carries the request ID and is recorded as a 101.
func TestUpgrade(t *testing.T) {
	u := serveUpstream(t, "orders", func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\nX-Request-Id: upstream-own\r\n\r\n")
		_ = brw.Flush()
	})
	g := newGateway(t, newResolver(u), oneUpstream(u))
	conn := dial(t, g.Listener.Addr().String())
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: test\r\nX-Request-Id: up-1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ids := resp.Header.Values("X-Request-Id"); resp.StatusCode != http.StatusSwitchingProtocols || len(ids) != 1 || ids[0] != "up-1" {
		t.Errorf("upgrade answered %d with request IDs %q, want 101 with up-1", resp.StatusCode, ids)
	}
	_ = conn.Close()
	if line := g.logs.access(t, "up-1"); line["status"] != 101.0 {
		t.Errorf("access log status = %v, want 101", line["status"])
	}
}
