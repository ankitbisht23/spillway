// Package loadgen is the open-model load generator of cmd/loadgen
// (ADR-0014) and its per-tenant slot histograms (ADR-0074, ADR-0083),
// shared with cmd/demo-traffic (ADR-0096 §1).
package loadgen

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// Clock is the generator's time source, so tests can run a schedule on a fake
// one.
type Clock interface {
	Now() time.Time
	// SleepUntil returns at t, at once if t has passed, or when ctx ends.
	SleepUntil(ctx context.Context, t time.Time)
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) SleepUntil(ctx context.Context, t time.Time) {
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

// Generator makes one open-model run (ADR-0014). Request i is due at
// Start + i·Per/Rate whatever happened to the requests before it, and its latency
// runs from that due time to the end of its response, so a stalled target
// shows in the latencies instead of slowing the generator down (no
// coordinated omission).
type Generator struct {
	Clock       Clock
	Rate        int           // requests per Per
	Per         time.Duration // Rate's period; zero: one second
	Warmup      time.Duration // sent but not recorded
	Duration    time.Duration // recorded
	MaxInFlight int
	// Do sends the request due at due and reads its response to the end.
	Do func(ctx context.Context, due time.Time) (status int, proto string, err error)

	// The per-tenant mode's additions (NFR-15, ADR-0074 §D9); all zero in
	// the single-URL mode.
	Start  time.Time     // the schedule's start; zero: when Run starts
	Slot   time.Duration // > 0: cut the measured time into slots of this length
	Settle time.Duration // left out of each slot's histogram, from its start
	// Toggle schedules requests only in odd slots, counted from the start
	// of the measured time (slot 0 off), warm-up included.
	Toggle bool
}

// SlotMaxLatency is the top of a slot's histogram, smaller than MaxLatency
// to keep the many slot histograms small; a longer latency is recorded as
// SlotMaxLatency.
const SlotMaxLatency = time.Minute

// SlotIndex is the slot of a request due d after the measured time starts:
// floor(d ÷ slot), negative during the warm-up.
func SlotIndex(d, slot time.Duration) int {
	if d >= 0 {
		return int(d / slot)
	}
	return -int((-d + slot - 1) / slot)
}

// SlotOn reports whether a toggled schedule sends in slot i: odd slots only.
func SlotOn(i int) bool { return i&1 == 1 }

// Run sends the requests, waits for the last answers and returns the Result
// of the measured ones. A request due while MaxInFlight requests are in
// flight is dropped, never queued, and so is every request not sent by the
// end of the run: both lower the achieved rate. The run is valid when the
// achieved rate is at least 99% of the target.
func (g *Generator) Run(ctx context.Context) Result {
	r, _ := g.RunStats(ctx)
	return r
}

// RunStats is Run, also returning the measured requests' Stats.
//
// With Slot set, the measured time is cut into slots: each answered
// request due in a slot, after its first settle, also goes into that slot's
// histogram, and the Result lists every slot. With Toggle set, a request due
// in an even slot is not scheduled at all, and the run is valid when at
// least 99% of the scheduled requests were sent.
func (g *Generator) RunStats(ctx context.Context) (Result, *Stats) {
	s := &Stats{
		Latency: hdrhistogram.New(1, MaxLatency.Microseconds(), 3),
		status:  map[string]int64{},
		errors:  map[string]int64{},
	}
	start := g.Start
	if start.IsZero() {
		start = g.Clock.Now()
	}
	from, end := start.Add(g.Warmup), start.Add(g.Warmup+g.Duration)
	if g.Slot > 0 {
		n := int((g.Duration + g.Slot - 1) / g.Slot)
		s.slots = make([]*hdrhistogram.Histogram, n)
		for i := range s.slots {
			s.slots[i] = hdrhistogram.New(1, SlotMaxLatency.Microseconds(), 3)
		}
		for i := range s.pooled {
			s.pooled[i] = hdrhistogram.New(1, SlotMaxLatency.Microseconds(), 3)
		}
	}
	per := g.Per
	if per <= 0 {
		per = time.Second
	}
	perSecond := float64(g.Rate) / per.Seconds()
	inFlight := make(chan struct{}, g.MaxInFlight)
	var wg sync.WaitGroup
	var sent, dropped int64
	stopped := false
	for i := int64(0); ; i++ {
		due := start.Add(time.Duration(i) * per / time.Duration(g.Rate))
		if !due.Before(end) {
			break
		}
		slot := 0
		if g.Slot > 0 {
			slot = SlotIndex(due.Sub(from), g.Slot)
			if g.Toggle && !SlotOn(slot) {
				continue
			}
		}
		settled := g.Slot > 0 && due.Sub(from.Add(time.Duration(slot)*g.Slot)) >= g.Settle
		measured := !due.Before(from)
		send := false
		if !stopped {
			g.Clock.SleepUntil(ctx, due)
			stopped = ctx.Err() != nil || !g.Clock.Now().Before(end)
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
			status, proto, err := g.Do(ctx, due)
			done := g.Clock.Now()
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

	achieved := float64(sent) / g.Duration.Seconds()
	valid := achieved >= 0.99*perSecond
	if g.Toggle {
		// Sent per second of the time it was scheduled to send.
		achieved = 0
		if scheduled := sent + dropped; scheduled > 0 {
			achieved = perSecond * float64(sent) / float64(scheduled)
		}
		valid = float64(sent) >= 0.99*float64(sent+dropped)
	}
	var slots []SlotResult
	var pooled *PooledP99
	if g.Slot > 0 {
		pooled = &PooledP99{Even: s.pooled[0].ValueAtQuantile(99), Odd: s.pooled[1].ValueAtQuantile(99)}
	}
	for i, h := range s.slots {
		sr := SlotResult{Index: i, On: !g.Toggle || SlotOn(i), Requests: h.TotalCount()}
		if sr.Requests > 0 {
			sr.P99 = h.ValueAtQuantile(99)
		}
		slots = append(slots, sr)
	}
	return Result{
		Protocol:    s.protocol,
		TargetRPS:   g.Rate,
		AchievedRPS: achieved,
		Valid:       valid,
		WarmupS:     g.Warmup.Seconds(),
		DurationS:   g.Duration.Seconds(),
		Requests:    RequestCounts{Scheduled: sent + dropped, Sent: sent, Dropped: dropped},
		Status:      s.status,
		Errors:      s.errors,
		Failures:    s.failures,
		LatencyUS: Latencies{
			P50:  s.Latency.ValueAtQuantile(50),
			P99:  s.Latency.ValueAtQuantile(99),
			P999: s.Latency.ValueAtQuantile(99.9),
			Max:  s.Latency.Max(),
		},
		Slots:  slots,
		Pooled: pooled,
	}, s
}

// MaxLatency is the top of the latency histogram; a longer latency is
// recorded as MaxLatency.
const MaxLatency = time.Hour

// Stats collects the measured requests' outcomes.
type Stats struct {
	mu       sync.Mutex
	Latency  *hdrhistogram.Histogram    // microseconds, answered requests only
	status   map[string]int64           // answered requests by status class
	errors   map[string]int64           // unanswered requests by ErrorKind
	protocol string                     // of the first answer
	failures *Failures                  // every request's, warm-up included; nil if none
	slots    []*hdrhistogram.Histogram  // per slot, settled answered requests only; nil without slots
	pooled   [2]*hdrhistogram.Histogram // the even and the odd slots' together
}

// recordSlot records the latency of an answered request in slot i's
// histogram.
func (s *Stats) recordSlot(i int, latency time.Duration) {
	if i < 0 || i >= len(s.slots) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := min(latency, SlotMaxLatency).Microseconds()
	_ = s.slots[i].RecordValue(v)    // in range
	_ = s.pooled[i&1].RecordValue(v) // in range
}

func (s *Stats) record(status int, proto string, err error, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errors[ErrorKind(err)]++
		return
	}
	s.status[fmt.Sprintf("%dxx", status/100)]++
	if s.protocol == "" {
		s.protocol = proto
	}
	_ = s.Latency.RecordValue(min(latency, MaxLatency).Microseconds()) // in range
}

// fail notes a request, warm-up included, that was due at due and got no
// answer or a non-2xx one at done.
func (s *Stats) fail(due, done time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures
	if f == nil {
		f = &Failures{First: due, Last: done}
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

// ErrorKind sorts a request without an answer: "timeout" (the request
// timeout or a network timeout), "dial" (no connection) or "other".
func ErrorKind(err error) string {
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

// Result is a run's result: the JSON loadgen prints, without its url,
// hardware and tenants (ADR-0014). Latencies are in microseconds.
type Result struct {
	Protocol    string           `json:"protocol"`
	TargetRPS   int              `json:"target_rps"`
	AchievedRPS float64          `json:"achieved_rps"`
	Valid       bool             `json:"valid"`
	WarmupS     float64          `json:"warmup_s"`
	DurationS   float64          `json:"duration_s"`
	Requests    RequestCounts    `json:"requests"`
	Status      map[string]int64 `json:"status"`
	Errors      map[string]int64 `json:"errors"`
	Failures    *Failures        `json:"failures,omitempty"`
	LatencyUS   Latencies        `json:"latency_us"`
	// Slots and Pooled are a tenant's, in the per-tenant mode only.
	Slots  []SlotResult `json:"-"`
	Pooled *PooledP99   `json:"-"`
}

// PooledP99 is a tenant's p99 (µs) over the settled answers of all its even
// slots and of all its odd slots: with a toggled tenant, the slots it is off
// and on.
type PooledP99 struct {
	Even int64 `json:"even_slots"`
	Odd  int64 `json:"odd_slots"`
}

// SlotResult is one slot of a tenant: whether the tenant sent in it, how
// many settled answers its histogram holds and their p99 (µs; 0 if none).
type SlotResult struct {
	Index    int   `json:"index"`
	On       bool  `json:"on"`
	Requests int64 `json:"requests"`
	P99      int64 `json:"p99"`
}

type RequestCounts struct {
	Scheduled int64 `json:"scheduled"`
	Sent      int64 `json:"sent"`
	Dropped   int64 `json:"dropped"`
}

// Failures are the run's requests, warm-up included, that got no answer or
// an answer other than 2xx: how many, and the window from the earliest one's
// due time to the latest one's end, so that a check can tell one burst of
// failures from several (ADR-0034).
type Failures struct {
	Count  int64     `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	SpanMS float64   `json:"span_ms"`
}

type Latencies struct {
	P50  int64 `json:"p50"`
	P99  int64 `json:"p99"`
	P999 int64 `json:"p99_9"`
	Max  int64 `json:"max"`
}
