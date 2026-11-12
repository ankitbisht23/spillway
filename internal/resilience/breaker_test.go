package resilience

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// breakerSettings are ADR-0030's defaults.
func breakerSettings() *config.CircuitBreaker {
	return &config.CircuitBreaker{MaxConnections: 1024, MaxPending: 1024, MaxConcurrent: 1024, ErrorRate: 0.5,
		Window: 10 * time.Second, MinRequests: 20, OpenFor: 10 * time.Second, HalfOpenProbes: 5}
}

// breakerTest is a breaker on a fake clock that keeps its transitions.
type breakerTest struct {
	*Breaker
	t     *testing.T
	clock *ratelimit.FakeClock
	s     *config.CircuitBreaker
	mu    sync.Mutex
	moves []Transition
}

func newBreakerTest(t *testing.T) *breakerTest {
	bt := &breakerTest{t: t, clock: ratelimit.NewFakeClock(t0), s: breakerSettings()}
	bt.Breaker = NewBreaker(bt.s.Window, func(tr Transition) {
		bt.mu.Lock()
		bt.moves = append(bt.moves, tr)
		bt.mu.Unlock()
	})
	return bt
}

// request sends one request with one attempt of the given outcome.
func (bt *breakerTest) request(failed bool) {
	bt.t.Helper()
	p, err := bt.Acquire(bt.clock.Now(), bt.s)
	if err != nil {
		bt.t.Fatalf("request refused: %v", err)
	}
	p.Record(bt.clock.Now(), failed)
	p.Done()
}

// requests sends n requests whose attempts fail as failed says.
func (bt *breakerTest) requests(n int, failed bool) {
	bt.t.Helper()
	for range n {
		bt.request(failed)
	}
}

func (bt *breakerTest) want(state State) {
	bt.t.Helper()
	if got := bt.State(); got != state {
		bt.t.Fatalf("state %v, want %v", got, state)
	}
}

// refused checks that a request is refused as open, with retryAfter.
func (bt *breakerTest) refused(retryAfter time.Duration) {
	bt.t.Helper()
	p, err := bt.Acquire(bt.clock.Now(), bt.s)
	var oe *OpenError
	if !errors.As(err, &oe) || !errors.Is(err, ErrOpen) || oe.RetryAfter != retryAfter {
		bt.t.Fatalf("Acquire = %v, %v; want ErrOpen with RetryAfter %v", p, err, retryAfter)
	}
}

func (bt *breakerTest) lastMove() Transition {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if len(bt.moves) == 0 {
		return Transition{}
	}
	return bt.moves[len(bt.moves)-1]
}

// open trips bt's breaker with min_requests failures.
func (bt *breakerTest) open() {
	bt.t.Helper()
	bt.requests(bt.s.MinRequests, true)
	bt.want(Open)
}

// TestBreakerNoTripBelowMinRequests: closed stays closed below min_requests
// even when every attempt fails.
func TestBreakerNoTripBelowMinRequests(t *testing.T) {
	bt := newBreakerTest(t)
	bt.requests(19, true)
	bt.want(Closed)
	if st := bt.Status(bt.clock.Now()); st.Total != 19 || st.Failed != 19 {
		t.Errorf("window %d total, %d failed; want 19, 19", st.Total, st.Failed)
	}
	bt.request(true)
	bt.want(Open)
	if m := bt.lastMove(); m != (Transition{Closed, Open, 20, 20}) {
		t.Errorf("transition %+v, want closed to open on 20 of 20", m)
	}
}

// TestBreakerTripAtThreshold: the breaker opens on the failure that makes
// the window hold min_requests attempts with error_rate of them failed,
// and not before; only a failure trips it.
func TestBreakerTripAtThreshold(t *testing.T) {
	bt := newBreakerTest(t)
	bt.requests(11, false)
	bt.requests(9, true) // 20 attempts, 45% failed
	bt.want(Closed)
	bt.request(true) // 21, 47.6%
	bt.want(Closed)
	bt.request(true) // 22, 50%
	bt.want(Open)
	if m := bt.lastMove(); m != (Transition{Closed, Open, 22, 11}) {
		t.Errorf("transition %+v, want closed to open on 11 of 22", m)
	}

	// A success never trips it, even when the window is over the rate.
	bt = newBreakerTest(t)
	bt.s.MinRequests = 3
	bt.requests(2, true)
	bt.want(Closed)
	bt.request(false) // 3 attempts, 67% failed
	bt.want(Closed)
	bt.request(true)
	bt.want(Open)

	// error_rate 0.3 trips at exactly 3 of 10.
	bt = newBreakerTest(t)
	bt.s.MinRequests, bt.s.ErrorRate = 10, 0.3
	bt.requests(7, false)
	bt.requests(2, true)
	bt.want(Closed)
	bt.request(true)
	bt.want(Open)
}

// TestBreakerOpen: an open breaker refuses every request with the time left
// until half-open, sends nothing, and becomes half-open on the first
// request after open_for.
func TestBreakerOpen(t *testing.T) {
	bt := newBreakerTest(t)
	bt.open()
	bt.refused(10 * time.Second)
	bt.clock.Advance(2500 * time.Millisecond)
	bt.refused(7500 * time.Millisecond)
	bt.clock.Advance(7499 * time.Millisecond)
	bt.refused(time.Millisecond)
	if st := bt.Status(bt.clock.Now()); st.Concurrent != 0 {
		t.Errorf("%d requests in flight after refusals, want 0", st.Concurrent)
	}
	bt.clock.Advance(time.Millisecond)
	bt.want(Open) // checked lazily
	p, err := bt.Acquire(bt.clock.Now(), bt.s)
	if err != nil {
		t.Fatalf("first request after open_for: %v", err)
	}
	bt.want(HalfOpen)
	if m := bt.lastMove(); m.From != Open || m.To != HalfOpen {
		t.Errorf("transition %+v, want open to half-open", m)
	}
	p.Done()
}

// halfOpen trips bt's breaker and waits out open_for.
func (bt *breakerTest) halfOpen() {
	bt.t.Helper()
	bt.open()
	bt.clock.Advance(bt.s.OpenFor)
}

// probes admits n requests as probes.
func (bt *breakerTest) probes(n int) []*Pass {
	bt.t.Helper()
	var ps []*Pass
	for i := range n {
		p, err := bt.Acquire(bt.clock.Now(), bt.s)
		if err != nil {
			bt.t.Fatalf("probe %d refused: %v", i+1, err)
		}
		if p.MayRetry() {
			bt.t.Fatalf("probe %d may retry", i+1)
		}
		ps = append(ps, p)
	}
	return ps
}

// TestBreakerHalfOpenProbes: half-open admits exactly half_open_probes
// requests; the others are refused with Retry-After 1 s.
func TestBreakerHalfOpenProbes(t *testing.T) {
	bt := newBreakerTest(t)
	bt.halfOpen()
	ps := bt.probes(5)
	bt.want(HalfOpen)
	for range 3 {
		bt.refused(time.Second)
	}
	for i, p := range ps[:4] {
		p.Record(bt.clock.Now(), false)
		p.Done()
		bt.want(HalfOpen) // a finished probe's place is not reused
		bt.refused(time.Second)
		if st := bt.Status(bt.clock.Now()); st.Passed != i+1 || st.Admitted != 5 {
			t.Fatalf("after %d probes passed: %+v", i+1, st)
		}
	}
}

// TestBreakerCloses: when every probe succeeds the breaker closes with an
// empty window, so earlier failures cannot open it again at once.
func TestBreakerCloses(t *testing.T) {
	bt := newBreakerTest(t)
	bt.s.Window = time.Hour // the failures that opened it would still be in the window
	bt.SetWindow(bt.s.Window)
	bt.halfOpen()
	for _, p := range bt.probes(5) {
		p.Record(bt.clock.Now(), false)
		p.Done()
	}
	bt.want(Closed)
	if m := bt.lastMove(); m != (Transition{HalfOpen, Closed, 5, 0}) {
		t.Errorf("transition %+v, want half-open to closed on 5 of 5", m)
	}
	if st := bt.Status(bt.clock.Now()); st.Total != 0 || st.Failed != 0 || st.Concurrent != 0 {
		t.Errorf("after closing: %+v, want an empty window and nothing in flight", st)
	}
	p, err := bt.Acquire(bt.clock.Now(), bt.s)
	if err != nil || !p.MayRetry() {
		t.Fatalf("closed: Acquire = %v, MayRetry %v; want admitted, may retry", err, p != nil && p.MayRetry())
	}
	p.Record(bt.clock.Now(), true)
	p.Done()
	bt.want(Closed)
}

// TestBreakerStaleOutcome: a request admitted while closed whose attempt
// ends after the breaker opened, went half-open and closed again counts for
// nothing in the fresh window (ADR-0030).
func TestBreakerStaleOutcome(t *testing.T) {
	bt := newBreakerTest(t)
	stale, err := bt.Acquire(bt.clock.Now(), bt.s)
	if err != nil {
		t.Fatal(err)
	}
	bt.halfOpen()
	for _, p := range bt.probes(5) {
		p.Record(bt.clock.Now(), false)
		p.Done()
	}
	bt.want(Closed)
	stale.Record(bt.clock.Now(), true)
	stale.Done()
	if st := bt.Status(bt.clock.Now()); st.Total != 0 || st.Failed != 0 || st.Concurrent != 0 {
		t.Errorf("after a stale outcome: %+v, want an empty window and nothing in flight", st)
	}
	// A request admitted in the new closed state counts.
	bt.request(true)
	if st := bt.Status(bt.clock.Now()); st.Total != 1 || st.Failed != 1 {
		t.Errorf("after a fresh outcome: %+v, want 1 of 1 failed", st)
	}
}

// TestBreakerReopens: any failed probe opens the breaker again for another
// open_for; the other probes' outcomes then count for nothing.
func TestBreakerReopens(t *testing.T) {
	bt := newBreakerTest(t)
	bt.halfOpen()
	ps := bt.probes(5)
	ps[0].Record(bt.clock.Now(), false)
	bt.clock.Advance(time.Second)
	ps[1].Record(bt.clock.Now(), true)
	bt.want(Open)
	if m := bt.lastMove(); m != (Transition{HalfOpen, Open, 2, 1}) {
		t.Errorf("transition %+v, want half-open to open on 1 of 2", m)
	}
	bt.refused(10 * time.Second)
	for _, p := range ps[2:] {
		p.Record(bt.clock.Now(), false) // late probes of the half-open that ended
	}
	for _, p := range ps {
		p.Done()
	}
	bt.want(Open)
	bt.clock.Advance(10 * time.Second)
	bt.probes(5) // a whole new half-open
	bt.refused(time.Second)
}

// TestBreakerCancelledProbe: a probe that ends without an outcome (its
// client left, or the gateway refused its attempt) gives its place to the
// next request, once.
func TestBreakerCancelledProbe(t *testing.T) {
	bt := newBreakerTest(t)
	bt.halfOpen()
	ps := bt.probes(5)
	bt.refused(time.Second)
	ps[0].Done()
	ps[0].Done() // a second Done gives nothing back
	next := bt.probes(1)
	bt.refused(time.Second)
	for _, p := range append(ps[1:], next...) {
		p.Record(bt.clock.Now(), false)
		p.Done()
	}
	bt.want(Closed)
}

// TestBreakerWindowExpires: attempts older than the window no longer count.
func TestBreakerWindowExpires(t *testing.T) {
	bt := newBreakerTest(t)
	bt.requests(19, true)
	bt.clock.Advance(10 * time.Second) // their bucket is a window old
	bt.request(true)
	bt.want(Closed)
	if st := bt.Status(bt.clock.Now()); st.Total != 1 || st.Failed != 1 {
		t.Errorf("window %d total, %d failed; want 1, 1", st.Total, st.Failed)
	}
	bt.clock.Advance(9 * time.Second) // within the window of the last one
	bt.requests(19, true)
	bt.want(Open)
}

// TestBreakerSetWindow: a changed window starts empty and keeps the state
// and the requests in flight; the same window keeps its counts (ADR-0030).
func TestBreakerSetWindow(t *testing.T) {
	bt := newBreakerTest(t)
	bt.requests(10, true)
	p, err := bt.Acquire(bt.clock.Now(), bt.s)
	if err != nil {
		t.Fatal(err)
	}
	bt.SetWindow(10 * time.Second)
	if st := bt.Status(bt.clock.Now()); st.Total != 10 || st.Concurrent != 1 {
		t.Errorf("same window: %+v, want 10 attempts and 1 in flight", st)
	}
	bt.SetWindow(20 * time.Second)
	if st := bt.Status(bt.clock.Now()); st.Total != 0 || st.State != Closed || st.Concurrent != 1 {
		t.Errorf("changed window: %+v, want an empty window, closed, 1 in flight", st)
	}
	p.Done()

	bt.open()
	bt.SetWindow(30 * time.Second)
	bt.want(Open)
	bt.refused(10 * time.Second)
}

// TestBreakerMaxConcurrent: past max_concurrent requests in flight, a
// request is refused at once with ErrOverloaded, in every state; Done frees
// a place; a probe refused by the cap gives its probe place back.
func TestBreakerMaxConcurrent(t *testing.T) {
	bt := newBreakerTest(t)
	bt.s.MaxConcurrent = 2
	var held []*Pass
	for range 2 {
		p, err := bt.Acquire(bt.clock.Now(), bt.s)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, p)
	}
	if _, err := bt.Acquire(bt.clock.Now(), bt.s); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("third request: %v, want ErrOverloaded", err)
	}
	held[0].Done()
	held[0].Done() // frees once
	if st := bt.Status(bt.clock.Now()); st.Concurrent != 1 {
		t.Fatalf("%d in flight, want 1", st.Concurrent)
	}
	p, err := bt.Acquire(bt.clock.Now(), bt.s)
	if err != nil {
		t.Fatalf("after Done: %v", err)
	}
	p.Done()
	held[1].Done()

	// Half-open, with both places held by probes: a probe refused by the
	// cap gives its probe place back.
	bt.halfOpen()
	held = bt.probes(2)
	if _, err := bt.Acquire(bt.clock.Now(), bt.s); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("half-open, cap full: %v, want ErrOverloaded", err)
	}
	if st := bt.Status(bt.clock.Now()); st.Admitted != 2 {
		t.Errorf("%d probes admitted after a cap refusal, want 2", st.Admitted)
	}
	for _, p := range held {
		p.Done()
	}
}

// TestBreakerMayRetry: only a request admitted while closed may retry, and
// only while the breaker stays closed.
func TestBreakerMayRetry(t *testing.T) {
	bt := newBreakerTest(t)
	p, err := bt.Acquire(bt.clock.Now(), bt.s)
	if err != nil || !p.MayRetry() {
		t.Fatalf("closed: %v, MayRetry false", err)
	}
	p.Record(bt.clock.Now(), true)
	bt.requests(bt.s.MinRequests-1, true) // opens it
	bt.want(Open)
	if p.MayRetry() {
		t.Error("open: a request admitted while closed may retry")
	}
	p.Record(bt.clock.Now(), false) // late outcomes count for nothing
	p.Done()
	bt.clock.Advance(bt.s.OpenFor)
	probe := bt.probes(1)[0] // probes never retry (checked in probes)
	if p.MayRetry() {
		t.Error("half-open: a request admitted while closed may retry")
	}
	probe.Done()
}

// TestBreakerConcurrent runs requests from many goroutines through every
// state (run with -race): nothing stays in flight, and probes never exceed
// half_open_probes.
func TestBreakerConcurrent(t *testing.T) {
	bt := newBreakerTest(t)
	bt.s.OpenFor = 100 * time.Millisecond
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				now := t0.Add(time.Duration(i) * time.Millisecond)
				p, err := bt.Acquire(now, bt.s)
				if err != nil {
					continue
				}
				if st := bt.Status(now); st.Admitted > bt.s.HalfOpenProbes {
					t.Errorf("%d probes admitted", st.Admitted)
				}
				if (i+g)%7 != 0 {
					p.Record(now, (i/300)%2 == 0)
				}
				p.Done()
			}
		})
	}
	wg.Wait()
	if st := bt.Status(t0); st.Concurrent != 0 {
		t.Errorf("%d in flight after every request ended", st.Concurrent)
	}
}

// TestGate: a gate admits up to its limit, never waits, and frees on
// Release.
func TestGate(t *testing.T) {
	var g Gate
	for range 3 {
		if !g.TryAcquire(3) {
			t.Fatal("refused below the limit")
		}
	}
	if g.TryAcquire(3) || g.InUse() != 3 {
		t.Fatalf("over the limit: in use %d", g.InUse())
	}
	g.Release()
	if !g.TryAcquire(3) {
		t.Error("refused after Release")
	}
	if g.TryAcquire(2) { // a lowered limit
		t.Error("admitted above a lowered limit")
	}
}
