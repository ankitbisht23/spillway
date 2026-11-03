package proxy

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/router"
)

// Handler routes each request on the current snapshot and proxies it to the
// route's upstream.
type Handler struct {
	snapshot func() *config.Snapshot
	pools    *Registry
	log      *slog.Logger
	errLog   *log.Logger
}

// NewHandler returns a Handler that reads the current snapshot from snapshot
// once per request (GW-21).
func NewHandler(snapshot func() *config.Snapshot, pools *Registry, log *slog.Logger) *Handler {
	return &Handler{snapshot: snapshot, pools: pools, log: log, errLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT is not supported", http.StatusMethodNotAllowed)
		return
	}
	// Route on the cleaned path and forward that same path, changed only by
	// the route's own prefix rewrite (GW-02), so the upstream never sees a
	// path that would have matched another route (ADR-0007).
	path := router.CleanPath(r.URL.Path)
	route, _, ok := h.snapshot().Match(r.Host, r.Method, path, r.Header)
	if !ok {
		http.Error(w, "no route", http.StatusNotFound)
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
	addr, err := p.pick()
	if err != nil {
		h.log.Warn("no upstream address", "route", route.Name, "upstream", p.name)
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
		Transport: p.transport,
		// Hop-by-hop headers, and any X-Forwarded-* the client sent, are
		// already removed from pr.Out when Rewrite runs (SEC-07).
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = addr
			pr.Out.URL.User = nil // an absolute-form URL's userinfo would become an Authorization header
			pr.Out.URL.Path = upstreamPath
			pr.Out.URL.RawPath = ""
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Forwarded")
			pr.SetXForwarded()
			route.RequestHeaders.Apply(pr.Out.Header)
		},
		// Hop-by-hop headers are already removed from resp, except on a 101.
		ModifyResponse: func(resp *http.Response) error {
			route.ResponseHeaders.Apply(resp.Header)
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
		ErrorHandler: h.proxyError,
		ErrorLog:     h.errLog,
	}
	rp.ServeHTTP(w, r)
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
// gets nothing.
func (h *Handler) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	cause := context.Cause(r.Context())
	if errors.Is(cause, context.Canceled) {
		return
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	status := http.StatusBadGateway
	var ne net.Error
	if cause != nil || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		status = http.StatusGatewayTimeout // the route's deadline, idle timeout or connect timeout
	}
	level := slog.LevelWarn
	if errors.Is(err, ErrNotAllowed) {
		level = slog.LevelError // the dial-time check caught what resolution let through
	}
	h.log.Log(r.Context(), level, "upstream request failed", "status", status, "err", err)
	http.Error(w, http.StatusText(status), status)
}
