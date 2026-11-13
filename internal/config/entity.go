package config

import (
	"errors"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/router"
)

// The Check functions apply the rules of Compile that concern one entity
// alone, as the control plane does on every write (ADR-0037). Rules that span
// entities (a route's upstream, a policy's routes and their cost, names
// unique across the config) wait for a whole-config Compile.

// CheckUpstream checks one upstream.
func CheckUpstream(u *configpb.Upstream) error {
	_, err := compileUpstream(u)
	return err
}

// CheckRoute checks one route, everything but its upstream.
func CheckRoute(r *configpb.Route) error {
	if r.GetName() == "" {
		return errors.New("name is required")
	}
	_, rr, err := compileRoute(r, explicitHead([]*configpb.Route{r}))
	if err != nil {
		return err
	}
	if _, err := router.New([]router.Route{rr}); err != nil {
		if re, ok := errors.AsType[*router.RouteError](err); ok {
			return re.Err
		}
		return err
	}
	return nil
}

// CheckPolicy checks one policy, everything but whether the routes it names
// exist and what they cost.
func CheckPolicy(p *configpb.Policy) error {
	// Each named route stands in at the lowest cost, which every valid limit
	// admits, so only the policy's own rules can fail.
	var routes []Route
	for _, name := range p.GetMatch().GetRoutes() {
		if name != "*" && !hasRoute(routes, name) {
			routes = append(routes, Route{Name: name, Cost: 1})
		}
	}
	_, err := compilePolicy(p, 1, routes)
	return err
}

// CheckPlan checks one plan.
func CheckPlan(p *configpb.Plan) error {
	return compilePlan(p)
}
