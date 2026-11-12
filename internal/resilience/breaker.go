package resilience

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
)

// State is a Breaker's state, and gw_circuit_state's value.
type State int32

const (
	Closed   State = 0
	Open     State = 1
	HalfOpen State = 2
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	}
	return "half_open"
}

// breakerBuckets is how many buckets a breaker's window has (ADR-0030), so
// that it covers between 0.9 and 1 times circuit.window.
const breakerBuckets = 10

var (
	// ErrOpen is a refusal by an open breaker, or a half-open one whose
	// probes are all out; Acquire returns it as an *OpenError.
	ErrOpen = errors.New("upstream circuit open")
	// ErrOverloaded is a refusal by max_concurrent.
	ErrOverloaded = errors.New("upstream overloaded")
)

// OpenError is ErrOpen with the time until the breaker lets probes through,
// for Retry-After: at least 1 s while half-open.
type OpenError struct {
	RetryAfter time.Duration
}

func (e *OpenError) Error() string        { return ErrOpen.Error() }
func (e *OpenError) Is(target error) bool { return target == ErrOpen }

// Transition is a change of a Breaker's state, for logs. Total and Failed
// are the attempts that decided it: the window's when it opened from
// closed, the probes' otherwise.
type Transition struct {
	From, To      State
	Total, Failed int64
}

// Breaker is one upstream's circuit breaker and max_concurrent cap (GW-14,
// ADR-0030). While closed it counts every attempt's outcome in a rolling
// window, and opens when, after a failure, the window holds at least
// min_requests attempts of which at least error_rate failed. Open, it
// refuses every request until open_for has passed; the next request makes
// it half-open. Half-open, it admits half_open_probes requests as probes,
// with one attempt each, and refuses the rest: all probes succeeding close
// it with an empty window, any failing opens it again. A probe that ends
// without an outcome gives its place to the next request. It is safe for
// concurrent use.
type Breaker struct {
	state      atomic.Int32 // a State, read without mu by Acquire and MayRetry
	concurrent Gate         // max_concurrent
	onChange   func(Transition)

	mu       sync.Mutex
	window   *Window
	width    time.Duration // the window's duration
	until    time.Time     // open until
	gen      atomic.Uint64 // counts transitions, so that an outcome from an earlier state counts for nothing; written under mu
	admitted int           // probes admitted in this half-open and not given back
	passed   int           // probes of this half-open that succeeded
}

// NewBreaker returns a closed breaker with a window of d. onChange, if not
// nil, is called after each transition, outside the breaker's lock.
func NewBreaker(d time.Duration, onChange func(Transition)) *Breaker {
	return &Breaker{window: NewWindow(d, breakerBuckets), width: d, onChange: onChange}
}

// SetWindow makes the window d long. A changed d starts an empty window,
// since buckets of the old width cannot be re-cut; the state and what is in
// flight stay (ADR-0030).
func (b *Breaker) SetWindow(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d != b.width {
		b.window, b.width = NewWindow(d, breakerBuckets), d
	}
}

// State returns the breaker's state. An open breaker whose open_for has
// passed stays open until a request arrives.
func (b *Breaker) State() State {
	return State(b.state.Load())
}

// Pass is one admitted request's place in the breaker, held from Acquire to
// Done. It is used by one goroutine at a time.
type Pass struct {
	b        *Breaker
	s        *config.CircuitBreaker
	probe    bool
	gen      uint64 // the breaker's state when the request was admitted
	recorded bool   // a probe's outcome was recorded
	done     bool
}

// Acquire admits a request at now under s, or refuses it with an
// *OpenError, or with ErrOverloaded when max_concurrent requests are in
// flight.
func (b *Breaker) Acquire(now time.Time, s *config.CircuitBreaker) (*Pass, error) {
	// gen is read before the state: if the breaker moves in between, the
	// pass belongs to an earlier state and its outcome counts for nothing.
	p := &Pass{b: b, s: s, gen: b.gen.Load()}
	if b.State() != Closed {
		t, err := b.admit(now, p)
		b.notify(t)
		if err != nil {
			return nil, err
		}
	}
	if !b.concurrent.TryAcquire(int64(s.MaxConcurrent)) {
		p.giveBack()
		return nil, ErrOverloaded
	}
	return p, nil
}

// admit decides for a breaker that was not closed when p's request
// arrived: it may have closed since; an open one past open_for becomes
// half-open; a half-open one with a free place makes p a probe.
func (b *Breaker) admit(now time.Time, p *Pass) (*Transition, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var t *Transition
	switch b.State() {
	case Closed:
		p.gen = b.gen.Load()
		return nil, nil
	case Open:
		if now.Before(b.until) {
			return nil, &OpenError{RetryAfter: b.until.Sub(now)}
		}
		t = b.set(HalfOpen, 0, 0)
	}
	if b.admitted >= p.s.HalfOpenProbes {
		return t, &OpenError{RetryAfter: time.Second}
	}
	b.admitted++
	p.probe, p.gen = true, b.gen.Load()
	return t, nil
}

// set moves the breaker to state to; b.mu must be held.
func (b *Breaker) set(to State, total, failed int64) *Transition {
	t := &Transition{From: b.State(), To: to, Total: total, Failed: failed}
	b.state.Store(int32(to))
	b.gen.Add(1)
	b.admitted, b.passed = 0, 0
	return t
}

func (b *Breaker) notify(t *Transition) {
	if t != nil && b.onChange != nil {
		b.onChange(*t)
	}
}

// Record notes the outcome of one of the request's attempts at now. While
// the breaker is closed it counts in the window, and a failure may open the
// breaker. A probe's outcome decides its half-open: a failure opens the
// breaker again, and the last of half_open_probes successes closes it.
// Outcomes of requests admitted in an earlier state count for nothing: one
// admitted while closed whose outcome arrives once the breaker has opened,
// even if it has closed again since.
func (p *Pass) Record(now time.Time, failed bool) {
	b := p.b
	b.mu.Lock()
	var t *Transition
	switch {
	case p.gen != b.gen.Load():
	case p.probe:
		if p.recorded {
			break
		}
		p.recorded = true
		if failed {
			t = b.set(Open, int64(b.passed)+1, 1)
			b.until = now.Add(p.s.OpenFor)
			break
		}
		if b.passed++; b.passed >= p.s.HalfOpenProbes {
			t = b.set(Closed, int64(b.passed), 0)
			b.window.Reset()
		}
	case b.State() == Closed:
		b.window.Add(now, failed)
		if !failed {
			break
		}
		if total, f := b.window.Counts(now); total >= int64(p.s.MinRequests) && float64(f)/float64(total) >= p.s.ErrorRate {
			t = b.set(Open, total, f)
			b.until = now.Add(p.s.OpenFor)
		}
	}
	b.mu.Unlock()
	b.notify(t)
}

// MayRetry reports whether the request may make another attempt: the
// breaker is closed and the request is not a probe (ADR-0030).
func (p *Pass) MayRetry() bool {
	return !p.probe && p.b.State() == Closed
}

// Done ends the request: it frees its max_concurrent place, and a probe
// whose outcome was never recorded gives its place to the next request.
// Calls after the first do nothing.
func (p *Pass) Done() {
	if p.done {
		return
	}
	p.done = true
	p.b.concurrent.Release()
	p.giveBack()
}

// giveBack frees a probe's place if it has no outcome.
func (p *Pass) giveBack() {
	if !p.probe || p.recorded {
		return
	}
	b := p.b
	b.mu.Lock()
	if p.gen == b.gen.Load() {
		b.admitted--
	}
	p.recorded = true // gives back once
	b.mu.Unlock()
}

// BreakerState is a Breaker's state by value, for tests.
type BreakerState struct {
	State         State
	Total, Failed int64     // the window's counts at the time asked
	Until         time.Time // the end of the last opening
	Admitted      int       // probes of this half-open admitted and not given back
	Passed        int       // probes of this half-open that succeeded
	Concurrent    int64     // requests in flight
}

// Status returns the breaker's state at now.
func (b *Breaker) Status(now time.Time) BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	total, failed := b.window.Counts(now)
	return BreakerState{b.State(), total, failed, b.until, b.admitted, b.passed, b.concurrent.InUse()}
}
