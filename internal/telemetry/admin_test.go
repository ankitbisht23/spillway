package telemetry

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdminServer(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(NewAdminServer(NewRegistry(), ready.Load, slog.New(slog.DiscardHandler)).Handler)
	t.Cleanup(srv.Close)

	get := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz while not ready = %d, want 503", code)
	}
	ready.Store(true)
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Errorf("/readyz while ready = %d, want 200", code)
	}

	code, body := get("/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	for _, m := range []string{"go_goroutines", "go_gc_duration_seconds", "process_"} {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics has no %s", m)
		}
	}

	for _, p := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/goroutine?debug=1", "/debug/pprof/cmdline"} {
		if code, _ := get(p); code != http.StatusOK {
			t.Errorf("%s = %d, want 200", p, code)
		}
	}
	if code, _ := get("/"); code != http.StatusNotFound {
		t.Errorf("/ = %d, want 404", code)
	}
}

// TestAdminServerExemplars: /metrics serves a sampled request's exemplar in
// the OpenMetrics format, which Prometheus asks for, and the classic text
// format, which has no exemplars, to anything else (OB-06).
func TestAdminServerExemplars(t *testing.T) {
	reg := NewRegistry()
	rec := NewRecorder(reg, slog.New(slog.DiscardHandler), 0)
	const trace = "4bf92f3577b34da6a3ce929d0e0e4736"
	rec.Request(t.Context(), &Request{ID: "x", TraceID: trace, Sampled: true, Route: "r", Method: "GET", Status: 200, Origin: OriginUpstream, Reason: ReasonNone, Tenant: NoTenant})
	srv := httptest.NewServer(NewAdminServer(reg, func() bool { return true }, slog.New(slog.DiscardHandler)).Handler)
	t.Cleanup(srv.Close)

	for _, tt := range []struct {
		accept   string
		exemplar bool
	}{
		{"application/openmetrics-text;version=1.0.0,text/plain;version=0.0.4;q=0.5", true},
		{"", false},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		if tt.accept != "" {
			req.Header.Set("Accept", tt.accept)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		has := strings.Contains(string(body), `# {trace_id="`+trace+`"}`)
		if has != tt.exemplar || !strings.Contains(string(body), "gw_request_duration_seconds_bucket") {
			t.Errorf("Accept %q (%s): exemplar served %v, want %v", tt.accept, resp.Header.Get("Content-Type"), has, tt.exemplar)
		}
	}
}
