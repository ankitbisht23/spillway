package main

import (
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
)

func TestBound(t *testing.T) {
	for w, want := range map[time.Duration]int64{
		10 * time.Millisecond:  101,
		100 * time.Millisecond: 110,
		time.Second:            200,
		10 * time.Second:       1100,
		time.Minute:            6100,
		5 * time.Millisecond:   100, // 100.5, rounded down
	} {
		if got := policy.bound(w); got != want {
			t.Errorf("bound(%v) = %d, want %d", w, got, want)
		}
	}
}

// TestOverAdmissionGCRA: a GCRA limiter of the harness policy, fed every
// pattern's arrivals for one key on a fake clock, never over-admits.
func TestOverAdmissionGCRA(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for _, p := range patterns {
		clock := ratelimit.NewFakeClock(t0)
		g := local.NewGCRA(clock, policy.rate, policy.period, policy.burst)
		var admitted []int64
		for _, a := range schedule(p, 1, time.Minute) {
			clock.Advance(t0.Add(a.at).Sub(clock.Now()))
			if d, _ := g.Allow(t.Context(), "k", 1); d.Allowed {
				admitted = append(admitted, clock.Now().UnixMicro())
			}
		}
		if over, w, at := overAdmission(admitted, policy); over != 0 {
			t.Errorf("%s: over-admission %d in a %v window from %d", p, over, w, at)
		}
		// 100 per second plus the burst, less what bursty's last 500 ms
		// leave unused.
		if n := len(admitted); n < 6040 || n > 6100 {
			t.Errorf("%s: %d admitted, want 6,040 to 6,100", p, n)
		}
	}
}

// TestOverAdmission: inputs that break the bound must show it, each in the
// window where it is largest.
func TestOverAdmission(t *testing.T) {
	steady := func(n int, every time.Duration) []int64 {
		var a []int64
		for i := range n {
			a = append(a, int64(i)*every.Microseconds())
		}
		return a
	}
	for _, tt := range []struct {
		name     string
		admitted []int64
		over     int64
		window   time.Duration
		at       int64
	}{
		{"empty", nil, 0, 0, 0},
		// The burst, then one per emission interval: exactly the bound.
		{"at the bound", append(make([]int64, 100), steady(6000, 10*time.Millisecond)[1:]...), 0, 0, 0},
		// The bound of the shortest window, 10 ms, is 101: 102 at one
		// instant is 1 over.
		{"burst + 2", make([]int64, 102), 1, 10 * time.Millisecond, 0},
		// Windows are closed: one more at their end counts, one just
		// after it does not.
		{"closed window", append(make([]int64, 102), 10_000), 2, 10 * time.Millisecond, 0},
		{"after the window", append(make([]int64, 102), 10_001), 1, 10 * time.Millisecond, 0},
		// 1 every 9 ms for a minute: within any w, w/9 ms + 1 against
		// 100 + w/10 ms; above it only from w > 9 s, most in 60 s.
		{"steady over the rate", steady(6667, 9*time.Millisecond), 6667 - 6100, time.Minute, 0},
	} {
		over, w, at := overAdmission(tt.admitted, policy)
		if over != tt.over || w != tt.window || at != tt.at {
			t.Errorf("%s: over-admission %d in a %v window from %d, want %d in %v from %d", tt.name, over, w, at, tt.over, tt.window, tt.at)
		}
	}
}
