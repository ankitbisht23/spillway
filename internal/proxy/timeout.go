package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// connectTimeoutKey carries a route's connect timeout in the request context
// to Registry.dial. The transport keeps context values for the dial but not
// the request's cancellation (GW-11).
type connectTimeoutKey struct{}

// errStreamIdle is the cause of a request cancelled by its idle timeout.
var errStreamIdle = errors.New("no body bytes moved in either direction within the route's idle timeout")

// idleTimer cancels a request when no body bytes have moved in either
// direction for its timeout (GW-11). Reads through idleBody restart it.
type idleTimer struct {
	*time.Timer
	timeout time.Duration
}

func withIdleTimeout(ctx context.Context, timeout time.Duration) (context.Context, *idleTimer) {
	ctx, cancel := context.WithCancelCause(ctx)
	return ctx, &idleTimer{time.AfterFunc(timeout, func() { cancel(errStreamIdle) }), timeout}
}

// idleBody is a request or response body that restarts an idle timer
// whenever bytes arrive.
type idleBody struct {
	io.ReadCloser
	idle *idleTimer
}

func (b idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.idle.Reset(b.idle.timeout)
	}
	return n, err
}

// unblockBody makes the client's body reads fail at once when ctx ends
// while the body is still arriving: when the route's total or idle timeout
// ends a request whose client has stopped sending its body (ADR-0033).
// Otherwise Go's transport waits for the body read it started, and the
// handler, with its in-flight place and its place in the upstream's
// max_concurrent, would last until the server's read timeout. stop must be
// called before the handler returns; w is not touched after it.
func unblockBody(ctx context.Context, w http.ResponseWriter, body *countingBody, length int64) (stop func()) {
	if length == 0 {
		return func() {}
	}
	var mu sync.Mutex
	stopped := false
	after := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !stopped && !body.ended.Load() {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		}
	})
	return func() {
		after()
		mu.Lock()
		stopped = true
		mu.Unlock()
	}
}
