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
	// Route on the cleaned path and forward that same path, so the upstream
	// never sees a path that would have matched another route (ADR-0007).
	path := router.CleanPath(r.URL.Path)
	route, _, ok := h.snapshot().Match(r.Host, r.Method, path, r.Header)
	if !ok {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	p := h.pools.pool(route.Upstream)
	addr, err := p.pick()
	if err != nil {
		h.log.Warn("no upstream address", "route", route.Name, "upstream", p.name)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	rp := &httputil.ReverseProxy{
		Transport: p.transport,
		// Hop-by-hop headers, and any X-Forwarded-* the client sent, are
		// already removed from pr.Out when Rewrite runs (SEC-07).
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = addr
			pr.Out.URL.User = nil // an absolute-form URL's userinfo would become an Authorization header
			pr.Out.URL.Path = path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Forwarded")
			pr.SetXForwarded()
		},
		ErrorHandler: h.proxyError,
		ErrorLog:     h.errLog,
	}
	rp.ServeHTTP(w, r)
}

// proxyError answers a failed upstream request: 504 for a timeout, 502
// otherwise. A client that went away gets nothing.
func (h *Handler) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}
	status := http.StatusBadGateway
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		status = http.StatusGatewayTimeout
	}
	level := slog.LevelWarn
	if errors.Is(err, ErrNotAllowed) {
		level = slog.LevelError // the dial-time check caught what resolution let through
	}
	h.log.Log(r.Context(), level, "upstream request failed", "status", status, "err", err)
	http.Error(w, http.StatusText(status), status)
}
