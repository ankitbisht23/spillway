package local

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// ID is a limit's stable ID: policy name, algorithm and period, so
// "per-consumer|gcra|60s"; a concurrency limit has no period, so its policy
// name alone tells it apart (GW-29, ADR-0017). Degraded marks the in-memory
// limiter a Redis policy with failure_mode degraded falls back to (ADR-0021).
type ID struct {
	Policy    string
	Algorithm string
	Period    time.Duration // 0 for concurrency
	Degraded  bool
}

// LimitID returns the ID of limit l of local policy p.
func LimitID(p *config.Policy, l config.Limit) ID {
	return ID{Policy: p.Name, Algorithm: p.Algorithm, Period: l.Period}
}

// DegradedID returns the ID of the degraded limiter of limit l of Redis
// policy p.
func DegradedID(p *config.Policy, l config.Limit) ID {
	return ID{Policy: p.Name, Algorithm: p.Algorithm, Period: l.Period, Degraded: true}
}

// Registry holds the limiter of every policy limit, keyed by ID and kept
// outside any snapshot, so a reload keeps their state: TATs, window counts and
// in-flight counts (GW-29, ADR-0017).
type Registry struct {
	clock    ratelimit.Clock
	mu       sync.Mutex                               // serializes Apply
	limiters atomic.Pointer[map[ID]ratelimit.Limiter] // read without locking on every request
}

// NewRegistry returns an empty registry whose limiters read clock.
func NewRegistry(clock ratelimit.Clock) *Registry {
	r := &Registry{clock: clock}
	r.limiters.Store(&map[ID]ratelimit.Limiter{})
	return r
}

// Apply reconciles the registry with a new snapshot before it is swapped in
// (config.Watcher's apply hook). IDs that remain keep their state and take the
// snapshot's per-replica rate and burst (RL-31); new IDs start empty; removed
// ones are dropped. A request admitted by a removed concurrency limiter still
// releases into it, so its count drains. A Redis policy has a limiter here
// only with failure_mode degraded: its degraded limiter, at the per-replica
// rate and burst, whose state survives reloads and outages alike (RL-23,
// ADR-0021).
func (r *Registry) Apply(s *config.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := *r.limiters.Load()
	next := map[ID]ratelimit.Limiter{}
	for i := range s.Policies {
		p := &s.Policies[i]
		for _, l := range p.Limits {
			switch {
			case p.Local:
				id := LimitID(p, l)
				next[id] = r.newLimiter(p.Algorithm, l, old[id])
			case p.FailureMode == config.Degraded:
				id := DegradedID(p, l)
				next[id] = r.newLimiter(p.Algorithm, l.PerReplica(s.Replicas), old[id])
			}
		}
	}
	r.limiters.Store(&next)
}

// newLimiter builds the limiter for l, sharing old's state when old has the
// same algorithm.
func (r *Registry) newLimiter(algorithm string, l config.Limit, old ratelimit.Limiter) ratelimit.Limiter {
	switch algorithm {
	case config.GCRA:
		g := NewGCRA(r.clock, l.Rate, l.Period, l.Burst)
		if o, ok := old.(*GCRA); ok {
			g.state = o.state
		}
		return g
	case config.FixedWindow:
		f := NewFixedWindow(r.clock, l.Rate, l.Period)
		if o, ok := old.(*FixedWindow); ok {
			f.state = o.state
		}
		return f
	case config.SlidingWindow:
		sw := NewSlidingWindow(r.clock, l.Rate, l.Period)
		if o, ok := old.(*SlidingWindow); ok {
			sw.state = o.state
		}
		return sw
	default: // config.Concurrency; Compile allows no other algorithm
		c := NewConcurrency(l.Rate)
		if o, ok := old.(*Concurrency); ok {
			c.state = o.state
		}
		return c
	}
}

// Now is the time on the registry's clock, the one its limiters read.
func (r *Registry) Now() time.Time {
	return r.clock.Now()
}

// Limiter returns the limiter for id, or nil if the registry no longer has
// it: a request still on an older snapshot skips that limit (ADR-0017).
func (r *Registry) Limiter(id ID) ratelimit.Limiter {
	return (*r.limiters.Load())[id]
}

// Run drops idle keys from every limiter each interval until ctx ends; a
// dropped key behaves exactly like an idle one (ADR-0017).
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep()
		}
	}
}

func (r *Registry) sweep() {
	for _, l := range *r.limiters.Load() {
		if s, ok := l.(interface{ Sweep() }); ok {
			s.Sweep()
		}
	}
}
