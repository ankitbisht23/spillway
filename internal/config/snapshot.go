package config

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/router"
)

// The snapshot schema version (CP-18, ADR-0038). It travels with every
// published config version.
const (
	SchemaMajor = 1
	SchemaMinor = 0
)

// Snapshot is a compiled config. It never changes once compiled: a reload
// builds a new one, and a request keeps the snapshot it started with (GW-21).
type Snapshot struct {
	Version   int64
	Routes    []Route
	Upstreams map[string]*Upstream // by name
	Policies  []Policy             // in evaluation order (ADR-0017)
	// Replicas is ratelimit.replicas, at least 1 (RL-31).
	Replicas int64
	// RedisBudget is ratelimit.redis_budget: one Redis time budget per
	// request (RL-22, ADR-0021).
	RedisBudget time.Duration
	router      *router.Router
}

// Route is a compiled route.
type Route struct {
	Name            string
	Upstream        *Upstream
	AddPrefix       string // see UpstreamPath
	RequestHeaders  HeaderTransform
	ResponseHeaders HeaderTransform
	MaxBodyBytes    int64 // 0 means no limit
	MaxHeaderBytes  int   // 0 means the server's limit
	Timeouts        Timeouts
	HSTS            string // Strict-Transport-Security value for TLS responses; empty means none
	Cost            int64  // cost units each request deducts (RL-07)
	Retry           Retry  // attempts, and whether every method retries (GW-12)
	strip           int    // leading path segments that strip_prefix removes
}

// Upstream is a compiled upstream.
type Upstream struct {
	Name        string
	Endpoints   []string     // host:port
	LB          string       // LBRoundRobin, LBP2C or LBConsistentHash (GW-07)
	HashOn      *KeyTemplate // LBConsistentHash's key; nil for the others
	HealthCheck *HealthCheck // nil means no active checks (GW-08)
	Outlier     OutlierDetection
	Circuit     CircuitBreaker
	RetryBudget RetryBudget
}

// Match returns the route for a request; path must be clean
// (router.CleanPath).
func (s *Snapshot) Match(host, method, path string, h http.Header) (*Route, router.Match, bool) {
	m, ok := s.router.Match(host, method, path, h)
	if !ok {
		return nil, m, false
	}
	return &s.Routes[m.Index], m, true
}

// Compile validates a config and builds its snapshot.
func Compile(c *configpb.Config) (*Snapshot, error) {
	s := &Snapshot{Version: c.GetVersion(), Upstreams: map[string]*Upstream{}}
	for _, u := range c.GetUpstreams() {
		up, err := compileUpstream(u)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: %w", u.GetName(), err)
		}
		if s.Upstreams[up.Name] != nil {
			return nil, fmt.Errorf("upstream %q: defined twice", up.Name)
		}
		s.Upstreams[up.Name] = up
	}

	names := map[string]bool{}
	var routes []router.Route
	heads := explicitHead(c.GetRoutes())
	for _, r := range c.GetRoutes() {
		if r.GetName() == "" {
			return nil, errors.New("route: name is required")
		}
		if names[r.GetName()] {
			return nil, fmt.Errorf("route %q: defined twice", r.GetName())
		}
		names[r.GetName()] = true
		route, rr, err := compileRoute(r, heads)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", r.GetName(), err)
		}
		if route.Upstream = s.Upstreams[r.GetUpstream()]; route.Upstream == nil {
			return nil, fmt.Errorf("route %q: upstream %q is not defined", r.GetName(), r.GetUpstream())
		}
		routes = append(routes, rr)
		s.Routes = append(s.Routes, route)
	}

	var err error
	if s.router, err = router.New(routes); err != nil {
		if re, ok := errors.AsType[*router.RouteError](err); ok {
			return nil, fmt.Errorf("route %q: %w", s.Routes[re.Index].Name, re.Err)
		}
		return nil, err
	}
	if err := s.compileRateLimit(c); err != nil {
		return nil, err
	}
	if s.Policies, err = compilePolicies(c, s.Routes, s.Replicas); err != nil {
		return nil, err
	}
	if err := compilePlans(c.GetPlans()); err != nil {
		return nil, err
	}
	return s, nil
}

// compileRoute checks the rules of r that concern r alone, everything but
// its upstream, and builds its router entry; heads is explicitHead of the
// config's routes.
func compileRoute(r *configpb.Route, heads map[string]bool) (Route, router.Route, error) {
	if r.GetName() == "none" {
		return Route{}, router.Route{}, errors.New(`the name is reserved for requests no route matches (OB-01)`)
	}
	m := r.GetMatch()
	if (m.GetPath() == "") == (m.GetPathPrefix() == "") {
		return Route{}, router.Route{}, errors.New("set exactly one of match.path and match.path_prefix")
	}
	rr := router.Route{
		Host:     m.GetHost(),
		Methods:  m.GetMethods(),
		Path:     m.GetPath() + m.GetPathPrefix(),
		Prefix:   m.GetPathPrefix() != "",
		Priority: int(r.GetPriority()),
	}
	if !heads[headKey(m)] {
		rr.Methods = withHead(rr.Methods)
	}
	for _, h := range m.GetHeaders() {
		rr.Headers = append(rr.Headers, router.HeaderMatch{Name: h.GetName(), Exact: h.GetExact(), Regex: h.GetRegex()})
	}
	route := Route{Name: r.GetName()}
	if err := route.compileTransforms(r, rr.Path); err != nil {
		return Route{}, router.Route{}, err
	}
	switch mode := r.GetAuthMode(); mode {
	case "", AuthNone:
	case AuthAPIKey, AuthJWT, AuthEither:
		// Interim until the jwt task (ADR-0035): nothing half-built can be
		// published.
		return Route{}, router.Route{}, fmt.Errorf("auth_mode %q is not supported until M5's jwt task", mode)
	default:
		return Route{}, router.Route{}, fmt.Errorf("auth_mode %q: want none, api_key, jwt or either", mode)
	}
	return route, rr, nil
}

func compileUpstream(u *configpb.Upstream) (*Upstream, error) {
	if u.GetName() == "" {
		return nil, errors.New("name is required")
	}
	if len(u.GetEndpoints()) == 0 {
		return nil, errors.New("needs at least one endpoint")
	}
	up := &Upstream{Name: u.GetName()}
	seen := map[string]bool{}
	for _, e := range u.GetEndpoints() {
		// A draining endpoint is checked like the others and gets no new
		// requests (ADR-0038); with every endpoint draining, the upstream has
		// no usable address and answers 503 (ADR-0008).
		addr := e.GetAddress()
		host, port, err := net.SplitHostPort(addr)
		if err != nil || host == "" {
			return nil, fmt.Errorf("endpoint %q: want host:port", addr)
		}
		if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
			return nil, fmt.Errorf("endpoint %q: port must be 1-65535", addr)
		}
		if seen[addr] {
			return nil, fmt.Errorf("endpoint %q: listed twice", addr)
		}
		seen[addr] = true
		if !e.GetDraining() {
			up.Endpoints = append(up.Endpoints, addr)
		}
	}
	var err error
	if up.LB, up.HashOn, err = compileLB(u.GetLb(), u.GetHashOn()); err != nil {
		return nil, err
	}
	if up.HealthCheck, err = compileHealthCheck(u.GetHealthCheck()); err != nil {
		return nil, fmt.Errorf("health_check.%w", err)
	}
	if up.Outlier, err = compileOutlier(u.GetOutlierDetection()); err != nil {
		return nil, fmt.Errorf("outlier_detection.%w", err)
	}
	if up.Circuit, err = compileCircuit(u.GetCircuit()); err != nil {
		return nil, fmt.Errorf("circuit.%w", err)
	}
	if up.RetryBudget, err = compileRetryBudget(u.GetRetryBudget()); err != nil {
		return nil, fmt.Errorf("retry_budget.%w", err)
	}
	return up, nil
}

// withHead returns methods with HEAD added when they list GET and not HEAD:
// HEAD is GET without the body (RFC 9110 §9.3.2), so a HEAD request matches
// the routes and policies a GET would, and cannot skip a GET route and its
// limits for a broader route (D1, ADR-0030). methods is not changed.
func withHead(methods []string) []string {
	if !slices.Contains(methods, http.MethodGet) || slices.Contains(methods, http.MethodHead) {
		return methods
	}
	return append(slices.Clip(methods), http.MethodHead)
}

// explicitHead returns the headKey of every route that lists HEAD itself: a
// GET route with the same host and path leaves HEAD to it (ADR-0033).
func explicitHead(routes []*configpb.Route) map[string]bool {
	heads := map[string]bool{}
	for _, r := range routes {
		if slices.Contains(r.GetMatch().GetMethods(), http.MethodHead) {
			heads[headKey(r.GetMatch())] = true
		}
	}
	return heads
}

// headKey is a route match's host and path, as explicitHead compares them.
func headKey(m *configpb.RouteMatch) string {
	return strings.ToLower(m.GetHost()) + " " + m.GetPath() + " " + m.GetPathPrefix()
}
