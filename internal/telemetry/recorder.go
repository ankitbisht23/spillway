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
	// ReasonRateLimited: the gateway answered 429 because a rate limit
	// denied the request (RL-25).
	ReasonRateLimited = "ratelimited"
	// ReasonFailClosed: the gateway answered 503 because Redis failed for
	// an enforce-mode fail_closed policy (RL-23).
	ReasonFailClosed = "fail_closed"

	BackendLocal = "local" // rate limit checks in the gateway's memory
	BackendRedis = "redis" // rate limit checks on Redis Cluster (ADR-0021)
)

// redisErrorKinds are the label values of gw_redis_errors_total{kind}: the
// limiter's error kinds (ADR-0021).
var redisErrorKinds = []string{"timeout", "conn", "script"}

// maxTenants is how many tenants keep their own tenant label (OB-01).
const maxTenants = 50

// durationBuckets, in seconds, start well below the 2 ms added-latency target
// (NFR-03) and end at the 59 s longest route timeout (GW-11).
var durationBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// checkBuckets, in seconds, run from 10 µs, for local checks, to 50 ms, past
// M3's 5 ms Redis budget (ADR-0017); 7 ms is the budget plus the chaos
// tests' 2 ms slack (ADR-0021).
var checkBuckets = []float64{.00001, .000025, .00005, .0001, .00025, .0005, .001, .0025, .005, .007, .01, .025, .05}

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
	Attempts int    // upstream attempts made, 0 if none (GW-12)
	KeyHash  string // config.KeyHash of the API key presented, "" if none (SEC-03)
	// Decisions has one entry per rate limit policy that applied to the
	// request, in evaluation order (RL-27).
	Decisions []Decision
	Latency   time.Duration
	BytesIn   int64 // request body bytes read from the client
	BytesOut  int64 // response body bytes written to the client
}

// Decision is one policy's rate limit decision for a request (RL-27).
type Decision struct {
	Policy   string `json:"policy"`
	Decision string `json:"decision"` // "allow", or "deny" if any of the policy's limits denied
	Mode     string `json:"mode"`     // "enforce" or "shadow"
}

// Recorder writes the access log (OB-03) and the request metrics (OB-01).
type Recorder struct {
	log        *slog.Logger
	sampleRate float64
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	upstream   *prometheus.HistogramVec
	retries    *prometheus.CounterVec
	decisions  *prometheus.CounterVec
	checks     *prometheus.HistogramVec
	redis      *prometheus.CounterVec
	refunds    prometheus.Counter
	keyCap     *prometheus.CounterVec

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
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gw_retries_total",
			Help: "Retries an upstream's retry budget granted (sent) or refused (budget_exhausted).",
		}, []string{"upstream", "outcome"}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gw_ratelimit_decisions_total",
			Help: "Rate limit decisions, one per applying policy per request; a shadow would-deny is decision deny, mode shadow.",
		}, []string{"policy", "decision", "mode"}),
		checks: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gw_ratelimit_check_duration_seconds",
			Help:    "Time to evaluate a request's rate limit policies: per local phase, refunds included, or all of its Redis calls, failure modes included.",
			Buckets: checkBuckets,
		}, []string{"backend"}),
		redis: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gw_redis_errors_total",
			Help: "Redis limiter calls that failed, refunds included, by kind: timeout (incl. calls the request's budget left no time for), conn (incl. calls an open breaker refused) or script.",
		}, []string{"kind"}),
		refunds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gw_ratelimit_refund_failures_total",
			Help: "Redis refunds that failed; each leaves at most a request's cost held until the limit's state decays.",
		}),
		keyCap: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gw_ratelimit_key_cap_skips_total",
			Help: "Limits of in-memory limiters skipped for a request because its key was new and the limiter held its cap of keys.",
		}, []string{"policy"}),
		tenants: newTenantCapper(maxTenants),
	}
	for _, k := range redisErrorKinds {
		rec.redis.WithLabelValues(k) // every kind is exported from the start, at 0
	}
	reg.MustRegister(rec.requests, rec.duration, rec.upstream, rec.retries, rec.decisions, rec.checks, rec.redis, rec.refunds, rec.keyCap)
	return rec
}

// Request records an answered request in the metrics, and in the access log
// if it is a 5xx, a 4xx the gateway answered itself, or sampled.
func (rec *Recorder) Request(ctx context.Context, q *Request) {
	rec.observe(q)
	always := q.Status >= 500 || q.Origin == OriginGateway && q.Status >= 400
	if !always && !sampled(q.ID, rec.sampleRate) {
		return
	}
	decisions := q.Decisions
	if decisions == nil {
		decisions = []Decision{} // [], not null
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
		slog.Int("attempts", q.Attempts),
		slog.Any("decisions", decisions),
		slog.String("key_hash", q.KeyHash),
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
	for _, d := range q.Decisions {
		rec.decisions.WithLabelValues(d.Policy, d.Decision, d.Mode).Inc()
	}
}

// RateLimitCheck records how long one phase of a request's rate limit checks
// took on backend (ADR-0017), or all of its Redis calls (ADR-0021).
func (rec *Recorder) RateLimitCheck(backend string, d time.Duration) {
	rec.checks.WithLabelValues(backend).Observe(d.Seconds())
}

// RedisError records a failed Redis limiter call of kind (ADR-0021).
func (rec *Recorder) RedisError(kind string) {
	rec.redis.WithLabelValues(kind).Inc()
}

// RefundFailure records a Redis refund that failed (ADR-0021).
func (rec *Recorder) RefundFailure() {
	rec.refunds.Inc()
}

// KeyCapSkip records a limit of policy that a request skipped because its
// in-memory limiter was full (ADR-0021, U1).
func (rec *Recorder) KeyCapSkip(policy string) {
	rec.keyCap.WithLabelValues(policy).Inc()
}

// UpstreamAttempt records how long one attempt at an upstream took.
func (rec *Recorder) UpstreamAttempt(upstream string, attempt int, d time.Duration) {
	rec.upstream.WithLabelValues(upstream, strconv.Itoa(attempt)).Observe(d.Seconds())
}

// Retry records a retry the upstream's budget granted (sent) or refused
// (GW-13).
func (rec *Recorder) Retry(upstream string, sent bool) {
	outcome := "budget_exhausted"
	if sent {
		outcome = "sent"
	}
	rec.retries.WithLabelValues(upstream, outcome).Inc()
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
