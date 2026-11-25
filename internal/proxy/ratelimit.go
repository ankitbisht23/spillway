package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/heavyhitters"
	"github.com/ankitbisht23/spillway/internal/quota"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// Redis runs limiter script calls on Redis Cluster; *rlredis.Client
// implements it (ADR-0021).
type Redis interface {
	Call(ctx context.Context, cost int64, limits []rlredis.Limit) (rlredis.Result, error)
	Refund(ctx context.Context, cost, admittedUS int64, limits []rlredis.Limit) (rlredis.Result, error)
}

// limits evaluates one request's rate limit policies: the local ones in two
// phases, before and after API key resolution (RL-26, ADR-0017), then the
// Redis ones (ADR-0021). It writes what they decided into the response
// headers (RL-25, ADR-0018) and q (RL-27).
type limits struct {
	snap     *config.Snapshot // the request's: its plans' quotas (RL-10)
	policies []config.Policy
	registry *local.Registry
	redis    Redis         // nil without Redis: the gateway then has no Redis policies
	budget   time.Duration // one Redis time budget for all of the request's calls (RL-22)
	replicas int64         // degraded limiters' divisor (RL-31)
	tracer   trace.Tracer  // nil when tracing is off (OB-02)
	rec      *telemetry.Recorder
	log      *slog.Logger
	r        *http.Request
	q        *telemetry.Request
	cost     int64
	values   config.KeyValues
	// hitters counts denies and shadow would-denies (RL-21); nil in
	// static mode.
	hitters *heavyhitters.Sketch
	// consumer and identity are the request's, once authenticated; they
	// choose the limits of per-consumer policies (limitsOf).
	consumer *config.Consumer
	identity *config.Identity
	keyID    int64        // the managed API key that resolved the consumer; 0 for none
	checks   []check      // in evaluation order
	calls    []*redisCall // the Redis calls that answered, in order
	denied   bool         // an enforce limit denied the request
	failures bool         // a Redis call failed or was not sent for lack of budget
	errKinds []string     // the failed calls' error kinds, in order
	// unavailable: Redis failed for an enforce fail_closed policy, so the
	// request is answered 503 (RL-23).
	unavailable bool
	// deny is the generation of the replica's deny cache for the request's
	// snapshot and identity (RL-19); nil for none, or when a newer pair is
	// already in use.
	deny *denyGeneration
	// lease is the replica's lease table for the request's snapshot and
	// identity (RL-18); nil for none, or when a newer pair is already in
	// use: batched policies are then checked strictly.
	lease *leaseGeneration
	// leases are the request's batched policies that took cost from a lease
	// or reserved one, in order.
	leases []leaseUse
	// leaseUS is the latest decision time of the policies served from
	// leases: a grant's Redis TIME plus the time since it arrived.
	leaseUS int64
}

// leaseUse is one batched policy of the request that took its cost from a
// lease or reserved one (ADR-0069).
type leaseUse struct {
	key      string     // the lease's key: the Redis key of the policy's first limit
	k        int64      // the policy's lease_k
	call     *redisCall // the call carrying the reservation; nil if served
	first    int        // reserved: the index of the policy's first limit in call
	reserved bool       // a reservation not yet granted or ended
	served   bool       // holds the cost from the lease: put back if the request is denied
}

// check is one limit's evaluation for the request.
type check struct {
	policy  *config.Policy
	limit   config.Limit      // a degraded limiter's per-replica limit
	windows int               // the limits the policy checks for this request
	limiter ratelimit.Limiter // nil for a Redis limit; local or degraded otherwise
	key     string
	d       ratelimit.Decision // the last decision: the refund's once refunded
	at      time.Time          // on the registry's clock, at or before the admission
	held    bool               // admitted and not given back; for Redis, written
}

// redisCall is one limiter script call: the limits of one hash slot
// (ADR-0021).
type redisCall struct {
	limits   []rlredis.Limit
	policies []*config.Policy // of each limit; a policy's limits are adjacent
	conf     []config.Limit   // of each limit
	windows  []int            // of each limit: its policy's limits for this request
	keys     []string         // of each limit: its policy's filled key template's ratelimit.Key
	checks   []int            // index in limits.checks of each limit, once answered; -1 for a quota the script skipped
	nowUS    int64            // the call's Redis TIME, which its refund names
	slot     string           // the hash slot's kind, for its span: "tenant", "key" or "global"
}

// errBudgetSpent fails a Redis call that the request's budget left no time
// for; it is never sent (RL-22).
var errBudgetSpent = errors.New("redis time budget spent before the call")

// refunder is a limiter whose refund depends on when the admission was made:
// a window limiter's is lost if it crosses a window boundary (ADR-0017), and
// GCRA gives back less the longer ago it was (ADR-0028).
type refunder interface {
	Refund(key string, cost int64, at time.Time) ratelimit.Decision
}

// newLimits prepares the checks of a request for route on snapshot s, sent
// by the client at ip; tracer, if not nil, traces them under r's span, and
// hitters, if not nil, counts their denies.
func newLimits(s *config.Snapshot, route *config.Route, registry *local.Registry, redis Redis, tracer trace.Tracer, hitters *heavyhitters.Sketch, rec *telemetry.Recorder, log *slog.Logger, r *http.Request, q *telemetry.Request, ip netip.Addr) *limits {
	l := &limits{
		snap: s, policies: s.Policies, registry: registry, redis: redis, budget: s.RedisBudget, replicas: s.Replicas, tracer: tracer, hitters: hitters, rec: rec, log: log, r: r, q: q, cost: route.Cost,
		// Upper-cased: many upstreams take "post" for POST, so it must not
		// skip POST policies or get a {method} bucket of its own (ADR-0020).
		values: config.KeyValues{Route: route.Name, Method: strings.ToUpper(r.Method), Header: r.Header},
	}
	if ip.IsValid() && len(s.Policies) > 0 {
		l.values.IP = config.IPKey(ip) // invalid only if the peer address did not parse: {ip} stays unfilled
	}
	return l
}

// identify fills the identity placeholders for phase 2 from the request's
// consumer and the API key that resolved it; anonymous, they stay unfilled,
// and so does {api_key} for a consumer a token resolved (ADR-0039). id is
// the identity the consumer comes from, whose overrides apply (RL-08);
// keyID is the key's ID, 0 for none or a static key.
func (l *limits) identify(c *config.Consumer, key string, keyID int64, id *config.Identity) {
	if c == nil {
		return
	}
	l.consumer, l.identity, l.keyID = c, id, keyID
	if key != "" {
		sum := sha256.Sum256([]byte(key))
		l.values.APIKey = hex.EncodeToString(sum[:])
	}
	l.values.Consumer, l.values.Tenant = c.Name, c.Tenant
}

// useDenyCache makes the request's Redis phase use c, the replica's deny
// cache, for s and id, the request's snapshot and identity (RL-19).
func (l *limits) useDenyCache(c *denyCache, s *config.Snapshot, id *config.Identity) {
	l.deny = c.generation(s.Seq(), id.Seq())
}

// useLeases makes the request's Redis phase use t, the replica's lease
// table, for s and id, the request's snapshot and identity (RL-18).
func (l *limits) useLeases(t *leaseTable, s *config.Snapshot, id *config.Identity) {
	l.lease = t.generation(s.Seq(), id.Seq())
}

// limitsOf returns the limits p checks the request against, or false when
// p does not apply to it (RL-08, D3, ADR-0041): for a per-consumer policy,
// the override of p that applies now to the request's consumer, else its
// plan's limits for p; otherwise p's own. A from_plan policy without plan
// limits for the request (an anonymous one) does not apply, like a
// template that does not fill.
func (l *limits) limitsOf(p *config.Policy) ([]config.Limit, bool) {
	if c := l.consumer; c != nil && p.PerConsumer {
		if c.ID != 0 {
			if raw, ok := l.identity.Override(c.ID, p.ID, l.registry.Now()); ok {
				if ls, ok := p.OverrideLimits(raw, l.replicas); ok {
					return ls, true
				}
			}
		}
		if ls, ok := p.PlanLimits[c.Plan]; ok && c.Plan != 0 {
			return ls, true
		}
	}
	return p.Limits, !p.FromPlan
}

// check evaluates one local phase: the local policies whose key templates
// need no identity, or with identity true the rest, in config order. Every
// limit of every applying policy is checked, even after a deny. If an
// enforce limit has denied the request, in this phase or the first, every
// admission of both phases is refunded and check returns false.
func (l *limits) check(identity bool) bool {
	if len(l.policies) == 0 {
		return true
	}
	start := time.Now()
	at := l.registry.Now()
	n := len(l.checks)
	policies := 0 // that applied
	for i := range l.policies {
		p := &l.policies[i]
		if !p.Local || p.Mode == config.Off || p.Key.UsesIdentity != identity || !p.Matches(l.values.Route, l.values.Method) {
			continue
		}
		filled, ok := p.Key.Fill(&l.values)
		if !ok {
			continue // a placeholder without a value: the policy does not apply
		}
		lims, ok := l.limitsOf(p)
		if !ok {
			continue
		}
		key := ratelimit.Key(filled)
		decision := "allow"
		applied := false
		for _, lim := range lims {
			limiter := l.registry.For(local.LimitID(p, lim), lim)
			if limiter == nil {
				continue // removed by a reload since this request's snapshot
			}
			d, err := limiter.Allow(l.r.Context(), key, l.cost) // local limiters fail only with ErrKeyCap
			if err != nil {
				l.rec.KeyCapSkip(p.Name) // a new key in a full limiter: the limit is skipped (ADR-0021, U1)
				continue
			}
			applied = true
			l.checks = append(l.checks, check{policy: p, limit: lim, windows: len(lims), limiter: limiter, key: key, d: d, at: at, held: d.Allowed})
			if !d.Allowed {
				decision = "deny"
				l.denied = l.denied || p.Mode == config.Enforce
			}
		}
		if applied {
			policies++
			l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: p.Name, Decision: decision, Mode: p.Mode})
			if decision == "deny" {
				l.throttled(p, key)
			}
		}
	}
	if l.denied {
		l.refund()
	}
	if len(l.checks) > n {
		l.rec.RateLimitCheck(telemetry.BackendLocal, time.Since(start))
		l.traceLocal(identity, start, policies)
	}
	return !l.denied
}

// checkRedis evaluates the Redis policies once both local phases allowed
// the request (ADR-0021): the tenant-slot call, or without a tenant one call
// per {k:} digest, then one call per global policy (RL-05, RL-14). A call is
// sent only if every call before it allowed, or failed and its failure
// modes let the request go on. All of them share one time budget from the
// start of the first (RL-22): a call the budget leaves no time for is not
// sent and fails as a timeout. If one denies, or a failure mode ends the
// request, everything the request holds is refunded and checkRedis returns
// false; unavailable then says whether the answer is 503 (RL-23). Before
// any call, the deny cache can deny the request, and a call's enforce deny
// is cached (RL-19, ADR-0069). Just before each call, its batched policies
// are served from leases or reserve one in it (RL-18, ADR-0069); a call
// left empty is not sent.
func (l *limits) checkRedis() bool {
	if l.redis == nil {
		return true
	}
	calls := l.redisCalls()
	if len(calls) == 0 {
		return true
	}
	if l.cachedDeny(calls) {
		return false
	}
	start := time.Now()
	called := false // only requests that called Redis observe its check duration
	defer func() {
		if called {
			l.rec.RateLimitCheck(telemetry.BackendRedis, time.Since(start))
		}
	}()
	deadline := start.Add(l.budget)
	var ctx context.Context // made for the first call sent, not for a request its leases serve
	for _, c := range calls {
		if l.lease != nil {
			l.useLease(c)
			if len(c.limits) == 0 {
				continue // every policy served from a lease
			}
		}
		called = true
		if ctx == nil {
			// The request's values reach the calls, but a client that goes
			// away cannot cut a sent call short.
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(context.WithoutCancel(l.r.Context()), deadline)
			defer cancel()
		}
		var res rlredis.Result
		var sent time.Time
		var err error
		if time.Now().Before(deadline) {
			span := l.startRedisSpan(ctx, c.slot, false)
			if l.deny != nil || l.lease != nil {
				sent = l.registry.Now()
			}
			res, err = l.redis.Call(ctx, l.cost, c.limits)
			endRedisSpan(span, res.Allowed, err)
		} else {
			err = &rlredis.Error{Kind: rlredis.KindTimeout, Err: errBudgetSpent}
		}
		l.reserved(c, res, err, sent)
		if err != nil {
			if !l.failed(c, err) {
				return false
			}
			continue
		}
		c.nowUS = res.NowUS
		l.calls = append(l.calls, c)
		for i, lim := range c.limits {
			if lim.Algorithm == rlredis.Quota && res.QuotaUnseeded {
				c.checks = append(c.checks, -1) // skipped by the script: not checked
				continue
			}
			c.checks = append(c.checks, len(l.checks))
			// Written: enforce limits of an allowed call, and its shadow
			// limits that would allow (the script's write rule). A lease
			// group's grant stays with the lease, never refunded: the
			// request's view counts the lease's tokens left as remaining.
			written := res.Allowed && (lim.Enforce || res.ShadowWouldDeny&(1<<i) == 0)
			d := res.Limits[i]
			if lim.LeaseK > 0 {
				written = false
				if res.Allowed {
					d.Remaining += res.Granted[i] - l.cost
				}
			}
			l.checks = append(l.checks, check{policy: c.policies[i], limit: c.conf[i], windows: c.windows[i], d: d, held: written})
		}
		closed := res.QuotaUnseeded && res.Allowed && failClosed(c)
		l.decide(c, func(i int) string {
			switch {
			case c.limits[i].Algorithm == rlredis.Quota && res.QuotaUnseeded && closed:
				return "fail_closed"
			case c.limits[i].Algorithm == rlredis.Quota && res.QuotaUnseeded:
				return "fail_open"
			case res.Limits[i].Allowed:
				return "allow"
			}
			return "deny"
		})
		if closed {
			// The tenant's quotas are unseeded and an enforce fail_closed
			// policy shares the call: 503, as if the call had failed
			// (ADR-0070).
			l.unavailable = true
			l.refund()
			return false
		}
		if !res.Allowed {
			if l.deny != nil {
				l.cacheDeny(c, res, sent)
			}
			l.denied = true
			l.refund()
			return false
		}
	}
	return true
}

// useLease serves c's batched policies from this replica's leases before c
// is sent (RL-18, ADR-0069, step 3): an enforce policy with lease_k ≥ the
// request's cost takes the cost from a live lease that holds it and leaves
// c, with an allow decision; otherwise it reserves in c (lease_k = k), or is
// checked strictly while another request's reservation for the key is in
// flight or the table is full.
func (l *limits) useLease(c *redisCall) {
	var now time.Time
	keep := 0
	for i := 0; i < len(c.limits); {
		p := c.policies[i]
		j := i + 1
		for j < len(c.limits) && c.policies[j] == p {
			j++
		}
		if p.LeaseK > 0 && p.Mode == config.Enforce && l.cost <= p.LeaseK {
			if now.IsZero() {
				now = l.registry.Now()
			}
			key := c.limits[i].Key
			take := l.lease.acquire(key, c.limits[i:j], l.cost, now)
			switch take.step {
			case leaseServe:
				l.rec.Lease(leaseLocal)
				l.served(c, i, j, take, now)
				l.leases = append(l.leases, leaseUse{key: key, k: p.LeaseK, served: true})
				i = j
				continue // decided: not in the call
			case leaseReserve:
				for x := i; x < j; x++ {
					c.limits[x].LeaseK = p.LeaseK
				}
				l.leases = append(l.leases, leaseUse{key: key, k: p.LeaseK, call: c, first: keep, reserved: true})
			default:
				if take.result != "" {
					l.rec.Lease(take.result)
				}
			}
		}
		for x := i; x < j; x++ { // keep the policy in the call
			c.limits[keep], c.policies[keep], c.conf[keep], c.windows[keep], c.keys[keep] = c.limits[x], c.policies[x], c.conf[x], c.windows[x], c.keys[x]
			keep++
		}
		i = j
	}
	c.limits, c.policies, c.conf, c.windows, c.keys = c.limits[:keep], c.policies[:keep], c.conf[:keep], c.windows[:keep], c.keys[:keep]
}

// served records the policy of c's limits [i, j) as served from a lease at
// now: an allow, with each limit's remaining the grant's plus the lease's
// tokens left and its reset the grant's less the time since (ADR-0069,
// "Headers and decision time"); the decision time is the grant's Redis
// TIME plus that time.
func (l *limits) served(c *redisCall, i, j int, take leaseTake, now time.Time) {
	since := now.Sub(take.grantAt)
	for x := i; x < j; x++ {
		ll := take.limits[x-i]
		d := ratelimit.Decision{Allowed: true, Remaining: ll.remaining + take.tokens, ResetAfter: max(0, ll.reset-since)}
		l.checks = append(l.checks, check{policy: c.policies[x], limit: c.conf[x], windows: c.windows[x], d: d})
	}
	l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: c.policies[i].Name, Decision: "allow", Mode: c.policies[i].Mode})
	l.leaseUS = max(l.leaseUS, take.grantUS+since.Microseconds())
}

// reserved ends the reservations c carried, once c, sent at sent, answered
// res or failed with err: an allowed call's grant becomes the lease, less
// the request's cost, which the request holds until it is answered; a
// denied, failed or unsent call grants nothing (ADR-0069, "Reserve").
func (l *limits) reserved(c *redisCall, res rlredis.Result, err error, sent time.Time) {
	var now time.Time
	for i := range l.leases {
		u := &l.leases[i]
		if !u.reserved || u.call != c {
			continue
		}
		u.reserved = false
		var g int64
		if err == nil && res.Allowed {
			g = res.Granted[u.first]
		}
		switch {
		case err != nil:
			l.lease.ended(u.key)
			l.rec.Lease(leaseError)
			continue
		case g < l.cost: // denied: nothing written
			l.lease.ended(u.key)
			l.rec.Lease(leaseDenied)
			continue
		case g < u.k:
			l.rec.Lease(leasePartial)
		default:
			l.rec.Lease(leaseGranted)
		}
		if now.IsZero() {
			now = l.registry.Now()
		}
		var lims []leaseLimit
		for x := u.first; x < len(c.limits) && c.policies[x] == c.policies[u.first]; x++ {
			lim, d := c.limits[x], res.Limits[x]
			lims = append(lims, leaseLimit{rate: lim.Rate, burst: lim.Burst, period: lim.Period, remaining: d.Remaining, reset: d.ResetAfter})
		}
		l.lease.granted(u.key, l.cost, g, sent, now, res.NowUS, lims)
		u.served = true
	}
}

// putBack gives the cost back to every lease the request took it from, its
// own grant's included, once the request is denied (ADR-0069).
func (l *limits) putBack() {
	var now time.Time
	for i := range l.leases {
		u := &l.leases[i]
		if !u.served {
			continue
		}
		if now.IsZero() {
			now = l.registry.Now()
		}
		l.lease.putBack(u.key, u.k, l.cost, now)
		u.served = false
	}
}

// cachedDeny denies the request from the deny cache if an enforce limit of
// its calls has a live entry for a cost no higher than the request's (RL-19,
// ADR-0069). The deny is recorded as a Redis deny would be, the denying
// limit alone, marked cached; no other limit is evaluated, and the local
// phases' deductions are refunded.
func (l *limits) cachedDeny(calls []*redisCall) bool {
	if l.deny == nil || l.deny.entries.Load() == 0 {
		return false // nothing cached: the allowed hot path pays one atomic load
	}
	now := l.registry.Now()
	for _, c := range calls {
		for i, lim := range c.limits {
			if !lim.Enforce {
				continue
			}
			e, ok := l.deny.lookup(lim, l.cost, now)
			if !ok {
				continue
			}
			p := c.policies[i]
			l.checks = append(l.checks, check{policy: p, limit: c.conf[i], windows: c.windows[i], d: ratelimit.Decision{ResetAfter: max(0, e.reset.Sub(now)), RetryAfter: e.until.Sub(now)}})
			l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: p.Name, Decision: "deny", Mode: p.Mode, Cached: true})
			l.throttled(p, c.keys[i])
			l.rec.DenyCache(denyCacheHit)
			l.denied = true
			l.refund()
			return true
		}
	}
	return false
}

// cacheDeny caches the deny of c, a call sent at sent that res denied: for
// its denying limit only, if that is an enforce limit, for min(retry-after,
// 10 s) from sent (RL-19, ADR-0069). Timed from the send, which precedes the
// script's TIME, the entry never outlives Redis's deny.
func (l *limits) cacheDeny(c *redisCall, res rlredis.Result, sent time.Time) {
	i := res.DenyingLimit
	if i < 0 || i >= len(c.limits) || !c.limits[i].Enforce {
		return
	}
	d := res.Limits[i]
	hold := min(d.RetryAfter, denyCacheMaxHold)
	if hold <= 0 {
		return
	}
	lim := c.limits[i]
	e := denyEntry{until: sent.Add(hold), reset: sent.Add(d.ResetAfter), cost: l.cost, rate: lim.Rate, burst: lim.Burst}
	l.rec.DenyCache(l.deny.insert(lim.Key, e, sent))
}

// failed applies the failure modes of the policies in c, a call that failed
// (RL-23, ADR-0021). An enforce fail_closed policy makes the request a 503,
// and nothing else in c is evaluated; otherwise every degraded policy is
// checked against its in-memory limiter, and an enforce one that denies
// makes it a 429. fail_open policies allow, and shadow policies never change
// the outcome. It returns false, with everything the request holds
// refunded, if the request ends here.
func (l *limits) failed(c *redisCall, err error) bool {
	l.failures = true
	kind, node := errorKind(err)
	l.rec.RedisError(kind)
	l.errKinds = append(l.errKinds, kind)
	// Debug: during an outage every request fails; the metric and the
	// breaker's transition logs carry it.
	l.log.Debug("redis limiter call failed; failure modes apply", "kind", kind, "node", node, "request_id", l.q.ID)
	l.unavailable = l.unavailable || failClosed(c)
	at := l.registry.Now()
	for i := 0; i < len(c.limits); {
		p, decision, applied := c.policies[i], "allow", true
		j := i
		for j < len(c.limits) && c.policies[j] == p {
			j++
		}
		switch {
		case p.Algorithm == rlredis.Quota: // the call's policies decide (failClosed)
			decision = "fail_open"
			if l.unavailable {
				decision = "fail_closed"
			}
		case p.FailureMode == config.FailOpen:
			decision = "fail_open"
		case p.FailureMode == config.FailClosed:
			decision = "fail_closed"
		default: // config.Degraded
			applied = false
			if l.unavailable {
				break // not evaluated: the request is answered 503
			}
			for k := i; k < j; k++ {
				lim := c.conf[k].PerReplica(l.replicas)
				limiter := l.registry.For(local.DegradedID(p, c.conf[k]), lim)
				if limiter == nil {
					continue // removed by a reload since this request's snapshot
				}
				// Keyed like the Redis key, so a tenant's degraded state is
				// its own as in Redis.
				key := ratelimit.Key(c.limits[k].Key)
				d, err := limiter.Allow(l.r.Context(), key, l.cost) // local limiters fail only with ErrKeyCap
				if err != nil {
					l.rec.KeyCapSkip(p.Name) // a new key in a full limiter: the limit is skipped (ADR-0021, U1)
					continue
				}
				applied = true
				l.checks = append(l.checks, check{policy: p, limit: lim, windows: c.windows[k], limiter: limiter, key: key, d: d, at: at, held: d.Allowed})
				if !d.Allowed {
					decision = "deny"
					l.denied = l.denied || p.Mode == config.Enforce
				}
			}
		}
		if applied {
			l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: p.Name, Decision: decision, Mode: p.Mode})
			if decision == "deny" {
				l.throttled(p, c.keys[i])
			}
		}
		i = j
	}
	if l.unavailable || l.denied {
		l.refund()
		return false
	}
	return true
}

// failClosed reports whether an enforce fail_closed policy is in c: then a
// failure of c, or c's quotas unseeded, makes the request a 503 (RL-23,
// ADR-0070). A quota has no failure mode of its own; it follows the
// policies of its call, and is fail_open alone.
func failClosed(c *redisCall) bool {
	for _, p := range c.policies {
		if p.FailureMode == config.FailClosed && p.Mode == config.Enforce {
			return true
		}
	}
	return false
}

// decisionTimeHeader carries, in dev mode, the Redis time of the call that
// decided the request (ADR-0021).
const decisionTimeHeader = "Spillway-Decision-Time"

// decisionTime is the Spillway-Decision-Time value: the latest of the
// now_us of the last Redis call that ran and the decision times of the
// policies served from leases (ADR-0069), if the request had either and
// no call failed; otherwise "" (ADR-0021).
func (l *limits) decisionTime() string {
	us := l.leaseUS
	if len(l.calls) > 0 {
		us = max(us, l.calls[len(l.calls)-1].nowUS)
	}
	if us == 0 || l.failures {
		return ""
	}
	return strconv.FormatInt(us, 10)
}

// redisErrorHeader carries, in dev mode, the kinds of the request's failed
// Redis calls (ADR-0034).
const redisErrorHeader = "Spillway-Redis-Error"

// redisErrors is the Spillway-Redis-Error value: the error kind of each of
// the request's Redis calls that failed or was not sent for lack of budget
// (timeout), in order, comma-separated; "" if none (ADR-0034).
func (l *limits) redisErrors() string { return strings.Join(l.errKinds, ",") }

// errorKind returns a limiter error's kind and node.
func errorKind(err error) (kind, node string) {
	var e *rlredis.Error
	if errors.As(err, &e) {
		return e.Kind, e.Node
	}
	return rlredis.KindConn, ""
}

// decide records one decision per policy of c: of its limits, the first
// that is not "allow", else "allow".
func (l *limits) decide(c *redisCall, decision func(i int) string) {
	for i := 0; i < len(c.limits); {
		p, d, first := c.policies[i], "allow", i
		for ; i < len(c.limits) && c.policies[i] == p; i++ {
			if x := decision(i); d == "allow" {
				d = x
			}
		}
		l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: p.Name, Decision: d, Mode: p.Mode})
		if d == "deny" {
			l.throttled(p, c.keys[first])
		}
	}
}

// throttled counts a deny of policy p, or a would-deny if p is in shadow
// mode, for the key whose ratelimit.Key is key, in the heavy hitters
// (RL-21, ADR-0041): once per policy and request, whatever the cost. A
// failure mode's outcome is not a deny and is not counted.
func (l *limits) throttled(p *config.Policy, key string) {
	if l.hitters == nil || p.ID == 0 { // a quota (ID 0) is not a policy the heavy hitters name
		return
	}
	var consumer int64
	if l.consumer != nil {
		consumer = l.consumer.ID
	}
	l.hitters.Add(p.ID, key, p.Mode == config.Shadow, consumer, l.keyID)
}

// redisCalls groups the Redis policies that apply to the request into calls
// by hash slot, in the order they run (ADR-0021): every tenant-, consumer-
// and route-level policy in one {t:<tenant>} call when the request has a
// tenant, else one {k:<digest>} call per distinct digest; then each global
// policy in its own {g:<policy>} call. Within a call, limits follow config
// order.
func (l *limits) redisCalls() []*redisCall {
	var slot, global []*redisCall // slot: the tenant call or the {k:} calls
	var byDigest map[string]*redisCall
	for i := range l.policies {
		p := &l.policies[i]
		if p.Local || p.Mode == config.Off || !p.Matches(l.values.Route, l.values.Method) {
			continue
		}
		filled, ok := p.Key.Fill(&l.values)
		if !ok {
			continue // a placeholder without a value: the policy does not apply
		}
		lims, ok := l.limitsOf(p)
		if !ok {
			continue
		}
		raw := ratelimit.Key(filled)
		digest := rlredis.KeyDigest(raw)
		var c *redisCall
		var key func(period time.Duration) string
		switch {
		case p.Level == "global":
			c = &redisCall{slot: "global"}
			global = append(global, c)
			key = func(period time.Duration) string { return rlredis.GlobalKey(p.Ref, period, digest) }
		case l.values.Tenant != "":
			if len(slot) == 0 {
				slot = append(slot, &redisCall{slot: "tenant"})
			}
			c = slot[0]
			key = func(period time.Duration) string { return rlredis.TenantKey(l.values.Tenant, p.Ref, period, digest) }
		default:
			if c = byDigest[digest]; c == nil {
				c = &redisCall{slot: "key"}
				if byDigest == nil {
					byDigest = map[string]*redisCall{}
				}
				byDigest[digest] = c
				slot = append(slot, c)
			}
			key = func(period time.Duration) string { return rlredis.NoTenantKey(digest, p.Ref, period) }
		}
		for _, lim := range lims {
			c.limits = append(c.limits, rlredis.Limit{
				Key: key(lim.Period), Level: p.Level, Enforce: p.Mode == config.Enforce,
				Algorithm: p.Algorithm, Rate: lim.Rate, Period: lim.Period, Burst: lim.Burst, Group: i + 1,
			})
			c.policies = append(c.policies, p)
			c.conf = append(c.conf, lim)
			c.windows = append(c.windows, len(lims))
			c.keys = append(c.keys, raw)
		}
	}
	if q := l.snap.Quotas(l.consumer, l.identity, l.registry.Now()); q.Any() {
		if len(slot) == 0 {
			slot = append(slot, &redisCall{slot: "tenant"})
		}
		l.addQuotas(slot[0], q)
	}
	return append(slot, global...)
}

// The quota limits' stand-in policies (RL-10, ADR-0070): enforce, and
// fail_open alone, since a quota follows the failure modes of the policies
// in its call (failClosed). Their names are the RateLimit and metric names;
// ID 0 keeps them out of the heavy hitters.
var (
	quotaDaily   = &config.Policy{Name: "quota/daily", Level: "consumer", Algorithm: rlredis.Quota, Mode: config.Enforce, FailureMode: config.FailOpen}
	quotaMonthly = &config.Policy{Name: "quota/monthly", Level: "consumer", Algorithm: rlredis.Quota, Mode: config.Enforce, FailureMode: config.FailOpen}
)

// addQuotas adds the request's quotas q to c, its tenant-slot call (RL-10,
// ADR-0070): one limit per period with a quota, for the UTC day and month
// the gateway's clock is in; the script checks that Redis's TIME agrees.
// They follow the policies, so a quota counts only requests every limit of
// the call admits.
func (l *limits) addQuotas(c *redisCall, q config.Quotas) {
	now := l.registry.Now()
	tenant, consumer := l.consumer.Tenant, l.consumer.Name
	for _, x := range []struct {
		policy *config.Policy
		quota  int64
		period quota.Period
	}{{quotaDaily, q.Daily, quota.Day(now)}, {quotaMonthly, q.Monthly, quota.Month(now)}} {
		if x.quota <= 0 {
			continue
		}
		c.limits = append(c.limits, rlredis.Limit{
			Key: rlredis.QuotaKey(tenant, consumer, x.period.ID), Level: "consumer", Enforce: true, Algorithm: rlredis.Quota, Rate: x.quota,
			PeriodStart: x.period.Start, PeriodEnd: x.period.End, Epoch: rlredis.QuotaEpochKey(tenant),
		})
		c.policies = append(c.policies, x.policy)
		c.conf = append(c.conf, config.Limit{Rate: x.quota, Period: time.Duration(x.period.Seconds()) * time.Second})
		c.windows = append(c.windows, 1)
		c.keys = append(c.keys, ratelimit.Key(consumer))
	}
}

// refund gives back everything a denied request holds, shadow limits
// included: local deductions at once, Redis ones asynchronously, one refund
// call per Redis call with the limits it wrote (ADR-0021); costs taken from
// leases go back to them (ADR-0069). The headers show the state after the
// refunds.
func (l *limits) refund() {
	l.putBack()
	for i := range l.checks {
		c := &l.checks[i]
		if !c.held || c.limiter == nil {
			continue
		}
		if r, ok := c.limiter.(refunder); ok {
			c.d = r.Refund(c.key, l.cost, c.at)
		} else {
			c.d, _ = c.limiter.Allow(l.r.Context(), c.key, -l.cost)
		}
		c.held = false
	}
	for _, call := range l.calls {
		var written []rlredis.Limit
		for i, j := range call.checks {
			if j < 0 {
				continue
			}
			if c := &l.checks[j]; c.held {
				written = append(written, call.limits[i])
				c.d = refunded(call.limits[i], c.d, l.cost)
				c.held = false
			}
		}
		if len(written) > 0 {
			// The span starts here, while the request's span is still
			// recording (OB-02).
			span := l.startRedisSpan(l.r.Context(), call.slot, true)
			go l.refundRedis(call.nowUS, span, written)
		}
	}
}

// refundRedis reverses a Redis call's deduction on the limits it wrote. It
// is never retried; a failure leaves at most cost units held until the state
// decays (ADR-0021). It ends span, the refund's, if not nil.
func (l *limits) refundRedis(admittedUS int64, span trace.Span, written []rlredis.Limit) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.r.Context()), l.budget)
	defer cancel()
	_, err := l.redis.Refund(ctx, l.cost, admittedUS, written)
	endRedisSpan(span, true, err) // a refund that went through is "allow"
	if err != nil {
		kind, node := errorKind(err)
		l.rec.RedisError(kind)
		l.rec.RefundFailure()
		l.log.Debug("redis limiter refund failed", "kind", kind, "node", node, "request_id", l.q.ID)
	}
}

// refunded is a Redis limit's decision d once cost is given back, without
// waiting for the refund (ADR-0021): cost more remaining, up to the limit's
// capacity, and for GCRA a reset cost·T sooner.
func refunded(lim rlredis.Limit, d ratelimit.Decision, cost int64) ratelimit.Decision {
	capacity := lim.Rate
	if lim.Algorithm == config.GCRA {
		capacity = lim.Burst
		t := (lim.Period.Microseconds() + lim.Rate - 1) / lim.Rate // µs, as the script (ADR-0023)
		d.ResetAfter = max(0, d.ResetAfter-time.Duration(cost*t)*time.Microsecond)
	}
	d.Remaining = min(d.Remaining+cost, capacity)
	return d
}

// release gives back the concurrency slots the request still holds, once it
// has been answered, timed out or lost its client (RL-02).
func (l *limits) release() {
	for i := range l.checks {
		c := &l.checks[i]
		if r, ok := c.limiter.(*local.Concurrency); ok && c.held {
			r.Release(c.key, l.cost)
			c.held = false
		}
	}
}

// headers returns the RateLimit-Policy and RateLimit field values of the
// enforce-mode limits checked, concurrency excepted, or "" if there are none
// (ADR-0018).
func (l *limits) headers() (policy, limit string) {
	var p, r strings.Builder
	// Two passes: the quota items follow the policies' (ADR-0070).
	for _, quotas := range [2]bool{false, true} {
		for _, c := range l.checks {
			if c.policy.Mode != config.Enforce || c.policy.Algorithm == config.Concurrency || (c.policy.Algorithm == rlredis.Quota) != quotas {
				continue
			}
			if p.Len() > 0 {
				p.WriteString(", ")
				r.WriteString(", ")
			}
			name := c.policy.Name
			if c.windows > 1 {
				name += "/" + strconv.FormatInt(int64(c.limit.Period/time.Second), 10) + "s"
			}
			p.WriteString(`"` + name + `";q=` + strconv.FormatInt(c.limit.Rate, 10) + ";w=" + strconv.FormatInt(int64(c.limit.Period/time.Second), 10))
			r.WriteString(`"` + name + `";r=` + strconv.FormatInt(c.d.Remaining, 10) + ";t=" + strconv.FormatInt(ceilSeconds(c.d.ResetAfter), 10))
		}
	}
	return p.String(), r.String()
}

// retryAfter is the Retry-After of a denied request: the longest RetryAfter
// of the enforce limits that denied it, in whole seconds rounded up, at
// least 1 (RL-25).
func (l *limits) retryAfter() int64 {
	var d time.Duration
	for _, c := range l.checks {
		if !c.d.Allowed && c.policy.Mode == config.Enforce {
			d = max(d, c.d.RetryAfter)
		}
	}
	return max(1, ceilSeconds(d))
}

func ceilSeconds(d time.Duration) int64 {
	return int64((d + time.Second - 1) / time.Second)
}
