package proxy

import (
	"log/slog"
	"net/http"
	"time"
)

// Server limits (GW-28), so slow or oversized clients cannot pin connections.
// Per-route timeouts (GW-11) have to fit within writeTimeout.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second // whole request, body included
	writeTimeout      = 60 * time.Second // from the end of the headers to the end of the response
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10 // larger request headers get 431
)

// NewServer returns the traffic server for h.
func NewServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
