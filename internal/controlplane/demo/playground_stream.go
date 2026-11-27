package demo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// A Playground run's events (the spec's PlaygroundTick, PlaygroundEnd,
// PlaygroundCounts and PlaygroundLast) and its stream (ADR-0096 §3).

// Counts are one tenant's requests in a second or a whole run.
type Counts struct {
	Sent      int64    `json:"sent"`
	OK        int64    `json:"ok"`
	Limited   int64    `json:"limited"`
	Forbidden int64    `json:"forbidden"`
	Failed    int64    `json:"failed"`
	Other     int64    `json:"other"`
	Dropped   int64    `json:"dropped"`
	P50Ms     *float64 `json:"p50_ms"`
	P99Ms     *float64 `json:"p99_ms"`
}

// Last is a tenant's latest answer: its status, replica and rate-limit
// headers, verbatim.
type Last struct {
	Status          int32   `json:"status"`
	Replica         string  `json:"replica"`
	RateLimit       *string `json:"ratelimit"`
	RateLimitPolicy *string `json:"ratelimit_policy"`
	RetryAfter      *string `json:"retry_after"`
}

// Tick is one second of a run.
type Tick struct {
	T        time.Time         `json:"t"`
	ElapsedS int32             `json:"elapsed_s"`
	Tenants  map[string]Counts `json:"tenants"`
	Replicas map[string]int64  `json:"replicas"`
	Last     map[string]Last   `json:"last"`
}

// End ends a stream.
type End struct {
	Reason string            `json:"reason"`
	Totals map[string]Counts `json:"totals"`
}

// counts accumulates Counts, with the answers' latencies in microseconds.
type counts struct {
	sent, ok, limited, forbidden, failed, other, dropped int64
	latency                                              *hdrhistogram.Histogram
}

func newCounts() *counts {
	return &counts{latency: hdrhistogram.New(1, (2 * requestTimeout).Microseconds(), 3)}
}

// add counts an answer with status code, 0 for none.
func (c *counts) add(code int, latency time.Duration) {
	switch {
	case code >= 200 && code < 300:
		c.ok++
	case code == http.StatusTooManyRequests:
		c.limited++
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		c.forbidden++
	case code == http.StatusServiceUnavailable:
		c.failed++
	default:
		c.other++
	}
	if code != 0 {
		_ = c.latency.RecordValue(max(latency.Microseconds(), 1)) // over the top: recorded as the top
	}
}

func (c *counts) out() Counts {
	o := Counts{Sent: c.sent, OK: c.ok, Limited: c.limited, Forbidden: c.forbidden, Failed: c.failed, Other: c.other, Dropped: c.dropped}
	if c.latency.TotalCount() > 0 {
		p50, p99 := float64(c.latency.ValueAtQuantile(50))/1000, float64(c.latency.ValueAtQuantile(99))/1000
		o.P50Ms, o.P99Ms = &p50, &p99
	}
	return o
}

func (c *counts) reset() {
	*c = counts{latency: c.latency}
	c.latency.Reset()
}

// frame is one SSE event.
func frame(name string, data any) []byte {
	b, err := json.Marshal(data)
	if err != nil { // every event is made of JSON-safe values
		panic("playground: marshalling a " + name + " event: " + err.Error())
	}
	return append(append([]byte("event: "+name+"\ndata: "), b...), '\n', '\n')
}

// Subscription is an admitted stream of a run, counted against the stream
// caps until Serve returns.
type Subscription struct {
	run *Run
}

// Subscribe admits a stream of the caller's run id: at most 2 per run and
// 50 in all. It returns ErrNoRun or a *CapError.
func (p *Playground) Subscribe(sub, id string) (*Subscription, error) {
	r, err := p.Run(sub, id)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case p.closed:
		return nil, &CapError{"the control plane is shutting down: try again", streamRetry}
	case r.streams >= streamsPerRun:
		return nil, &CapError{"this run has 2 open streams already (another tab?): close one", streamRetry}
	case p.streams >= streamsTotal:
		return nil, &CapError{"too many open Playground streams: try again shortly", streamRetry}
	}
	r.streams++
	p.streams++
	return &Subscription{run: r}, nil
}

func (s *Subscription) release() {
	p := s.run.pg
	p.mu.Lock()
	defer p.mu.Unlock()
	s.run.mu.Lock()
	defer s.run.mu.Unlock()
	s.run.streams--
	p.streams--
}

var keepaliveFrame = []byte(": keepalive\n\n")

// Serve streams the run to w: ready, then each tick from now on, and end
// when the run ends, the token expires at expiry (zero: never), or the
// client goes away or takes longer than 10 s over an event; then it
// releases s. A run that has ended gets ready and end only. It first
// clears the connection's deadlines, so the server's timeouts end neither
// the stream nor its context.
func (s *Subscription) Serve(w http.ResponseWriter, r *http.Request, expiry time.Time) {
	defer s.release()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(b []byte) error {
		if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		return rc.Flush()
	}
	s.run.stream(r.Context(), write, expiry)
}

// stream writes the run's events until it ends.
func (run *Run) stream(ctx context.Context, write func([]byte) error, expiry time.Time) {
	if write(frame("ready", run.Info())) != nil {
		return
	}
	run.mu.Lock()
	next := len(run.frames) // ticks from now on; none after the end
	run.mu.Unlock()
	keep := time.NewTicker(keepalive)
	defer keep.Stop()
	var expired <-chan time.Time
	if !expiry.IsZero() {
		t := time.NewTimer(time.Until(expiry))
		defer t.Stop()
		expired = t.C
	}
	for {
		run.mu.Lock()
		frames, end, changed := run.frames[next:], run.end, run.changed
		next = len(run.frames)
		run.mu.Unlock()
		for _, f := range frames {
			if write(f) != nil {
				return
			}
		}
		if end != nil {
			_ = write(end)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-expired:
			_ = write(frame("end", End{Reason: ReasonTokenExpired, Totals: run.Totals()}))
			return
		case <-keep.C:
			if write(keepaliveFrame) != nil {
				return
			}
		case <-changed:
		}
	}
}
