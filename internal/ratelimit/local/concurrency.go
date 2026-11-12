package local

import (
	"context"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// Concurrency limits the cost units in flight per key (RL-02). The caller
// releases exactly once for every admission: when the request completes,
// times out or the client disconnects, or at once as a refund if another limit
// denies the request.
type Concurrency struct {
	limit int64
	state *table[int64]
}

var _ ratelimit.Limiter = (*Concurrency)(nil)

// NewConcurrency returns a Concurrency that admits up to limit cost units in
// flight per key.
func NewConcurrency(limit int64) *Concurrency {
	return &Concurrency{limit: limit, state: newTable[int64]()}
}

// Allow implements ratelimit.Limiter; a negative cost is a Release.
// ResetAfter and RetryAfter are always 0.
func (c *Concurrency) Allow(_ context.Context, key string, cost int64) (ratelimit.Decision, error) {
	return c.take(key, cost)
}

// Release gives back cost units admitted earlier. It never takes a count
// below 0, and a key whose count reaches 0 is removed.
func (c *Concurrency) Release(key string, cost int64) {
	_, _ = c.take(key, -cost) // never adds key
}

func (c *Concurrency) take(key string, cost int64) (ratelimit.Decision, error) {
	var d ratelimit.Decision
	err := c.state.update(key, func(n int64, _ bool) (int64, bool) {
		switch {
		case cost < 0:
			n = max(0, n+cost)
			d.Allowed = true
		case n+cost <= c.limit:
			n += cost
			d.Allowed = true
		}
		d.Remaining = max(0, c.limit-n) // a reload can lower the max below the count
		return n, n > 0
	})
	if err != nil {
		return ratelimit.Decision{}, err
	}
	return d, nil
}
