package api

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/proxy"
)

// validConfig is a config that passes every check: an upstream, two
// routes, a policy on one of them and a plan.
const validConfig = `{
  "upstreams": [{"id": 1, "name": "orders", "endpoints": [{"id": 1, "address": "172.29.0.10:8080"}, {"id": 2, "address": "upstream-1:8080", "draining": true}]}],
  "routes": [
    {"id": 2, "name": "orders-api", "upstream": "orders", "match": {"path_prefix": "/v1/orders", "methods": ["GET", "POST"]}, "cost": 2},
    {"id": 3, "name": "health", "upstream": "orders", "match": {"path": "/healthz"}}
  ],
  "policies": [{"id": 4, "name": "per-route", "level": "route", "match": {"routes": ["orders-api"]}, "key_template": "{route}", "algorithm": "gcra", "limits": [{"rate": 100, "period": "1s", "burst": 200}]}],
  "plans": [{"id": 5, "name": "free"}]
}`

func parseConfig(t *testing.T, js string) *configpb.Config {
	t.Helper()
	c := &configpb.Config{}
	if err := protojson.Unmarshal([]byte(js), c); err != nil {
		t.Fatalf("%v\n%s", err, js)
	}
	return c
}

func validator(t *testing.T, allow string) *Server {
	t.Helper()
	allowed, err := proxy.ParseAllowlist(allow)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{allowed: allowed, resolver: fakeResolver{
		"upstream-1": {netip.MustParseAddr("172.29.0.10")},
		"metadata":   {netip.MustParseAddr("172.29.0.11"), netip.MustParseAddr("169.254.169.254")},
		"outside":    {netip.MustParseAddr("10.1.2.3")},
	}}
}

func show(errs []apiv1.ErrorDetail) string {
	var b strings.Builder
	for _, e := range errs {
		name, path := "", ""
		if e.Name != nil {
			name = *e.Name
		}
		if e.Path != nil {
			path = *e.Path
		}
		fmt.Fprintf(&b, "%s %q %s: %s\n", *e.Entity, name, path, *e.Message)
	}
	return b.String()
}

// TestValidateConfig checks each rule of CP-03 and SEC-06 (ADR-0037) on a
// whole config: the valid config passes, and each change breaks exactly
// the rule it names, or, for the cases that must pass, none.
func TestValidateConfig(t *testing.T) {
	s := validator(t, "172.29.0.0/16")
	if errs := s.validateConfig(t.Context(), parseConfig(t, validConfig), nil); len(errs) != 0 {
		t.Fatalf("the valid config fails:\n%s", show(errs))
	}
	policy := func(name, mode, level, algorithm, template, routes, methods string) *configpb.Policy {
		p := &configpb.Policy{}
		js := fmt.Sprintf(`{"id": 9, "name": %q, "mode": %q, "level": %q, "algorithm": %q, "key_template": %q, "match": {"routes": %s, "methods": %s}, "limits": [{"rate": 10, "period": "1s"}], "local": true}`,
			name, mode, level, algorithm, template, routes, methods)
		if algorithm == "gcra" {
			js = strings.Replace(js, `"local": true`, `"failure_mode": "fail_open"`, 1)
		}
		if err := protojson.Unmarshal([]byte(js), p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name   string
		change func(c *configpb.Config)
		refs   []store.IdentityRef
		want   string // a line of show's output; empty: valid
	}{
		// Schema: each entity's rules.
		{"upstream rule", func(c *configpb.Config) { c.Upstreams[0].Lb = "random" }, nil, `upstream "orders" : lb "random"`},
		{"route rule", func(c *configpb.Config) { c.Routes[1].Match.PathPrefix = "/x" }, nil, `route "health" : set exactly one of match.path`},
		{"policy rule", func(c *configpb.Config) { c.Policies[0].Level = "user" }, nil, `policy "per-route" : level "user"`},
		{"plan rule", func(c *configpb.Config) {
			c.Plans[0].DefaultLimits = []*configpb.PlanLimits{{Policy: "per-route", Limits: []*configpb.Limit{{Rate: 1, Period: "1s"}}}}
		}, nil, `plan "free" : default_limits are not supported`},
		// References.
		{"route to a missing upstream", func(c *configpb.Config) { c.Routes[0].Upstream = "payments" }, nil, `route "orders-api" upstream: upstream "payments" does not exist`},
		{"policy on a missing route", func(c *configpb.Config) { c.Policies[0].Match.Routes = []string{"orders-api", "gone"} }, nil, `policy "per-route" match.routes: route "gone" does not exist`},
		{"policy on every route", func(c *configpb.Config) { c.Policies[0].Match.Routes = []string{"*"} }, nil, ""},
		// Identity references (ADR-0005).
		{"override of a removed policy", func(c *configpb.Config) { c.Policies = nil }, []store.IdentityRef{{Entity: "override", ID: 7, Field: "policy_id", Ref: 4}},
			`override "" policy_id: override 7 refers to policy 4, which this config does not have: change or delete the override first`},
		{"override of a kept policy", nil, []store.IdentityRef{{Entity: "override", ID: 7, Field: "policy_id", Ref: 4}}, ""},
		{"tenant on a removed plan", func(c *configpb.Config) { c.Plans = nil }, []store.IdentityRef{{Entity: "tenant", ID: 3, Name: "acme", Field: "plan_id", Ref: 5}},
			`tenant "acme" plan_id: tenant 3 refers to plan 5, which this config does not have`},
		{"consumer on a removed plan", func(c *configpb.Config) { c.Plans = nil }, []store.IdentityRef{{Entity: "consumer", ID: 8, Name: "bot", Field: "plan_id", Ref: 5}},
			`consumer "bot" plan_id: consumer 8 refers to plan 5`},
		{"tenant on a kept plan", nil, []store.IdentityRef{{Entity: "tenant", ID: 3, Name: "acme", Field: "plan_id", Ref: 5}}, ""},
		// Regex compile.
		{"header regex", func(c *configpb.Config) {
			c.Routes[1].Match.Headers = []*configpb.HeaderMatch{{Name: "X-Tier", Regex: "("}}
		}, nil, `route "health" : header "X-Tier"`},
		{"good header regex", func(c *configpb.Config) {
			c.Routes[1].Match.Headers = []*configpb.HeaderMatch{{Name: "X-Tier", Regex: "^(gold|silver)$"}}
		}, nil, ""},
		// Burst and cost rules (RL-03).
		{"burst required over a minute", func(c *configpb.Config) { c.Policies[0].Limits[0] = &configpb.Limit{Rate: 100, Period: "1h"} }, nil, `burst: required for periods over 1m0s`},
		{"cost above the burst", func(c *configpb.Config) { c.Routes[0].Cost = 201 }, nil, `config "" : policy "per-route": route "orders-api" costs 201, more than the 200`},
		{"cost at the burst", func(c *configpb.Config) { c.Routes[0].Cost = 200 }, nil, ""},
		// The dry compile: what only the whole config shows.
		{"too many Redis limits on one route", func(c *configpb.Config) {
			for i := range 4 {
				p := policy(fmt.Sprintf("p%d", i), "shadow", "route", "gcra", fmt.Sprintf("{route}:%d", i), `["orders-api"]`, `[]`)
				for d := 2; d <= 8; d++ {
					p.Limits = append(p.Limits, &configpb.Limit{Rate: 10, Period: fmt.Sprintf("%ds", d)})
				}
				c.Policies = append(c.Policies, p)
			}
		}, nil, `would carry 33 Redis limits in one call`},
		// Policy conflicts (ADR-0037).
		{"conflict", func(c *configpb.Config) {
			c.Policies = append(c.Policies, policy("twin", "enforce", "route", "gcra", "{route}", `["*"]`, `["POST"]`))
		}, nil, `policy "twin" match: conflicts with policy "per-route": both enforce route-level gcra limits keyed by "{route}" on route "orders-api", method POST`},
		{"conflict, mode unset means enforce", func(c *configpb.Config) {
			c.Policies = append(c.Policies, policy("twin", "", "route", "gcra", "{route}", `["orders-api"]`, `[]`))
		}, nil, `policy "twin" match: conflicts with policy "per-route"`},
		{"shadow beside enforce", func(c *configpb.Config) {
			c.Policies = append(c.Policies, policy("twin", "shadow", "route", "gcra", "{route}", `["*"]`, `[]`))
		}, nil, ""},
		{"another key template", func(c *configpb.Config) {
			c.Policies = append(c.Policies, policy("twin", "enforce", "route", "gcra", "{route}:{method}", `["*"]`, `[]`))
		}, nil, ""},
		{"another algorithm", func(c *configpb.Config) {
			c.Policies = append(c.Policies, policy("twin", "enforce", "route", "fixed_window", "{route}", `["*"]`, `[]`))
		}, nil, ""},
		{"another level", func(c *configpb.Config) {
			c.Policies = append(c.Policies, policy("twin", "enforce", "global", "gcra", "{route}", `["*"]`, `[]`))
		}, nil, ""},
		{"other routes", func(c *configpb.Config) {
			c.Policies = append(c.Policies, policy("twin", "enforce", "route", "gcra", "{route}", `["health"]`, `[]`))
		}, nil, ""},
		{"other methods", func(c *configpb.Config) {
			c.Policies[0].Match.Methods = []string{"GET"}
			c.Policies = append(c.Policies, policy("twin", "enforce", "route", "gcra", "{route}", `["orders-api"]`, `["POST"]`))
		}, nil, ""},
		{"HEAD rides with GET", func(c *configpb.Config) {
			c.Policies[0].Match.Methods = []string{"GET"}
			c.Policies = append(c.Policies, policy("twin", "enforce", "route", "gcra", "{route}", `["orders-api"]`, `["HEAD"]`))
		}, nil, `method HEAD`},
		// SEC-06: allow-list, link-local and metadata always.
		{"endpoint outside the allow-list", func(c *configpb.Config) { c.Upstreams[0].Endpoints[0].Address = "10.0.0.1:80" }, nil,
			`upstream "orders" endpoints: endpoint "10.0.0.1:80" is not in the allowed upstream networks (SEC-06)`},
		{"draining endpoint outside the allow-list", func(c *configpb.Config) { c.Upstreams[0].Endpoints[1].Address = "outside:80" }, nil,
			`upstream "orders" endpoints: endpoint "outside:80" resolves to 10.1.2.3`},
		{"metadata address", func(c *configpb.Config) { c.Upstreams[0].Endpoints[0].Address = "169.254.169.254:80" }, nil, `"169.254.169.254:80" is not in the allowed`},
		{"name resolving to metadata", func(c *configpb.Config) { c.Upstreams[0].Endpoints[0].Address = "metadata:80" }, nil, `resolves to 169.254.169.254`},
		{"name that does not resolve", func(c *configpb.Config) { c.Upstreams[0].Endpoints[0].Address = "not-yet:80" }, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := parseConfig(t, validConfig)
			if tt.change != nil {
				tt.change(c)
			}
			got := show(s.validateConfig(t.Context(), c, tt.refs))
			switch {
			case tt.want == "" && got != "":
				t.Errorf("want no errors, got:\n%s", got)
			case tt.want != "" && !strings.Contains(got, tt.want):
				t.Errorf("want %q among the errors, got:\n%s", tt.want, got)
			case tt.want != "" && strings.Count(got, "\n") != 1:
				t.Errorf("want one error, got:\n%s", got)
			}
		})
	}
}

// TestValidateMetadataAlways checks that link-local and metadata addresses
// fail SEC-06 even when the allow-list covers every address.
func TestValidateMetadataAlways(t *testing.T) {
	s := validator(t, "0.0.0.0/0,::/0")
	for _, addr := range []string{"169.254.169.254:80", "169.254.1.1:80", "[fe80::1]:80", "[fd00:ec2::254]:80", "100.100.100.200:80", "metadata:80"} {
		c := parseConfig(t, validConfig)
		c.Upstreams[0].Endpoints[0].Address = addr
		if got := show(s.validateConfig(t.Context(), c, nil)); !strings.Contains(got, "not in the allowed upstream networks") {
			t.Errorf("%s: %q, want it rejected", addr, got)
		}
	}
	c := parseConfig(t, validConfig)
	c.Upstreams[0].Endpoints[0].Address = "10.1.2.3:80"
	if got := show(s.validateConfig(t.Context(), c, nil)); got != "" {
		t.Errorf("10.1.2.3 under 0.0.0.0/0: %s", got)
	}
}

// TestValidationFailed checks the 422 that a failed publish answers.
func TestValidationFailed(t *testing.T) {
	c := parseConfig(t, validConfig)
	c.Routes[0].Upstream = "payments"
	c.Routes[1].Upstream = "payments"
	e := validationFailed(validator(t, "172.29.0.0/16").validateConfig(t.Context(), c, nil))
	if e.status != 422 || e.code != apiv1.ErrorCodeValidationFailed || len(e.details) != 2 ||
		e.msg != `validation failed: route "orders-api": upstream "payments" does not exist (and 1 more errors)` {
		t.Errorf("%d %s %q, %d details", e.status, e.code, e.msg, len(e.details))
	}
}
