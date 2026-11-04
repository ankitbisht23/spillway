package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeClock's SleepUntil moves the time to t at once (never back) and records
// t, so a schedule runs as fast as the generator can make it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	lag    time.Duration // added to every sleep, as by a generator that runs late
	sleeps []time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) SleepUntil(_ context.Context, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, t)
	if t = t.Add(c.lag); t.After(c.now) {
		c.now = t
	}
}

// TestSchedule runs 10 requests a second for a 1 s warm-up and 2 s measured on
// a fake clock. Request i is due at start + i/10 s and is sent while no
// request has been answered (open model); the 20 measured ones are recorded,
// the 10 warm-up ones are not; and each latency runs from the due time, not
// from when the generator got round to it.
func TestSchedule(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	clk := &fakeClock{now: start}
	const total = 30
	var started atomic.Int64
	answer := make(chan struct{})
	g := generator{clock: clk, rate: 10, warmup: time.Second, duration: 2 * time.Second, maxInFlight: total,
		do: func(context.Context) (int, string, error) {
			// Nothing is answered until the last request has been sent.
			if started.Add(1) == total {
				close(answer)
			}
			select {
			case <-answer:
				return http.StatusOK, "HTTP/1.1", nil
			case <-time.After(2 * time.Second):
				return 0, "", errors.New("the generator waited for an answer before sending")
			}
		}}
	r := g.run(t.Context())

	var want []time.Time
	for i := range total {
		want = append(want, start.Add(time.Duration(i)*100*time.Millisecond))
	}
	if !slices.Equal(clk.sleeps, want) {
		t.Errorf("due times = %v, want every 100 ms from the start: %v", clk.sleeps, want)
	}
	if r.Requests != (requestCounts{Scheduled: 20, Sent: 20}) || r.Status["2xx"] != 20 || len(r.Status) != 1 || len(r.Errors) != 0 {
		t.Errorf("requests = %+v, status = %v, errors = %v; want 20 scheduled, sent and 2xx", r.Requests, r.Status, r.Errors)
	}
	if r.TargetRPS != 10 || r.AchievedRPS != 10 || !r.Valid || r.WarmupS != 1 || r.DurationS != 2 || r.Protocol != "HTTP/1.1" {
		t.Errorf("result = %+v, want 10 rps target and achieved, valid, HTTP/1.1", r)
	}
	// Every answer comes at 2.9 s, when the last request is due, so the
	// measured latencies are 1.9 s (due at 1 s), 1.8 s, … 0 s.
	near := func(got int64, want time.Duration) bool { // within HDR's 3 significant digits
		w := want.Microseconds()
		return got >= w && got <= w+w/1000
	}
	if l := r.LatencyUS; !near(l.P50, 900*time.Millisecond) || !near(l.P99, 1900*time.Millisecond) || !near(l.P999, 1900*time.Millisecond) || !near(l.Max, 1900*time.Millisecond) {
		t.Errorf("latency = %+v µs, want p50 0.9 s, p99, p99.9 and max 1.9 s", l)
	}
}

// TestScheduleLate runs a generator that is always 1 s late: it catches up on
// the requests it owes, but the ones still unsent at the end of the run are
// dropped, so the achieved rate is half the target and the run is invalid.
func TestScheduleLate(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	clk := &fakeClock{now: start, lag: time.Second}
	g := generator{clock: clk, rate: 10, duration: 2 * time.Second, maxInFlight: 100,
		do: func(context.Context) (int, string, error) { return http.StatusOK, "HTTP/1.1", nil }}
	r := g.run(t.Context())
	if r.Requests != (requestCounts{Scheduled: 20, Sent: 10, Dropped: 10}) || r.AchievedRPS != 5 || r.Valid {
		t.Errorf("requests = %+v, achieved = %v rps, valid = %v; want 10 of 20 sent, 5 rps, invalid", r.Requests, r.AchievedRPS, r.Valid)
	}
	// Latency includes the lateness: the first request was due at 0 and sent at 1 s.
	if r.LatencyUS.Max < time.Second.Microseconds() {
		t.Errorf("max latency = %d µs, want at least 1 s", r.LatencyUS.Max)
	}
}

func TestErrorKind(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{&url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}, "timeout"},
		{&url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, "dial"},
		{&url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}, "other"},
		{errors.New("unexpected EOF"), "other"},
	} {
		if got := errorKind(tt.err); got != tt.want {
			t.Errorf("errorKind(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
