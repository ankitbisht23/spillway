// Package heavyhitters keeps a gateway's top throttled keys (RL-21,
// ADR-0041): one Space-Saving sketch of 100 counters over all policies,
// keyed by (policy ID, key digest), that each heartbeat reports as top_keys.
package heavyhitters

import (
	"encoding/base64"
	"slices"
	"sync"
	"time"
)

// Capacity is the sketch's number of counters (RL-21).
const Capacity = 100

// Entry is one counter of the sketch.
type Entry struct {
	Policy int64 // the policy's control-plane ID
	// Digest is the first 16 bytes of the SHA-256 of the policy's filled
	// key template (ratelimit.Key): never the raw key.
	Digest [16]byte
	// Consumer and APIKey are the IDs of the latest event's consumer and
	// API key; 0 when anonymous or unknown.
	Consumer, APIKey int64
	// Denied and ShadowDenied count the events since the entry entered
	// the sketch: enforce denies and shadow would-denies. They never
	// overstate the key's events.
	Denied, ShadowDenied int64
	// Count is Space-Saving's counter: the events since the entry entered
	// plus the count it inherited then, an upper bound of the key's events
	// in the interval. It orders entries for eviction.
	Count int64
}

// KeyDigest is the entry's key digest as top_keys carry it: base64url
// without padding, 22 characters (ADR-0041).
func (e Entry) KeyDigest() string {
	return base64.RawURLEncoding.EncodeToString(e.Digest[:])
}

type key struct {
	policy int64
	digest [16]byte
}

// Sketch is a Space-Saving sketch of Capacity counters (Metwally et al.,
// 2005). After N events, every key with more than N/Capacity of them has
// an entry, and every entry's Count exceeds its key's events by at most
// N/Capacity. It is safe for concurrent use.
type Sketch struct {
	mu      sync.Mutex
	entries []Entry
	index   map[key]int // into entries
	since   time.Time   // the start of the interval
}

// New returns an empty sketch whose interval starts now.
func New() *Sketch {
	return &Sketch{entries: make([]Entry, 0, Capacity), index: make(map[key]int, Capacity), since: time.Now()}
}

// Add counts one event (ADR-0041): a deny of policy's limit for the key
// whose digest is digest (ratelimit.Key, 16 bytes), or with shadow a
// would-deny, of a request by consumer with apiKey (0 for none).
func (s *Sketch) Add(policy int64, digest string, shadow bool, consumer, apiKey int64) {
	e := Entry{Policy: policy, Consumer: consumer, APIKey: apiKey, Count: 1}
	copy(e.Digest[:], digest)
	if shadow {
		e.ShadowDenied = 1
	} else {
		e.Denied = 1
	}
	s.mu.Lock()
	s.add(&e, true)
	s.mu.Unlock()
}

// add is one weighted Space-Saving update with e's counts: into its key's
// entry if there is one (its IDs replacing the entry's if newer), else into
// a new entry while there is room, else in place of the entry with the
// smallest Count, whose Count it inherits.
func (s *Sketch) add(e *Entry, newer bool) {
	k := key{e.Policy, e.Digest}
	if i, ok := s.index[k]; ok {
		x := &s.entries[i]
		x.Count += e.Count
		x.Denied += e.Denied
		x.ShadowDenied += e.ShadowDenied
		if newer {
			x.Consumer, x.APIKey = e.Consumer, e.APIKey
		}
		return
	}
	if len(s.entries) < Capacity {
		s.index[k] = len(s.entries)
		s.entries = append(s.entries, *e)
		return
	}
	m := 0
	for i := 1; i < len(s.entries); i++ {
		if s.entries[i].Count < s.entries[m].Count {
			m = i
		}
	}
	x := &s.entries[m]
	delete(s.index, key{x.Policy, x.Digest})
	inherited := x.Count
	*x = *e
	x.Count += inherited
	s.index[k] = m
}

// Take returns the entries and the start of their interval, and empties
// the sketch, whose next interval starts now. A heartbeat takes them; if
// it fails, Restore puts them back.
func (s *Sketch) Take() ([]Entry, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, since := slices.Clone(s.entries), s.since
	s.entries = s.entries[:0]
	clear(s.index)
	s.since = time.Now()
	return out, since
}

// Restore adds entries, which Take returned with since, back into the
// sketch, as weighted updates older than what it counted meanwhile, and
// starts its interval at since again: a failed heartbeat loses nothing
// beyond what Capacity counters can hold (ADR-0041).
func (s *Sketch) Restore(entries []Entry, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range entries {
		s.add(&entries[i], false)
	}
	s.since = since
}
