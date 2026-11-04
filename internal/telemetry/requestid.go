package telemetry

import (
	"context"
	"crypto/rand"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader carries a request's ID to the upstream and back to the
// client (GW-22).
const RequestIDHeader = "X-Request-Id"

// RequestID returns the request ID the client sent if it sent exactly one of
// 1 to 128 characters from [A-Za-z0-9._-], and a new random one otherwise
// (GW-22).
func RequestID(sent []string) string {
	if len(sent) == 1 && validRequestID(sent[0]) {
		return sent[0]
	}
	return rand.Text() // 26 base32 characters, 128 random bits
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range []byte(id) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// TraceID returns the trace ID of a valid W3C traceparent in h, or "" (GW-22).
// The gateway forwards traceparent unchanged; its own spans arrive with OB-02.
func TraceID(h http.Header) string {
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h)))
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
