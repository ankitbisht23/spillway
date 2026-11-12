package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// responseRecorder notes a response's status and body bytes for the access
// log and metrics, and puts the request ID on every response (GW-22), after
// the route's response transforms, as it does the RateLimit fields (ADR-0018).
type responseRecorder struct {
	http.ResponseWriter
	requestID string
	// RateLimit-Policy and RateLimit field values (ADR-0018), "" for none.
	rateLimitPolicy, rateLimit string
	decisionTime               string // Spillway-Decision-Time, dev mode only (ADR-0021); "" for none
	status                     int    // final status, 0 until written; 1xx other than 101 don't count
	bytes                      int64
}

func (w *responseRecorder) WriteHeader(code int) {
	w.Header().Set(telemetry.RequestIDHeader, w.requestID)
	w.setRateLimit(w.Header())
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// setRateLimit sets the request's RateLimit-Policy and RateLimit fields in h,
// replacing any the upstream sent; with none, the upstream's pass through.
// Spillway-Decision-Time is the gateway's alone: the upstream's never passes.
func (w *responseRecorder) setRateLimit(h http.Header) {
	if w.rateLimit != "" {
		h.Set("RateLimit-Policy", w.rateLimitPolicy)
		h.Set("RateLimit", w.rateLimit)
	}
	h.Del(decisionTimeHeader)
	if w.decisionTime != "" {
		h.Set(decisionTimeHeader, w.decisionTime)
	}
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Hijack is how httputil.ReverseProxy answers 101 Switching Protocols: it
// writes the response itself on the hijacked connection, without WriteHeader.
func (w *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, brw, err
}

// Unwrap lets http.ResponseController reach the server's Flush.
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// countingBody counts the bytes read from a request body, and notes a read
// that failed other than at the body's end. The transport may still be
// reading it on its own goroutine when the handler returns.
type countingBody struct {
	io.ReadCloser
	n      atomic.Int64
	broken atomic.Bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	if err != nil && !errors.Is(err, io.EOF) {
		b.broken.Store(true)
	}
	return n, err
}
