// Package local holds the in-memory limiters: GCRA, sliding-window counter,
// fixed window and concurrency (RL-01, RL-02, ADR-0017). They are safe for
// concurrent use, never return an error and ignore ctx. Their rates and
// bursts are per-replica values (RL-31); callers pass keys made with
// ratelimit.Key.
package local

import "sync"

const shards = 64

// table is per-key state in maps sharded by the key digest, each behind its
// own mutex.
type table[V any] struct {
	shards [shards]struct {
		mu sync.Mutex
		m  map[string]V
	}
}

func newTable[V any]() *table[V] {
	t := &table[V]{}
	for i := range t.shards {
		t.shards[i].m = make(map[string]V)
	}
	return t
}

// update runs f on key's state under its shard's lock. f gets the zero value
// and ok false when key has no entry; it returns the state to store, or keep
// false to delete the entry.
func (t *table[V]) update(key string, f func(v V, ok bool) (V, bool)) {
	var i byte
	if key != "" {
		i = key[0] % shards // keys are digests, so their first byte is uniform
	}
	s := &t.shards[i]
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if v, keep := f(v, ok); keep {
		s.m[key] = v
	} else if ok {
		delete(s.m, key)
	}
}

// sweep deletes every entry for which idle returns true.
func (t *table[V]) sweep(idle func(V) bool) {
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		for k, v := range s.m {
			if idle(v) {
				delete(s.m, k)
			}
		}
		s.mu.Unlock()
	}
}
