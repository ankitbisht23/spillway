package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Label values with a fixed meaning (OB-01).
const (
	NoRoute        = "none"     // route of a request no route matched
	NoTenant       = "none"     // tenant of a request without one; every request until M5
	OriginGateway  = "gateway"  // the gateway answered the request itself
	OriginUpstream = "upstream" // the upstream's response was passed on
	ReasonNone     = "none"
	ReasonUpstream = "upstream" // the gateway answered because the upstream failed or has no address
)

// maxTenants is how many tenants keep their own tenant label (OB-01).
const maxTenants = 50

// durationBuckets, in seconds, start well below the 2 ms added-latency target
// (NFR-03) and end at the 59 s longest route timeout (GW-11).
var durationBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// Request is what the access log and the metrics record about a request once
// it has been answered.
type Request struct {
	ID       string // X-Request-Id (GW-22)
	TraceID  string // from a valid traceparent, else ""
	Route    string // the matched route's name, or NoRoute
	Method   string
	Status   int
	Origin   string // OriginGateway or OriginUpstream
	Reason   string // why the gateway answered itself: ReasonNone or ReasonUpstream
	Tenant   string // NoTenant until tenants exist (M5)
	Upstream string // "" if no upstream was chosen
	Latency  time.Duration
	BytesIn  int64 // request body bytes read from the client
	BytesOut int64 // response body bytes written to the client
}

// Recorder writes the access log (OB-03) and the request metrics (OB-01).
type Recorder struct {
	log        *slog.Logger
	sampleRate float64
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	upstream   *prometheus.HistogramVec

	mu      sync.Mutex // a tenant rotation never runs between labelling and observing
	tenants *tenantCapper
}

// NewRecorder registers the request metrics with reg. The access log goes to
// log and keeps sampleRate (0 to 1) of the requests it may sample.
func NewRecorder(reg prometheus.Registerer, log *slog.Logger, sampleRate float64) *Recorder {
	rec := &Recorder{
		log:        log,
		sampleRate: sampleRate,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gw_requests_total",
			Help: "Requests answered, by who answered (origin) and why the gateway answered itself (reason).",
		}, []string{"route", "method", "code_class", "origin", "reason", "tenant"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gw_request_duration_seconds",
			Help:    "Time from receiving a request to the end of its response.",
			Buckets: durationBuckets,
		}, []string{"route", "tenant"}),
		upstream: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gw_upstream_duration_seconds",
			Help:    "Time from sending an upstream attempt to its response headers or its failure.",
			Buckets: durationBuckets,
		}, []string{"upstream", "attempt"}),
		tenants: newTenantCapper(maxTenants),
	}
	reg.MustRegister(rec.requests, rec.duration, rec.upstream)
	return rec
}

// noDecisions stands in for the rate-limit decisions that M2 adds (RL-27).
var noDecisions = slog.Any("decisions", []string{})

// Request records an answered request in the metrics, and in the access log
// if it is a 5xx, a 4xx the gateway answered itself, or sampled.
func (rec *Recorder) Request(ctx context.Context, q *Request) {
	rec.observe(q)
	always := q.Status >= 500 || q.Origin == OriginGateway && q.Status >= 400
	if !always && !sampled(q.ID, rec.sampleRate) {
		return
	}
	rec.log.LogAttrs(ctx, slog.LevelInfo, "access",
		slog.String("request_id", q.ID),
		slog.String("trace_id", q.TraceID),
		slog.String("route", q.Route),
		slog.String("method", q.Method),
		slog.Int("status", q.Status),
		slog.Float64("latency_ms", float64(q.Latency.Microseconds())/1000),
		slog.Int64("bytes_in", q.BytesIn),
		slog.Int64("bytes_out", q.BytesOut),
		slog.String("upstream", q.Upstream),
		noDecisions,
		slog.String("key_hash", ""), // API keys arrive in M2 (SEC-03)
	)
}

func (rec *Recorder) observe(q *Request) {
	tenant := q.Tenant
	if tenant != NoTenant {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		tenant = rec.tenants.label(tenant)
	}
	rec.requests.WithLabelValues(q.Route, methodLabel(q.Method), codeClass(q.Status), q.Origin, q.Reason, tenant).Inc()
	rec.duration.WithLabelValues(q.Route, tenant).Observe(q.Latency.Seconds())
}

// UpstreamAttempt records how long one attempt at an upstream took.
func (rec *Recorder) UpstreamAttempt(upstream string, attempt int, d time.Duration) {
	rec.upstream.WithLabelValues(upstream, strconv.Itoa(attempt)).Observe(d.Seconds())
}

// Run ranks the tenants every interval until ctx ends, and deletes the series
// of those that lost their own label (OB-01).
func (rec *Recorder) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rec.rotateTenants()
		}
	}
}

func (rec *Recorder) rotateTenants() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, t := range rec.tenants.rotate() {
		rec.requests.DeletePartialMatch(prometheus.Labels{"tenant": t})
		rec.duration.DeletePartialMatch(prometheus.Labels{"tenant": t})
	}
}

// methodLabel collapses every method but RFC 9110's eight and PATCH into
// "other", so clients can't add label values.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m
	}
	return "other"
}

var codeClasses = [...]string{"1xx", "2xx", "3xx", "4xx", "5xx"}

// codeClass is "2xx" for 200 to 299 and so on; codes an upstream sends above
// 599 are "other".
func codeClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return codeClasses[status/100-1]
}

// sampled reports whether a request ID falls within the sampled share of
// IDs. It depends only on the ID, so every gateway that sees an ID decides
// alike.
func sampled(id string, rate float64) bool {
	if rate >= 1 {
		return true
	}
	sum := sha256.Sum256([]byte(id))
	return float64(binary.BigEndian.Uint64(sum[:])>>11)/(1<<53) < rate
}
