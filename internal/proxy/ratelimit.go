package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// limits evaluates one request's rate limit policies in two phases, before
// and after API key resolution (RL-26, ADR-0017), and writes what they
// decided into the response headers (RL-25, ADR-0018) and q (RL-27).
type limits struct {
	policies []config.Policy
	registry *local.Registry
	rec      *telemetry.Recorder
	r        *http.Request
	q        *telemetry.Request
	cost     int64
	values   config.KeyValues
	checks   []check // in evaluation order
	denied   bool    // an enforce limit denied the request
}

// check is one limit's evaluation for the request.
type check struct {
	policy  *config.Policy
	limit   config.Limit
	limiter ratelimit.Limiter
	key     string
	d       ratelimit.Decision // the last decision: the refund's once refunded
	at      time.Time          // on the registry's clock, at or before the admission
	held    bool               // admitted and not given back
}

// refunder is a window limiter, whose refund is lost if it crosses a window
// boundary since the admission (ADR-0017).
type refunder interface {
	Refund(key string, cost int64, at time.Time) ratelimit.Decision
}

// newLimits prepares the checks of a request for route on snapshot s, sent
// by the client at ip.
func newLimits(s *config.Snapshot, route *config.Route, registry *local.Registry, rec *telemetry.Recorder, r *http.Request, q *telemetry.Request, ip netip.Addr) *limits {
	l := &limits{
		policies: s.Policies, registry: registry, rec: rec, r: r, q: q, cost: route.Cost,
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

// check evaluates one phase: the policies whose key templates need no
// identity, or with identity true the rest, in config order. Every limit of
// every applying policy is checked, even after a deny. If an enforce limit
// has denied the request, in this phase or the first, every admission of
// both phases is refunded and check returns false.
func (l *limits) check(identity bool) bool {
	if len(l.policies) == 0 {
		return true
	}
	start := time.Now()
	at := l.registry.Now()
	n := len(l.checks)
	for i := range l.policies {
		p := &l.policies[i]
		if p.Mode == config.Off || p.Key.UsesIdentity != identity || !p.Matches(l.values.Route, l.values.Method) {
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
		// A denied request consumes nothing, shadow limits included.
		for i := range l.checks {
			if c := &l.checks[i]; c.held {
				if r, ok := c.limiter.(refunder); ok {
					c.d = r.Refund(c.key, l.cost, c.at)
				} else {
					c.d, _ = c.limiter.Allow(l.r.Context(), c.key, -l.cost)
				}
				c.held = false
			}
		}
	}
	if len(l.checks) > n {
		l.rec.RateLimitCheck(telemetry.BackendLocal, time.Since(start))
	}
	return !l.denied
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
