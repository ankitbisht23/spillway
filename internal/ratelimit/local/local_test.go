package local

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// base is aligned to every period the tests use up to 1h.
var base = time.Unix(1_800_000_000, 0)

var ctx = context.Background()

func (t *table[V]) len() int {
	n := 0
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}

type step struct {
	advance time.Duration
	key     string
	cost    int64
	want    ratelimit.Decision
}

func runSteps(t *testing.T, clock *ratelimit.FakeClock, l ratelimit.Limiter, steps []step) {
	t.Helper()
	for i, s := range steps {
		clock.Advance(s.advance)
		key := s.key
		if key == "" {
			key = ratelimit.Key("k")
		}
		got, err := l.Allow(ctx, key, s.cost)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got != s.want {
			t.Fatalf("step %d (cost %d): got %+v, want %+v", i, s.cost, got, s.want)
		}
	}
}

// take calls l.Allow for its side effect; local limiters never return an
// error.
func take(l ratelimit.Limiter, key string, cost int64) ratelimit.Decision {
	d, _ := l.Allow(ctx, key, cost)
	return d
}

func allow(remaining int64, reset time.Duration) ratelimit.Decision {
	return ratelimit.Decision{Allowed: true, Remaining: remaining, ResetAfter: reset}
}

func deny(remaining int64, reset, retry time.Duration) ratelimit.Decision {
	return ratelimit.Decision{Remaining: remaining, ResetAfter: reset, RetryAfter: retry}
}

const ms = time.Millisecond

func TestGCRA(t *testing.T) {
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "burst, then one per T",
			steps: []step{
				{cost: 1, want: allow(4, 100*ms)},
				{cost: 1, want: allow(3, 200*ms)},
				{cost: 1, want: allow(2, 300*ms)},
				{cost: 1, want: allow(1, 400*ms)},
				{cost: 1, want: allow(0, 500*ms)},
				{cost: 1, want: deny(0, 500*ms, 100*ms)},
				{advance: 99 * ms, cost: 1, want: deny(0, 401*ms, 1*ms)},
				{advance: 1 * ms, cost: 1, want: allow(0, 500*ms)},
				{advance: 500 * ms, cost: 1, want: allow(4, 100*ms)},
			},
		},
		{
			name: "cost above 1",
			steps: []step{
				{cost: 3, want: allow(2, 300*ms)},
				{cost: 3, want: deny(2, 300*ms, 100*ms)},
				{cost: 2, want: allow(0, 500*ms)},
				{advance: 250 * ms, cost: 3, want: deny(2, 250*ms, 50*ms)},
				{cost: 6, want: deny(2, 250*ms, 350*ms)}, // above burst: never admitted
			},
		},
		{
			name: "refund",
			steps: []step{
				{cost: 5, want: allow(0, 500*ms)},
				{cost: -2, want: allow(2, 300*ms)},
				{cost: 2, want: allow(0, 500*ms)},
				{cost: 1, want: deny(0, 500*ms, 100*ms)},
				{advance: time.Second, cost: -5, want: allow(5, 0)}, // a refund never goes below empty
			},
		},
		{
			name: "refund of a missing key",
			steps: []step{
				{cost: -3, want: allow(5, 0)},
				{cost: 5, want: allow(0, 500*ms)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := ratelimit.NewFakeClock(base)
			runSteps(t, clock, NewGCRA(clock, 10, time.Second, 5), tt.steps)
		})
	}
}

func TestGCRARoundsTUp(t *testing.T) {
	g := NewGCRA(ratelimit.SystemClock, 3, time.Second, 3)
	if g.t != 333_333_334 || g.tau != 1_000_000_002 {
		t.Fatalf("T = %d, τ = %d", g.t, g.tau)
	}
}

func TestGCRASweep(t *testing.T) {
	clock := ratelimit.NewFakeClock(base)
	g := NewGCRA(clock, 10, time.Second, 5)
	take(g, ratelimit.Key("a"), 2)
	take(g, ratelimit.Key("b"), 5)
	take(g, ratelimit.Key("c"), 6) // denied: stores nothing
	if n := g.state.len(); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}
	clock.Advance(200 * ms)
	g.Sweep()
	if n := g.state.len(); n != 1 {
		t.Fatalf("after 200ms: entries = %d, want 1", n)
	}
	clock.Advance(300 * ms)
	g.Sweep()
	if n := g.state.len(); n != 0 {
		t.Fatalf("after 500ms: entries = %d, want 0", n)
	}
}

func TestFixedWindow(t *testing.T) {
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "limit per aligned window",
			steps: []step{
				{advance: 50 * time.Second, cost: 1, want: allow(2, 10*time.Second)},
				{cost: 1, want: allow(1, 10*time.Second)},
				{cost: 1, want: allow(0, 10*time.Second)},
				{advance: 4 * time.Second, cost: 1, want: deny(0, 6*time.Second, 6*time.Second)},
				// The next window starts at 60s: up to 2× the limit across a boundary.
				{advance: 6 * time.Second, cost: 3, want: allow(0, time.Minute)},
				{advance: 59 * time.Second, cost: 1, want: deny(0, time.Second, time.Second)},
			},
		},
		{
			name: "cost and refund",
			steps: []step{
				{cost: 2, want: allow(1, time.Minute)},
				{cost: 2, want: deny(1, time.Minute, time.Minute)},
				{cost: -1, want: allow(2, time.Minute)},
				{cost: 2, want: allow(0, time.Minute)},
				{cost: -5, want: allow(3, time.Minute)}, // never below 0
				// A refund that crosses a window boundary is lost.
				{cost: 3, want: allow(0, time.Minute)},
				{advance: time.Minute, cost: -3, want: allow(3, time.Minute)},
				{cost: 4, want: deny(3, time.Minute, time.Minute)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := ratelimit.NewFakeClock(base)
			runSteps(t, clock, NewFixedWindow(clock, 3, time.Minute), tt.steps)
		})
	}
}

func TestFixedWindowSweep(t *testing.T) {
	clock := ratelimit.NewFakeClock(base.Add(30 * time.Second))
	f := NewFixedWindow(clock, 3, time.Minute)
	take(f, ratelimit.Key("a"), 1)
	take(f, ratelimit.Key("b"), 4) // denied: stores nothing
	take(f, ratelimit.Key("c"), -1)
	clock.Advance(29 * time.Second)
	f.Sweep()
	if n := f.state.len(); n != 1 {
		t.Fatalf("before the window ends: entries = %d, want 1", n)
	}
	clock.Advance(time.Second)
	f.Sweep()
	if n := f.state.len(); n != 0 {
		t.Fatalf("after the window ends: entries = %d, want 0", n)
	}
}

func TestSlidingWindow(t *testing.T) {
	s := time.Second
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "full current window waits for the next one",
			steps: []step{
				{cost: 10, want: allow(0, 10*s)},
				// Fits once 10·(1 − e/10s) + 1 ≤ 10 in the next window: e = 1s.
				{cost: 1, want: deny(0, 10*s, 11*s)},
				{advance: 10 * s, cost: 1, want: deny(0, 10*s, s)},
				{advance: s - 1, cost: 1, want: deny(0, 9*s+1, 1)},
				{advance: 1, cost: 1, want: allow(0, 9*s)},
			},
		},
		{
			name: "previous window's weight decays",
			steps: []step{
				{cost: 10, want: allow(0, 10*s)},
				{advance: 15 * s, cost: 5, want: allow(0, 5*s)},
				// Fits once 10·(1 − e/10s) + 5 + 1 ≤ 10: e = 6s.
				{cost: 1, want: deny(0, 5*s, s)},
				{advance: s, cost: 1, want: allow(0, 4*s)},
				// Fits once 10·(1 − e/10s) + 6 + 3 ≤ 10: e = 9s.
				{cost: 3, want: deny(0, 4*s, 3*s)},
				{advance: 3 * s, cost: 3, want: allow(0, s)},
				// Fits in the next window once 9·(1 − e/10s) + 2 ≤ 10: 1s + 10s/9.
				{cost: 2, want: deny(0, s, 2_111_111_112)},
				{advance: 20 * s, cost: 0, want: allow(10, s)}, // two windows on: empty
			},
		},
		{
			name: "weighted previous count rounds up",
			steps: []step{
				{advance: 9 * s, cost: 1, want: allow(9, s)},
				// ⌈1·(1 − e/10s)⌉ = 1 until the window ends.
				{advance: s, cost: 10, want: deny(9, 10*s, 10*s)},
				{advance: 10*s - 1, cost: 10, want: deny(9, 1, 1)},
				{advance: 1, cost: 10, want: allow(0, 10*s)},
			},
		},
		{
			name: "refund",
			steps: []step{
				{cost: 10, want: allow(0, 10*s)},
				{cost: -4, want: allow(4, 10*s)},
				{cost: -20, want: allow(10, 10*s)}, // never below 0
				{cost: 6, want: allow(4, 10*s)},
				// A refund that crosses a window boundary is lost.
				{advance: 10 * s, cost: -6, want: allow(4, 10*s)},
			},
		},
		{
			name: "cost above the limit is never admitted",
			steps: []step{
				{cost: 11, want: deny(10, 10*s, 20*s)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := ratelimit.NewFakeClock(base)
			runSteps(t, clock, NewSlidingWindow(clock, 10, 10*time.Second), tt.steps)
		})
	}
}

func TestSlidingWindowSweep(t *testing.T) {
	clock := ratelimit.NewFakeClock(base)
	sw := NewSlidingWindow(clock, 10, 10*time.Second)
	take(sw, ratelimit.Key("a"), 1)
	clock.Advance(10 * time.Second)
	take(sw, ratelimit.Key("b"), 1)
	clock.Advance(10*time.Second - 1)
	sw.Sweep()
	if n := sw.state.len(); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}
	clock.Advance(1)
	sw.Sweep()
	if n := sw.state.len(); n != 1 {
		t.Fatalf("two windows after a: entries = %d, want 1", n)
	}
}

// TestGCRARefund: a refund gives back cost·T less the time since the
// admission (ADR-0028). The review's sequence at 10/s, burst 2 (T = 100 ms):
// A finds the key idle, B comes 10 ms later, A is refunded; giving back all
// of A's 100 ms would then admit C at once and D 100 ms after A, three in a
// 90 ms window where the bound is 2.9.
func TestGCRARefund(t *testing.T) {
	clock := ratelimit.NewFakeClock(base)
	g := NewGCRA(clock, 10, time.Second, 2)
	k := ratelimit.Key("k")
	a := clock.Now()
	take(g, k, 1)
	clock.Advance(10 * ms)
	take(g, k, 1) // B: TAT 200 ms after A
	clock.Advance(2 * ms)
	// 12 ms after A: 88 ms back, so the TAT is 112 ms after A.
	if d := g.Refund(k, 1, a); d != allow(1, 100*ms) {
		t.Fatalf("refund of A = %+v, want %+v", d, allow(1, 100*ms))
	}
	if !take(g, k, 1).Allowed { // C, 12 ms after A
		t.Fatal("C denied")
	}
	clock.Advance(88 * ms)
	if d := take(g, k, 1); d.Allowed || d.RetryAfter != 12*ms {
		t.Fatalf("D 100 ms after A = %+v, want a deny with RetryAfter 12ms: B, C and D would be 3 in 90 ms", d)
	}
}

// TestWindowRefund: a refund lands in the window of the admission it
// reverses, and is lost if another request has since moved the key into the
// next window, where it would erase that request's admission (ADR-0017).
func TestWindowRefund(t *testing.T) {
	type window interface {
		ratelimit.Limiter
		Refund(key string, cost int64, at time.Time) ratelimit.Decision
	}
	for name, newLimiter := range map[string]func(ratelimit.Clock) window{
		"fixed":   func(c ratelimit.Clock) window { return NewFixedWindow(c, 2, time.Second) },
		"sliding": func(c ratelimit.Clock) window { return NewSlidingWindow(c, 2, time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			clock := ratelimit.NewFakeClock(base)
			l := newLimiter(clock)
			k := ratelimit.Key("k")

			// In the same window the refund is taken.
			at := clock.Now()
			take(l, k, 2)
			if d := l.Refund(k, 1, at); !d.Allowed || d.Remaining != 1 {
				t.Fatalf("refund in the admission's window: %+v, want allowed, remaining 1", d)
			}
			if !take(l, k, 1).Allowed {
				t.Fatal("the refunded unit was not admitted")
			}

			// Admitted 1 ns before the next window, refunded after other
			// requests filled the next one: the refund is lost.
			clock.Advance(3*time.Second - 1)
			at = clock.Now()
			if !take(l, k, 1).Allowed {
				t.Fatal("first request denied")
			}
			clock.Advance(1)
			admitted := 0
			for take(l, k, 1).Allowed {
				if admitted++; admitted > 2 {
					t.Fatal("more than 2 admitted in the next window")
				}
			}
			if admitted == 0 {
				t.Fatal("nothing admitted in the next window")
			}
			l.Refund(k, 1, at)
			if d := take(l, k, 1); d.Allowed {
				t.Fatalf("after the old window's refund: %+v, want a deny: the window is full", d)
			}
		})
	}
}

func TestConcurrency(t *testing.T) {
	c := NewConcurrency(3)
	clock := ratelimit.NewFakeClock(base)
	runSteps(t, clock, c, []step{
		{cost: 2, want: allow(1, 0)},
		{cost: 2, want: deny(1, 0, 0)},
		{cost: 1, want: allow(0, 0)},
		{cost: -1, want: allow(1, 0)}, // a refund is a release
		{cost: 1, want: allow(0, 0)},
	})
	key := ratelimit.Key("k")
	c.Release(key, 2)
	if d, _ := c.Allow(ctx, key, 0); d.Remaining != 2 {
		t.Fatalf("after release: remaining = %d, want 2", d.Remaining)
	}
	c.Release(key, 1)
	if n := c.state.len(); n != 0 {
		t.Fatalf("entry kept at count 0")
	}
	// Releasing more than is in flight, or a key that has nothing in flight,
	// never goes negative.
	c.Release(key, 5)
	c.Release(ratelimit.Key("other"), 1)
	take(c, key, 1)
	c.Release(key, 4)
	if d, _ := c.Allow(ctx, key, 3); !d.Allowed || d.Remaining != 0 {
		t.Fatalf("after over-release: %+v, want allowed with 0 remaining", d)
	}
	if d, _ := c.Allow(ctx, key, 1); d.Allowed {
		t.Fatalf("admitted above the limit after over-release")
	}
}

// TestConcurrentCallers checks each limiter admits exactly its capacity when
// many goroutines call it, and sweeps run, at one instant.
func TestConcurrentCallers(t *testing.T) {
	clock := ratelimit.NewFakeClock(base)
	tests := []struct {
		name  string
		l     ratelimit.Limiter
		sweep func()
		want  int64
	}{
		{"gcra", NewGCRA(clock, 10, time.Second, 50), nil, 50},
		{"fixed window", NewFixedWindow(clock, 70, time.Second), nil, 70},
		{"sliding window", NewSlidingWindow(clock, 90, time.Second), nil, 90},
		{"concurrency", NewConcurrency(30), nil, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if s, ok := tt.l.(interface{ Sweep() }); ok {
				tt.sweep = s.Sweep
			}
			var admitted atomic.Int64
			var wg sync.WaitGroup
			for g := range 8 {
				wg.Go(func() {
					for i := range 100 {
						if tt.sweep != nil && i%10 == g {
							tt.sweep()
						}
						if d, _ := tt.l.Allow(ctx, ratelimit.Key("k"), 1); d.Allowed {
							admitted.Add(1)
						}
					}
				})
			}
			wg.Wait()
			if n := admitted.Load(); n != tt.want {
				t.Fatalf("admitted %d, want %d", n, tt.want)
			}
		})
	}
}

// TestKeyCap: a limiter holds at most shardKeys keys per shard. A new key in
// a full shard gets ErrKeyCap with nothing taken, while the shard's keys and
// other shards keep working, refunds and releases never fail, and a sweep or
// a release makes room again (ADR-0021, U1).
func TestKeyCap(t *testing.T) {
	type limiter interface {
		ratelimit.Limiter
		Sweep()
	}
	clock := ratelimit.NewFakeClock(base)
	for name, l := range map[string]ratelimit.Limiter{
		"gcra":        NewGCRA(clock, 1, time.Second, 1),
		"fixed":       NewFixedWindow(clock, 1, time.Second),
		"sliding":     NewSlidingWindow(clock, 1, time.Second),
		"concurrency": NewConcurrency(1),
	} {
		t.Run(name, func(t *testing.T) {
			// Shard 5's keys start with byte 5, 69, 133 or 197.
			key := func(i int) string { return string([]byte{5, byte(i), byte(i >> 8)}) }
			for i := range shardKeys {
				if d, err := l.Allow(ctx, key(i), 1); err != nil || !d.Allowed {
					t.Fatalf("key %d: %+v, %v", i, d, err)
				}
			}
			newKey := string([]byte{69, 0, 0})
			if d, err := l.Allow(ctx, newKey, 1); !errors.Is(err, ErrKeyCap) || d != (ratelimit.Decision{}) {
				t.Fatalf("a new key in a full shard: %+v, %v; want ErrKeyCap and a zero decision", d, err)
			}
			if d, err := l.Allow(ctx, key(0), 1); err != nil || d.Allowed {
				t.Errorf("a key the shard holds: %+v, %v; want a deny", d, err)
			}
			if d, err := l.Allow(ctx, string([]byte{6}), 1); err != nil || !d.Allowed {
				t.Errorf("a key in another shard: %+v, %v; want allowed", d, err)
			}
			if _, err := l.Allow(ctx, newKey, -1); err != nil {
				t.Errorf("a refund of a missing key in a full shard: %v", err)
			}
			if c, ok := l.(*Concurrency); ok {
				c.Release(key(0), 1)
			} else {
				clock.Advance(2 * time.Second)
				l.(limiter).Sweep()
			}
			if d, err := l.Allow(ctx, newKey, 1); err != nil || !d.Allowed {
				t.Errorf("a new key once there is room: %+v, %v; want allowed", d, err)
			}
		})
	}
}
