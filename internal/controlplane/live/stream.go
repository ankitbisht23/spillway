package live

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// LimitError is a stream refused because a limit is reached or the
// control plane is shutting down (503 with Retry-After).
type LimitError struct{ Msg string }

func (e *LimitError) Error() string { return e.Msg }

// RetryAfter is the Retry-After of a LimitError's answer, in seconds.
const RetryAfter = "10"

// Stream is an admitted stream: validated and counted against the limits
// until Serve returns.
type Stream struct {
	hub     *Hub
	subject string
	req     request

	mu      sync.Mutex
	pending []*event      // per subscription: the latest result not yet written
	wake    chan struct{} // signalled when pending changes
}

// Open validates a stream's query string, route and tenant against the
// store first, then admits it for subject, a token's sub: at most
// PerSubject open streams per subject and Total in all. It returns an
// *InvalidError (400), a *LimitError (503) or the store's error.
func (h *Hub) Open(ctx context.Context, subject string, q url.Values) (*Stream, error) {
	r, err := parse(q)
	if err == nil {
		err = r.check(ctx, h.cfg.Postgres)
	}
	if _, ok := errors.AsType[*InvalidError](err); ok {
		h.metrics.rejected.WithLabelValues("invalid").Inc()
	}
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		err = &LimitError{"the control plane is shutting down: try again"}
	case h.open >= h.cfg.Total:
		err = &LimitError{"too many open streams on this control plane: try again later"}
	case h.bySubject[subject] >= h.cfg.PerSubject:
		err = &LimitError{"too many open streams for this caller: close one (another tab?) or try again later"}
	}
	if err != nil {
		h.metrics.rejected.WithLabelValues("limit").Inc()
		return nil, err
	}
	h.open++
	h.bySubject[subject]++
	h.metrics.connections.Inc()
	return &Stream{hub: h, subject: subject, req: r, pending: make([]*event, len(r.subs)), wake: make(chan struct{}, 1)}, nil
}

func (s *Stream) release() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open--
	if h.bySubject[s.subject]--; h.bySubject[s.subject] <= 0 {
		delete(h.bySubject, s.subject)
	}
	h.metrics.connections.Dec()
}

// deliver puts ev in subscription i's slot, replacing a result the client
// has not taken yet (coalesced): a slow client never makes the server hold
// more than one pending event per subscription.
func (s *Stream) deliver(i int, ev *event) {
	s.mu.Lock()
	if s.pending[i] != nil {
		s.hub.metrics.coalesced.Inc()
	}
	s.pending[i] = ev
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// take empties the slots, in subscription order.
func (s *Stream) take() []*event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*event
	for i, ev := range s.pending {
		if ev != nil {
			out = append(out, ev)
			s.pending[i] = nil
		}
	}
	return out
}

var keepalive = []byte(": keepalive\n\n")

// Serve streams s's events to w until the client goes away, takes longer
// than the write timeout over an event (slow), the token expires at
// expiry (zero: never) or the hub shuts down; then it releases s. It
// first clears the connection's read and write deadlines, so the server's
// ReadTimeout and WriteTimeout end neither the stream nor its context.
func (s *Stream) Serve(w http.ResponseWriter, r *http.Request, expiry time.Time) {
	defer s.release()
	h := s.hub
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(frame []byte) error {
		if err := rc.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		return rc.Flush()
	}
	h.metrics.closed.WithLabelValues(s.run(r.Context(), write, expiry)).Inc()
}

// run writes the ready event, subscribes, and writes events until the
// stream ends; it returns why it ended.
func (s *Stream) run(ctx context.Context, write func([]byte) error, expiry time.Time) string {
	h := s.hub
	names := make([]string, len(s.req.subs))
	for i, q := range s.req.subs {
		names[i] = q.name
	}
	if err := write(newEvent("ready", readyData{Subscriptions: names, IntervalS: int(h.cfg.Interval / time.Second)}).frame); err != nil {
		return closeReason(err)
	}
	for i, q := range s.req.subs {
		h.join(q, s.req.params, sink{s, i})
		defer h.leave(q, s.req.params, sink{s, i})
	}
	tick := time.NewTicker(h.cfg.Keepalive)
	defer tick.Stop()
	var expired <-chan time.Time
	if !expiry.IsZero() {
		t := time.NewTimer(time.Until(expiry))
		defer t.Stop()
		expired = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return "client"
		case <-h.shutdown:
			_ = write(newEvent("end", endData{Reason: "shutdown"}).frame)
			return "shutdown"
		case <-expired:
			_ = write(newEvent("end", endData{Reason: "token_expired"}).frame)
			return "token_expired"
		case <-tick.C:
			if err := write(keepalive); err != nil {
				return closeReason(err)
			}
		case <-s.wake:
			for _, ev := range s.take() {
				if err := write(ev.frame); err != nil {
					return closeReason(err)
				}
			}
		}
	}
}

// closeReason is why a failed write ended a stream: a client that did not
// take the event within the write timeout is slow; any other failure means
// it went away.
func closeReason(err error) string {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return "slow"
	}
	return "client"
}
