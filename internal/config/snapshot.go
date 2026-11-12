package config

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/router"
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
	Retry           Retry
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
	for _, r := range c.GetRoutes() {
		if r.GetName() == "" {
			return nil, errors.New("route: name is required")
		}
		if r.GetName() == "none" {
			return nil, errors.New(`route "none": the name is reserved for requests no route matches (OB-01)`)
		}
		if names[r.GetName()] {
			return nil, fmt.Errorf("route %q: defined twice", r.GetName())
		}
		names[r.GetName()] = true
		up := s.Upstreams[r.GetUpstream()]
		if up == nil {
			return nil, fmt.Errorf("route %q: upstream %q is not defined", r.GetName(), r.GetUpstream())
		}
		m := r.GetMatch()
		if (m.GetPath() == "") == (m.GetPathPrefix() == "") {
			return nil, fmt.Errorf("route %q: set exactly one of match.path and match.path_prefix", r.GetName())
		}
		rr := router.Route{
			Host:     m.GetHost(),
			Methods:  m.GetMethods(),
			Path:     m.GetPath() + m.GetPathPrefix(),
			Prefix:   m.GetPathPrefix() != "",
			Priority: int(r.GetPriority()),
		}
		for _, h := range m.GetHeaders() {
			rr.Headers = append(rr.Headers, router.HeaderMatch{Name: h.GetName(), Exact: h.GetExact(), Regex: h.GetRegex()})
		}
		route := Route{Name: r.GetName(), Upstream: up}
		if err := route.compileTransforms(r, rr.Path); err != nil {
			return nil, fmt.Errorf("route %q: %w", r.GetName(), err)
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
	return s, nil
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
		up.Endpoints = append(up.Endpoints, addr)
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
	if up.RetryBudget, err = compileRetryBudget(u.GetRetryBudget()); err != nil {
		return nil, fmt.Errorf("retry_budget.%w", err)
	}
	return up, nil
}
