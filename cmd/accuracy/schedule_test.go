package main

import (
	"testing"
	"time"
)

// TestSchedule checks each arrival pattern's shape (ADR-0021): every key at
// twice the limit, 200 requests per second, in due-time order.
func TestSchedule(t *testing.T) {
	const keys, d = 10, 60 * time.Second
	for _, p := range patterns {
		a := schedule(p, keys, d)
		perKey := make([][]time.Duration, keys)
		for i, x := range a {
			if i > 0 && x.at < a[i-1].at {
				t.Fatalf("%s: arrival %d at %v before %v", p, i, x.at, a[i-1].at)
			}
			if x.at < 0 || x.at >= d {
				t.Fatalf("%s: arrival at %v, outside [0, %v)", p, x.at, d)
			}
			perKey[x.key] = append(perKey[x.key], x.at)
		}
		for k, at := range perKey {
			if len(at) != 12000 {
				t.Errorf("%s: key %d has %d arrivals, want 12,000 (200/s for 60 s)", p, k, len(at))
			}
		}
	}

	uniform := schedule("uniform", keys, d)
	for k := range keys {
		var at []time.Duration
		for _, x := range uniform {
			if x.key == k {
				at = append(at, x.at)
			}
		}
		if at[0] != time.Duration(k)*500*time.Microsecond {
			t.Errorf("uniform: key %d starts at %v, want %v", k, at[0], time.Duration(k)*500*time.Microsecond)
		}
		for i := 1; i < len(at); i++ {
			if at[i]-at[i-1] != 5*time.Millisecond {
				t.Fatalf("uniform: key %d arrivals %d and %d are %v apart, want 5ms", k, i-1, i, at[i]-at[i-1])
			}
		}
	}

	// bursty: 100 arrivals per key at each instant, key k at k·50 ms
	// within every 500 ms.
	bursts := map[time.Duration]map[int]int{}
	for _, x := range schedule("bursty", keys, d) {
		if bursts[x.at] == nil {
			bursts[x.at] = map[int]int{}
		}
		bursts[x.at][x.key]++
	}
	if len(bursts) != keys*120 {
		t.Errorf("bursty: %d instants, want %d", len(bursts), keys*120)
	}
	for at, byKey := range bursts {
		for k, n := range byKey {
			if n != 100 || len(byKey) != 1 || at%(500*time.Millisecond) != time.Duration(k)*50*time.Millisecond {
				t.Fatalf("bursty: at %v, key %d sends %d (of %d keys), want 100 from key %d only", at, k, n, len(byKey), int(at%(500*time.Millisecond)/(50*time.Millisecond)))
			}
		}
	}

	// boundary: every arrival within 10 ms of a whole second, 100 per key
	// on each side of each second inside the run, 0.1 ms apart.
	sides := map[int]map[bool]int{} // per second, before or after
	for _, x := range schedule("boundary", 1, d) {
		s := (x.at + 10*time.Millisecond) / time.Second
		off := x.at - s*time.Second
		if off < -10*time.Millisecond || off >= 10*time.Millisecond || off%(100*time.Microsecond) != 0 {
			t.Fatalf("boundary: arrival at %v, %v from second %d", x.at, off, s)
		}
		if sides[int(s)] == nil {
			sides[int(s)] = map[bool]int{}
		}
		sides[int(s)][off < 0]++
	}
	for s := 0; s <= 60; s++ {
		before, after := 100, 100
		if s == 0 {
			before = 0 // before the run
		}
		if s == 60 {
			after = 0 // after it
		}
		if sides[s][true] != before || sides[s][false] != after {
			t.Errorf("boundary: second %d has %d before and %d after, want %d and %d", s, sides[s][true], sides[s][false], before, after)
		}
	}
}
