package config

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Algorithms and modes of a policy (RL-01, RL-26).
const (
	GCRA          = "gcra"
	SlidingWindow = "sliding_window"
	FixedWindow   = "fixed_window"
	Concurrency   = "concurrency"

	Enforce = "enforce"
	Shadow  = "shadow"
	Off     = "off"

	// Route auth modes (GW-20, ADR-0039).
	AuthNone   = "none"
	AuthAPIKey = "api_key"
	AuthJWT    = "jwt"
	AuthEither = "either"

	FailOpen   = "fail_open"
	FailClosed = "fail_closed"
	Degraded   = "degraded"
)

// Policy validation bounds (ADR-0017).
const (
	// maxQuantity is the largest rate, burst and route cost.
	maxQuantity = 1_000_000_000
	// maxPeriod is the longest period and the largest GCRA burst tolerance
	// τ, which keeps the limiters' nanosecond maths far from overflow.
	maxPeriod = 8760 * time.Hour
	// maxDefaultBurstPeriod is the longest period whose burst defaults to
	// its rate (RL-03).
	maxDefaultBurstPeriod = time.Minute
	// maxCallLimits is the most limits one Redis call carries: the script's
	// shadow_would_deny is a 32-bit mask (ADR-0021).
	maxCallLimits = 32
	// defaultRedisBudget and maxRedisBudget bound ratelimit.redis_budget
	// (RL-22, ADR-0021).
	defaultRedisBudget = 5 * time.Millisecond
	maxRedisBudget     = time.Second
)

var (
	levels     = map[string]bool{"global": true, "tenant": true, "consumer": true, "route": true}
	algorithms = map[string]bool{GCRA: true, SlidingWindow: true, FixedWindow: true, Concurrency: true}
	modes      = map[string]bool{Enforce: true, Shadow: true, Off: true}
	failModes  = map[string]bool{FailOpen: true, FailClosed: true, Degraded: true}
)

// Policy is a compiled rate limiting policy (RL-03, ADR-0017).
type Policy struct {
	Name      string
	Level     string          // global, tenant, consumer or route; picks Redis slots from M3
	Routes    map[string]bool // route names; nil means every route
	Methods   []string        // empty means every method
	Key       KeyTemplate
	Algorithm string // GCRA, SlidingWindow, FixedWindow or Concurrency
	Limits    []Limit
	Mode      string // Enforce, Shadow or Off
	// Local is true for a policy that runs in gateway memory (local: true,
	// or concurrency); the others run in Redis (ADR-0021).
	Local       bool
	FailureMode string // Redis policies: FailOpen, FailClosed or Degraded; "" if Local
	// ID is the control plane's ID of the policy, 0 in static mode. Ref
	// names the policy in limiter IDs and Redis keys: its decimal ID in
	// managed mode, its name in static mode (ADR-0036, ADR-0050).
	ID  int64
	Ref string
	// FromPlan: the policy has no limits of its own and takes them from
	// the consumer's plan (RL-08, ADR-0041).
	FromPlan bool
	// PerConsumer: plans and overrides may set the policy's limits; its
	// level is consumer or route and its key template has {consumer} or
	// {api_key}, so every counter belongs to one consumer (ADR-0041).
	PerConsumer bool
	// PlanLimits are the limits of each plan that has an entry for the
	// policy, by plan ID, as Limits hold them (per replica for a local
	// policy); plans without an ID (static mode) are not kept, since no
	// consumer can be on one (ADR-0050).
	PlanLimits map[int64][]Limit
	// maxLimits is the most limits one request can check: of Limits and
	// of every plan entry; overrides are checked on their own.
	maxLimits int
}

// Limit is one limit of a policy. A local policy's limits hold per-replica
// values, the configured rate and burst ÷ ratelimit.replicas, rounded down,
// minimum 1 (RL-31); a Redis policy's hold the configured, cluster-wide ones
// (ADR-0021).
type Limit struct {
	Rate   int64         // cost units per period; for Concurrency, the max in flight
	Period time.Duration // whole seconds; 0 for Concurrency
	Burst  int64         // GCRA only, after its default
}

// PerReplica is l divided among replicas gateways: rate and burst ÷
// replicas, rounded down, minimum 1 (RL-31). A Redis policy's degraded
// limiters use it during a Redis outage (RL-23, ADR-0021).
func (l Limit) PerReplica(replicas int64) Limit {
	l.Rate = max(1, l.Rate/replicas)
	if l.Burst > 0 {
		l.Burst = max(1, l.Burst/replicas)
	}
	return l
}

// Matches reports whether p's match covers a request for route and method.
func (p *Policy) Matches(route, method string) bool {
	if p.Routes != nil && !p.Routes[route] {
		return false
	}
	if len(p.Methods) == 0 {
		return true
	}
	for _, m := range p.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// compileRateLimit validates c's ratelimit settings into s.
func (s *Snapshot) compileRateLimit(c *configpb.Config) error {
	var err error
	s.Replicas, s.RedisBudget, err = rateLimitSettings(c.GetRatelimit())
	return err
}

// CheckRateLimit checks a config's ratelimit section as Compile does; the
// control plane keeps it as the config settings (ADR-0045).
func CheckRateLimit(r *configpb.RateLimitSettings) error {
	_, _, err := rateLimitSettings(r)
	return err
}

func rateLimitSettings(r *configpb.RateLimitSettings) (replicas int64, budget time.Duration, err error) {
	replicas = int64(r.GetReplicas())
	if replicas < 0 {
		return 0, 0, errors.New("ratelimit.replicas must not be negative")
	}
	budget = defaultRedisBudget
	if v := r.GetRedisBudget(); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > maxRedisBudget {
			return 0, 0, fmt.Errorf("ratelimit.redis_budget: %q is not a duration greater than 0 and at most %v", v, maxRedisBudget)
		}
		budget = d
	}
	return max(replicas, 1), budget, nil
}

// compilePolicies validates the policies of c, whose routes are compiled,
// with replicas gateways sharing each limit; managed names each policy by
// its ID in limiter IDs and Redis keys (ADR-0036).
func compilePolicies(c *configpb.Config, routes []Route, replicas int64, managed bool) ([]Policy, error) {
	names := map[string]bool{}
	var out []Policy
	for _, p := range c.GetPolicies() {
		if p.GetName() == "" {
			return nil, errors.New("policy: name is required")
		}
		if names[p.GetName()] {
			return nil, fmt.Errorf("policy %q: defined twice", p.GetName())
		}
		names[p.GetName()] = true
		pol, err := compilePolicy(p, replicas, routes)
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", p.GetName(), err)
		}
		if managed && pol.ID > 0 {
			pol.Ref = strconv.FormatInt(pol.ID, 10)
		}
		out = append(out, pol)
	}
	return out, nil
}

// checkCallLimits checks that no route's tenant-slot call carries more
// than maxCallLimits limits: the Redis limits of every tenant-, consumer-
// and route-level policy that matches it, each counted at the most one
// request can check (ADR-0021); with extra, policy extra counts n limits
// if that is more, as an override of it would (ADR-0041).
func checkCallLimits(routes []Route, policies []Policy, extra *Policy, n int) error {
	for _, r := range routes {
		sum := 0
		for i := range policies {
			p := &policies[i]
			if !p.Local && p.Level != "global" && p.Mode != Off && (p.Routes == nil || p.Routes[r.Name]) {
				m := p.maxLimits
				if p == extra {
					m = max(m, n)
				}
				sum += m
			}
		}
		if sum > maxCallLimits {
			return fmt.Errorf("route %q would carry %d Redis limits in one call; at most %d", r.Name, sum, maxCallLimits)
		}
	}
	return nil
}

func compilePolicy(p *configpb.Policy, replicas int64, routes []Route) (Policy, error) {
	pol := Policy{Name: p.GetName(), Ref: p.GetName(), ID: p.GetId(), Level: p.GetLevel(), Algorithm: p.GetAlgorithm(), Mode: p.GetMode(), FromPlan: p.GetFromPlan()}
	if !ValidName(pol.Name) {
		return pol, errors.New("name must be 1-64 characters from A-Z, a-z, 0-9, '.', '_' and '-'")
	}
	if !levels[pol.Level] {
		return pol, fmt.Errorf("level %q: want global, tenant, consumer or route", pol.Level)
	}
	if !algorithms[pol.Algorithm] {
		return pol, fmt.Errorf("algorithm %q: want gcra, sliding_window, fixed_window or concurrency", pol.Algorithm)
	}
	if pol.Mode == "" {
		pol.Mode = Enforce
	} else if !modes[pol.Mode] {
		return pol, fmt.Errorf("mode %q: want enforce, shadow or off", pol.Mode)
	}
	// Concurrency is always local (RL-02); the other policies run in Redis
	// unless they say local: true (ADR-0021).
	pol.Local = p.GetLocal() || pol.Algorithm == Concurrency
	if pol.Local {
		if p.GetFailureMode() != "" {
			return pol, errors.New("failure_mode applies only to Redis policies; local policies never fail")
		}
	} else {
		if pol.Algorithm != GCRA && pol.Algorithm != SlidingWindow {
			return pol, fmt.Errorf("algorithm %s must be local (local: true); Redis policies use gcra or sliding_window", pol.Algorithm)
		}
		pol.FailureMode = cmp.Or(p.GetFailureMode(), FailOpen)
		if !failModes[pol.FailureMode] {
			return pol, fmt.Errorf("failure_mode %q: want fail_open, fail_closed or degraded", pol.FailureMode)
		}
	}
	if err := pol.compileMatch(p.GetMatch(), routes); err != nil {
		return pol, err
	}
	var err error
	if pol.Key, err = parseKeyTemplate(p.GetKeyTemplate()); err != nil {
		return pol, err
	}
	pol.PerConsumer = (pol.Level == "consumer" || pol.Level == "route") && pol.Key.perConsumer
	if pol.FromPlan {
		// Plans give the limits (RL-08, ADR-0041).
		if len(p.GetLimits()) > 0 {
			return pol, errors.New("from_plan: the policy takes its limits from plans; remove limits")
		}
		if !pol.PerConsumer {
			return pol, errors.New("from_plan: " + perConsumerRule)
		}
		return pol, nil
	}
	if pol.Limits, err = pol.limitsFor(p.GetLimits(), replicas, routes); err != nil {
		return pol, err
	}
	pol.maxLimits = len(pol.Limits)
	return pol, nil
}

// perConsumerRule is why a policy cannot take limits from plans or
// overrides (ADR-0041).
const perConsumerRule = "plans and overrides apply only to consumer- and route-level policies whose key_template has {consumer} or {api_key}, so that every counter belongs to one consumer"

// limitsFor compiles ls as limits of pol: its own, a plan's or an
// override's, with replicas gateways sharing each limit of a local policy
// (RL-31), and checks that every route pol matches can be admitted under
// them (RL-03).
func (pol *Policy) limitsFor(ls []*configpb.Limit, replicas int64, routes []Route) ([]Limit, error) {
	if !pol.Local && len(ls) > maxCallLimits {
		return nil, fmt.Errorf("limits: a Redis policy has at most %d", maxCallLimits)
	}
	// Redis policies keep the cluster-wide numbers (ADR-0021).
	perReplica := replicas
	if !pol.Local {
		perReplica = 1
	}
	limits, err := compileLimits(pol.Algorithm, ls, perReplica)
	if err != nil {
		return nil, err
	}
	// A route whose cost is above a limit's capacity could never be admitted
	// (RL-03); a degraded policy's per-replica capacity counts too, since it
	// applies during a Redis outage (ADR-0021).
	for _, r := range routes {
		if pol.Routes != nil && !pol.Routes[r.Name] {
			continue
		}
		for i, l := range limits {
			capacity := l.Rate
			if pol.Algorithm == GCRA {
				capacity = l.Burst
			}
			switch {
			case r.Cost > capacity && pol.Local:
				return nil, fmt.Errorf("route %q costs %d, more than the %d that limits[%d] admits per replica, so its requests could never be admitted", r.Name, r.Cost, capacity, i)
			case r.Cost > capacity:
				return nil, fmt.Errorf("route %q costs %d, more than the %d that limits[%d] admits, so its requests could never be admitted", r.Name, r.Cost, capacity, i)
			case pol.FailureMode == Degraded && r.Cost > max(1, capacity/replicas):
				return nil, fmt.Errorf("route %q costs %d, more than the %d that limits[%d] admits per replica in degraded mode, so its requests could never be admitted during a Redis outage", r.Name, r.Cost, max(1, capacity/replicas), i)
			}
		}
	}
	return limits, nil
}

// OverrideLimits compiles an override's limits for p as limitsFor does, with
// replicas gateways sharing each limit of a local policy, or returns false
// if they do not fit p. The control plane checks every override against
// the policy at its write and at every publish (ADR-0041), so false means
// a control plane that skipped them; the request then goes on without the
// override (ADR-0050). Route costs are not checked here.
func (p *Policy) OverrideLimits(ls []*configpb.Limit, replicas int64) ([]Limit, bool) {
	if !p.PerConsumer || !p.Local && len(ls) > maxCallLimits {
		return nil, false
	}
	if !p.Local {
		replicas = 1
	}
	limits, err := compileLimits(p.Algorithm, ls, replicas)
	return limits, err == nil
}

func (pol *Policy) compileMatch(m *configpb.PolicyMatch, routes []Route) error {
	rs := m.GetRoutes()
	switch {
	case len(rs) == 0:
		return errors.New(`match.routes is required; ["*"] matches every route`)
	case len(rs) == 1 && rs[0] == "*":
	default:
		pol.Routes = map[string]bool{}
		for _, name := range rs {
			if name == "*" {
				return errors.New(`match.routes: "*" must be the only entry`)
			}
			if pol.Routes[name] {
				return fmt.Errorf("match.routes: %q listed twice", name)
			}
			if !hasRoute(routes, name) {
				return fmt.Errorf("match.routes: route %q is not defined", name)
			}
			pol.Routes[name] = true
		}
	}
	for _, method := range m.GetMethods() {
		if method == "" || strings.ContainsFunc(method, notTokenChar) || method != strings.ToUpper(method) {
			return fmt.Errorf("match.methods: %q is not an upper-case method such as GET", method)
		}
		for _, seen := range pol.Methods {
			if seen == method {
				return fmt.Errorf("match.methods: %q listed twice", method)
			}
		}
		pol.Methods = append(pol.Methods, method)
	}
	pol.Methods = withHead(pol.Methods) // D1
	return nil
}

func hasRoute(routes []Route, name string) bool {
	for _, r := range routes {
		if r.Name == name {
			return true
		}
	}
	return false
}

// compileLimits compiles the limits of a policy with algorithm, dividing
// each among replicas gateways (RL-31).
func compileLimits(algorithm string, ls []*configpb.Limit, replicas int64) ([]Limit, error) {
	if len(ls) == 0 {
		return nil, errors.New("limits: at least one is required")
	}
	if algorithm == Concurrency {
		if len(ls) != 1 || ls[0].GetPeriod() != "" || ls[0].GetBurst() != 0 {
			return nil, errors.New("limits: concurrency takes exactly one limit, with only rate (the max in flight)")
		}
	}
	var out []Limit
	periods := map[time.Duration]bool{}
	for i, l := range ls {
		if l.GetRate() < 1 || l.GetRate() > maxQuantity {
			return nil, fmt.Errorf("limits[%d].rate: %d is not 1 to %d", i, l.GetRate(), maxQuantity)
		}
		lim := Limit{Rate: l.GetRate()}
		if algorithm == Concurrency {
			out = append(out, lim.PerReplica(replicas))
			continue
		}
		d, err := time.ParseDuration(l.GetPeriod())
		if err != nil || d < time.Second || d > maxPeriod || d%time.Second != 0 {
			return nil, fmt.Errorf("limits[%d].period: %q is not a duration in whole seconds from 1s to 8760h", i, l.GetPeriod())
		}
		if periods[d] {
			return nil, fmt.Errorf("limits[%d].period: another limit has period %v", i, d)
		}
		periods[d] = true
		lim.Period = d
		if algorithm != GCRA {
			if l.GetBurst() != 0 {
				return nil, fmt.Errorf("limits[%d].burst: only gcra has a burst", i)
			}
			out = append(out, lim.PerReplica(replicas))
			continue
		}
		burst := l.GetBurst()
		switch {
		case burst == 0 && d > maxDefaultBurstPeriod:
			return nil, fmt.Errorf("limits[%d].burst: required for periods over %v (RL-03)", i, maxDefaultBurstPeriod)
		case burst == 0:
			burst = l.GetRate()
		case burst < 1 || burst > maxQuantity:
			return nil, fmt.Errorf("limits[%d].burst: %d is not 1 to %d", i, burst, maxQuantity)
		}
		lim.Burst = burst
		lim = lim.PerReplica(replicas)
		// τ = T·burst, with T = period/rate rounded up as the limiter does.
		if t := (int64(d) + lim.Rate - 1) / lim.Rate; lim.Burst > int64(maxPeriod)/t {
			return nil, fmt.Errorf("limits[%d].burst: %d per replica at %d per %v is more than %v of burst tolerance", i, lim.Burst, lim.Rate, d, maxPeriod)
		}
		out = append(out, lim)
	}
	return out, nil
}

// ValidName reports whether name is 1-64 characters from A-Z, a-z, 0-9, '.',
// '_' and '-', the rule for policy, plan and control-plane entity names
// (ADR-0017, ADR-0036).
func ValidName(name string) bool {
	return name != "" && len(name) <= 64 && !strings.ContainsFunc(name, notNameChar)
}

// compilePlans checks the plans of a config against its compiled policies
// and gives each policy its plans' limits (RL-08, ADR-0041): every entry
// names a policy that plans may set, its limits pass the policy's rules
// and admit every route it matches, and every plan has an entry for every
// from_plan policy.
func compilePlans(plans []*configpb.Plan, policies []Policy, routes []Route, replicas int64) error {
	names, ids := map[string]bool{}, map[int64]string{}
	for _, p := range plans {
		if err := compilePlan(p); err != nil {
			return fmt.Errorf("plan %q: %w", p.GetName(), err)
		}
		if names[p.GetName()] {
			return fmt.Errorf("plan %q: defined twice", p.GetName())
		}
		names[p.GetName()] = true
		if id := p.GetId(); id > 0 {
			if other, ok := ids[id]; ok {
				return fmt.Errorf("plan %q: id %d is plan %q's", p.GetName(), id, other)
			}
			ids[id] = p.GetName()
		}
		for i, e := range p.GetDefaultLimits() {
			j := slices.IndexFunc(policies, func(pol Policy) bool { return pol.Name == e.GetPolicy() })
			if j < 0 {
				return fmt.Errorf("plan %q: default_limits[%d]: policy %q is not defined", p.GetName(), i, e.GetPolicy())
			}
			pol := &policies[j]
			if !pol.PerConsumer {
				return fmt.Errorf("plan %q: default_limits[%d]: policy %q: %s", p.GetName(), i, pol.Name, perConsumerRule)
			}
			limits, err := pol.limitsFor(e.GetLimits(), replicas, routes)
			if err != nil {
				return fmt.Errorf("plan %q: default_limits[%d] (policy %q): %w", p.GetName(), i, pol.Name, err)
			}
			pol.maxLimits = max(pol.maxLimits, len(limits))
			if p.GetId() > 0 {
				if pol.PlanLimits == nil {
					pol.PlanLimits = map[int64][]Limit{}
				}
				pol.PlanLimits[p.GetId()] = limits
			}
		}
		for j := range policies {
			pol := &policies[j]
			if pol.FromPlan && !slices.ContainsFunc(p.GetDefaultLimits(), func(e *configpb.PlanLimits) bool { return e.GetPolicy() == pol.Name }) {
				return fmt.Errorf("plan %q: no default_limits for policy %q, which takes its limits from plans (from_plan)", p.GetName(), pol.Name)
			}
		}
	}
	return nil
}

// compilePlan checks the rules of one plan that need no policy: its name,
// and one entry per policy, each with limits.
func compilePlan(p *configpb.Plan) error {
	if !ValidName(p.GetName()) {
		return errors.New("name must be 1-64 characters from A-Z, a-z, 0-9, '.', '_' and '-'")
	}
	seen := map[string]bool{}
	for i, e := range p.GetDefaultLimits() {
		switch {
		case e.GetPolicy() == "":
			return fmt.Errorf("default_limits[%d].policy is required", i)
		case seen[e.GetPolicy()]:
			return fmt.Errorf("default_limits[%d]: policy %q has another entry", i, e.GetPolicy())
		case len(e.GetLimits()) == 0:
			return fmt.Errorf("default_limits[%d].limits: at least one is required", i)
		}
		seen[e.GetPolicy()] = true
	}
	return nil
}

func notNameChar(c rune) bool {
	return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-'
}

// Key template placeholders (RL-04, ADR-0017).
type keyField uint8

const (
	literal keyField = iota
	fieldIP
	fieldAPIKey
	fieldConsumer
	fieldTenant
	fieldRoute
	fieldMethod
	fieldHeader
)

var keyFields = map[string]keyField{
	"ip": fieldIP, "api_key": fieldAPIKey, "consumer": fieldConsumer, "tenant": fieldTenant,
	"route": fieldRoute, "method": fieldMethod,
}

// KeyTemplate is a compiled key template: literal text and placeholders
// (RL-04, ADR-0017).
type KeyTemplate struct {
	parts []keyPart
	// UsesIdentity is true when the template has {api_key}, {consumer} or
	// {tenant}, so it can be filled only after API key resolution.
	UsesIdentity bool
	// ClientChosen is true when the template has a {header:<Name>}, whose
	// values the client picks freely (D2, ADR-0030).
	ClientChosen bool
	// perConsumer is true when the template has {consumer} or {api_key}.
	perConsumer bool
}

type keyPart struct {
	field keyField
	text  string // the literal, or the canonical header name for fieldHeader
}

// KeyValues are what a request fills key templates with; an empty value
// leaves its placeholder unfilled.
type KeyValues struct {
	IP       string // the client IP (SEC-05)
	APIKey   string // lower-case hex SHA-256 of the API key, never the key
	Consumer string
	Tenant   string
	Route    string
	Method   string
	Header   http.Header
}

// Fill returns the filled template, or false if a placeholder has no value;
// the policy then does not apply to the request.
func (t *KeyTemplate) Fill(v *KeyValues) (string, bool) {
	var b strings.Builder
	for _, p := range t.parts {
		var s string
		switch p.field {
		case literal:
			b.WriteString(p.text)
			continue
		case fieldIP:
			s = v.IP
		case fieldAPIKey:
			s = v.APIKey
		case fieldConsumer:
			s = v.Consumer
		case fieldTenant:
			s = v.Tenant
		case fieldRoute:
			s = v.Route
		case fieldMethod:
			s = v.Method
		case fieldHeader:
			if hv := v.Header[p.text]; len(hv) > 0 {
				s = hv[0]
			}
		}
		if s == "" {
			return "", false
		}
		b.WriteString(s)
	}
	return b.String(), true
}

func parseKeyTemplate(tmpl string) (KeyTemplate, error) {
	var t KeyTemplate
	if tmpl == "" {
		return t, errors.New("key_template is required")
	}
	for s := tmpl; s != ""; {
		i := strings.IndexAny(s, "{}")
		if i < 0 {
			t.parts = append(t.parts, keyPart{text: s})
			break
		}
		if s[i] == '}' {
			return t, fmt.Errorf("key_template %q: '}' without '{'", tmpl)
		}
		if i > 0 {
			t.parts = append(t.parts, keyPart{text: s[:i]})
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return t, fmt.Errorf("key_template %q: '{' without '}'", tmpl)
		}
		name := s[i+1 : i+j]
		s = s[i+j+1:]
		if h, ok := strings.CutPrefix(name, "header:"); ok {
			if h == "" || strings.ContainsFunc(h, notTokenChar) {
				return t, fmt.Errorf("key_template %q: header name %q is not valid", tmpl, h)
			}
			t.parts = append(t.parts, keyPart{field: fieldHeader, text: http.CanonicalHeaderKey(h)})
			t.ClientChosen = true
			continue
		}
		f, ok := keyFields[name]
		if !ok {
			return t, fmt.Errorf("key_template %q: unknown placeholder {%s}; want {ip}, {api_key}, {consumer}, {tenant}, {route}, {method} or {header:<Name>}", tmpl, name)
		}
		t.parts = append(t.parts, keyPart{field: f})
		t.UsesIdentity = t.UsesIdentity || f == fieldAPIKey || f == fieldConsumer || f == fieldTenant
		t.perConsumer = t.perConsumer || f == fieldAPIKey || f == fieldConsumer
	}
	return t, nil
}
