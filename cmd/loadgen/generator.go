package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// clock is the generator's time source, so tests can run a schedule on a fake
// one.
type clock interface {
	Now() time.Time
	// SleepUntil returns at t, at once if t has passed, or when ctx ends.
	SleepUntil(ctx context.Context, t time.Time)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) SleepUntil(ctx context.Context, t time.Time) {
	d := time.Until(t)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// generator makes one open-model run (ADR-0014). Request i is due at
// start + i/rate whatever happened to the requests before it, and its latency
// runs from that due time to the end of its response, so a stalled target
// shows in the latencies instead of slowing the generator down (no
// coordinated omission).
type generator struct {
	clock       clock
	rate        int           // requests per second
	warmup      time.Duration // sent but not recorded
	duration    time.Duration // recorded
	maxInFlight int
	// do sends one request and reads its response to the end.
	do func(ctx context.Context) (status int, proto string, err error)
}

// run sends the requests, waits for the last answers and returns the result
// of the measured ones. A request due while maxInFlight requests are in
// flight is dropped, never queued, and so is every request not sent by the
// end of the run: both lower the achieved rate. The run is valid when the
// achieved rate is at least 99% of the target.
func (g *generator) run(ctx context.Context) result {
	s := &stats{
		latency: hdrhistogram.New(1, maxLatency.Microseconds(), 3),
		status:  map[string]int64{},
		errors:  map[string]int64{},
	}
	start := g.clock.Now()
	from, end := start.Add(g.warmup), start.Add(g.warmup+g.duration)
	inFlight := make(chan struct{}, g.maxInFlight)
	var wg sync.WaitGroup
	var sent, dropped int64
	stopped := false
	for i := int64(0); ; i++ {
		due := start.Add(time.Duration(i) * time.Second / time.Duration(g.rate))
		if !due.Before(end) {
			break
		}
		measured := !due.Before(from)
		send := false
		if !stopped {
			g.clock.SleepUntil(ctx, due)
			stopped = ctx.Err() != nil || !g.clock.Now().Before(end)
		}
		if !stopped {
			select {
			case inFlight <- struct{}{}:
				send = true
			default:
			}
		}
		if !send {
			if measured {
				dropped++
			}
			continue
		}
		if measured {
			sent++
		}
		wg.Go(func() {
			status, proto, err := g.do(ctx)
			latency := g.clock.Now().Sub(due)
			<-inFlight
			if measured {
				s.record(status, proto, err, latency)
			}
		})
	}
	wg.Wait()

	achieved := float64(sent) / g.duration.Seconds()
	return result{
		Protocol:    s.protocol,
		TargetRPS:   g.rate,
		AchievedRPS: achieved,
		Valid:       achieved >= 0.99*float64(g.rate),
		WarmupS:     g.warmup.Seconds(),
		DurationS:   g.duration.Seconds(),
		Requests:    requestCounts{Scheduled: sent + dropped, Sent: sent, Dropped: dropped},
		Status:      s.status,
		Errors:      s.errors,
		LatencyUS: latencies{
			P50:  s.latency.ValueAtQuantile(50),
			P99:  s.latency.ValueAtQuantile(99),
			P999: s.latency.ValueAtQuantile(99.9),
			Max:  s.latency.Max(),
		},
	}
}

// maxLatency is the top of the latency histogram; a longer latency is
// recorded as maxLatency.
const maxLatency = time.Hour

// stats collects the measured requests' outcomes.
type stats struct {
	mu       sync.Mutex
	latency  *hdrhistogram.Histogram // microseconds, answered requests only
	status   map[string]int64        // answered requests by status class
	errors   map[string]int64        // unanswered requests by errorKind
	protocol string                  // of the first answer
}

func (s *stats) record(status int, proto string, err error, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errors[errorKind(err)]++
		return
	}
	s.status[fmt.Sprintf("%dxx", status/100)]++
	if s.protocol == "" {
		s.protocol = proto
	}
	_ = s.latency.RecordValue(min(latency, maxLatency).Microseconds()) // in range
}

// errorKind sorts a request without an answer: "timeout" (the request
// timeout or a network timeout), "dial" (no connection) or "other".
func errorKind(err error) string {
	var ne net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.As(err, &op) && op.Op == "dial":
		return "dial"
	}
	return "other"
}

// result is the JSON a run prints (ADR-0014). Latencies are in microseconds.
type result struct {
	URL         string           `json:"url"`
	Protocol    string           `json:"protocol"`
	TargetRPS   int              `json:"target_rps"`
	AchievedRPS float64          `json:"achieved_rps"`
	Valid       bool             `json:"valid"`
	WarmupS     float64          `json:"warmup_s"`
	DurationS   float64          `json:"duration_s"`
	Requests    requestCounts    `json:"requests"`
	Status      map[string]int64 `json:"status"`
	Errors      map[string]int64 `json:"errors"`
	LatencyUS   latencies        `json:"latency_us"`
	Hardware    hardware         `json:"hardware"`
}

type requestCounts struct {
	Scheduled int64 `json:"scheduled"`
	Sent      int64 `json:"sent"`
	Dropped   int64 `json:"dropped"`
}

type latencies struct {
	P50  int64 `json:"p50"`
	P99  int64 `json:"p99"`
	P999 int64 `json:"p99_9"`
	Max  int64 `json:"max"`
}
