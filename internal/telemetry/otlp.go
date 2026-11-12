package telemetry

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// NewOTLPLogHandler returns an slog handler that sends records over OTLP/gRPC
// to endpoint, the OTel Collector, which forwards them to Loki (OB-03), and a
// function that flushes what is queued and stops it. endpoint is a URL:
// http:// sends in plaintext, https:// over TLS. Records are batched and
// exported in the background; when the queue is full they are dropped, so
// logging never waits for the Collector.
func NewOTLPLogHandler(ctx context.Context, endpoint, service, version string) (slog.Handler, func(context.Context) error, error) {
	exp, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(newResource(service, version)),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	return otelslog.NewHandler(service, otelslog.WithLoggerProvider(lp)), lp.Shutdown, nil
}

// newResource describes the process in the logs and traces it exports.
func newResource(service, version string) *resource.Resource {
	host, _ := os.Hostname() // tells replicas apart in Loki and Tempo
	return resource.NewSchemaless(
		attribute.String("service.name", service),
		attribute.String("service.version", version),
		attribute.String("service.instance.id", host),
	)
}
