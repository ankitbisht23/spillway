package proxy

import (
	"context"
	"errors"
	"io"
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
