package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"

	"github.com/ankitbisht23/spillway/internal/hardware"
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

	// The per-tenant mode's additions (NFR-15, ADR-0074 §D9); all zero in
	// the single-URL mode.
	start  time.Time     // the schedule's start; zero: when run starts
	slot   time.Duration // > 0: cut the measured time into slots of this length
	settle time.Duration // left out of each slot's histogram, from its start
	// toggle schedules requests only in odd slots, counted from the start
	// of the measured time (slot 0 off), warm-up included.
	toggle bool
}

// slotMaxLatency is the top of a slot's histogram, smaller than maxLatency
// to keep the many slot histograms small; a longer latency is recorded as
// slotMaxLatency.
const slotMaxLatency = time.Minute

// slotIndex is the slot of a request due d after the measured time starts:
// floor(d ÷ slot), negative during the warm-up.
func slotIndex(d, slot time.Duration) int {
	if d >= 0 {
		return int(d / slot)
	}
	return -int((-d + slot - 1) / slot)
}

// slotOn reports whether a toggled schedule sends in slot i: odd slots only.
func slotOn(i int) bool { return i&1 == 1 }

// run sends the requests, waits for the last answers and returns the result
// of the measured ones. A request due while maxInFlight requests are in
// flight is dropped, never queued, and so is every request not sent by the
// end of the run: both lower the achieved rate. The run is valid when the
// achieved rate is at least 99% of the target.
func (g *generator) run(ctx context.Context) result {
	r, _ := g.runStats(ctx)
	return r
}

// runStats is run, also returning the measured requests' stats.
//
// With slot set, the measured time is cut into slots: each answered
// request due in a slot, after its first settle, also goes into that slot's
// histogram, and the result lists every slot. With toggle set, a request due
// in an even slot is not scheduled at all, and the run is valid when at
// least 99% of the scheduled requests were sent.
func (g *generator) runStats(ctx context.Context) (result, *stats) {
	s := &stats{
		latency: hdrhistogram.New(1, maxLatency.Microseconds(), 3),
		status:  map[string]int64{},
		errors:  map[string]int64{},
	}
	start := g.start
	if start.IsZero() {
		start = g.clock.Now()
	}
	from, end := start.Add(g.warmup), start.Add(g.warmup+g.duration)
	if g.slot > 0 {
		n := int((g.duration + g.slot - 1) / g.slot)
		s.slots = make([]*hdrhistogram.Histogram, n)
		for i := range s.slots {
			s.slots[i] = hdrhistogram.New(1, slotMaxLatency.Microseconds(), 3)
		}
	}
	inFlight := make(chan struct{}, g.maxInFlight)
	var wg sync.WaitGroup
	var sent, dropped int64
	stopped := false
	for i := int64(0); ; i++ {
		due := start.Add(time.Duration(i) * time.Second / time.Duration(g.rate))
		if !due.Before(end) {
			break
		}
		slot := 0
		if g.slot > 0 {
			slot = slotIndex(due.Sub(from), g.slot)
			if g.toggle && !slotOn(slot) {
				continue
			}
		}
		settled := g.slot > 0 && due.Sub(from.Add(time.Duration(slot)*g.slot)) >= g.settle
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
			done := g.clock.Now()
			<-inFlight
			if err != nil || status/100 != 2 {
				s.fail(due, done)
			}
			if measured {
				s.record(status, proto, err, done.Sub(due))
				if settled && err == nil {
					s.recordSlot(slot, done.Sub(due))
				}
			}
		})
	}
	wg.Wait()

	achieved := float64(sent) / g.duration.Seconds()
	valid := achieved >= 0.99*float64(g.rate)
	if g.toggle {
		// Sent per second of the time it was scheduled to send.
		achieved = 0
		if scheduled := sent + dropped; scheduled > 0 {
			achieved = float64(g.rate) * float64(sent) / float64(scheduled)
		}
		valid = float64(sent) >= 0.99*float64(sent+dropped)
	}
	var slots []slotResult
	for i, h := range s.slots {
		sr := slotResult{Index: i, On: !g.toggle || slotOn(i), Requests: h.TotalCount()}
		if sr.Requests > 0 {
			sr.P99 = h.ValueAtQuantile(99)
		}
		slots = append(slots, sr)
	}
	return result{
		Protocol:    s.protocol,
		TargetRPS:   g.rate,
		AchievedRPS: achieved,
		Valid:       valid,
		WarmupS:     g.warmup.Seconds(),
		DurationS:   g.duration.Seconds(),
		Requests:    requestCounts{Scheduled: sent + dropped, Sent: sent, Dropped: dropped},
		Status:      s.status,
		Errors:      s.errors,
		Failures:    s.failures,
		LatencyUS: latencies{
			P50:  s.latency.ValueAtQuantile(50),
			P99:  s.latency.ValueAtQuantile(99),
			P999: s.latency.ValueAtQuantile(99.9),
			Max:  s.latency.Max(),
		},
		Slots: slots,
	}, s
}

// maxLatency is the top of the latency histogram; a longer latency is
// recorded as maxLatency.
const maxLatency = time.Hour

// stats collects the measured requests' outcomes.
type stats struct {
	mu       sync.Mutex
	latency  *hdrhistogram.Histogram   // microseconds, answered requests only
	status   map[string]int64          // answered requests by status class
	errors   map[string]int64          // unanswered requests by errorKind
	protocol string                    // of the first answer
	failures *failures                 // every request's, warm-up included; nil if none
	slots    []*hdrhistogram.Histogram // per slot, settled answered requests only; nil without slots
}

// recordSlot records the latency of an answered request in slot i's
// histogram.
func (s *stats) recordSlot(i int, latency time.Duration) {
	if i < 0 || i >= len(s.slots) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.slots[i].RecordValue(min(latency, slotMaxLatency).Microseconds()) // in range
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

// fail notes a request, warm-up included, that was due at due and got no
// answer or a non-2xx one at done.
func (s *stats) fail(due, done time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures
	if f == nil {
		f = &failures{First: due, Last: done}
		s.failures = f
	}
	f.Count++
	if due.Before(f.First) {
		f.First = due
	}
	if done.After(f.Last) {
		f.Last = done
	}
	f.SpanMS = float64(f.Last.Sub(f.First).Microseconds()) / 1000
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
	Failures    *failures        `json:"failures,omitempty"`
	LatencyUS   latencies        `json:"latency_us"`
	Hardware    hardware.Info    `json:"hardware"`
	// Slots is a tenant's, in the per-tenant mode only.
	Slots []slotResult `json:"-"`
	// Tenants is the per-tenant mode's result per tenant name (ADR-0074
	// §D9); the top-level numbers are then the tenants' together.
	Tenants map[string]tenantResult `json:"tenants,omitempty"`
}

// slotResult is one slot of a tenant: whether the tenant sent in it, how
// many settled answers its histogram holds and their p99 (µs; 0 if none).
type slotResult struct {
	Index    int   `json:"index"`
	On       bool  `json:"on"`
	Requests int64 `json:"requests"`
	P99      int64 `json:"p99"`
}

type requestCounts struct {
	Scheduled int64 `json:"scheduled"`
	Sent      int64 `json:"sent"`
	Dropped   int64 `json:"dropped"`
}

// failures are the run's requests, warm-up included, that got no answer or
// an answer other than 2xx: how many, and the window from the earliest one's
// due time to the latest one's end, so that a check can tell one burst of
// failures from several (ADR-0034).
type failures struct {
	Count  int64     `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	SpanMS float64   `json:"span_ms"`
}

type latencies struct {
	P50  int64 `json:"p50"`
	P99  int64 `json:"p99"`
	P999 int64 `json:"p99_9"`
	Max  int64 `json:"max"`
}
