package local

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// ID is a limit's stable ID: the policy's Ref (its name in static mode, its
// ID in managed mode, ADR-0050), algorithm and period, so
// "per-consumer|gcra|60s"; a concurrency limit has no period, so its policy
// alone tells it apart (GW-29, ADR-0017). Degraded marks the in-memory
// limiter a Redis policy with failure_mode degraded falls back to
// (ADR-0021).
type ID struct {
	Policy    string
	Algorithm string
	Period    time.Duration // 0 for concurrency
	Degraded  bool
}

// LimitID returns the ID of limit l of local policy p.
func LimitID(p *config.Policy, l config.Limit) ID {
	return ID{Policy: p.Ref, Algorithm: p.Algorithm, Period: l.Period}
}

// DegradedID returns the ID of the degraded limiter of limit l of Redis
// policy p.
func DegradedID(p *config.Policy, l config.Limit) ID {
	return ID{Policy: p.Ref, Algorithm: p.Algorithm, Period: l.Period, Degraded: true}
}

// Registry holds the limiter of every policy limit, keyed by ID and kept
// outside any snapshot, so a reload keeps their state: TATs, window counts and
// in-flight counts (GW-29, ADR-0017). A limiter keeps the state of one
// period; each call gives the rate and burst it is checked against
// (ForLimit), which plans and overrides choose per consumer (RL-08,
// ADR-0041).
type Registry struct {
	clock ratelimit.Clock
	mu    sync.Mutex          // serializes Apply and For's additions
	set   atomic.Pointer[set] // read without locking on every request
}

// set is the registry's limiters. policies holds the ID, with period 0, of
// every policy that has limiters, so that For can add one for a period that
// only an override has; dynamic marks the limiters For added.
type set struct {
	limiters map[ID]ratelimit.Limiter
	policies map[ID]bool
	dynamic  map[ID]bool
}

// NewRegistry returns an empty registry whose limiters read clock.
func NewRegistry(clock ratelimit.Clock) *Registry {
	r := &Registry{clock: clock}
	r.set.Store(&set{limiters: map[ID]ratelimit.Limiter{}})
	return r
}

// Apply reconciles the registry with a new snapshot before it is swapped in
// (config.Watcher's apply hook). IDs that remain keep their state and take the
// snapshot's per-replica rate and burst (RL-31); new IDs start empty; removed
// ones are dropped. A request admitted by a removed concurrency limiter still
// releases into it, so its count drains. A Redis policy has a limiter here
// only with failure_mode degraded: its degraded limiter, at the per-replica
// rate and burst, whose state survives reloads and outages alike (RL-23,
// ADR-0021). The limits are a policy's own and its plans' (RL-08); a
// limiter For added for an override's period stays while its policy does.
func (r *Registry) Apply(s *config.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.set.Load()
	next := &set{limiters: map[ID]ratelimit.Limiter{}, policies: map[ID]bool{}, dynamic: map[ID]bool{}}
	add := func(id ID, algorithm string, l config.Limit) {
		if _, ok := next.limiters[id]; !ok {
			next.limiters[id] = r.newLimiter(algorithm, l, old.limiters[id])
		}
	}
	for i := range s.Policies {
		p := &s.Policies[i]
		if !p.Local && p.FailureMode != config.Degraded {
			continue
		}
		for _, ls := range append([][]config.Limit{p.Limits}, mapValues(p.PlanLimits)...) {
			for _, l := range ls {
				if p.Local {
					add(LimitID(p, l), p.Algorithm, l)
				} else {
					add(DegradedID(p, l), p.Algorithm, l.PerReplica(s.Replicas))
				}
			}
		}
		next.policies[ID{Policy: p.Ref, Algorithm: p.Algorithm, Degraded: !p.Local}] = true
	}
	for id := range old.dynamic {
		pid := id
		pid.Period = 0
		if _, declared := next.limiters[id]; !declared && next.policies[pid] {
			next.limiters[id], next.dynamic[id] = old.limiters[id], true
		}
	}
	r.set.Store(next)
}

// mapValues returns m's values in key order, so Apply is deterministic.
func mapValues(m map[int64][]config.Limit) [][]config.Limit {
	var out [][]config.Limit
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return out
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
	return r.set.Load().limiters[id]
}

// For returns the limiter for id checked against l (ForLimit). For a period
// no policy or plan of the snapshot has, an override's, it adds a limiter
// that starts empty while the policy is in the snapshot; otherwise it
// returns nil, and the request skips that limit, as Limiter's callers do.
func (r *Registry) For(id ID, l config.Limit) ratelimit.Limiter {
	s := r.set.Load()
	if lim, ok := s.limiters[id]; ok {
		return ForLimit(lim, l)
	}
	pid := id
	pid.Period = 0
	if !s.policies[pid] {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s = r.set.Load()
	if lim, ok := s.limiters[id]; ok {
		return ForLimit(lim, l)
	}
	if !s.policies[pid] {
		return nil
	}
	next := &set{limiters: maps.Clone(s.limiters), policies: s.policies, dynamic: maps.Clone(s.dynamic)}
	if next.dynamic == nil {
		next.dynamic = map[ID]bool{}
	}
	lim := r.newLimiter(id.Algorithm, l, nil)
	next.limiters[id], next.dynamic[id] = lim, true
	r.set.Store(next)
	return lim
}

// ForLimit returns limiter checked against l, a limit of its period: the
// same state, with l's rate and burst (RL-08, ADR-0041). It is limiter
// itself when l is the limit it was built with, which a policy's own
// limits are.
func ForLimit(limiter ratelimit.Limiter, l config.Limit) ratelimit.Limiter {
	switch lim := limiter.(type) {
	case *GCRA:
		t := (int64(l.Period) + l.Rate - 1) / l.Rate
		if t == lim.t && t*l.Burst == lim.tau {
			return lim
		}
		c := *lim
		c.t, c.tau = t, t*l.Burst
		return &c
	case *FixedWindow:
		if l.Rate == lim.rate {
			return lim
		}
		c := *lim
		c.rate = l.Rate
		return &c
	case *SlidingWindow:
		if l.Rate == lim.rate {
			return lim
		}
		c := *lim
		c.rate = l.Rate
		return &c
	case *Concurrency:
		if l.Rate == lim.limit {
			return lim
		}
		c := *lim
		c.limit = l.Rate
		return &c
	}
	return limiter
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
	for _, l := range r.set.Load().limiters {
		if s, ok := l.(interface{ Sweep() }); ok {
			s.Sweep()
		}
	}
}
