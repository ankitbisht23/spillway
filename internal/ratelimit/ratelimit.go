// Package ratelimit holds the Limiter interface that the in-memory limiters
// (package local) and, from M3, the Redis limiters implement (RL-01,
// ADR-0017).
package ratelimit

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// Decision is a limiter's answer for one call.
type Decision struct {
	Allowed    bool
	Remaining  int64         // cost units still available under this limit after this call
	ResetAfter time.Duration // until the limit is back to full (windows: until the window ends)
	RetryAfter time.Duration // denied only: until this request's cost could be admitted; 0 if allowed
}

// Limiter decides whether cost units may be taken from key's budget. A
// negative cost is a refund: it makes no decision, returns Allowed with the
// state after the refund, and reverses an earlier admission of that cost.
type Limiter interface {
	Allow(ctx context.Context, key string, cost int64) (Decision, error)
}

// Key returns the map key for a filled key template: the first 16 bytes of its
// SHA-256, the same digest as the Redis key spec. Raw API keys and header
// values never become keys.
func Key(filled string) string {
	sum := sha256.Sum256([]byte(filled))
	return string(sum[:16])
}

// Clock tells limiters the time. Decisions use its Unix time.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock.
var SystemClock Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// FakeClock is a Clock for tests that only moves when told to. It is safe for
// concurrent use.
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewFakeClock returns a FakeClock that reads t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t} }

// Now returns the clock's time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
