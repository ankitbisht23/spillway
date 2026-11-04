package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
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

// NewServer returns the traffic server for h. Its own log lines, such as a
// failed TLS handshake, go to log without addresses, since a client's can be
// personal data (ADR-0013).
func NewServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(noAddresses{log.Handler()}, slog.LevelWarn),
	}
}

// address matches an ip:port as net/http logs one, such as the client's in
// "http: TLS handshake error from 203.0.113.7:51234: EOF".
var address = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}:\d+|\[[0-9A-Fa-f:.]+(?:%[^\]]*)?\]:\d+`)

// noAddresses replaces every ip:port in a record's message with <address>.
// slog.NewLogLogger only calls Handle, so the embedded WithAttrs and
// WithGroup are never used.
type noAddresses struct{ slog.Handler }

func (h noAddresses) Handle(ctx context.Context, r slog.Record) error {
	r.Message = address.ReplaceAllString(r.Message, "<address>")
	return h.Handler.Handle(ctx, r)
}
