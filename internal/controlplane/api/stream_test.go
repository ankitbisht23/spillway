package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/goleak"

	"github.com/ankitbisht23/spillway/internal/controlplane/live"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// GET /stream over real HTTP (UI-03, ADR-0059): the guard, validation
// before any byte of stream, the limits, the server's timeouts, slow
// clients, the token's expiry, and no token in a log or an answer. The
// hub's own behaviour is tested in package live.

// streamPG is the store side of the stream: two routes, one tenant, and
// replicas of a chosen size.
type streamPG struct{ pad int }

func (streamPG) RouteExists(_ context.Context, name string) (bool, error) {
	return name == "r1" || name == "orders", nil
}

func (streamPG) TenantExists(_ context.Context, name string) (bool, error) {
	return name == "acme", nil
}

func (p streamPG) Replicas(context.Context) (any, error) {
	return map[string]any{"items": []any{}, "pad": strings.Repeat("x", p.pad)}, nil
}

func (streamPG) HeavyHitters(context.Context, time.Time, *int64) (any, error) {
	return map[string]any{"items": []any{}}, nil
}

func (streamPG) Unavailable(error) bool { return false }

// expAuth is fakeAuth with a token expiry.
type expAuth struct{ exp time.Time }

func (a expAuth) Authenticate(ctx context.Context, tok string) (store.Actor, error) {
	actor, err := fakeAuth{}.Authenticate(ctx, tok)
	actor.Expiry = a.exp
	return actor, err
}

// streamEnv is a control plane serving the stream over HTTP, with a
// Prometheus that counts its queries.
type streamEnv struct {
	url      string
	hub      *live.Hub
	reg      *prometheus.Registry
	logs     *lockedBuffer
	promHits atomic.Int64
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type envOpts struct {
	cfg        func(*live.Config)
	authn      Authenticator
	pg         live.Postgres
	timeouts   time.Duration // the server's ReadTimeout and WriteTimeout; 0 for none
	promStatus int           // what Prometheus answers; 0 for data
}

func newStreamEnv(t *testing.T, o envOpts) *streamEnv {
	t.Helper()
	env := &streamEnv{reg: prometheus.NewRegistry(), logs: &lockedBuffer{}}
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.promHits.Add(1)
		if o.promStatus != 0 {
			w.WriteHeader(o.promStatus)
			return
		}
		if strings.HasSuffix(r.URL.Path, "query_range") {
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(prom.Close)
	u, err := live.ParsePrometheusURL(prom.URL)
	if err != nil {
		t.Fatal(err)
	}
	if o.pg == nil {
		o.pg = streamPG{}
	}
	if o.authn == nil {
		o.authn = fakeAuth{}
	}
	cfg := live.Config{Prometheus: live.NewPrometheus(u, prom.Client()), Postgres: o.pg, Registerer: env.reg, Log: logger(env.logs)}
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	env.hub = live.NewHub(cfg)
	allowed, err := proxy.ParseAllowlist("172.29.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(New(nil, allowed, fakeResolver{}, o.authn, secret.New("dev-only-not-secret"), logger(env.logs)).WithStreams(env.hub).Handler())
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = o.timeouts, o.timeouts
	srv.Config.RegisterOnShutdown(env.hub.Shutdown)
	srv.Start()
	t.Cleanup(func() {
		env.hub.Shutdown()
		srv.Close()
	})
	env.url = srv.URL
	return env
}

// open sends GET /api/v1/stream?query with header Authorization: auth
// (none if empty).
func (env *streamEnv) open(t *testing.T, ctx context.Context, query, authz string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.url+"/api/v1/stream?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func bearer(roles string) string { return "Bearer " + token(roles) }

// sseReader reads events from a stream body.
type sseReader struct{ r *bufio.Reader }

// next returns the next event's name and data, skipping comments.
func (s sseReader) next() (name, data string, err error) {
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "" && name != "":
			return name, data, nil
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func decodeErr(t *testing.T, resp *http.Response) (code, msg, requestID string) {
	t.Helper()
	var e struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("%d with a body that is not the error JSON: %v", resp.StatusCode, err)
	}
	return e.Code, e.Error, e.RequestID
}

// TestStreamAuth: no token or a bad one is 401 before anything else;
// every Spillway role may open a stream; a token without one is 403; a
// token in the URL is no token (D5).
func TestStreamAuth(t *testing.T) {
	env := newStreamEnv(t, envOpts{})
	for _, tt := range []struct {
		name, query, authz string
		status             int
	}{
		{"no token", "sub=replicas", "", 401},
		{"no token, bad sub", "sub=nope", "", 401},
		{"bad token", "sub=replicas", bearer("bad"), 401},
		{"basic", "sub=replicas", "Basic " + token("admin"), 401},
		{"token in the URL", "sub=replicas&access_token=" + url.QueryEscape(token("admin")), "", 401},
		{"no Spillway role", "sub=replicas", bearer("operator"), 403},
		{"keys never fetched", "sub=replicas", bearer("cold"), 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := env.open(t, t.Context(), tt.query, tt.authz)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.status || resp.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%d %s, want %d JSON", resp.StatusCode, resp.Header.Get("Content-Type"), tt.status)
			}
		})
	}
	for _, role := range Roles {
		t.Run(role, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			resp := env.open(t, ctx, "sub=replicas", bearer(role))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" ||
				resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("X-Request-Id") == "" {
				t.Fatalf("%d %v", resp.StatusCode, resp.Header)
			}
			if name, data, err := (sseReader{bufio.NewReader(resp.Body)}).next(); err != nil || name != "ready" || data != `{"subscriptions":["replicas"],"interval_s":5}` {
				t.Errorf("first event %s %s %v", name, data, err)
			}
		})
	}
}

// TestStreamRejectsBeforeStreaming: unknown names, unknown or unused
// parameters and bad values, PromQL injections included, are 400 JSON
// before any byte of stream, and Prometheus is never asked (D6).
func TestStreamRejectsBeforeStreaming(t *testing.T) {
	env := newStreamEnv(t, envOpts{})
	queries := []string{
		"",
		"sub=nope",
		"sub=" + url.QueryEscape(`sum(rate(gw_requests_total[5m]))`),
		"sub=overview.requests&route=" + url.QueryEscape(`"}or vector(1)`),
		"sub=overview.requests&tenant=" + url.QueryEscape(`"}or vector(1)`),
		"sub=overview.requests&route=" + url.QueryEscape(`r1",tenant=~".*`),
		"sub=overview.requests&route=" + url.QueryEscape("r1\n"),
		"sub=overview.requests&range=" + url.QueryEscape(`1m]) or vector(1) #`),
		"sub=overview.requests&route=unknown-route",
		"sub=overview.requests&tenant=unknown-tenant",
		"sub=throttling.heavy_hitters&policy_id=" + url.QueryEscape("1 or 1=1"),
		"sub=replicas&route=r1",
		"sub=replicas&query=up",
		"sub=replicas&sub=replicas",
		"sub=replicas" + strings.Repeat("&sub=overview.requests", 8),
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			resp := env.open(t, t.Context(), q, bearer("viewer"))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 400 || resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			code, msg, id := decodeErr(t, resp)
			if code != "invalid_request" || id == "" {
				t.Errorf("%s %q %q", code, msg, id)
			}
			values, _ := url.ParseQuery(q)
			for name, vs := range values {
				for _, v := range vs {
					if len(v) > 2 && name != "sub" && strings.Contains(msg, v) {
						t.Errorf("the message echoes %q: %s", v, msg)
					}
				}
			}
		})
	}
	if n := env.promHits.Load(); n != 0 {
		t.Errorf("Prometheus was asked %d times", n)
	}
	if got := gauge(t, env.reg, "cp_stream_rejected_total", "invalid"); got != float64(len(queries)) {
		t.Errorf("rejected invalid = %v, want %d", got, len(queries))
	}
}

// gauge is the value of a counter or gauge of reg, with one label value
// if given.
func gauge(t *testing.T, reg *prometheus.Registry, name string, label ...string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if len(label) > 0 && (len(m.GetLabel()) == 0 || m.GetLabel()[0].GetValue() != label[0]) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("no metric %s %v", name, label)
	return 0
}

// TestStreamLimits: past the per-caller limit, 503 unavailable with
// Retry-After: 10; another caller still gets a stream.
func TestStreamLimits(t *testing.T) {
	env := newStreamEnv(t, envOpts{cfg: func(c *live.Config) { c.PerSubject = 1 }})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := env.open(t, ctx, "sub=replicas", bearer("viewer"))
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != 200 {
		t.Fatalf("first stream: %d", first.StatusCode)
	}
	second := env.open(t, t.Context(), "sub=replicas", bearer("viewer"))
	defer func() { _ = second.Body.Close() }()
	if code, _, _ := decodeErr(t, second); second.StatusCode != 503 || code != "unavailable" || second.Header.Get("Retry-After") != "10" {
		t.Errorf("second stream: %d %s Retry-After %q", second.StatusCode, code, second.Header.Get("Retry-After"))
	}
	other := env.open(t, ctx, "sub=replicas", bearer("admin"))
	defer func() { _ = other.Body.Close() }()
	if other.StatusCode != 200 {
		t.Errorf("another caller: %d", other.StatusCode)
	}
}

// TestStreamOutlivesServerTimeouts: the stream clears the connection's
// deadlines, so a server with 1 s ReadTimeout and WriteTimeout neither
// cuts it nor cancels its context (ADR-0059).
func TestStreamOutlivesServerTimeouts(t *testing.T) {
	env := newStreamEnv(t, envOpts{timeouts: time.Second, cfg: func(c *live.Config) {
		c.Interval, c.Keepalive = 200*time.Millisecond, 300*time.Millisecond
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp := env.open(t, ctx, "sub=replicas&sub=overview.requests", bearer("viewer"))
	defer func() { _ = resp.Body.Close() }()
	r := sseReader{bufio.NewReader(resp.Body)}
	results := 0
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		name, _, err := r.next()
		if err != nil {
			t.Fatalf("after %d results: %v", results, err)
		}
		if name == "result" {
			results++
		}
	}
	if results < 10 {
		t.Errorf("%d results in 3 s at a 200 ms interval", results)
	}
}

// TestStreamSlowClient: a client that stops reading is disconnected once
// an event waits past the write timeout, over a real socket.
func TestStreamSlowClient(t *testing.T) {
	env := newStreamEnv(t, envOpts{pg: streamPG{pad: 256 << 10}, cfg: func(c *live.Config) {
		c.Interval, c.WriteTimeout = 20*time.Millisecond, 300*time.Millisecond
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp := env.open(t, ctx, "sub=replicas", bearer("viewer"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	// Read nothing: the socket buffers fill, and a write then blocks.
	for deadline := time.Now().Add(20 * time.Second); gauge(t, env.reg, "cp_stream_closed_total", "slow") == 0; {
		if time.Now().After(deadline) {
			t.Fatal("not disconnected within 20 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c := gauge(t, env.reg, "cp_stream_connections"); c != 0 {
		t.Errorf("connections = %v after the disconnect", c)
	}
}

// TestStreamTokenExpiry: the guard passes the token's exp, and the stream
// ends there with end token_expired.
func TestStreamTokenExpiry(t *testing.T) {
	env := newStreamEnv(t, envOpts{authn: expAuth{exp: time.Now().Add(1500 * time.Millisecond)}})
	start := time.Now()
	resp := env.open(t, t.Context(), "sub=replicas", bearer("viewer"))
	defer func() { _ = resp.Body.Close() }()
	r := sseReader{bufio.NewReader(resp.Body)}
	var last, data string
	for {
		name, d, err := r.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		last, data = name, d
	}
	if last != "end" || data != `{"reason":"token_expired"}` || time.Since(start) < time.Second {
		t.Errorf("ended with %s %s after %v", last, data, time.Since(start))
	}
}

// TestStreamShutdown: http.Server.Shutdown ends open streams with end
// shutdown at once instead of waiting for them.
func TestStreamShutdown(t *testing.T) {
	hub := live.NewHub(live.Config{Postgres: streamPG{}})
	allowed, _ := proxy.ParseAllowlist("172.29.0.0/16")
	srv := httptest.NewServer(New(nil, allowed, fakeResolver{}, fakeAuth{}, secret.New("dev-only-not-secret"), logger(io.Discard)).WithStreams(hub).Handler())
	defer srv.Close()
	srv.Config.RegisterOnShutdown(hub.Shutdown)
	env := &streamEnv{url: srv.URL}
	resp := env.open(t, t.Context(), "sub=replicas", bearer("viewer"))
	defer func() { _ = resp.Body.Close() }()
	r := sseReader{bufio.NewReader(resp.Body)}
	if name, _, err := r.next(); err != nil || name != "ready" {
		t.Fatal(name, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Config.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v after %v", err, time.Since(start))
	}
	var last string
	for {
		name, data, err := r.next()
		if err != nil {
			break
		}
		last = name + " " + data
	}
	if last != `end {"reason":"shutdown"}` {
		t.Errorf("last event %q", last)
	}
}

// TestStreamNoTokenLeak: no token, valid or not, appears in a log line, an
// error answer or a stream, whatever the outcome; and every goroutine of a
// stream ends when its client goes away.
func TestStreamNoTokenLeak(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	t.Run("streams", func(t *testing.T) { // its cleanups close the servers before the leak check
		env := newStreamEnv(t, envOpts{promStatus: 503, cfg: func(c *live.Config) { c.Interval = 100 * time.Millisecond }})
		var bodies strings.Builder
		for _, tt := range []struct{ query, authz string }{
			{"sub=replicas", ""},
			{"sub=replicas", bearer("bad")},
			{"sub=replicas", bearer("operator")},
			{"sub=nope", bearer("viewer")},
			{"sub=overview.requests&route=" + url.QueryEscape(token("admin")), bearer("admin")},
			{"sub=overview.requests&sub=replicas", bearer("viewer")},
		} {
			ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond) // far above a host stall (ADR-0034)
			resp := env.open(t, ctx, tt.query, tt.authz)
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body) // a stream until the timeout
			bodies.Write(b)
			cancel()
			_ = resp.Body.Close()
		}
		if !strings.Contains(bodies.String(), "prometheus_unavailable") {
			t.Errorf("no error event in the streams:\n%s", bodies.String())
		}
		for deadline := time.Now().Add(5 * time.Second); gauge(t, env.reg, "cp_stream_connections") != 0 || gauge(t, env.reg, "cp_stream_keys") != 0; {
			if time.Now().After(deadline) {
				t.Fatal("streams still open 5 s after their clients left")
			}
			time.Sleep(10 * time.Millisecond)
		}
		logs := env.logs.String()
		if strings.Contains(logs, tokenSecret) || strings.Contains(bodies.String(), tokenSecret) {
			t.Errorf("a token appears in the logs or an answer:\nlogs: %s\nanswers: %s", logs, bodies.String())
		}
		if !strings.Contains(logs, `"operation":"openStream","status":403,"sub":"user-operator"`) {
			t.Errorf("the denial is not logged with its sub:\n%s", logs)
		}
	})
	http.DefaultClient.CloseIdleConnections()
}

// TestStreamWithoutHub: a server without a hub answers 503.
func TestStreamWithoutHub(t *testing.T) {
	a := do(t, newHandler(t, io.Discard), "GET", "/api/v1/stream?sub=replicas", "")
	if a.status != 503 || a.err.Code != "unavailable" {
		t.Errorf("%d %s", a.status, a.err.Code)
	}
}
