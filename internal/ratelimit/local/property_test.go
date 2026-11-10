package local

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// Property tests: each algorithm against its own bound (design Testing,
// Property) for any arrival pattern. go test -run Property runs them all.

type arrival struct {
	key  int
	at   int64 // Unix ns
	cost int64
}

// drive runs an arbitrary arrival pattern over three keys against the limiter
// newLimiter returns, with its sweep, and returns the admitted arrivals in time order. Sweeps run at random points.
// After some denies of a cost up to capacity it checks that RetryAfter is
// exact: the same request is still denied 1 ns before it and admitted at it.
func drive(t *rapid.T, period time.Duration, capacity int64, newLimiter func(ratelimit.Clock) (ratelimit.Limiter, func())) []arrival {
	clock := ratelimit.NewFakeClock(base.Add(time.Duration(rapid.Int64Range(0, int64(period)-1).Draw(t, "offset"))))
	l, sweep := newLimiter(clock)
	keys := []string{ratelimit.Key("a"), ratelimit.Key("b"), ratelimit.Key("c")}
	gap := rapid.OneOf(
		rapid.Just(int64(0)),
		rapid.Int64Range(0, int64(period)/8),
		rapid.Int64Range(0, 2*int64(period)),
	)
	var admitted []arrival
	try := func(a arrival) ratelimit.Decision {
		d, _ := l.Allow(ctx, keys[a.key], a.cost)
		if d.Allowed {
			admitted = append(admitted, a)
		}
		return d
	}
	n := rapid.IntRange(1, 300).Draw(t, "n")
	for range n {
		clock.Advance(time.Duration(gap.Draw(t, "gap")))
		if rapid.IntRange(0, 9).Draw(t, "sweep") == 0 {
			sweep()
		}
		a := arrival{
			key:  rapid.IntRange(0, len(keys)-1).Draw(t, "key"),
			at:   clock.Now().UnixNano(),
			cost: rapid.Int64Range(1, capacity+1).Draw(t, "cost"),
		}
		d := try(a)
		if d.Allowed || a.cost > capacity || !rapid.Bool().Draw(t, "probe") {
			continue
		}
		if d.RetryAfter <= 0 {
			t.Fatalf("deny of cost %d with RetryAfter %v", a.cost, d.RetryAfter)
		}
		clock.Advance(d.RetryAfter - 1)
		a.at = clock.Now().UnixNano()
		if try(a).Allowed {
			t.Fatalf("cost %d admitted 1ns before RetryAfter %v", a.cost, d.RetryAfter)
		}
		clock.Advance(1)
		a.at++
		if !try(a).Allowed {
			t.Fatalf("cost %d denied at RetryAfter %v", a.cost, d.RetryAfter)
		}
	}
	return admitted
}

// TestPropertyGCRA: admitted in any closed window w ≤ burst + w·rate/period.
func TestPropertyGCRA(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		rate := rapid.Int64Range(1, 20).Draw(t, "rate")
		period := time.Duration(rapid.Int64Range(1, 5).Draw(t, "period_s")) * time.Second
		burst := rapid.Int64Range(1, 30).Draw(t, "burst")
		admitted := drive(t, period, burst, func(c ratelimit.Clock) (ratelimit.Limiter, func()) {
			g := NewGCRA(c, rate, period, burst)
			return g, g.Sweep
		})
		// The fullest closed window starts and ends at admissions.
		for i, a := range admitted {
			sum := int64(0)
			for _, b := range admitted[i:] {
				if b.key != a.key {
					continue
				}
				sum += b.cost
				if w := b.at - a.at; sum*int64(period) > burst*int64(period)+w*rate {
					t.Fatalf("admitted %d in a closed window of %v; bound %d + %v·%d/%v", sum, time.Duration(w), burst, time.Duration(w), rate, period)
				}
			}
		}
	})
}

// TestPropertyFixedWindow: admitted ≤ limit per aligned window.
func TestPropertyFixedWindow(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		rate := rapid.Int64Range(1, 30).Draw(t, "rate")
		period := time.Duration(rapid.Int64Range(1, 5).Draw(t, "period_s")) * time.Second
		admitted := drive(t, period, rate, func(c ratelimit.Clock) (ratelimit.Limiter, func()) {
			f := NewFixedWindow(c, rate, period)
			return f, f.Sweep
		})
		sums := map[[2]int64]int64{}
		for _, a := range admitted {
			w := [2]int64{int64(a.key), a.at / int64(period)}
			if sums[w] += a.cost; sums[w] > rate {
				t.Fatalf("admitted %d in aligned window %d, limit %d", sums[w], w[1], rate)
			}
		}
	})
}

// TestPropertySlidingWindow: the estimate is ≤ limit at every admission, and
// admitted in any true window ≤ 2 × limit.
func TestPropertySlidingWindow(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		rate := rapid.Int64Range(1, 30).Draw(t, "rate")
		period := time.Duration(rapid.Int64Range(1, 5).Draw(t, "period_s")) * time.Second
		p := int64(period)
		admitted := drive(t, period, rate, func(c ratelimit.Clock) (ratelimit.Limiter, func()) {
			sw := NewSlidingWindow(c, rate, period)
			return sw, sw.Sweep
		})
		for i, a := range admitted {
			// The estimate from the admitted history, this admission included:
			// prev·(1 − e/period) + cur ≤ rate, multiplied out by period.
			start := a.at - a.at%p
			var prev, cur int64
			for _, b := range admitted[:i+1] {
				switch {
				case b.key != a.key:
				case b.at >= start:
					cur += b.cost
				case b.at >= start-p:
					prev += b.cost
				}
			}
			if prev*(p-(a.at-start)) > (rate-cur)*p {
				t.Fatalf("estimate %d·(1 − %v/%v) + %d above limit %d", prev, time.Duration(a.at-start), period, cur, rate)
			}
		}
		for i, a := range admitted {
			sum := int64(0)
			for _, b := range admitted[i:] {
				if b.at-a.at >= p {
					break
				}
				if b.key != a.key {
					continue
				}
				if sum += b.cost; sum > 2*rate {
					t.Fatalf("admitted %d in a true window of %v, limit %d", sum, period, rate)
				}
			}
		}
	})
}

// TestPropertyConcurrency: in-flight ≤ limit under concurrent goroutines that
// each take and release slots, and every slot is back once they finish.
func TestPropertyConcurrency(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		limit := rapid.Int64Range(1, 10).Draw(t, "limit")
		costs := rapid.SliceOfN(rapid.SliceOfN(rapid.Int64Range(1, limit+1), 1, 50), 2, 16).Draw(t, "costs")
		c := NewConcurrency(limit)
		key := ratelimit.Key("k")
		var inflight, peak atomic.Int64
		var wg sync.WaitGroup
		for _, mine := range costs {
			wg.Go(func() {
				for _, cost := range mine {
					if d, _ := c.Allow(ctx, key, cost); !d.Allowed {
						continue
					}
					n := inflight.Add(cost)
					for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
					}
					runtime.Gosched()
					inflight.Add(-cost)
					c.Release(key, cost)
				}
			})
		}
		wg.Wait()
		if p := peak.Load(); p > limit {
			t.Fatalf("%d in flight, limit %d", p, limit)
		}
		if n := c.state.len(); n != 0 {
			t.Fatalf("%d entries left after every release", n)
		}
	})
}
