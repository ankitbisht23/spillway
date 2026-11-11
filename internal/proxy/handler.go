package proxy

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/router"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// Handler routes each request on the current snapshot and proxies it to the
// route's upstream, and records every request it answers.
type Handler struct {
	snapshot func() *config.Snapshot
	identity func() *config.Identity
	trusted  TrustedProxies
	pools    *Registry
	limiters *local.Registry
	redis    Redis
	devMode  bool // Spillway-Decision-Time on Redis-decided responses (ADR-0021)
	rec      *telemetry.Recorder
	log      *slog.Logger
	errLog   *log.Logger
}

// NewHandler returns a Handler that reads the current snapshot from snapshot
// and the current identity from identity once per request (GW-21, GW-17),
// takes the client IP from X-Forwarded-For only behind trusted proxies
// (SEC-05), checks the snapshot's local rate limit policies with the
// limiters in limiters (ADR-0017) and its Redis policies with redis, nil if
// the gateway has no Redis (ADR-0021), and records requests with rec (OB-01,
// OB-03). With devMode, a response decided in Redis carries the decision's
// Redis time for the accuracy harness (ADR-0021).
func NewHandler(snapshot func() *config.Snapshot, identity func() *config.Identity, trusted TrustedProxies, pools *Registry, limiters *local.Registry, redis Redis, devMode bool, rec *telemetry.Recorder, log *slog.Logger) *Handler {
	return &Handler{snapshot: snapshot, identity: identity, trusted: trusted, pools: pools, limiters: limiters, redis: redis, devMode: devMode, rec: rec, log: log, errLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
}

// statusClientClosed is recorded for a request whose client went away before
// it was answered, as nginx does.
const statusClientClosed = 499

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := telemetry.Request{
		ID:      telemetry.RequestID(r.Header.Values(telemetry.RequestIDHeader)),
		TraceID: telemetry.TraceID(r.Header), // traceparent itself goes upstream unchanged (GW-22)
		Route:   telemetry.NoRoute,
		Method:  r.Method,
		Origin:  telemetry.OriginGateway,
		Reason:  telemetry.ReasonNone,
		Tenant:  telemetry.NoTenant, // tenants arrive in M5
	}
	rw := &responseRecorder{ResponseWriter: w, requestID: q.ID}
	body := &countingBody{ReadCloser: r.Body}
	r.Body = body
	// Deferred, so that a response cut off mid-body, which ends the handler
	// with a panic, is recorded too. Only a client that went away gets no
	// answer (proxyError).
	defer func() {
		q.Status = cmp.Or(rw.status, statusClientClosed)
		q.Latency, q.BytesIn, q.BytesOut = time.Since(start), body.n.Load(), rw.bytes
		h.rec.Request(r.Context(), &q)
	}()
	h.serve(rw, r, &q)
}

// serve answers r and notes in q what the access log and metrics need.
func (h *Handler) serve(w *responseRecorder, r *http.Request, q *telemetry.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT is not supported", http.StatusMethodNotAllowed)
		return
	}
	// Go's HTTP/2 server lets a raw space into the query, where it would
	// split the upstream's HTTP/1.1 request line; over HTTP/1.1 the same
	// target is already a 400.
	if strings.Contains(r.URL.RawQuery, " ") {
		http.Error(w, "invalid request target", http.StatusBadRequest)
		return
	}
	// Route on the cleaned path and forward that same path, changed only by
	// the route's own prefix rewrite (GW-02), so the upstream never sees a
	// path that would have matched another route (ADR-0007).
	path := router.CleanPath(r.URL.Path)
	s := h.snapshot()
	route, _, ok := s.Match(r.Host, r.Method, path, r.Header)
	if !ok {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	q.Route = route.Name

	// Rate limits (ADR-0017, ADR-0021): the local policies that need no
	// identity, then API key resolution, then the local policies that use
	// the key's identity, then the Redis policies. They run before the size
	// limits, so oversized requests count too. A request denied in the first
	// phase is not authenticated.
	peer, _ := netip.ParseAddrPort(r.RemoteAddr)
	lim := newLimits(s, route, h.limiters, h.redis, h.rec, h.log, r, q, h.trusted.ClientIP(peer.Addr(), r.Header.Values("X-Forwarded-For")))
	defer lim.release()
	allowed := lim.check(false)
	if allowed {
		consumer, ok := h.authenticate(r.Header, q)
		if !ok {
			w.rateLimitPolicy, w.rateLimit = lim.headers() // the first phase's deductions stay
			jsonError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		lim.identify(consumer, r.Header.Get("X-API-Key"))
		allowed = lim.check(true) && lim.checkRedis()
	}
	w.rateLimitPolicy, w.rateLimit = lim.headers()
	if h.devMode {
		w.decisionTime = lim.decisionTime()
	}
	if lim.unavailable {
		// Redis failed for an enforce fail_closed policy (RL-23); Retry-After
		// is the shortest whole number of seconds, longer than the health
		// checks take to find a node back (ADR-0021, ADR-0025).
		q.Reason = telemetry.ReasonFailClosed
		w.Header().Set("Retry-After", "1")
		jsonError(w, http.StatusServiceUnavailable, "rate limit unavailable")
		return
	}
	if !allowed {
		q.Reason = telemetry.ReasonRateLimited
		w.Header().Set("Retry-After", strconv.FormatInt(lim.retryAfter(), 10))
		jsonError(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	// Size limits (GW-03), before anything reaches the upstream.
	if route.MaxHeaderBytes > 0 && headerBytes(r) > route.MaxHeaderBytes {
		http.Error(w, http.StatusText(http.StatusRequestHeaderFieldsTooLarge), http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	if route.MaxBodyBytes > 0 {
		if r.ContentLength > route.MaxBodyBytes {
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return
		}
		// For bodies of unknown length; proxyError answers 413. The
		// transport reads the body on its own goroutine, which must not
		// touch w, so w is not passed.
		r.Body = http.MaxBytesReader(nil, r.Body, route.MaxBodyBytes)
	}
	p := h.pools.pool(route.Upstream)
	q.Upstream = p.name
	addr, err := p.pick()
	if err != nil {
		q.Reason = telemetry.ReasonUpstream
		h.log.Warn("no upstream address", "route", route.Name, "upstream", p.name, "request_id", q.ID)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	// Timeouts (GW-11). Until retries arrive (GW-12, M4) the one attempt
	// spans the whole request, so per_try and total bound the same span.
	t := route.Timeouts
	ctx, cancel := context.WithTimeout(r.Context(), min(t.PerTry, t.Total))
	defer cancel()
	ctx = context.WithValue(ctx, connectTimeoutKey{}, t.Connect)
	var idle *idleTimer
	if t.Idle > 0 {
		ctx, idle = withIdleTimeout(ctx, t.Idle)
		defer idle.Stop()
		r.Body = idleBody{r.Body, idle}
	}
	r = r.WithContext(ctx)

	upstreamPath := route.UpstreamPath(path)
	rp := &httputil.ReverseProxy{
		Transport: roundTripFunc(func(out *http.Request) (*http.Response, error) {
			start := time.Now()
			resp, err := p.transport.RoundTrip(out)
			h.rec.UpstreamAttempt(p.name, 1, time.Since(start)) // one attempt until retries (GW-12, M4)
			return resp, err
		}),
		// Hop-by-hop headers are already removed from pr.Out when Rewrite
		// runs, and so are X-Forwarded-For, -Host and -Proto, but not the
		// other X-Forwarded-* the client sent (SEC-07).
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = addr
			pr.Out.URL.User = nil // an absolute-form URL's userinfo would become an Authorization header
			pr.Out.URL.Path = upstreamPath
			pr.Out.URL.RawPath = ""
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Forwarded")
			for name := range pr.Out.Header {
				if strings.HasPrefix(name, "X-Forwarded-") {
					delete(pr.Out.Header, name)
				}
			}
			pr.SetXForwarded()
			// Upstreams speak HTTP/1.1 only (ADR-0008): an h2c upgrade would
			// open an HTTP/2 tunnel whose requests skip routing, so it is
			// ignored, as RFC 9110 §7.8 lets a server do.
			for p := range strings.SplitSeq(pr.Out.Header.Get("Upgrade"), ",") {
				if strings.EqualFold(strings.TrimSpace(p), "h2c") {
					pr.Out.Header.Del("Connection")
					pr.Out.Header.Del("Upgrade")
					break
				}
			}
			pr.Out.Header.Set(telemetry.RequestIDHeader, q.ID) // GW-22
			route.RequestHeaders.Apply(pr.Out.Header)
		},
		// Hop-by-hop headers are already removed from resp, except on a 101.
		ModifyResponse: func(resp *http.Response) error {
			q.Origin = telemetry.OriginUpstream
			route.ResponseHeaders.Apply(resp.Header)
			// Replaces any the upstream sent. responseRecorder sets it too,
			// but a 101 is written without WriteHeader (GW-22).
			resp.Header.Set(telemetry.RequestIDHeader, q.ID)
			w.setRateLimit(resp.Header)
			if route.HSTS != "" && r.TLS != nil {
				resp.Header.Set("Strict-Transport-Security", route.HSTS) // SEC-01; replaces the upstream's
			}
			if idle != nil {
				if resp.StatusCode == http.StatusSwitchingProtocols {
					idle.Stop() // an upgraded connection's body must stay writable; total still bounds it
				} else {
					resp.Body = idleBody{resp.Body, idle}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) { h.proxyError(w, r, err, q) },
		ErrorLog:     h.errLog,
	}
	rp.ServeHTTP(w, r)
}

// authenticate resolves a request's API key (GW-17, SEC-03), sent in
// X-API-Key on every route until per-route auth modes (GW-20, M5). No key is
// anonymous: a nil consumer and true. One key that resolves gives its
// consumer; anything else, an empty or unknown key or several values, is
// false. It notes the key's hash, never the key, in q for the access log.
func (h *Handler) authenticate(header http.Header, q *telemetry.Request) (*config.Consumer, bool) {
	keys := header.Values("X-API-Key")
	if len(keys) == 0 {
		return nil, true
	}
	q.KeyHash = config.KeyHash(keys[0])
	if len(keys) != 1 {
		return nil, false
	}
	return h.identity().Lookup(keys[0])
}

// jsonError answers with the gateway's JSON error body, {"error": msg}
// (ADR-0017); msg must not need escaping.
func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `{"error":"`+msg+`"}`)
}

// headerBytes is the size of r's header fields as HTTP/1.1 sends them,
// "Name: value\r\n" each, Host included (GW-03).
func headerBytes(r *http.Request) int {
	n := len("Host: \r\n") + len(r.Host)
	for name, values := range r.Header {
		for _, v := range values {
			n += len(name) + len(v) + len(": \r\n")
		}
	}
	return n
}

// proxyError answers a failed upstream request: 413 for a body over the
// route's limit, 504 for a timeout, 502 otherwise. A client that went away
// gets nothing. It notes in q that the gateway answered, and why.
func (h *Handler) proxyError(w http.ResponseWriter, r *http.Request, err error, q *telemetry.Request) {
	q.Origin, q.Reason = telemetry.OriginGateway, telemetry.ReasonNone
	cause := context.Cause(r.Context())
	if errors.Is(cause, context.Canceled) {
		return
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	q.Reason = telemetry.ReasonUpstream
	status := http.StatusBadGateway
	var ne net.Error
	if cause != nil || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		status = http.StatusGatewayTimeout // the route's deadline, idle timeout or connect timeout
	}
	level := slog.LevelWarn
	if errors.Is(err, ErrNotAllowed) {
		level = slog.LevelError // the dial-time check caught what resolution let through
	}
	h.log.Log(r.Context(), level, "upstream request failed", "status", status, "err", err, "request_id", q.ID)
	http.Error(w, http.StatusText(status), status)
}
