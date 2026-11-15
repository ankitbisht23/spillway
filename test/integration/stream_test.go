//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/live"
)

// The live-data stream (UI-03, ADR-0059) against the compose stack: the
// catalogue's PromQL against compose's Prometheus, and the compose control
// plane's GET /api/v1/stream with Keycloak tokens.

// anyName is a store in which every route and tenant exists.
type anyName struct{}

func (anyName) RouteExists(context.Context, string) (bool, error)  { return true, nil }
func (anyName) TenantExists(context.Context, string) (bool, error) { return true, nil }
func (anyName) Replicas(context.Context) (any, error)              { return map[string]any{"items": []any{}}, nil }
func (anyName) HeavyHitters(context.Context, time.Time, *int64) (any, error) {
	return map[string]any{"items": []any{}}, nil
}
func (anyName) Unavailable(error) bool { return false }

// streamEvent is one event of a stream.
type streamEvent struct {
	name string
	data map[string]any
}

// readEvents reads events from body until done says it has enough, or
// the deadline passes (returned events so far, and false).
func readEvents(t *testing.T, body *bufio.Reader, deadline time.Duration, done func([]streamEvent) bool) ([]streamEvent, bool) {
	t.Helper()
	type line struct {
		s   string
		err error
	}
	lines := make(chan line)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			s, err := body.ReadString('\n')
			select {
			case lines <- line{s, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var events []streamEvent
	var name string
	timeout := time.After(deadline)
	for {
		select {
		case <-timeout:
			return events, false
		case l := <-lines:
			if l.err != nil {
				return events, done(events)
			}
			s := strings.TrimSuffix(l.s, "\n")
			switch {
			case strings.HasPrefix(s, "event: "):
				name = strings.TrimPrefix(s, "event: ")
			case strings.HasPrefix(s, "data: "):
				e := streamEvent{name: name}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(s, "data: ")), &e.data); err != nil {
					t.Fatalf("event data %q: %v", s, err)
				}
				events = append(events, e)
				if done(events) {
					return events, true
				}
			}
		}
	}
}

// lastBySub is each subscription's last result or error event.
func lastBySub(events []streamEvent) map[string]streamEvent {
	out := map[string]streamEvent{}
	for _, e := range events {
		if sub, ok := e.data["sub"].(string); ok {
			out[sub] = e
		}
	}
	return out
}

// TestStreamCatalogueOnPrometheus runs every Prometheus-backed named query
// against compose's Prometheus (v3.15.1) at every range and with route and
// tenant filters: Prometheus accepts each PromQL text the catalogue builds,
// and the answers decode, with live data from gateway-1 for requests.
func TestStreamCatalogueOnPrometheus(t *testing.T) {
	// Traffic through gateway-1, so gw_requests_total has a series to show.
	for range 20 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/anything/it-stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	u, err := live.ParsePrometheusURL("http://127.0.0.1:9091")
	if err != nil {
		t.Fatal(err)
	}
	hub := live.NewHub(live.Config{Prometheus: live.NewPrometheus(u, nil), Postgres: anyName{}, Interval: time.Second})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, err := hub.Open(r.Context(), "it", r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		st.Serve(w, r, time.Time{})
	}))
	defer srv.Close()
	defer hub.Shutdown()
	subs := []string{"overview.requests", "overview.ratelimited", "overview.latency", "overview.routes", "overview.tenants", "throttling.policies"}
	for _, params := range []string{"range=15m", "range=1h", "range=6h", "range=24h", "range=7d&route=it-route&tenant=none"} {
		t.Run(params, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/?sub="+strings.Join(subs, "&sub=")+"&"+params, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 200 {
				t.Fatalf("%d", resp.StatusCode)
			}
			// 15 s: far above a host stall (ADR-0034); the answers take
			// milliseconds.
			events, ok := readEvents(t, bufio.NewReader(resp.Body), 15*time.Second, func(ev []streamEvent) bool { return len(lastBySub(ev)) == len(subs) })
			if !ok {
				t.Fatalf("not every subscription answered in 15 s: %v", events)
			}
			for sub, e := range lastBySub(events) {
				if e.name != "result" {
					t.Errorf("%s: %s %v", sub, e.name, e.data)
				}
			}
		})
	}
	// gateway-1's requests show within two scrapes (5 s each), via a
	// fresh evaluation every second.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/?sub=overview.requests", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, ok := readEvents(t, bufio.NewReader(resp.Body), 30*time.Second, func(ev []streamEvent) bool {
		e, found := lastBySub(ev)["overview.requests"]
		if !found || e.name != "result" {
			return false
		}
		series, _ := e.data["data"].(map[string]any)["series"].([]any)
		return len(series) > 0
	})
	if !ok {
		t.Error("overview.requests shows no series 30 s after gateway-1's requests")
	}
}

// TestComposeStream opens the compose control plane's stream with Keycloak
// tokens (UI-03, D5, ADR-0059): every role may, the Prometheus-backed and
// Postgres-backed subscriptions both answer results (so
// SPILLWAY_CP_PROMETHEUS_URL reaches compose's Prometheus), and no token
// or a bad parameter is refused before any stream.
func TestComposeStream(t *testing.T) {
	composeControlPlane(t) // waits for its token keys
	const base = "http://127.0.0.1:8080/api/v1/stream?"
	get := func(ctx context.Context, query, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get(t.Context(), "sub=replicas", "")
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	viewer := keycloakToken(t, "password", "rbac-tests", "test-viewer")
	resp = get(t.Context(), "sub=overview.requests&route="+`%22%7Dor+vector(1)`, viewer)
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("an injected route: %d", resp.StatusCode)
	}
	subs := []string{"overview.requests", "replicas", "throttling.heavy_hitters"}
	for name, tok := range map[string]string{"viewer": viewer, "spillway-ci": keycloakToken(t, "client_credentials", "spillway-ci", "")} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			resp := get(ctx, "sub="+strings.Join(subs, "&sub=")+"&range=1h", tok)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %v", resp.StatusCode, resp.Header)
			}
			events, ok := readEvents(t, bufio.NewReader(resp.Body), 15*time.Second, func(ev []streamEvent) bool { return len(lastBySub(ev)) == len(subs) })
			if !ok || events[0].name != "ready" {
				t.Fatalf("events in 15 s: %v", events)
			}
			for sub, e := range lastBySub(events) {
				if e.name != "result" {
					t.Errorf("%s: %s %v", sub, e.name, e.data)
				}
			}
			items, _ := lastBySub(events)["replicas"].data["data"].(map[string]any)["items"].([]any)
			for _, it := range items {
				r := it.(map[string]any)
				if _, ok := r["replica"].(map[string]any)["id"].(string); !ok {
					t.Errorf("a replica without an id: %v", r)
				}
				if _, ok := r["staleness_s"]; !ok {
					t.Errorf("a replica without staleness_s: %v", r)
				}
			}
		})
	}
}
