package proxy

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"

	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// Token leasing (RL-18, ADR-0069): for an enforce policy with lease_k = k,
// a replica reserves up to k cost units per key in Redis, in the request's
// own call, and serves later requests from them locally. It holds at most k
// unspent units per key and never has a second grant for a key on the way,
// so admissions exceed Redis's bound by at most k per replica.
const (
	leaseLife      = time.Second // from the reservation's send time; every period is whole seconds
	leaseShards    = 64
	leaseShardSize = 1024 // leases per shard: 65,536 in all
)

// Lease outcomes, the result label of gw_ratelimit_lease_total.
const (
	leaseLocal    = "local"    // served from a lease
	leaseGranted  = "granted"  // a reservation got g = k
	leasePartial  = "partial"  // a reservation got cost ≤ g < k
	leaseDenied   = "denied"   // a reservation got nothing: its call denied
	leaseError    = "error"    // a reservation failed, or the budget left no time to send it
	leaseInflight = "inflight" // checked strictly: a reservation for the key was in flight
	leaseFull     = "full"     // checked strictly: the key's shard was full
)

// Reasons for dropped lease tokens, the reason label of
// gw_ratelimit_lease_tokens_dropped_total.
const (
	leaseDropExpired = "expired" // left when the lease expired
	leaseDropReset   = "reset"   // left when a snapshot swap cleared the table, or the limits changed
	leaseDropReserve = "reserve" // fewer than a request's cost, dropped when it reserved
)

// leaseSeed hashes lease keys to shards.
var leaseSeed = maphash.MakeSeed()

// leaseTable holds one replica's leases. Like the deny cache, its leases
// belong to the config snapshot and identity they were granted on: the
// first request on a newer pair replaces every shard (a new generation),
// the old generation's tokens are dropped, and a request still on an older
// pair checks strictly (ADR-0069, "Generation").
type leaseTable struct {
	cur  atomic.Pointer[leaseGeneration]
	drop func(reason string, n int64) // records dropped tokens; nil for none
}

// leaseGeneration is the table for one snapshot and identity, by their Seq.
type leaseGeneration struct {
	snapshot, identity uint64
	// dead: a newer generation replaced this one. Requests that still hold
	// it neither serve from it nor put tokens back, so only the current
	// generation's leases are spent.
	dead   atomic.Bool
	drop   func(reason string, n int64)
	shards [leaseShards]leaseShard
}

type leaseShard struct {
	mu sync.Mutex
	m  map[string]*lease
}

// lease is one policy's lease for one key, by the Redis key of the
// policy's first limit.
type lease struct {
	tokens    int64     // unspent cost units, at most k
	expires   time.Time // serves while now < expires
	reserving bool      // a reservation for the key is in flight
	grantUS   int64     // the grant's Redis TIME, µs
	grantAt   time.Time // when the grant arrived, on the gateway's clock
	// limits are the policy's limits the grant was for, with their state
	// after it; replaced, never changed, on each grant, so a request may
	// read them once it has let go of the shard.
	limits []leaseLimit
}

// leaseLimit is one limit of a lease's grant.
type leaseLimit struct {
	rate, burst int64
	period      time.Duration
	remaining   int64         // the limit's remaining after the grant
	reset       time.Duration // its reset_after at the grant
}

// leaseStep is what a request does for one batched policy.
type leaseStep int

const (
	leaseStrict  leaseStep = iota // check the policy strictly in the call
	leaseServe                    // served from the lease
	leaseReserve                  // send the policy with lease_k = k, then grant or end the reservation
)

// leaseTake is a request's step for one batched policy and what came of it.
type leaseTake struct {
	step    leaseStep
	result  string // the gw_ratelimit_lease_total result for a strict step, "" for none
	tokens  int64  // served: the tokens left after the request's cost
	grantUS int64  // served: the lease's grant
	grantAt time.Time
	limits  []leaseLimit // served: the grant's limits
}

func newLeaseTable(drop func(reason string, n int64)) *leaseTable {
	return &leaseTable{drop: drop}
}

// generation returns the table for a request on the snapshot and identity
// whose Seqs are snapshot and identity, making it the current one if that
// pair is newer than the current one's and dropping the old one's tokens.
// It returns nil if a newer pair is already in use: the request then checks
// its batched policies strictly.
func (t *leaseTable) generation(snapshot, identity uint64) *leaseGeneration {
	for {
		g := t.cur.Load()
		if g != nil && g.snapshot == snapshot && g.identity == identity {
			return g
		}
		if g != nil && (snapshot < g.snapshot || identity < g.identity) {
			return nil
		}
		next := &leaseGeneration{snapshot: snapshot, identity: identity, drop: t.drop}
		if t.cur.CompareAndSwap(g, next) {
			if g != nil {
				g.close()
			}
			return next
		}
	}
}

// close marks g replaced and drops every token it holds.
func (g *leaseGeneration) close() {
	g.dead.Store(true)
	var n int64
	for i := range g.shards {
		s := &g.shards[i]
		s.mu.Lock()
		for _, e := range s.m {
			n += e.tokens
			e.tokens = 0
		}
		s.mu.Unlock()
	}
	g.dropped(leaseDropReset, n)
}

func (g *leaseGeneration) dropped(reason string, n int64) {
	if n > 0 && g.drop != nil {
		g.drop(reason, n)
	}
}

func (g *leaseGeneration) shard(key string) *leaseShard {
	return &g.shards[maphash.String(leaseSeed, key)%leaseShards]
}

// matches reports whether the lease's grant was for limits, the limits the
// request checks the policy against: an override that starts or ends
// changes them without a snapshot swap.
func (e *lease) matches(limits []rlredis.Limit) bool {
	if len(e.limits) != len(limits) {
		return false
	}
	for i, l := range limits {
		if m := e.limits[i]; m.rate != l.Rate || m.burst != l.Burst || m.period != l.Period {
			return false
		}
	}
	return true
}

// acquire is a request's step for the batched policy whose first limit's
// Redis key is key, checked against limits, at cost and now (ADR-0069,
// step 3): serve from a live lease that holds the cost; check strictly while
// a reservation for the key is in flight, or when its shard is full;
// otherwise reserve, dropping any tokens left.
func (g *leaseGeneration) acquire(key string, limits []rlredis.Limit, cost int64, now time.Time) leaseTake {
	s := g.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.dead.Load() {
		return leaseTake{step: leaseStrict}
	}
	e := s.m[key]
	if e == nil {
		if len(s.m) >= leaseShardSize && !g.sweep(s, now) {
			return leaseTake{step: leaseStrict, result: leaseFull}
		}
		if s.m == nil {
			s.m = map[string]*lease{}
		}
		e = &lease{}
		s.m[key] = e
	}
	if e.reserving {
		return leaseTake{step: leaseStrict, result: leaseInflight}
	}
	live, same := now.Before(e.expires), e.matches(limits)
	if live && same && e.tokens >= cost {
		e.tokens -= cost
		return leaseTake{step: leaseServe, tokens: e.tokens, grantUS: e.grantUS, grantAt: e.grantAt, limits: e.limits}
	}
	switch {
	case !live:
		g.dropped(leaseDropExpired, e.tokens)
	case !same:
		g.dropped(leaseDropReset, e.tokens)
	default:
		g.dropped(leaseDropReserve, e.tokens)
	}
	e.tokens, e.reserving = 0, true
	return leaseTake{step: leaseReserve}
}

// sweep removes s's expired leases that no reservation is using, dropping
// their tokens, and reports whether s then has room for one more.
func (g *leaseGeneration) sweep(s *leaseShard, now time.Time) bool {
	for k, e := range s.m {
		if !e.reserving && !now.Before(e.expires) {
			g.dropped(leaseDropExpired, e.tokens)
			delete(s.m, k)
		}
	}
	return len(s.m) < leaseShardSize
}

// granted ends the reservation for key that a call sent at sent allowed,
// answered at now with Redis TIME nowUS: the lease holds the grant less the
// request's cost for leaseLife from sent, and limits become its view. In a
// replaced generation the grant's tokens are dropped.
func (g *leaseGeneration) granted(key string, cost, grant int64, sent, now time.Time, nowUS int64, limits []leaseLimit) {
	s := g.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[key]
	if g.dead.Load() || e == nil {
		g.dropped(leaseDropReset, grant-cost)
		return
	}
	e.reserving = false
	e.tokens, e.expires = grant-cost, sent.Add(leaseLife)
	e.grantUS, e.grantAt, e.limits = nowUS, now, limits
}

// ended ends the reservation for key with nothing granted: its call denied,
// failed or was not sent.
func (g *leaseGeneration) ended(key string) {
	s := g.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.m[key]; e != nil {
		e.reserving = false
	}
}

// putBack gives cost back to key's lease, never above k, for a request that
// took it and was then denied: only to a live lease with no reservation in
// flight, so the replica still holds at most k unspent units and none on
// the way beside them.
func (g *leaseGeneration) putBack(key string, k, cost int64, now time.Time) {
	s := g.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.m[key]; e != nil && !g.dead.Load() && !e.reserving && now.Before(e.expires) {
		e.tokens = min(k, e.tokens+cost)
	}
}
