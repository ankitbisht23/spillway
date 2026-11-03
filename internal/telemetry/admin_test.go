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
