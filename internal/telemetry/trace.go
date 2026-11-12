package telemetry

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// The span processor's limits (ADR-0030): spans are exported every second
// in batches of up to 512, from a queue of 2048 that drops spans when it is
// full, so tracing never holds up a request.
const (
	traceBatchTimeout = time.Second
	traceBatchSize    = 512
	traceQueueSize    = 2048
)

// NewTracerProvider returns a tracer provider that sends spans over
// OTLP/gRPC to endpoint, the OTel Collector, which forwards them to Tempo
// (OB-02), and a function that flushes what is queued and stops it.
// endpoint is a URL, as for NewOTLPLogHandler. A trace that arrives with a
// sampling decision keeps it; any other is sampled with probability ratio,
// by its trace ID.
func NewTracerProvider(ctx context.Context, endpoint, service, version string, ratio float64) (trace.TracerProvider, func(context.Context) error, error) {
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(newResource(service, version)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(traceBatchTimeout),
			sdktrace.WithMaxExportBatchSize(traceBatchSize),
			sdktrace.WithMaxQueueSize(traceQueueSize),
		),
	)
	return tp, tp.Shutdown, nil
}

// SpanName is the name of a request's server span (ADR-0030): its method and
// route, "GET default", or the method alone when no route matched. Methods
// outside the metrics' list are "HTTP", so clients can't add span names.
func SpanName(method, route string) string {
	m := methodLabel(method)
	if m == "other" {
		m = "HTTP"
	}
	if route == NoRoute {
		return m
	}
	return m + " " + route
}

// SpanMethod is a request's http.request.method span attribute: the method,
// or "_OTHER" outside the metrics' list, as OTel's HTTP conventions name it.
func SpanMethod(method string) string {
	if m := methodLabel(method); m != "other" {
		return m
	}
	return "_OTHER"
}

// ProtocolVersion is a request's network.protocol.version span attribute:
// "1.0", "1.1" or "2".
func ProtocolVersion(r *http.Request) string {
	switch {
	case r.ProtoMajor == 2:
		return "2"
	case r.ProtoMinor == 0:
		return "1.0"
	}
	return "1.1"
}
