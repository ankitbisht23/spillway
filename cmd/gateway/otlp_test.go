package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	"github.com/ankitbisht23/spillway/internal/proxy"
)

// collector is a fake OTel Collector that keeps the names of the spans and
// the bodies of the log records it receives.
type collector struct {
	collogspb.UnimplementedLogsServiceServer
	coltracepb.UnimplementedTraceServiceServer
	mu    sync.Mutex
	spans []string
	logs  []string
}

func (c *collector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				c.spans = append(c.spans, s.GetName())
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// logsService adapts collector to the logs service, whose Export method
// has another signature.
type logsService struct{ *collector }

func (l logsService) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				l.logs = append(l.logs, r.GetBody().GetStringValue())
			}
		}
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// TestTracesFlushedOnShutdown (OB-02, GW-23): with an OTLP endpoint, serve
// traces requests, and the shutdown's flush sends the spans and the log
// records still queued, which the exporters' 1 s batches have not.
func TestTracesFlushedOnShutdown(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, c)
	collogspb.RegisterLogsServiceServer(srv, logsService{c})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	u := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(u.Close)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: 1\nupstreams: [{ name: up, endpoints: [{ address: '" + u.Listener.Addr().String() + "' }] }]\n" +
		"routes: [{ name: all, match: { path_prefix: / }, upstream: up }]\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settings{
		configPath:     cfg,
		httpAddr:       freeAddr(t),
		adminAddr:      freeAddr(t),
		allow:          proxy.Allowlist{netip.MustParsePrefix("127.0.0.0/8")},
		dnsRefresh:     time.Minute,
		shutdownGrace:  time.Second,
		maxConnections: defaultMaxConnections,
		maxInFlight:    defaultMaxInFlight,

		accessLogSampleRate: 1,
		otlpEndpoint:        "http://" + ln.Addr().String(),
		traceSampleRatio:    1,
	}
	stdout := slog.New(slog.DiscardHandler)
	h, tracer, flush, err := otlpExport(t.Context(), s, stdout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, slog.New(h), tracer, flush) }()
	waitFor(t, "/readyz 200", func() bool { code, _ := get(t, "http://"+s.adminAddr+"/readyz"); return code == http.StatusOK })
	if code, _ := get(t, "http://"+s.httpAddr+"/x"); code != http.StatusOK {
		t.Fatalf("GET /x = %d, want 200", code)
	}
	cancel() // SIGTERM; the drain is 0
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	slices.Sort(c.spans)
	if want := []string{"GET all", "authn", "upstream attempt"}; !slices.Equal(c.spans, want) {
		t.Errorf("collector got spans %q, want %q", c.spans, want)
	}
	if !slices.Contains(c.logs, "access") {
		t.Errorf("collector got log records %q, want the access log line", c.logs)
	}
}
