package resilience

import "time"

// Window counts outcomes over a rolling duration in a fixed number of
// buckets of equal width (ADR-0030). The counts at a time cover the bucket
// it falls in and the buckets before it, so they span between d − d/buckets
// and d. A Window is not safe for concurrent use: its owner locks.
type Window struct {
	width   int64 // nanoseconds per bucket
	buckets []bucket
}

type bucket struct {
	idx           int64 // the bucket's start, in widths since the Unix epoch
	total, failed int64
}

// NewWindow returns an empty window of d in buckets buckets.
func NewWindow(d time.Duration, buckets int) *Window {
	return &Window{width: int64(d) / int64(buckets), buckets: make([]bucket, buckets)}
}

// Add counts one outcome at now.
func (w *Window) Add(now time.Time, failed bool) {
	idx := now.UnixNano() / w.width
	b := &w.buckets[idx%int64(len(w.buckets))]
	if b.idx != idx {
		*b = bucket{idx: idx}
	}
	b.total++
	if failed {
		b.failed++
	}
}

// Counts returns the outcomes counted in the window that ends with now's
// bucket.
func (w *Window) Counts(now time.Time) (total, failed int64) {
	idx := now.UnixNano() / w.width
	for _, b := range w.buckets {
		if b.idx <= idx && b.idx > idx-int64(len(w.buckets)) {
			total += b.total
			failed += b.failed
		}
	}
	return total, failed
}

// Reset empties the window.
func (w *Window) Reset() {
	clear(w.buckets)
}
