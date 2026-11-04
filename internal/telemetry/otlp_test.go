package telemetry

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
)

// collector is a fake OTel Collector that keeps the log records it receives.
type collector struct {
	collogspb.UnimplementedLogsServiceServer
	mu   sync.Mutex
	logs []*logspb.ResourceLogs
}

func (c *collector) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, req.GetResourceLogs()...)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// TestOTLPLogHandler: records reach the Collector over plaintext gRPC with
// their attributes and the service's resource once the handler is shut
// down, which flushes the queue (OB-03).
func TestOTLPLogHandler(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	srv := grpc.NewServer()
	collogspb.RegisterLogsServiceServer(srv, c)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	h, shutdown, err := NewOTLPLogHandler(t.Context(), "http://"+ln.Addr().String(), "spillway-test", "v9")
	if err != nil {
		t.Fatal(err)
	}
	slog.New(h).Info("access", "request_id", "req-7", "status", 200)
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) != 1 || len(c.logs[0].GetScopeLogs()) != 1 || len(c.logs[0].GetScopeLogs()[0].GetLogRecords()) != 1 {
		t.Fatalf("collector got %v, want one record", c.logs)
	}
	resource := map[string]string{}
	for _, kv := range c.logs[0].GetResource().GetAttributes() {
		resource[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	if resource["service.name"] != "spillway-test" || resource["service.version"] != "v9" || resource["service.instance.id"] == "" {
		t.Errorf("resource = %v", resource)
	}
	r := c.logs[0].GetScopeLogs()[0].GetLogRecords()[0]
	attrs := map[string]*commonpb.AnyValue{}
	for _, kv := range r.GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue()
	}
	if r.GetBody().GetStringValue() != "access" || r.GetSeverityText() != "INFO" || attrs["request_id"].GetStringValue() != "req-7" || attrs["status"].GetIntValue() != 200 {
		t.Errorf("record: body %v, severity %q, attributes %v", r.GetBody(), r.GetSeverityText(), attrs)
	}
}
