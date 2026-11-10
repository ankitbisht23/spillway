package local

import (
	"context"
	"math/bits"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// Windows start at multiples of the period since the Unix epoch.

// FixedWindow counts cost units per aligned window.
type FixedWindow struct {
	clock  ratelimit.Clock
	rate   int64
	period int64 // ns
	state  *table[fixedState]
}

type fixedState struct{ start, count int64 }

var _ ratelimit.Limiter = (*FixedWindow)(nil)

// NewFixedWindow returns a FixedWindow that admits rate cost units per
// aligned period.
func NewFixedWindow(clock ratelimit.Clock, rate int64, period time.Duration) *FixedWindow {
	return &FixedWindow{clock: clock, rate: rate, period: int64(period), state: newTable[fixedState]()}
}

// Allow implements ratelimit.Limiter. A refund (cost < 0) is taken from the
// current window; Refund is the one that knows the admission's window.
func (f *FixedWindow) Allow(_ context.Context, key string, cost int64) (ratelimit.Decision, error) {
	now := f.clock.Now().UnixNano()
	return f.allow(key, cost, now, now), nil
}

// Refund reverses an admission of cost made at at: it returns what Allow with
// −cost returns, but takes the refund only if at is in the current window. A
// refund that crosses a window boundary is lost, which errs towards denying
// (ADR-0017).
func (f *FixedWindow) Refund(key string, cost int64, at time.Time) ratelimit.Decision {
	return f.allow(key, -cost, f.clock.Now().UnixNano(), at.UnixNano())
}

// allow is Allow at now, for an admission made at admitted if cost < 0.
func (f *FixedWindow) allow(key string, cost, now, admitted int64) ratelimit.Decision {
	start := now - now%f.period
	d := ratelimit.Decision{ResetAfter: time.Duration(f.period - (now - start))}
	f.state.update(key, func(s fixedState, ok bool) (fixedState, bool) {
		if s.start != start {
			s = fixedState{start: start}
		}
		switch {
		case cost < 0:
			if admitted-admitted%f.period == start {
				s.count = max(0, s.count+cost)
			}
			d.Allowed = true
		case s.count+cost <= f.rate:
			s.count += cost
			d.Allowed, ok = true, true
		default:
			d.RetryAfter = d.ResetAfter
		}
		d.Remaining = f.rate - s.count
		return s, ok
	})
	return d
}

// Sweep drops keys whose window has ended.
func (f *FixedWindow) Sweep() {
	now := f.clock.Now().UnixNano()
	f.state.sweep(func(s fixedState) bool { return s.start+f.period <= now })
}

// SlidingWindow is the sliding-window counter: the previous window's count,
// weighted by how much of it the sliding window still covers, plus the
// current window's count.
type SlidingWindow struct {
	clock  ratelimit.Clock
	rate   int64
	period int64 // ns
	state  *table[slidingState]
}

type slidingState struct{ start, cur, prev int64 }

var _ ratelimit.Limiter = (*SlidingWindow)(nil)

// NewSlidingWindow returns a SlidingWindow whose estimate stays at most rate
// cost units per period.
func NewSlidingWindow(clock ratelimit.Clock, rate int64, period time.Duration) *SlidingWindow {
	return &SlidingWindow{clock: clock, rate: rate, period: int64(period), state: newTable[slidingState]()}
}

// Allow implements ratelimit.Limiter. With e the time since the window
// started, estimate = prev·(1 − e/period) + cur; it is computed exactly, with
// the weighted prev rounded up. A refund (cost < 0) is taken from the current
// window; Refund is the one that knows the admission's window.
func (sw *SlidingWindow) Allow(_ context.Context, key string, cost int64) (ratelimit.Decision, error) {
	now := sw.clock.Now().UnixNano()
	return sw.allow(key, cost, now, now), nil
}

// Refund reverses an admission of cost made at at: it returns what Allow with
// −cost returns, but takes the refund only if at is in the current window. A
// refund that crosses a window boundary is lost, which errs towards denying
// (ADR-0017).
func (sw *SlidingWindow) Refund(key string, cost int64, at time.Time) ratelimit.Decision {
	return sw.allow(key, -cost, sw.clock.Now().UnixNano(), at.UnixNano())
}

// allow is Allow at now, for an admission made at admitted if cost < 0.
func (sw *SlidingWindow) allow(key string, cost, now, admitted int64) ratelimit.Decision {
	start := now - now%sw.period
	e := now - start
	d := ratelimit.Decision{ResetAfter: time.Duration(sw.period - e)}
	sw.state.update(key, func(s slidingState, ok bool) (slidingState, bool) {
		switch s.start {
		case start:
		case start - sw.period:
			s = slidingState{start: start, prev: s.cur}
		default:
			s = slidingState{start: start}
		}
		weighted := mulDiv(s.prev, sw.period-e, sw.period, true)
		switch {
		case cost < 0:
			if admitted-admitted%sw.period == start {
				s.cur = max(0, s.cur+cost)
			}
			d.Allowed = true
		case weighted+s.cur+cost <= sw.rate:
			s.cur += cost
			d.Allowed, ok = true, true
		default:
			d.RetryAfter = time.Duration(sw.retryAfter(s, cost, e))
		}
		d.Remaining = max(0, sw.rate-s.cur-weighted)
		return s, ok
	})
	return d
}

// retryAfter is the time until cost would fit if nothing else were admitted,
// rounded up to a nanosecond.
func (sw *SlidingWindow) retryAfter(s slidingState, cost, e int64) int64 {
	if s.cur+cost <= sw.rate {
		// In this window, once prev's weight has decayed to rate − cost − cur.
		// The deny means prev > rate − cost − cur ≥ 0.
		return sw.period - mulDiv(sw.rate-cost-s.cur, sw.period, s.prev, false) - e
	}
	// In the next window, once cur (then prev) has decayed to rate − cost.
	r := 2*sw.period - e
	if a := sw.rate - cost; a > 0 { // then cur > a > 0
		r -= mulDiv(a, sw.period, s.cur, false)
	}
	return r
}

// Sweep drops keys two or more windows old, whose estimate is 0.
func (sw *SlidingWindow) Sweep() {
	now := sw.clock.Now().UnixNano()
	sw.state.sweep(func(s slidingState) bool { return s.start+2*sw.period <= now })
}

// mulDiv returns a·b/c rounded down, or up if ceil, without overflow in a·b.
// a, b ≥ 0, c > 0 and a·b/c fits in an int64.
func mulDiv(a, b, c int64, ceil bool) int64 {
	hi, lo := bits.Mul64(uint64(a), uint64(b)) //nolint:gosec // G115: a, b ≥ 0
	q, r := bits.Div64(hi, lo, uint64(c))      //nolint:gosec // G115: c > 0
	if ceil && r > 0 {
		q++
	}
	return int64(q) //nolint:gosec // G115: the quotient fits, as above
}
