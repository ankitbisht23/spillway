package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// Traces (OB-02, ADR-0030): a server span per request past the shedding
// gate, with child spans for API key resolution, each local rate limit
// phase, each Redis limiter call and each upstream attempt. W3C Trace
// Context comes in from the client and goes upstream from each attempt.
// Paths, queries, headers, keys and client addresses are never attributes.
// Unsampled requests get non-recording spans: their attributes are never
// built, and the child spans other than the attempts', which carry the
// trace context upstream, are not started at all.

// TracerName is the instrumentation scope of the gateway's spans.
const TracerName = "github.com/ankitbisht23/spillway/internal/proxy"

var traceContext propagation.TraceContext

// startServerSpan starts r's server span, continuing a valid traceparent
// the client sent (and its tracestate) or starting a new trace. The span is
// named once the route is known (endServerSpan).
func startServerSpan(tracer trace.Tracer, r *http.Request) (*http.Request, trace.Span) {
	ctx := traceContext.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := tracer.Start(ctx, "HTTP", trace.WithSpanKind(trace.SpanKindServer))
	return r.WithContext(ctx), span
}

// endServerSpan names and ends a request's server span once it has been
// answered: a 5xx is an error, with the status as its error.type.
func endServerSpan(span trace.Span, r *http.Request, q *telemetry.Request) {
	if !span.IsRecording() {
		span.End()
		return
	}
	span.SetName(telemetry.SpanName(q.Method, q.Route))
	span.SetAttributes(
		attribute.String("http.request.method", telemetry.SpanMethod(q.Method)),
		attribute.Int("http.response.status_code", q.Status),
		attribute.String("network.protocol.version", telemetry.ProtocolVersion(r)),
		attribute.String("spillway.request_id", q.ID),
		attribute.String("spillway.origin", q.Origin),
		attribute.String("spillway.reason", q.Reason),
		attribute.Int("spillway.attempts", q.Attempts),
	)
	if q.Route != telemetry.NoRoute {
		span.SetAttributes(attribute.String("http.route", q.Route))
	}
	if q.Upstream != "" {
		span.SetAttributes(attribute.String("spillway.upstream", q.Upstream))
	}
	if q.Status >= 500 {
		status := strconv.Itoa(q.Status)
		span.SetStatus(codes.Error, status)
		span.SetAttributes(attribute.String("error.type", status))
	}
	span.End()
}

// childSpan starts a span named name under the span in ctx, or returns nil
// when tracing is off or that span is not recording.
func childSpan(ctx context.Context, tracer trace.Tracer, name string, opts ...trace.SpanStartOption) trace.Span {
	if tracer == nil || !trace.SpanFromContext(ctx).IsRecording() {
		return nil
	}
	_, span := tracer.Start(ctx, name, opts...)
	return span
}

// authnResult is an authn span's spillway.authn.result.
func authnResult(c *config.Consumer, ok bool) string {
	switch {
	case !ok:
		return "invalid"
	case c == nil:
		return "anonymous"
	}
	return "consumer"
}

// traceLocal records a local rate limit phase that started at start and
// in which policies policies applied, as a span ending now (ADR-0030: one
// span per phase, since a span per policy would cost more than the checks).
func (l *limits) traceLocal(identity bool, start time.Time, policies int) {
	phase, decision := "pre_auth", "allow"
	if identity {
		phase = "post_auth"
	}
	if l.denied {
		decision = "deny"
	}
	span := childSpan(l.r.Context(), l.tracer, "ratelimit local", trace.WithTimestamp(start))
	if span == nil {
		return
	}
	span.SetAttributes(
		attribute.String("spillway.ratelimit.phase", phase),
		attribute.Int("spillway.ratelimit.policies", policies),
		attribute.String("spillway.ratelimit.decision", decision),
	)
	span.End()
}

// startRedisSpan starts the span of one Redis limiter call, or of its
// refund, to the hash slot kind slot: "tenant", "key" or "global".
func (l *limits) startRedisSpan(ctx context.Context, slot string, refund bool) trace.Span {
	span := childSpan(ctx, l.tracer, "ratelimit redis", trace.WithSpanKind(trace.SpanKindClient))
	if span != nil {
		span.SetAttributes(
			attribute.String("spillway.ratelimit.slot", slot),
			attribute.Bool("spillway.ratelimit.refund", refund),
		)
	}
	return span
}

// endRedisSpan ends a Redis call's span, nil when not traced, with its
// result: allow, deny, or the error's kind (timeout, conn, script).
func endRedisSpan(span trace.Span, allowed bool, err error) {
	if span == nil {
		return
	}
	result := "deny"
	switch {
	case err != nil:
		result, _ = errorKind(err)
		span.SetStatus(codes.Error, result)
	case allowed:
		result = "allow"
	}
	span.SetAttributes(attribute.String("spillway.ratelimit.result", result))
	span.End()
}

// startAttemptSpan starts upstream attempt n of a request to ep, and puts
// its trace context in the attempt's traceparent, replacing the client's,
// so that the upstream's spans nest under it. A retry notes the backoff it
// waited and why the attempt before it failed.
func (a *attempts) startAttemptSpan(ctx context.Context, req *http.Request, ep *endpoint, n int, backoff time.Duration, reason string) trace.Span {
	ctx, span := a.h.tracer.Start(ctx, "upstream attempt", trace.WithSpanKind(trace.SpanKindClient))
	// The client's tracestate goes on only with its trace (startServerSpan).
	req.Header.Del("Tracestate")
	traceContext.Inject(ctx, propagation.HeaderCarrier(req.Header))
	if !span.IsRecording() {
		return span
	}
	host, port, _ := net.SplitHostPort(ep.host)
	portN, _ := strconv.Atoi(port)
	span.SetAttributes(
		attribute.String("spillway.upstream", a.p.name),
		attribute.String("spillway.endpoint", ep.addr),
		attribute.String("server.address", host),
		attribute.Int("server.port", portN),
		attribute.Int("spillway.attempt", n),
		attribute.String("http.request.method", telemetry.SpanMethod(req.Method)),
	)
	if n > 1 {
		span.SetAttributes(
			attribute.Float64("spillway.backoff_ms", float64(backoff)/float64(time.Millisecond)),
			attribute.String("spillway.retry_reason", reason),
		)
	}
	return span
}

// endAttemptSpan ends an attempt's span with its status code, if it got a
// response, and as an error with errType if it failed (attemptOutcome).
func endAttemptSpan(span trace.Span, resp *http.Response, errType string) {
	if span.IsRecording() {
		if resp != nil {
			span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		}
		if errType != "" {
			span.SetStatus(codes.Error, errType)
			span.SetAttributes(attribute.String("error.type", errType))
		}
	}
	span.End()
}

// failureType names a failed attempt's outcome (ADR-0030, "Attempt
// outcomes"): its status for a response; for none, timeout when the
// attempt's deadline fired, connect_error when the connection could not be
// made (connect timeout included), and reset otherwise (reset or EOF before
// the response headers).
func failureType(ctx context.Context, resp *http.Response, err error) string {
	if err == nil {
		return strconv.Itoa(resp.StatusCode)
	}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return "timeout"
	}
	if oe, ok := errors.AsType[*net.OpError](err); ok && oe.Op == "dial" {
		return "connect_error"
	}
	return "reset"
}

// dropRetry notes on the request's server span why a retry it wanted was
// not sent: budget_exhausted, deadline, breaker or no_address.
func dropRetry(ctx context.Context, why string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attribute.String("spillway.retry_dropped", why))
	}
}
