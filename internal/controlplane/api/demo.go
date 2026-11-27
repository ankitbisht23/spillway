package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ankitbisht23/spillway/internal/controlplane/demo"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Demo mode (SC-08, ADR-0095). Off, every path under /api/v1/demo is 404
// before authentication, as an unknown path is, and keys follow no demo
// rule; on, those operations go through the usual guard, every key issued
// or rotated expires within 30 minutes, and the active replica runs the
// expired-key janitor.

// demoPrefix is the one prefix the demo guard covers.
const demoPrefix = BasePath + "/demo"

// WithDemo sets demo mode, off by default.
func (s *Server) WithDemo(on bool) *Server {
	s.demoKeys = demo.Keys{On: on}
	return s
}

// demoPath reports whether path is under the demo prefix.
func demoPath(path string) bool {
	return path == demoPrefix || strings.HasPrefix(path, demoPrefix+"/")
}

// demoGuard answers every request under the demo prefix with notFound,
// for every method and before authentication, unless demo mode is on
// (ADR-0095 §2).
func (s *Server) demoGuard(next, notFound http.Handler) http.Handler {
	if s.demoKeys.On {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if demoPath(r.URL.Path) {
			notFound.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The Playground's routes and scenarios (ADR-0095 §2, ADR-0096 §3).
var (
	demoRoutes    = []string{"orders", "catalog", "payments"}
	demoScenarios = []string{"d1", "d2", "d3"}
)

// GetDemo answers the demo's tenants with their plans' limits and the
// Playground's routes with the policy that limits each, from the latest
// published config version (none before the first publish).
func (s *Server) GetDemo(ctx context.Context, _ apiv1.GetDemoRequestObject) (apiv1.GetDemoResponseObject, error) {
	latest, err := s.store.LatestConfig(ctx)
	if err != nil {
		return nil, err
	}
	tenants, err := s.store.ListTenants(ctx, store.Page{Limit: maxLimit}, nil)
	if err != nil {
		return nil, err
	}
	cfg := latest.GetConfig()
	fromPlan := map[string]bool{}
	for _, p := range cfg.GetPolicies() {
		fromPlan[p.GetName()] = p.GetFromPlan()
	}
	plans := map[int64]*configpb.Plan{}
	for _, p := range cfg.GetPlans() {
		plans[p.GetId()] = p
	}
	out := apiv1.Demo{Tenants: []apiv1.DemoTenant{}, Routes: []apiv1.DemoRoute{}, Scenarios: demoScenarios}
	for _, t := range tenants[:min(len(tenants), maxLimit)] {
		dt := apiv1.DemoTenant{Name: t.Name}
		if p, ok := plans[t.PlanID]; ok {
			name := p.GetName()
			dt.Plan = &name
			for _, e := range p.GetDefaultLimits() {
				if fromPlan[e.GetPolicy()] && len(e.GetLimits()) > 0 {
					dt.Limit = demoLimit(e.GetLimits()[0])
					break
				}
			}
		}
		out.Tenants = append(out.Tenants, dt)
	}
	for _, name := range demoRoutes {
		if !slices.ContainsFunc(cfg.GetRoutes(), func(r *configpb.Route) bool { return r.GetName() == name }) {
			continue
		}
		dr := apiv1.DemoRoute{Name: name}
		for _, p := range cfg.GetPolicies() {
			routes := p.GetMatch().GetRoutes()
			if (p.GetLevel() == "consumer" || p.GetLevel() == "route") && (slices.Contains(routes, name) || slices.Contains(routes, "*")) {
				policy := p.GetName()
				dr.Policy = &policy
				if !p.GetFromPlan() && len(p.GetLimits()) > 0 {
					dr.Limit = demoLimit(p.GetLimits()[0])
				}
				break
			}
		}
		out.Routes = append(out.Routes, dr)
	}
	return apiv1.GetDemo200JSONResponse(out), nil
}

// demoLimit is l with its burst filled in: a GCRA burst defaults to the
// rate (RL-03).
func demoLimit(l *configpb.Limit) *apiv1.DemoLimit {
	burst := l.GetBurst()
	if burst == 0 {
		burst = l.GetRate()
	}
	return &apiv1.DemoLimit{Rate: l.GetRate(), Burst: burst, Period: l.GetPeriod()}
}

// The expired-key janitor (ADR-0095 §4).
const (
	janitorFirst = time.Minute      // the first sweep, after the start
	janitorEvery = 10 * time.Minute // then one sweep every
	janitorAge   = time.Hour        // keys expired longer ago than this go
	janitorBatch = 1000             // at most this many keys per sweep
)

// janitor is the audit records' actor.
var janitor = store.Actor{Sub: "system:demo-janitor", Name: "demo-janitor"}

// SweepExpiredKeys deletes, in one identity change, up to 1,000 keys that
// expired over an hour ago and that no key was rotated from, with one audit
// record per key; it returns how many it deleted.
func (s *Server) SweepExpiredKeys(ctx context.Context) (int, error) {
	a := store.Audit{Actor: janitor, RequestID: "demo-janitor-" + uuid.NewString(), Show: showKey}
	return s.store.DeleteExpiredKeys(ctx, a, janitorAge, janitorBatch)
}

// RunDemoJanitor sweeps a minute after it starts and every 10 minutes
// after, until ctx ends; with demo mode off it returns at once. The control
// plane runs it on the active replica only (CP-22). Lines name counts,
// never a key.
func (s *Server) RunDemoJanitor(ctx context.Context) {
	if !s.demoKeys.On {
		return
	}
	t := time.NewTimer(janitorFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		switch n, err := s.SweepExpiredKeys(ctx); {
		case err != nil && ctx.Err() == nil:
			s.log.Warn("demo janitor: sweep failed", "err", err)
		case n > 0:
			s.log.Info("demo janitor: deleted expired keys", "keys", n)
		}
		t.Reset(janitorEvery)
	}
}
