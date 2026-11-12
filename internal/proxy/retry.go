package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/resilience"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// Retries and deadline propagation (GW-12, GW-13, GW-15, ADR-0030).
const (
	// DeadlineHeader carries an attempt's remaining time to the upstream, in
	// whole milliseconds (GW-15). A client's copy is never forwarded.
	DeadlineHeader = "Spillway-Deadline-Ms"
	// attemptsHeader tells the exit check how many attempts a response
	// took; dev mode only (ADR-0030).
	attemptsHeader = "Spillway-Attempts"

	maxRetryBody   = 64 << 10              // request bodies buffered for retries; longer ones stream, with one attempt
	backoffBase    = 25 * time.Millisecond // before attempt 2; doubles per retry
	backoffCap     = 250 * time.Millisecond
	minAttemptTime = 25 * time.Millisecond // a retry needs this much left after its backoff
)

// idempotent methods are retried without a route's opt-in (RFC 9110
// §9.2.2, D4).
var idempotent = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true,
	http.MethodPut: true, http.MethodDelete: true, http.MethodTrace: true,
}

// mayRetry reports whether a route's requests with method may be retried.
func mayRetry(r *config.Retry, method string) bool {
	return r.Attempts > 1 && (r.NonIdempotent || idempotent[method])
}

// bufferBody reads a request body of length bytes (-1 for unknown) for
// retries: up to maxRetryBody, through the route's size limit and idle
// timer. A body that ends within it is returned whole, with ok. Otherwise
// nothing is kept, ok is false, and rest replays what was read, then the
// remainder or the read's error, so that the request streams with one
// attempt as it would have without retries.
func bufferBody(body io.ReadCloser, length int64) (buf []byte, rest io.ReadCloser, ok bool) {
	if length > maxRetryBody {
		return nil, body, false
	}
	buf, err := io.ReadAll(io.LimitReader(body, maxRetryBody+1))
	if err == nil && len(buf) <= maxRetryBody {
		return buf, body, true
	}
	var tail io.Reader = body
	if err != nil {
		tail = errReader{err}
	}
	return nil, readCloser{io.MultiReader(bytes.NewReader(buf), tail), body}, false
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type readCloser struct {
	io.Reader
	io.Closer
}

// pending counts one attempt in its upstream's max_pending gate while it
// waits for a connection, from httptrace's GetConn to GotConn (GW-14,
// ADR-0030). An attempt past the cap is cancelled at once with cause
// errPendingLimit. The hooks run on the attempt's goroutine.
type pending struct {
	gate   *resilience.Gate
	limit  int64
	cancel context.CancelCauseFunc
	held   bool
}

func (p *pending) getConn(string) {
	if p.held {
		return
	}
	if !p.gate.TryAcquire(p.limit) {
		p.cancel(errPendingLimit)
		return
	}
	p.held = true
}

func (p *pending) gotConn(httptrace.GotConnInfo) { p.end() }

// end frees the attempt's place, if it holds one.
func (p *pending) end() {
	if p.held {
		p.held = false
		p.gate.Release()
	}
}

// backoff is the full-jitter wait before attempt n + 1: uniform in
// [0, min(backoffCap, backoffBase × 2^(n−1))), drawn by jitter.
func backoff(n int, jitter func(time.Duration) time.Duration) time.Duration {
	return jitter(min(backoffCap, backoffBase<<(n-1)))
}

// deadlineMs is the Spillway-Deadline-Ms value for an attempt that must end
// by deadline: the whole milliseconds left, at least 1 (GW-15).
func deadlineMs(deadline time.Time) string {
	return strconv.FormatInt(max(int64(time.Until(deadline)/time.Millisecond), 1), 10)
}

// retryable reports whether an attempt's outcome may be retried (ADR-0030,
// "Attempt outcomes"): a failure without a response (connect error, reset
// or EOF before the headers, the attempt's own deadline), or a 502, 503 or
// 504.
func retryable(resp *http.Response, err error, failed, counted bool) bool {
	if !failed || !counted {
		return false
	}
	if err != nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// attempts sends one request's attempts to its upstream (GW-12): it is the
// request's http.RoundTripper, so every retry decision is made before
// httputil.ReverseProxy writes anything to the client.
type attempts struct {
	h        *Handler
	p        *pool
	route    *config.Route
	q        *telemetry.Request
	body     *countingBody
	pass     *resilience.Pass // the request's place in the upstream's breaker
	key      string           // the request's consistent-hash key
	first    *endpoint        // the first attempt's endpoint
	retry    bool             // the route, method and body allow retries
	buffered bool             // the body is buf, sent again on each attempt
	buf      []byte
	interim  atomic.Bool // a 1xx went to the client: no more retries

	// the last attempt's in-flight count and deadline, which last until
	// the response body is closed or the handler returns (done)
	release func()
	cancel  context.CancelFunc
}

// done ends the last attempt.
func (a *attempts) done() {
	if a.release != nil {
		a.release()
		a.cancel()
	}
}

func (a *attempts) RoundTrip(out *http.Request) (*http.Response, error) {
	ctx := out.Context()
	total, _ := ctx.Deadline() // the route's total timeout
	if a.retry {
		// httputil.ReverseProxy passes 1xx responses to the client as
		// they arrive (GW-12: no retry once a byte went to the client).
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error {
			a.interim.Store(true)
			return nil
		}})
	}
	var buf [config.MaxAttempts]*endpoint
	tried := buf[:0]
	ep := a.first
	for n := 1; ; n++ {
		a.q.Attempts = n
		deadline := time.Now().Add(a.route.Timeouts.PerTry)
		if total.Before(deadline) {
			deadline = total
		}
		dctx, cancelDeadline := context.WithDeadline(ctx, deadline)
		actx, cancelCause := context.WithCancelCause(dctx)
		cancel := func() { cancelCause(nil); cancelDeadline() }
		pend := &pending{gate: &a.p.pending, limit: int64(a.p.settings.Load().Circuit.MaxPending), cancel: cancelCause}
		actx = httptrace.WithClientTrace(actx, &httptrace.ClientTrace{GetConn: pend.getConn, GotConn: pend.gotConn})
		var req *http.Request
		if n == 1 {
			req = out.WithContext(actx)
		} else {
			req = out.Clone(actx)
		}
		req.URL.Host = ep.addr
		if a.buffered {
			req.Body = io.NopCloser(bytes.NewReader(a.buf))
		}
		req.Header.Set(DeadlineHeader, deadlineMs(deadline)) // replaces the client's
		release := ep.begin()
		start := time.Now()
		resp, err := a.p.transport.RoundTrip(req)
		pend.end()
		if err != nil && errors.Is(context.Cause(actx), errPendingLimit) {
			err = errPendingLimit
		}
		a.h.rec.UpstreamAttempt(a.p.name, n, time.Since(start))
		now := a.h.pools.clock.Now()
		if n == 1 {
			a.p.budget.Request(now) // with the attempt="1" observation, so the metrics show what the budget saw
		}
		failed, counted := attemptOutcome(actx, resp, err, a.body)
		if counted {
			a.p.record(ep, now, failed) // GW-09
			a.pass.Record(now, failed)  // GW-14
		}
		wait, ok := a.next(ctx, n, total, now, retryable(resp, err, failed, counted))
		if !ok {
			a.release, a.cancel = release, cancel
			if err != nil {
				release()
				return nil, err
			}
			if resp.StatusCode != http.StatusSwitchingProtocols { // a 101's body must stay an io.ReadWriteCloser
				resp.Body = releaseBody{resp.Body, release}
			}
			return resp, nil
		}
		if resp != nil {
			// A little of the body is drained, so that the connection
			// can be reused.
			_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
			_ = resp.Body.Close()
		}
		release()
		cancel()
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, context.Cause(ctx)
			case <-t.C:
			}
		}
		tried = append(tried, ep)
		if ep, err = a.p.pick(a.h.pools.clock.Now(), a.key, tried); err != nil {
			return nil, err
		}
	}
}

// next decides at now whether attempt n, whose outcome was retryable or
// not, is followed by another, and returns the backoff to wait first
// (ADR-0030, in order): attempts are left, the request may retry and is
// live, and no 1xx went to the client; the backoff and minAttemptTime fit
// before the total deadline; the breaker is closed and the request is not
// a half-open probe; the upstream's budget grants it, which
// gw_retries_total counts either way. A retry not sent is dropped, never
// queued: the client gets attempt n's answer.
func (a *attempts) next(ctx context.Context, n int, total, now time.Time, retryable bool) (time.Duration, bool) {
	if !retryable || !a.retry || n >= a.route.Retry.Attempts || ctx.Err() != nil || a.interim.Load() {
		return 0, false
	}
	wait := backoff(n, a.h.jitter)
	if !time.Now().Add(wait + minAttemptTime).Before(total) {
		return 0, false
	}
	if !a.pass.MayRetry() {
		return 0, false
	}
	b := &a.p.settings.Load().RetryBudget
	sent := a.p.budget.TryRetry(now, b.Ratio, b.MinPerSec)
	a.h.rec.Retry(a.p.name, sent)
	return wait, sent
}
