package live

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The named-query catalogue (UI-03, D6, ADR-0059): the browser names
// queries and gives parameter values; only this file turns them into
// PromQL, and every value it puts there has been checked against a fixed
// list or the store, and escaped.

// Source is where a query's data comes from.
type source int

const (
	fromPrometheus source = iota
	fromPostgres
)

func (s source) String() string {
	if s == fromPostgres {
		return "postgres"
	}
	return "prometheus"
}

// Parameter names, in a key's fixed order.
const (
	paramRoute    = "route"
	paramTenant   = "tenant"
	paramRange    = "range"
	paramPolicyID = "policy_id"
)

var paramOrder = []string{paramRoute, paramTenant, paramRange, paramPolicyID}

// query is one named query: its source, the parameters it uses and how it
// is evaluated.
type query struct {
	name   string
	source source
	params []string
	eval   func(ctx context.Context, h *Hub, p Params, at time.Time) (any, error)
}

// catalogue is every named query, by name (ADR-0059's table; routes.*
// arrive with the routes task, ADR-0060).
var catalogue = map[string]*query{
	"overview.requests":    {source: fromPrometheus, params: []string{paramRoute, paramTenant, paramRange}, eval: evalRequests},
	"overview.ratelimited": {source: fromPrometheus, params: []string{paramRoute, paramTenant, paramRange}, eval: evalRateLimited},
	"overview.latency":     {source: fromPrometheus, params: []string{paramRoute, paramTenant, paramRange}, eval: evalLatency},
	"overview.routes":      {source: fromPrometheus, params: []string{paramTenant, paramRange}, eval: evalTop("route")},
	"overview.tenants":     {source: fromPrometheus, params: []string{paramRoute, paramRange}, eval: evalTop("tenant")},
	"throttling.policies":  {source: fromPrometheus, params: []string{paramRange}, eval: evalPolicies},
	"throttling.heavy_hitters": {source: fromPostgres, params: []string{paramRange, paramPolicyID}, eval: func(ctx context.Context, h *Hub, p Params, at time.Time) (any, error) {
		return h.cfg.Postgres.HeavyHitters(ctx, at.Add(-p.Range.span), p.PolicyID)
	}},
	"replicas": {source: fromPostgres, eval: func(ctx context.Context, h *Hub, _ Params, _ time.Time) (any, error) {
		return h.cfg.Postgres.Replicas(ctx)
	}},
}

func init() {
	for name, q := range catalogue {
		q.name = name
	}
}

// Names are the catalogue's query names, sorted.
func Names() []string {
	names := make([]string, 0, len(catalogue))
	for n := range catalogue {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// timeRange is one of the fixed ranges, with its step and rate window.
type timeRange struct {
	name   string
	span   time.Duration
	step   time.Duration
	window string // a PromQL duration
}

var ranges = []timeRange{
	{"15m", 15 * time.Minute, 15 * time.Second, "1m"},
	{"1h", time.Hour, time.Minute, "2m"},
	{"6h", 6 * time.Hour, 5 * time.Minute, "10m"},
	{"24h", 24 * time.Hour, 15 * time.Minute, "30m"},
	{"7d", 7 * 24 * time.Hour, time.Hour, "2h"},
}

// Params are a stream's validated parameters. Route and Tenant are "*"
// for all.
type Params struct {
	Route, Tenant string
	Range         timeRange
	PolicyID      *int64
}

// value is p's value of parameter name, as it goes in a key.
func (p Params) value(name string) string {
	switch name {
	case paramRoute:
		return p.Route
	case paramTenant:
		return p.Tenant
	case paramRange:
		return p.Range.name
	case paramPolicyID:
		if p.PolicyID != nil {
			return strconv.FormatInt(*p.PolicyID, 10)
		}
	}
	return ""
}

// key is the evaluation key of q under p: its name and the values of the
// parameters it uses, in a fixed order, so streams that differ only in a
// parameter q ignores share it.
func (q *query) key(p Params) string {
	var b strings.Builder
	b.WriteString(q.name)
	for _, name := range paramOrder {
		if slices.Contains(q.params, name) {
			b.WriteString("&" + name + "=" + url.QueryEscape(p.value(name)))
		}
	}
	return b.String()
}

// Limits on a stream's subscriptions.
const maxSubs = 8

// nameRE is ADR-0036's rule for entity names, which route and tenant
// values must also match.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// policyIDRE is a positive decimal integer without a sign or leading zeros.
var policyIDRE = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// InvalidError is a request the stream refuses before it starts (400). Its
// message names the parameter at fault, never a value.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// request is a stream's validated subscriptions and parameters.
type request struct {
	subs   []*query
	params Params
}

// parse checks a stream's query string without the store: every
// parameter known, given once (sub 1 to 8 distinct times) and used by a
// requested query, and every value of the right form.
func parse(q url.Values) (request, error) {
	r := request{params: Params{Route: "*", Tenant: "*", Range: ranges[0]}}
	for name, values := range q {
		if name != "sub" && !slices.Contains(paramOrder, name) {
			return r, invalid("unknown query parameter: the stream takes sub, route, tenant, range and policy_id")
		}
		if name != "sub" && len(values) != 1 {
			return r, invalid("%s: give it at most once", name)
		}
	}
	subs := q["sub"]
	if len(subs) == 0 || len(subs) > maxSubs {
		return r, invalid("sub: give 1 to %d named queries", maxSubs)
	}
	for _, s := range subs {
		qu, ok := catalogue[s]
		if !ok {
			return r, invalid("sub: not a named query; the catalogue has %s", strings.Join(Names(), ", "))
		}
		if slices.Contains(r.subs, qu) {
			return r, invalid("sub: each named query at most once")
		}
		r.subs = append(r.subs, qu)
	}
	for _, name := range paramOrder {
		if !q.Has(name) {
			continue
		}
		used := false
		for _, qu := range r.subs {
			used = used || slices.Contains(qu.params, name)
		}
		if !used {
			return r, invalid("%s: none of the requested queries takes it", name)
		}
	}
	if q.Has(paramRoute) {
		v := q.Get(paramRoute)
		if v != "*" && !nameRE.MatchString(v) {
			return r, invalid("route: want * or a route name of 1-64 characters from [A-Za-z0-9._-]")
		}
		r.params.Route = v
	}
	if q.Has(paramTenant) {
		v := q.Get(paramTenant)
		if v != "*" && !nameRE.MatchString(v) {
			return r, invalid("tenant: want *, none, other or a tenant name of 1-64 characters from [A-Za-z0-9._-]")
		}
		r.params.Tenant = v
	}
	if q.Has(paramRange) {
		i := slices.IndexFunc(ranges, func(t timeRange) bool { return t.name == q.Get(paramRange) })
		if i < 0 {
			return r, invalid("range: want 15m, 1h, 6h, 24h or 7d")
		}
		r.params.Range = ranges[i]
	}
	if q.Has(paramPolicyID) {
		v := q.Get(paramPolicyID)
		id, err := strconv.ParseInt(v, 10, 64)
		if !policyIDRE.MatchString(v) || err != nil {
			return r, invalid("policy_id: want a policy ID, an integer of at least 1")
		}
		r.params.PolicyID = &id
	}
	return r, nil
}

// check looks route and tenant up in the store: a route of the latest
// published config, a tenant that exists, or one of the fixed values.
func (r request) check(ctx context.Context, pg Postgres) error {
	if r.params.Route != "*" {
		ok, err := pg.RouteExists(ctx, r.params.Route)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("route: no route of that name in the latest published config version")
		}
	}
	switch r.params.Tenant {
	case "*", "none", "other":
	default:
		ok, err := pg.TenantExists(ctx, r.params.Tenant)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("tenant: no tenant of that name")
		}
	}
	return nil
}

// PromQL. Values reach a query only through promString, inside a
// double-quoted label matcher.

// promString is s as a PromQL double-quoted string: backslash, double
// quote and newline escaped. Route and tenant values cannot hold them
// (nameRE), but nothing relies on that.
func promString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}

// matchers are the label matchers of the given parameters ("*" adds
// none) plus extra, comma-separated, without braces.
func matchers(p Params, names []string, extra ...string) string {
	var m []string
	for _, name := range names {
		if v := p.value(name); v != "*" {
			m = append(m, name+"="+promString(v))
		}
	}
	return strings.Join(append(m, extra...), ",")
}

func evalRequests(ctx context.Context, h *Hub, p Params, at time.Time) (any, error) {
	f := matchers(p, []string{paramRoute, paramTenant})
	return h.rangeQuery(ctx, p.Range, at, fmt.Sprintf(`sum by (code_class) (rate(gw_requests_total{%s}[%s]))`, f, p.Range.window))
}

func evalRateLimited(ctx context.Context, h *Hub, p Params, at time.Time) (any, error) {
	f := matchers(p, []string{paramRoute, paramTenant})
	limited := matchers(p, []string{paramRoute, paramTenant}, `reason="ratelimited"`)
	return h.rangeQuery(ctx, p.Range, at, fmt.Sprintf(`sum(rate(gw_requests_total{%s}[%s])) / sum(rate(gw_requests_total{%s}[%s]))`,
		limited, p.Range.window, f, p.Range.window))
}

func evalLatency(ctx context.Context, h *Hub, p Params, at time.Time) (any, error) {
	f := matchers(p, []string{paramRoute, paramTenant})
	out := matrix{Kind: "matrix", StepS: int(p.Range.step / time.Second), Series: []rangeSeries{}}
	for _, q := range []string{"0.5", "0.99"} {
		m, err := h.rangeQuery(ctx, p.Range, at, fmt.Sprintf(`histogram_quantile(%s, sum by (le) (rate(gw_request_duration_seconds_bucket{%s}[%s])))`, q, f, p.Range.window))
		if err != nil {
			return nil, err
		}
		for _, s := range m.Series {
			s.Labels["quantile"] = q
			out.Series = append(out.Series, s)
		}
	}
	return out, nil
}

// topN is how many routes or tenants overview.routes and .tenants keep.
const topN = 10

// evalTop is overview.routes (by "route", filtered by tenant) or
// overview.tenants (by "tenant", filtered by route): the request rate, the
// rate-limited rate and p99 over the whole range, per value of by, the
// top 10 by request rate, as series labelled by and stat.
func evalTop(by string) func(ctx context.Context, h *Hub, p Params, at time.Time) (any, error) {
	filter := []string{paramTenant}
	if by == "tenant" {
		filter = []string{paramRoute}
	}
	return func(ctx context.Context, h *Hub, p Params, at time.Time) (any, error) {
		f := matchers(p, filter)
		limited := matchers(p, filter, `reason="ratelimited"`)
		w := p.Range.name
		stats := []struct{ stat, promql string }{
			{"rps", fmt.Sprintf(`sum by (%s) (rate(gw_requests_total{%s}[%s]))`, by, f, w)},
			{"ratelimited_rps", fmt.Sprintf(`sum by (%s) (rate(gw_requests_total{%s}[%s]))`, by, limited, w)},
			{"p99_s", fmt.Sprintf(`histogram_quantile(0.99, sum by (%s, le) (rate(gw_request_duration_seconds_bucket{%s}[%s])))`, by, f, w)},
		}
		byStat := map[string]map[string]*float64{}
		for _, s := range stats {
			v, err := h.instantQuery(ctx, at, s.promql)
			if err != nil {
				return nil, err
			}
			byStat[s.stat] = map[string]*float64{}
			for _, sample := range v.Series {
				byStat[s.stat][sample.Labels[by]] = sample.Value
			}
		}
		keys := make([]string, 0, len(byStat["rps"]))
		for k := range byStat["rps"] {
			keys = append(keys, k)
		}
		rate := func(k string) float64 {
			if v := byStat["rps"][k]; v != nil {
				return *v
			}
			return -1
		}
		slices.SortFunc(keys, func(a, b string) int {
			if ra, rb := rate(a), rate(b); ra != rb {
				if ra > rb {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		keys = keys[:min(len(keys), topN)]
		out := vector{Kind: "vector", Series: []sample{}}
		for _, k := range keys {
			for _, s := range stats {
				out.Series = append(out.Series, sample{Labels: map[string]string{by: k, "stat": s.stat}, Value: byStat[s.stat][k]})
			}
		}
		return out, nil
	}
}

func evalPolicies(ctx context.Context, h *Hub, p Params, at time.Time) (any, error) {
	return h.rangeQuery(ctx, p.Range, at, fmt.Sprintf(`sum by (policy, mode) (rate(gw_ratelimit_decisions_total{decision="deny"}[%s]))`, p.Range.window))
}
