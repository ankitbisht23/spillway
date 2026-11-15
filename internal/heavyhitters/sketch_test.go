package heavyhitters

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// digest is a test key's ratelimit.Key.
func digest(i int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "client-%d", i))
	return string(sum[:16])
}

type truth struct{ denied, shadow int64 }

// TestSpaceSavingBounds checks Space-Saving's guarantees on random skewed
// streams of N events over two policies, with heartbeats that succeed
// (Take) and that fail (Take, then Restore) in between (RL-21, ADR-0041).
// Over the events since the last successful heartbeat: the Counts sum to
// N; every entry's Denied and ShadowDenied never overstate its key's
// events, and its Count never understates them nor overstates them by more
// than N/100; every key with more than N/100 events has an entry; a key
// with more than N/100 events more than another ranks above it.
func TestSpaceSavingBounds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		keys := rapid.IntRange(1, 400).Draw(t, "keys")
		skew := rapid.Float64Range(1, 6).Draw(t, "skew") // 1 uniform; higher, a few keys take most events
		n := rapid.IntRange(0, 3000).Draw(t, "events")
		s := New()
		counts := map[key]truth{}
		var total int64
		for range n {
			switch rapid.IntRange(0, 199).Draw(t, "op") {
			case 0: // a heartbeat that succeeds
				s.Take()
				clear(counts)
				total = 0
			case 1: // one that fails
				entries, since := s.Take()
				s.Restore(entries, since)
			}
			u := rapid.Float64Range(0, 1).Draw(t, "u")
			i := min(keys-1, int(float64(keys)*math.Pow(u, skew)))
			policy := int64(1 + i%2)
			shadow := rapid.Bool().Draw(t, "shadow")
			s.Add(policy, digest(i), shadow, int64(i), 0)
			k := key{policy: policy}
			copy(k.digest[:], digest(i))
			c := counts[k]
			if shadow {
				c.shadow++
			} else {
				c.denied++
			}
			counts[k] = c
			total++
		}
		entries, _ := s.Take()
		if len(entries) > Capacity {
			t.Fatalf("%d entries", len(entries))
		}
		var sum int64
		in := map[key]Entry{}
		bound := total / Capacity // N/k, rounded down: counts are whole
		for _, e := range entries {
			sum += e.Count
			k := key{e.Policy, e.Digest}
			in[k] = e
			c := counts[k]
			if e.Denied > c.denied || e.ShadowDenied > c.shadow || e.Denied+e.ShadowDenied < 1 {
				t.Fatalf("entry %+v overstates its key's events %+v", e, c)
			}
			if tr := c.denied + c.shadow; e.Count < tr || e.Count-tr > bound {
				t.Fatalf("entry %+v: count %d for %d events; error bound %d", e, e.Count, tr, bound)
			}
		}
		if sum != total {
			t.Fatalf("counts sum to %d over %d events", sum, total)
		}
		for k, c := range counts {
			if tr := c.denied + c.shadow; tr > bound {
				if _, ok := in[k]; !ok {
					t.Fatalf("a key with %d of %d events has no entry", tr, total)
				}
			}
		}
		for ka, a := range in {
			for kb, b := range in {
				ta, tb := counts[ka].denied+counts[ka].shadow, counts[kb].denied+counts[kb].shadow
				if ta > tb+bound && a.Count <= b.Count {
					t.Fatalf("%d events ranked %d, at or below %d events ranked %d", ta, a.Count, tb, b.Count)
				}
			}
		}
	})
}

// TestSketchCounts checks an entry's fields: denies and shadow
// would-denies apart, the latest event's consumer and key, a policy and
// digest per key, and the eviction rule.
func TestSketchCounts(t *testing.T) {
	s := New()
	s.Add(7, digest(1), false, 3, 30)
	s.Add(7, digest(1), true, 0, 0)
	s.Add(7, digest(1), false, 4, 40)
	s.Add(8, digest(1), true, 4, 40) // another policy: another entry
	entries, _ := s.Take()
	if len(entries) != 2 {
		t.Fatalf("entries %+v", entries)
	}
	if e := entries[0]; e.Policy != 7 || e.Denied != 2 || e.ShadowDenied != 1 || e.Count != 3 || e.Consumer != 4 || e.APIKey != 40 || string(e.Digest[:]) != digest(1) {
		t.Errorf("entry %+v", e)
	}
	if e := entries[1]; e.Policy != 8 || e.Denied != 0 || e.ShadowDenied != 1 || e.Count != 1 {
		t.Errorf("entry %+v", e)
	}

	// Full: a new key replaces the smallest count and inherits it.
	for i := range Capacity {
		for range i + 2 {
			s.Add(1, digest(i), false, 0, 0)
		}
	}
	s.Add(1, digest(Capacity), true, 9, 0)
	entries, _ = s.Take()
	if len(entries) != Capacity {
		t.Fatalf("%d entries", len(entries))
	}
	for _, e := range entries {
		if e.Digest == [16]byte([]byte(digest(0))) {
			t.Error("the smallest entry was kept")
		}
		if e.Digest == [16]byte([]byte(digest(Capacity))) && (e.Count != 3 || e.ShadowDenied != 1 || e.Denied != 0 || e.Consumer != 9) {
			t.Errorf("the new entry %+v, want count 2+1 and its own event only", e)
		}
	}
}

// TestTakeAndRestore checks the heartbeat's use of the sketch (ADR-0041):
// a successful heartbeat's Take resets it and starts a new interval; a
// failed one's Restore puts the entries back, keeps the newer IDs and the
// earlier interval start.
func TestTakeAndRestore(t *testing.T) {
	before := time.Now()
	s := New()
	s.Add(1, digest(1), false, 3, 30)
	entries, since := s.Take()
	if len(entries) != 1 || since.Before(before) {
		t.Fatalf("take: %+v %v", entries, since)
	}
	if again, next := s.Take(); len(again) != 0 || next.Before(since) {
		t.Fatalf("after a take: %+v %v", again, next)
	}
	s.Add(1, digest(1), true, 4, 40)
	s.Add(1, digest(2), false, 0, 0)
	s.Restore(entries, since)
	got, start := s.Take()
	if !start.Equal(since) || len(got) != 2 {
		t.Fatalf("restored: %+v from %v, want from %v", got, start, since)
	}
	if e := got[0]; e.Denied != 1 || e.ShadowDenied != 1 || e.Count != 2 || e.Consumer != 4 || e.APIKey != 40 {
		t.Errorf("restored entry %+v", e)
	}
}

// TestSketchConcurrent adds from many goroutines while heartbeats take
// and restore (run with -race): no event is lost or counted twice.
func TestSketchConcurrent(t *testing.T) {
	s := New()
	const workers, each = 8, 5000
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				s.Add(int64(w%2+1), digest(i%150), i%3 == 0, int64(w), 0)
			}
		})
	}
	var taken int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			entries, since := s.Take()
			if i%2 == 0 {
				s.Restore(entries, since)
				continue
			}
			for _, e := range entries {
				taken += e.Count
			}
		}
	}()
	wg.Wait()
	<-done
	rest, _ := s.Take()
	for _, e := range rest {
		taken += e.Count
	}
	if taken != workers*each {
		t.Errorf("counted %d events, want %d", taken, workers*each)
	}
}

// TestAddAllocations: counting an event allocates nothing, for a key the
// sketch tracks and for one that replaces another (the hot path's deny,
// ADR-0041).
func TestAddAllocations(t *testing.T) {
	s := New()
	keys := make([]string, 4*Capacity)
	for i := range keys {
		keys[i] = digest(i)
	}
	for i := range Capacity {
		s.Add(1, keys[i], false, 0, 0)
	}
	if a := testing.AllocsPerRun(1000, func() { s.Add(1, keys[0], false, 3, 4) }); a != 0 {
		t.Errorf("a tracked key: %v allocations", a)
	}
	i := 0
	if a := testing.AllocsPerRun(10000, func() { s.Add(1, keys[i%len(keys)], true, 3, 4); i++ }); a != 0 {
		t.Errorf("keys replacing others: %v allocations", a)
	}
}

// BenchmarkAdd is one event's cost: on a tracked key, on keys that keep
// replacing the smallest entry (every key new to a full sketch, as under
// a spread of abusive keys), and from all CPUs at once on one sketch.
func BenchmarkAdd(b *testing.B) {
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = digest(i)
	}
	b.Run("tracked", func(b *testing.B) {
		s := New()
		b.ReportAllocs()
		for b.Loop() {
			s.Add(1, keys[0], false, 3, 4)
		}
	})
	b.Run("replacing", func(b *testing.B) {
		s := New()
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			s.Add(1, keys[i%len(keys)], false, 3, 4)
			i++
		}
	})
	b.Run("parallel", func(b *testing.B) {
		s := New()
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				s.Add(1, keys[i%200], false, 3, 4)
				i++
			}
		})
	})
}

// TestPseudonym checks the reported digest (ADR-0053): HMAC-SHA256 of the
// limiter's key under the secret, cut to 16 bytes, 22 base64url
// characters; never the unkeyed digest that the limiter's own Redis key
// carries; the same for one secret wherever it is computed, and another
// under another secret or for another key.
func TestPseudonym(t *testing.T) {
	secret := []byte("dev-only-not-secret")
	key := digest(1)
	p := Pseudonym(secret, key)
	// HMAC-SHA256("dev-only-not-secret", ratelimit.Key("client-1")),
	// computed with openssl apart from this code.
	if p != "W9NTdi9m1FG65Nq5kR9D6g" {
		t.Errorf("Pseudonym = %q", p)
	}
	if b, err := base64.RawURLEncoding.Strict().DecodeString(p); err != nil || len(b) != 16 {
		t.Errorf("Pseudonym %q is not 16 bytes of base64url: %v", p, err)
	}
	if unkeyed := base64.RawURLEncoding.EncodeToString([]byte(key)); p == unkeyed {
		t.Error("the pseudonym is the unkeyed digest")
	}
	if Pseudonym(secret, key) != p || Pseudonym([]byte("dev-only-not-secret"), digest(1)) != p {
		t.Error("the pseudonym is not deterministic")
	}
	if Pseudonym([]byte("another-dev-only-not-secret"), key) == p || Pseudonym(secret, digest(2)) == p {
		t.Error("another secret or another key gives the same pseudonym")
	}
}
