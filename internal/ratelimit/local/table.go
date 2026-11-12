// Package local holds the in-memory limiters: GCRA, sliding-window counter,
// fixed window and concurrency (RL-01, RL-02, ADR-0017). They are safe for
// concurrent use, ignore ctx and return no error but ErrKeyCap. Their rates
// and bursts are per-replica values (RL-31); callers pass keys made with
// ratelimit.Key.
package local

import (
	"errors"
	"sync"
)

const shards = 64

// MaxKeys caps the keys of one limiter, so that client-chosen key values
// cannot grow its memory without bound: at most about 16 MB (ADR-0021, U1).
// It is enforced per shard, as shardKeys in each of the 64 (100,032 in all).
const (
	MaxKeys   = 100_000
	shardKeys = (MaxKeys + shards - 1) / shards
)

// ErrKeyCap is the error Allow returns, with a zero Decision and nothing
// taken, when key is new and its shard of the limiter already holds
// shardKeys keys. The caller skips the limit for that request (ADR-0021,
// U1). Refunds and releases never return it.
var ErrKeyCap = errors.New("local: limiter key cap reached")

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
// false to delete the entry. If f would add key to a shard that is full,
// nothing is stored and update returns ErrKeyCap; f's result must then be
// discarded.
func (t *table[V]) update(key string, f func(v V, ok bool) (V, bool)) error {
	var i byte
	if key != "" {
		i = key[0] % shards // keys are digests, so their first byte is uniform
	}
	s := &t.shards[i]
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	v, keep := f(v, ok)
	switch {
	case keep && !ok && len(s.m) >= shardKeys:
		return ErrKeyCap
	case keep:
		s.m[key] = v
	case ok:
		delete(s.m, key)
	}
	return nil
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
