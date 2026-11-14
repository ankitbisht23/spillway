package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// validateConfig runs CP-03's validation on c, a whole config about to be
// published, and returns every error it finds (ADR-0037): each entity's
// rules; references from routes to upstreams and from policies to routes;
// refs, the identity rows that refer to plans and policies; SEC-06's
// allow-list for every endpoint; and, when all of those pass, the gateway's
// own compile of c, routing table and plan rules included, policy
// conflicts, and the overrides' limits against their policies (ADR-0041).
func (s *Server) validateConfig(ctx context.Context, c *configpb.Config, refs []store.IdentityRef) []apiv1.ErrorDetail {
	var errs []apiv1.ErrorDetail
	add := func(entity string, id int64, name, path, msg string) {
		d := apiv1.ErrorDetail{Entity: &entity, Message: &msg}
		if id != 0 {
			d.Id = &id
		}
		if name != "" {
			d.Name = &name
		}
		if path != "" {
			d.Path = &path
		}
		errs = append(errs, d)
	}

	// Schema: the entity rules every write checked, again.
	upstreamNames, routeNames := map[string]bool{}, map[string]bool{}
	for _, u := range c.GetUpstreams() {
		upstreamNames[u.GetName()] = true
		if err := config.CheckUpstream(u); err != nil {
			add("upstream", u.GetId(), u.GetName(), "", err.Error())
		}
	}
	for _, r := range c.GetRoutes() {
		routeNames[r.GetName()] = true
		if err := config.CheckRoute(r); err != nil {
			add("route", r.GetId(), r.GetName(), "", err.Error())
		}
	}
	for _, p := range c.GetPolicies() {
		if err := config.CheckPolicy(p); err != nil {
			add("policy", p.GetId(), p.GetName(), "", err.Error())
		}
	}
	for _, p := range c.GetPlans() {
		if err := config.CheckPlan(p); err != nil {
			add("plan", p.GetId(), p.GetName(), "", err.Error())
		}
	}

	// References within the config.
	for _, r := range c.GetRoutes() {
		if !upstreamNames[r.GetUpstream()] {
			add("route", r.GetId(), r.GetName(), "upstream", fmt.Sprintf("upstream %q does not exist", r.GetUpstream()))
		}
	}
	for _, p := range c.GetPolicies() {
		for _, name := range p.GetMatch().GetRoutes() {
			if name != "*" && !routeNames[name] {
				add("policy", p.GetId(), p.GetName(), "match.routes", fmt.Sprintf("route %q does not exist", name))
			}
		}
	}

	// References from identity (ADR-0005): a plan or policy that identity
	// still refers to cannot go.
	for _, ref := range refs {
		var what string
		var found bool
		switch ref.Field {
		case "plan_id":
			what = "plan"
			found = slices.ContainsFunc(c.GetPlans(), func(p *configpb.Plan) bool { return p.GetId() == ref.Ref })
		default:
			what = "policy"
			found = slices.ContainsFunc(c.GetPolicies(), func(p *configpb.Policy) bool { return p.GetId() == ref.Ref })
		}
		if !found {
			add(ref.Entity, ref.ID, ref.Name, ref.Field, fmt.Sprintf("%s %d refers to %s %d, which this config does not have: change or delete the %s first",
				ref.Entity, ref.ID, what, ref.Ref, ref.Entity))
		}
	}

	// SEC-06, for every endpoint, draining ones included.
	for _, e := range s.blockedEndpoints(ctx, c) {
		add("upstream", e.upstream.GetId(), e.upstream.GetName(), "endpoints", e.err.Error())
	}

	if len(errs) > 0 {
		return errs
	}
	// The gateway's own compile: the routing table, regexes, burst and cost
	// rules (RL-03), names, call limits.
	snap, err := config.Compile(c)
	if err != nil {
		add("config", 0, "", "", err.Error())
		return errs
	}
	for _, cf := range conflicts(c, snap) {
		add("policy", cf.policy.GetId(), cf.policy.GetName(), "match", cf.msg)
	}
	// Overrides that have not ended must still fit their policies (RL-08,
	// ADR-0041); those whose policy goes are reported above.
	for _, ref := range refs {
		if ref.Limits == nil || !slices.ContainsFunc(c.GetPolicies(), func(p *configpb.Policy) bool { return p.GetId() == ref.Ref }) {
			continue
		}
		if err := snap.CheckOverride(ref.Ref, ref.Limits); err != nil {
			add(ref.Entity, ref.ID, "", "limits", fmt.Sprintf("override %d no longer fits its policy: %v; change or delete the override first", ref.ID, err))
		}
	}
	return errs
}

type blocked struct {
	upstream *configpb.Upstream
	err      error
}

// blockedEndpoints checks every endpoint of c against the allow-list
// (SEC-06), resolving host names at once so a publish holds the working
// set's lock for at most one lookup timeout.
func (s *Server) blockedEndpoints(ctx context.Context, c *configpb.Config) []blocked {
	type job struct {
		upstream *configpb.Upstream
		address  string
	}
	var jobs []job
	for _, u := range c.GetUpstreams() {
		for _, e := range u.GetEndpoints() {
			jobs = append(jobs, job{u, e.GetAddress()})
		}
	}
	results := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Go(func() { results[i] = s.allowedEndpoint(ctx, j.address) })
	}
	wg.Wait()
	var out []blocked
	for i, err := range results {
		if err != nil {
			out = append(out, blocked{jobs[i].upstream, err})
		}
	}
	return out
}

type conflict struct {
	policy *configpb.Policy
	msg    string
}

// conflicts finds policy conflicts (CP-03 as ADR-0037 narrows it): two
// enforce-mode policies at the same level, with the same algorithm and key
// template, that both match some route and method. Such a pair should be
// one policy with several limits (RL-06); a shadow policy beside the
// enforce policy it replaces is no conflict. snap is c compiled, whose
// policies are c's in the same order.
func conflicts(c *configpb.Config, snap *config.Snapshot) []conflict {
	var out []conflict
	ps := snap.Policies
	for j := range ps {
		for i := range j {
			a, b := &ps[i], &ps[j]
			if a.Mode != config.Enforce || b.Mode != config.Enforce || a.Level != b.Level || a.Algorithm != b.Algorithm ||
				c.Policies[i].GetKeyTemplate() != c.Policies[j].GetKeyTemplate() {
				continue
			}
			route, method, ok := sharedPair(a, b, snap.Routes)
			if !ok {
				continue
			}
			out = append(out, conflict{c.Policies[j], fmt.Sprintf(
				"conflicts with policy %q: both enforce %s-level %s limits keyed by %q on route %q, %s; make them one policy with several limits (RL-06), or put one in shadow mode",
				a.Name, a.Level, a.Algorithm, c.Policies[j].GetKeyTemplate(), route, method)})
		}
	}
	return out
}

// sharedPair returns a route and method that both a and b match.
func sharedPair(a, b *config.Policy, routes []config.Route) (string, string, bool) {
	method := "any method"
	switch {
	case len(a.Methods) == 0 && len(b.Methods) == 0:
	case len(a.Methods) == 0:
		method = "method " + b.Methods[0]
	case len(b.Methods) == 0:
		method = "method " + a.Methods[0]
	default:
		i := slices.IndexFunc(a.Methods, func(m string) bool { return slices.Contains(b.Methods, m) })
		if i < 0 {
			return "", "", false
		}
		method = "method " + a.Methods[i]
	}
	for _, r := range routes {
		if (a.Routes == nil || a.Routes[r.Name]) && (b.Routes == nil || b.Routes[r.Name]) {
			return r.Name, method, true
		}
	}
	return "", "", false
}

// validationFailed is a 422 with every error of a validation.
func validationFailed(errs []apiv1.ErrorDetail) *apiError {
	first := errs[0]
	msg := "validation failed: "
	if first.Name != nil {
		msg += fmt.Sprintf("%s %q: ", *first.Entity, *first.Name)
	}
	msg += *first.Message
	if len(errs) > 1 {
		msg += fmt.Sprintf(" (and %d more errors)", len(errs)-1)
	}
	return &apiError{http.StatusUnprocessableEntity, apiv1.ErrorCodeValidationFailed, msg, errs}
}
