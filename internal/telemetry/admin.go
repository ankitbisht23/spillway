// Package telemetry serves the admin port: health, readiness, Prometheus
// metrics and pprof (GW-24, GW-25, OB-07).
package telemetry

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry returns a metrics registry with the Go runtime and process
// collectors (OB-07).
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// NewAdminServer returns the admin server: /healthz answers 200 while the
// process runs, /readyz 200 only while ready returns true, /metrics serves g,
// and pprof is under /debug/pprof/. It has no write timeout, so CPU profiles
// and traces can run as long as asked.
func NewAdminServer(g prometheus.Gatherer, ready func() bool, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
	mux.HandleFunc("/debug/pprof/", pprof.Index) // also the named profiles: heap, goroutine, ...
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
