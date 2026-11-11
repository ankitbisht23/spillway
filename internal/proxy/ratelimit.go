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

	"github.com/ankitbisht23/spillway/internal/config"
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
	policies []config.Policy
	registry *local.Registry
	redis    Redis         // nil without Redis: the gateway then has no Redis policies
	budget   time.Duration // one Redis time budget for all of the request's calls (RL-22)
	replicas int64         // degraded limiters' divisor (RL-31)
	rec      *telemetry.Recorder
	log      *slog.Logger
	r        *http.Request
	q        *telemetry.Request
	cost     int64
	values   config.KeyValues
	checks   []check      // in evaluation order
	calls    []*redisCall // the Redis calls that answered, in order
	denied   bool         // an enforce limit denied the request
	// unavailable: Redis failed for an enforce fail_closed policy, so the
	// request is answered 503 (RL-23).
	unavailable bool
}

// check is one limit's evaluation for the request.
type check struct {
	policy  *config.Policy
	limit   config.Limit      // a degraded limiter's per-replica limit
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
	checks   []int            // index in limits.checks of each limit, once answered
	nowUS    int64            // the call's Redis TIME, which its refund names
}

// errBudgetSpent fails a Redis call that the request's budget left no time
// for; it is never sent (RL-22).
var errBudgetSpent = errors.New("redis time budget spent before the call")

// refunder is a window limiter, whose refund is lost if it crosses a window
// boundary since the admission (ADR-0017).
type refunder interface {
	Refund(key string, cost int64, at time.Time) ratelimit.Decision
}

// newLimits prepares the checks of a request for route on snapshot s, sent
// by the client at ip.
func newLimits(s *config.Snapshot, route *config.Route, registry *local.Registry, redis Redis, rec *telemetry.Recorder, log *slog.Logger, r *http.Request, q *telemetry.Request, ip netip.Addr) *limits {
	l := &limits{
		policies: s.Policies, registry: registry, redis: redis, budget: s.RedisBudget, replicas: s.Replicas, rec: rec, log: log, r: r, q: q, cost: route.Cost,
		// Upper-cased: many upstreams take "post" for POST, so it must not
		// skip POST policies or get a {method} bucket of its own (ADR-0020).
		values: config.KeyValues{Route: route.Name, Method: strings.ToUpper(r.Method), Header: r.Header},
	}
	if ip.IsValid() && len(s.Policies) > 0 {
		l.values.IP = ip.String() // invalid only if the peer address did not parse: {ip} stays unfilled
	}
	return l
}

// identify fills the identity placeholders for phase 2 from the request's
// resolved API key; with no key they stay unfilled.
func (l *limits) identify(c *config.Consumer, key string) {
	if c == nil {
		return
	}
	sum := sha256.Sum256([]byte(key))
	l.values.APIKey = hex.EncodeToString(sum[:])
	l.values.Consumer, l.values.Tenant = c.Name, c.Tenant
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
	for i := range l.policies {
		p := &l.policies[i]
		if !p.Local || p.Mode == config.Off || p.Key.UsesIdentity != identity || !p.Matches(l.values.Route, l.values.Method) {
			continue
		}
		filled, ok := p.Key.Fill(&l.values)
		if !ok {
			continue // a placeholder without a value: the policy does not apply
		}
		key := ratelimit.Key(filled)
		decision := "allow"
		applied := false
		for _, lim := range p.Limits {
			limiter := l.registry.Limiter(local.LimitID(p, lim))
			if limiter == nil {
				continue // removed by a reload since this request's snapshot
			}
			applied = true
			d, _ := limiter.Allow(l.r.Context(), key, l.cost) // local limiters never fail
			l.checks = append(l.checks, check{policy: p, limit: lim, limiter: limiter, key: key, d: d, at: at, held: d.Allowed})
			if !d.Allowed {
				decision = "deny"
				l.denied = l.denied || p.Mode == config.Enforce
			}
		}
		if applied {
			l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: p.Name, Decision: decision, Mode: p.Mode})
		}
	}
	if l.denied {
		l.refund()
	}
	if len(l.checks) > n {
		l.rec.RateLimitCheck(telemetry.BackendLocal, time.Since(start))
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
// false; unavailable then says whether the answer is 503 (RL-23).
func (l *limits) checkRedis() bool {
	if l.redis == nil {
		return true
	}
	calls := l.redisCalls()
	if len(calls) == 0 {
		return true
	}
	start := time.Now()
	defer func() { l.rec.RateLimitCheck(telemetry.BackendRedis, time.Since(start)) }()
	deadline := start.Add(l.budget)
	// The request's values reach the calls, but a client that goes away
	// cannot cut a sent call short.
	ctx, cancel := context.WithDeadline(context.WithoutCancel(l.r.Context()), deadline)
	defer cancel()
	for _, c := range calls {
		var res rlredis.Result
		err := error(&rlredis.Error{Kind: rlredis.KindTimeout, Err: errBudgetSpent})
		if time.Now().Before(deadline) {
			res, err = l.redis.Call(ctx, l.cost, c.limits)
		}
		if err != nil {
			if !l.failed(c, err) {
				return false
			}
			continue
		}
		c.nowUS = res.NowUS
		l.calls = append(l.calls, c)
		for i, lim := range c.limits {
			c.checks = append(c.checks, len(l.checks))
			// Written: enforce limits of an allowed call, and its shadow
			// limits that would allow (the script's write rule).
			written := res.Allowed && (lim.Enforce || res.ShadowWouldDeny&(1<<i) == 0)
			l.checks = append(l.checks, check{policy: c.policies[i], limit: c.conf[i], d: res.Limits[i], held: written})
		}
		l.decide(c, func(i int) string {
			if res.Limits[i].Allowed {
				return "allow"
			}
			return "deny"
		})
		if !res.Allowed {
			l.denied = true
			l.refund()
			return false
		}
	}
	return true
}

// failed applies the failure modes of the policies in c, a call that failed
// (RL-23, ADR-0021). An enforce fail_closed policy makes the request a 503,
// and nothing else in c is evaluated; otherwise every degraded policy is
// checked against its in-memory limiter, and an enforce one that denies
// makes it a 429. fail_open policies allow, and shadow policies never change
// the outcome. It returns false, with everything the request holds
// refunded, if the request ends here.
func (l *limits) failed(c *redisCall, err error) bool {
	kind, node := errorKind(err)
	l.rec.RedisError(kind)
	// Debug: during an outage every request fails; the metric and the
	// breaker's transition logs carry it.
	l.log.Debug("redis limiter call failed; failure modes apply", "kind", kind, "node", node, "request_id", l.q.ID)
	for _, p := range c.policies {
		l.unavailable = l.unavailable || p.FailureMode == config.FailClosed && p.Mode == config.Enforce
	}
	at := l.registry.Now()
	for i := 0; i < len(c.limits); {
		p, decision, applied := c.policies[i], "allow", true
		j := i
		for j < len(c.limits) && c.policies[j] == p {
			j++
		}
		switch p.FailureMode {
		case config.FailOpen:
			decision = "fail_open"
		case config.FailClosed:
			decision = "fail_closed"
		default: // config.Degraded
			applied = false
			if l.unavailable {
				break // not evaluated: the request is answered 503
			}
			for k := i; k < j; k++ {
				limiter := l.registry.Limiter(local.DegradedID(p, c.conf[k]))
				if limiter == nil {
					continue // removed by a reload since this request's snapshot
				}
				applied = true
				// Keyed like the Redis key, so a tenant's degraded state is
				// its own as in Redis.
				key := ratelimit.Key(c.limits[k].Key)
				d, _ := limiter.Allow(l.r.Context(), key, l.cost) // local limiters never fail
				l.checks = append(l.checks, check{policy: p, limit: c.conf[k].PerReplica(l.replicas), limiter: limiter, key: key, d: d, at: at, held: d.Allowed})
				if !d.Allowed {
					decision = "deny"
					l.denied = l.denied || p.Mode == config.Enforce
				}
			}
		}
		if applied {
			l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: p.Name, Decision: decision, Mode: p.Mode})
		}
		i = j
	}
	if l.unavailable || l.denied {
		l.refund()
		return false
	}
	return true
}

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
		p, d := c.policies[i], "allow"
		for ; i < len(c.limits) && c.policies[i] == p; i++ {
			if x := decision(i); d == "allow" {
				d = x
			}
		}
		l.q.Decisions = append(l.q.Decisions, telemetry.Decision{Policy: p.Name, Decision: d, Mode: p.Mode})
	}
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
		digest := rlredis.Digest(filled)
		var c *redisCall
		var key func(period time.Duration) string
		switch {
		case p.Level == "global":
			c = &redisCall{}
			global = append(global, c)
			key = func(period time.Duration) string { return rlredis.GlobalKey(p.Name, period, digest) }
		case l.values.Tenant != "":
			if len(slot) == 0 {
				slot = append(slot, &redisCall{})
			}
			c = slot[0]
			key = func(period time.Duration) string { return rlredis.TenantKey(l.values.Tenant, p.Name, period, digest) }
		default:
			if c = byDigest[digest]; c == nil {
				c = &redisCall{}
				if byDigest == nil {
					byDigest = map[string]*redisCall{}
				}
				byDigest[digest] = c
				slot = append(slot, c)
			}
			key = func(period time.Duration) string { return rlredis.NoTenantKey(digest, p.Name, period) }
		}
		for _, lim := range p.Limits {
			c.limits = append(c.limits, rlredis.Limit{
				Key: key(lim.Period), Level: p.Level, Enforce: p.Mode == config.Enforce,
				Algorithm: p.Algorithm, Rate: lim.Rate, Period: lim.Period, Burst: lim.Burst,
			})
			c.policies = append(c.policies, p)
			c.conf = append(c.conf, lim)
		}
	}
	return append(slot, global...)
}

// refund gives back everything a denied request holds, shadow limits
// included: local deductions at once, Redis ones asynchronously, one refund
// call per Redis call with the limits it wrote (ADR-0021). The headers show
// the state after the refunds.
func (l *limits) refund() {
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
			if c := &l.checks[j]; c.held {
				written = append(written, call.limits[i])
				c.d = refunded(call.limits[i], c.d, l.cost)
				c.held = false
			}
		}
		if len(written) > 0 {
			go l.refundRedis(call.nowUS, written)
		}
	}
}

// refundRedis reverses a Redis call's deduction on the limits it wrote. It
// is never retried; a failure leaves at most cost units held until the state
// decays (ADR-0021).
func (l *limits) refundRedis(admittedUS int64, written []rlredis.Limit) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.r.Context()), l.budget)
	defer cancel()
	if _, err := l.redis.Refund(ctx, l.cost, admittedUS, written); err != nil {
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
	for _, c := range l.checks {
		if c.policy.Mode != config.Enforce || c.policy.Algorithm == config.Concurrency {
			continue
		}
		if p.Len() > 0 {
			p.WriteString(", ")
			r.WriteString(", ")
		}
		name := c.policy.Name
		if len(c.policy.Limits) > 1 {
			name += "/" + strconv.FormatInt(int64(c.limit.Period/time.Second), 10) + "s"
		}
		p.WriteString(`"` + name + `";q=` + strconv.FormatInt(c.limit.Rate, 10) + ";w=" + strconv.FormatInt(int64(c.limit.Period/time.Second), 10))
		r.WriteString(`"` + name + `";r=` + strconv.FormatInt(c.d.Remaining, 10) + ";t=" + strconv.FormatInt(ceilSeconds(c.d.ResetAfter), 10))
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
