package proxy

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"

	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// The deny cache (RL-19, ADR-0069): after an enforce limit denies a request
// in Redis with retry-after T, this replica denies that limit's key locally
// for min(T, 10 s) without calling Redis.
const (
	denyCacheMaxHold   = 10 * time.Second
	denyCacheShards    = 64
	denyCacheShardSize = 1024 // entries per shard: 65,536 in all
)

// Deny cache outcomes, the result label of gw_ratelimit_deny_cache_total.
const (
	denyCacheHit    = "hit"
	denyCacheInsert = "insert"
	denyCacheFull   = "full"
)

// denySeed hashes Redis keys to shards.
var denySeed = maphash.MakeSeed()

// denyCache holds one replica's cached denies. Its entries belong to the
// config snapshot and identity they were made on: the first request on a
// newer pair replaces every shard (a new generation), and a request still on
// an older pair neither reads nor writes the current one (ADR-0069,
// "Clearing").
type denyCache struct {
	cur atomic.Pointer[denyGeneration]
}

// denyGeneration is the cache for one snapshot and identity, by their Seq.
type denyGeneration struct {
	snapshot, identity uint64
	shards             [denyCacheShards]denyShard
	entries            atomic.Int64 // in all shards, expired ones not yet removed included
}

type denyShard struct {
	mu sync.Mutex
	m  map[string]denyEntry
}

// denyEntry is a cached deny of one limit, by the limit's Redis key.
type denyEntry struct {
	until time.Time // the deny holds while now < until
	reset time.Time // when the limit's state is back to empty, as the deny reported it
	cost  int64     // the cost denied: a cheaper request goes to Redis
	// The limit the deny was computed for: an override that starts or ends
	// changes them without a swap, and the entry then no longer applies.
	rate, burst int64
}

func newDenyCache() *denyCache { return &denyCache{} }

// generation returns the cache for a request on the snapshot and identity
// whose Seqs are snapshot and identity, making it the current one if that
// pair is newer than the current one's. It returns nil if a newer pair is
// already in use: a request still on the old pair must neither find what
// the newer one cached nor repopulate it.
func (c *denyCache) generation(snapshot, identity uint64) *denyGeneration {
	for {
		g := c.cur.Load()
		if g != nil && g.snapshot == snapshot && g.identity == identity {
			return g
		}
		if g != nil && (snapshot < g.snapshot || identity < g.identity) {
			return nil
		}
		if next := (&denyGeneration{snapshot: snapshot, identity: identity}); c.cur.CompareAndSwap(g, next) {
			return next
		}
	}
}

// entries is the number of entries in the current generation, expired ones
// not yet removed included.
func (c *denyCache) entries() int64 {
	if g := c.cur.Load(); g != nil {
		return g.entries.Load()
	}
	return 0
}

func (g *denyGeneration) shard(key string) *denyShard {
	return &g.shards[maphash.String(denySeed, key)%denyCacheShards]
}

// lookup returns the live entry for lim's key that denies a request of
// cost at now: one whose cost is at most cost, made for lim's rate and
// burst. It removes an expired entry it finds.
func (g *denyGeneration) lookup(lim rlredis.Limit, cost int64, now time.Time) (denyEntry, bool) {
	s := g.shard(lim.Key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[lim.Key]
	if !ok {
		return denyEntry{}, false
	}
	if !now.Before(e.until) {
		delete(s.m, lim.Key)
		g.entries.Add(-1)
		return denyEntry{}, false
	}
	return e, e.cost <= cost && e.rate == lim.Rate && e.burst == lim.Burst
}

// insert caches e for key at now, replacing any entry for key, and returns
// denyCacheInsert, or denyCacheFull if key's shard is full of live entries:
// a live entry is never evicted early.
func (g *denyGeneration) insert(key string, e denyEntry, now time.Time) string {
	s := g.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok {
		if len(s.m) >= denyCacheShardSize {
			for k, old := range s.m {
				if !now.Before(old.until) {
					delete(s.m, k)
					g.entries.Add(-1)
				}
			}
			if len(s.m) >= denyCacheShardSize {
				return denyCacheFull
			}
		}
		if s.m == nil {
			s.m = map[string]denyEntry{}
		}
		g.entries.Add(1)
	}
	s.m[key] = e
	return denyCacheInsert
}
