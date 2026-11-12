package local

import (
	"context"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// GCRA is the generic cell rate algorithm (a token bucket kept as one
// theoretical arrival time, TAT, per key).
type GCRA struct {
	clock ratelimit.Clock
	t     int64 // emission interval T, ns
	tau   int64 // burst tolerance τ = T·burst, ns
	state *table[int64]
}

var _ ratelimit.Limiter = (*GCRA)(nil)

// NewGCRA returns a GCRA that admits rate cost units per period, up to burst
// at once. T = period/rate is rounded up to a whole nanosecond, so the limiter
// never admits more than burst + w·rate/period in any window w.
func NewGCRA(clock ratelimit.Clock, rate int64, period time.Duration, burst int64) *GCRA {
	t := (int64(period) + rate - 1) / rate
	return &GCRA{clock: clock, t: t, tau: t * burst, state: newTable[int64]()}
}

// Allow implements ratelimit.Limiter. A refund (cost < 0) is taken as one of
// an admission made now, so it gives back all of cost·T; Refund is the one
// that knows when the admission was made.
func (g *GCRA) Allow(_ context.Context, key string, cost int64) (ratelimit.Decision, error) {
	now := g.clock.Now().UnixNano()
	if cost < 0 {
		return g.refund(key, -cost*g.t, now), nil
	}
	var d ratelimit.Decision
	err := g.state.update(key, func(tat int64, ok bool) (int64, bool) {
		base := max(tat, now)
		next := base + cost*g.t
		if next-now <= g.tau {
			d = g.decision(next, now)
			d.Allowed = true
			return next, true
		}
		d = g.decision(base, now)
		d.RetryAfter = time.Duration(next - g.tau - now)
		return tat, ok
	})
	if err != nil {
		return ratelimit.Decision{}, err
	}
	return d, nil
}

// Refund reverses an admission of cost made at at. It gives back cost·T less
// the time since at, never less than 0: an admission that found the key idle
// moved the TAT up from at, not from the stored TAT, and requests admitted
// since may have used up to that time of its room, so giving back all of
// cost·T could admit more than burst + w·rate/period (ADR-0028).
func (g *GCRA) Refund(key string, cost int64, at time.Time) ratelimit.Decision {
	now := g.clock.Now().UnixNano()
	return g.refund(key, max(0, cost*g.t-max(0, now-at.UnixNano())), now)
}

// refund moves key's TAT back by back ns.
func (g *GCRA) refund(key string, back, now int64) ratelimit.Decision {
	var d ratelimit.Decision
	_ = g.state.update(key, func(tat int64, ok bool) (int64, bool) { // never adds key
		tat -= back
		d = g.decision(tat, now)
		d.Allowed = true
		return tat, ok
	})
	return d
}

func (g *GCRA) decision(tat, now int64) ratelimit.Decision {
	reset := max(0, tat-now)
	return ratelimit.Decision{Remaining: max(0, (g.tau-reset)/g.t), ResetAfter: time.Duration(reset)}
}

// Sweep drops idle keys (TAT ≤ now), which behave exactly like missing ones.
func (g *GCRA) Sweep() {
	now := g.clock.Now().UnixNano()
	g.state.sweep(func(tat int64) bool { return tat <= now })
}
