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

// Allow implements ratelimit.Limiter.
func (g *GCRA) Allow(_ context.Context, key string, cost int64) (ratelimit.Decision, error) {
	now := g.clock.Now().UnixNano()
	var d ratelimit.Decision
	g.state.update(key, func(tat int64, ok bool) (int64, bool) {
		if cost < 0 {
			tat += cost * g.t
			d = g.decision(tat, now)
			d.Allowed = true
			return tat, ok
		}
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
	return d, nil
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
