package resilience

import "sync/atomic"

// Gate counts what is in use against a cap and never waits: past the cap,
// TryAcquire fails at once (ADR-0030). The zero Gate is empty. It is safe
// for concurrent use.
type Gate struct {
	n atomic.Int64
}

// TryAcquire takes a place if fewer than limit are in use.
func (g *Gate) TryAcquire(limit int64) bool {
	for {
		n := g.n.Load()
		if n >= limit {
			return false
		}
		if g.n.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// Release gives back a place TryAcquire took.
func (g *Gate) Release() {
	g.n.Add(-1)
}

// InUse returns the places taken.
func (g *Gate) InUse() int64 {
	return g.n.Load()
}
