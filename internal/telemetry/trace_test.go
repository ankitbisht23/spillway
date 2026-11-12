package telemetry

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

// traceCollector is a fake OTel Collector that keeps the spans it receives.
type traceCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	mu    sync.Mutex
	spans []*tracepb.ResourceSpans
}

func (c *traceCollector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = append(c.spans, req.GetResourceSpans()...)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// names returns the names of the spans received, and the resource of the
// last batch.
func (c *traceCollector) names() ([]string, map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	resource := map[string]string{}
	for _, rs := range c.spans {
		for _, kv := range rs.GetResource().GetAttributes() {
			resource[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				names = append(names, s.GetName())
			}
		}
	}
	return names, resource
}

// tracerProvider starts a fake Collector and a provider sampling at ratio
// that exports to it.
func tracerProvider(t *testing.T, ratio float64) (trace.Tracer, func(context.Context) error, *traceCollector) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &traceCollector{}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, c)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	tp, shutdown, err := NewTracerProvider(t.Context(), "http://"+ln.Addr().String(), "spillway-test", "v9", ratio)
	if err != nil {
		t.Fatal(err)
	}
	return tp.Tracer("test"), shutdown, c
}

// TestTracerProvider: spans reach the Collector over plaintext gRPC with
// the service's resource once the provider is shut down, which flushes the
// queue. A trace that arrives with a decision keeps it, whatever the ratio
// (OB-02).
func TestTracerProvider(t *testing.T) {
	for _, tt := range []struct {
		name  string
		ratio float64
		flag  string // the client's sampled flag, "" for no traceparent
		want  bool
	}{
		{"ratio 1", 1, "", true},
		{"ratio 0", 0, "", false},
		{"ratio 0, sampled parent", 0, "01", true},
		{"ratio 1, unsampled parent", 1, "00", false},
	} {
		tracer, shutdown, c := tracerProvider(t, tt.ratio)
		ctx := t.Context()
		if tt.flag != "" {
			ctx = traceContext(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-"+tt.flag)
		}
		_, span := tracer.Start(ctx, "GET r")
		span.End()
		if err := shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		names, resource := c.names()
		if got := len(names) == 1 && names[0] == "GET r"; got != tt.want {
			t.Errorf("%s: collector got %v, want the span: %v", tt.name, names, tt.want)
		}
		if tt.want && (resource["service.name"] != "spillway-test" || resource["service.version"] != "v9" || resource["service.instance.id"] == "") {
			t.Errorf("%s: resource = %v", tt.name, resource)
		}
	}
}

// traceContext is a context with traceparent's span as a remote parent.
func traceContext(t *testing.T, traceparent string) context.Context {
	t.Helper()
	ctx := propagation.TraceContext{}.Extract(t.Context(), propagation.HeaderCarrier(http.Header{"Traceparent": {traceparent}}))
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatalf("traceparent %q is not valid", traceparent)
	}
	return ctx
}

// TestTraceSampleRatio: traces that arrive without a decision are sampled
// at the ratio (OB-02, D6): 25% of 2000 root spans, within 5 standard
// deviations (about 19 spans each).
func TestTraceSampleRatio(t *testing.T) {
	tracer, shutdown, c := tracerProvider(t, 0.25)
	for range 2000 {
		_, span := tracer.Start(t.Context(), "GET r")
		span.End()
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if names, _ := c.names(); len(names) < 403 || len(names) > 597 {
		t.Errorf("%d of 2000 traces sampled at 0.25, want 500 ± 97", len(names))
	}
}

// TestSpanNames: server span names and method attributes stay within the
// metrics' method list (ADR-0030).
func TestSpanNames(t *testing.T) {
	for _, tt := range []struct{ method, route, name, attr string }{
		{"GET", "orders", "GET orders", "GET"},
		{"PATCH", NoRoute, "PATCH", "PATCH"},
		{"PURGE", "orders", "HTTP orders", "_OTHER"},
		{"get", NoRoute, "HTTP", "_OTHER"},
	} {
		if got := SpanName(tt.method, tt.route); got != tt.name {
			t.Errorf("SpanName(%q, %q) = %q, want %q", tt.method, tt.route, got, tt.name)
		}
		if got := SpanMethod(tt.method); got != tt.attr {
			t.Errorf("SpanMethod(%q) = %q, want %q", tt.method, got, tt.attr)
		}
	}
	for _, tt := range []struct {
		major, minor int
		want         string
	}{{1, 0, "1.0"}, {1, 1, "1.1"}, {2, 0, "2"}} {
		if got := ProtocolVersion(&http.Request{ProtoMajor: tt.major, ProtoMinor: tt.minor}); got != tt.want {
			t.Errorf("ProtocolVersion(HTTP/%d.%d) = %q, want %q", tt.major, tt.minor, got, tt.want)
		}
	}
}
