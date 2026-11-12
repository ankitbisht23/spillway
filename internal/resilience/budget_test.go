package resilience

import (
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// TestWindow: counts cover the current bucket and the ones before it, a
// bucket expires as a whole once it is a window old, and Reset empties it.
func TestWindow(t *testing.T) {
	w := NewWindow(time.Second, 10) // buckets of 100 ms
	w.Add(t0, false)
	w.Add(t0.Add(50*time.Millisecond), true)
	w.Add(t0.Add(950*time.Millisecond), true)
	for _, tt := range []struct {
		at            time.Duration
		total, failed int64
	}{
		{0, 2, 1},
		{999 * time.Millisecond, 3, 2},
		{time.Second, 1, 1},             // t0's bucket is a window old
		{1899 * time.Millisecond, 1, 1}, // the last instant of 950 ms's bucket's window
		{1900 * time.Millisecond, 0, 0},
		{-time.Millisecond, 0, 0}, // before every bucket
	} {
		if total, failed := w.Counts(t0.Add(tt.at)); total != tt.total || failed != tt.failed {
			t.Errorf("at %v: %d total, %d failed; want %d, %d", tt.at, total, failed, tt.total, tt.failed)
		}
	}
	w.Add(t0.Add(3*time.Second), false) // reuses a stale bucket's slot
	if total, _ := w.Counts(t0.Add(3 * time.Second)); total != 1 {
		t.Errorf("after a stale slot was reused: %d total, want 1", total)
	}
	w.Reset()
	if total, _ := w.Counts(t0.Add(3 * time.Second)); total != 0 {
		t.Errorf("after Reset: %d total, want 0", total)
	}
}

// grant asks b for n retries at now and returns how many it granted.
func grant(b *Budget, now time.Time, n int, ratio, floor float64) int {
	granted := 0
	for range n {
		if b.TryRetry(now, ratio, floor) {
			granted++
		}
	}
	return granted
}

// TestBudgetFormula: with no requests the floor alone grants
// min_per_sec × 10 retries per 10 s; with requests the ratio adds to it
// (GW-13: "plus a floor"), R + 1 ≤ ratio × Q + min_per_sec × 10 exactly.
func TestBudgetFormula(t *testing.T) {
	tests := []struct {
		requests     int
		ratio, floor float64
		want         int
	}{
		{0, 0.2, 3, 30},
		{1, 0.2, 3, 30},  // 30.2
		{5, 0.2, 3, 31},  // 31
		{14, 0.2, 3, 32}, // 32.8
		{15, 0.2, 3, 33}, // 33, a whole number despite float rounding
		{1000, 0.2, 3, 230},
		{1000, 0.2, 0.1, 201},
		{7, 1, 0.5, 12},
		{3, 0.1, 0.1, 1}, // 1.3
	}
	for _, tt := range tests {
		clock := ratelimit.NewFakeClock(t0)
		b := NewBudget()
		for range tt.requests {
			b.Request(clock.Now())
		}
		if got := grant(b, clock.Now(), 10_000, tt.ratio, tt.floor); got != tt.want {
			t.Errorf("%d requests, ratio %v, floor %v/s: %d retries granted, want %d", tt.requests, tt.ratio, tt.floor, got, tt.want)
		}
		if q, r := b.Counts(clock.Now()); q != int64(tt.requests) || r != int64(tt.want) {
			t.Errorf("counts %d requests, %d retries; want %d, %d (refusals are not counted)", q, r, tt.requests, tt.want)
		}
	}
}

// TestBudgetRolls: the budget is over the last 10 s, in buckets of 100 ms
// that expire whole; exhausted, it grants again as old retries leave the
// window, and requests leave it the same way. Retries never count as
// requests.
func TestBudgetRolls(t *testing.T) {
	clock := ratelimit.NewFakeClock(t0)
	b := NewBudget()
	for range 50 {
		b.Request(clock.Now())
	}
	if got := grant(b, clock.Now(), 100, 0.2, 3); got != 40 { // 0.2 × 50 + 30
		t.Fatalf("at 0 s: %d granted, want 40", got)
	}
	clock.Advance(5 * time.Second)
	if got := grant(b, clock.Now(), 100, 0.2, 3); got != 0 {
		t.Fatalf("at 5 s: %d granted, want 0: the window still holds 40", got)
	}
	clock.Advance(5*time.Second - time.Nanosecond) // the last instant of t0's bucket's window
	if got := grant(b, clock.Now(), 100, 0.2, 3); got != 0 {
		t.Fatalf("just before 10 s: %d granted, want 0", got)
	}
	if q, r := b.Counts(clock.Now()); q != 50 || r != 40 {
		t.Fatalf("just before 10 s: %d requests, %d retries; want 50, 40", q, r)
	}
	clock.Advance(time.Nanosecond) // t0's bucket leaves: requests and retries both
	if q, r := b.Counts(clock.Now()); q != 0 || r != 0 {
		t.Fatalf("at 10 s: %d requests, %d retries; want 0, 0", q, r)
	}
	if got := grant(b, clock.Now(), 100, 0.2, 3); got != 30 {
		t.Fatalf("at 10 s: %d granted, want the floor's 30", got)
	}
	// Spread over time: 3 retries a second are always granted, and once
	// the window holds 10 such seconds, a 4th is not.
	clock.Advance(BudgetWindow)
	for i := range 30 {
		clock.Advance(time.Second)
		if got := grant(b, clock.Now(), 3, 0.2, 3); got != 3 {
			t.Fatalf("second %d: %d of 3 granted at the floor's rate", i, got)
		}
		if i >= 9 && b.TryRetry(clock.Now(), 0.2, 3) {
			t.Fatalf("second %d: a 4th retry granted with 30 in the window", i)
		}
	}
}

// TestBudgetConcurrent: concurrent retries never exceed the formula.
func TestBudgetConcurrent(t *testing.T) {
	b := NewBudget()
	var granted sync.WaitGroup
	var mu sync.Mutex
	n := 0
	for range 16 {
		granted.Go(func() {
			for range 100 {
				b.Request(t0)
				if b.TryRetry(t0, 0.2, 3) {
					mu.Lock()
					n++
					mu.Unlock()
				}
			}
		})
	}
	granted.Wait()
	if q, r := b.Counts(t0); q != 1600 || r != int64(n) || n > 0.2*1600+30 || n < 30 {
		t.Errorf("%d requests, %d retries counted, %d granted; want 1600 requests and 30 to 350 retries, as counted", q, r, n)
	}
}
