package config

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// planConfig is a config with two routes, a per-consumer local policy, a
// from_plan Redis policy and two plans, as the control plane sends it
// (IDs set); ratelimit.replicas is 2.
const planConfig = `
ratelimit: {replicas: 2}
upstreams: [{name: u, endpoints: [{address: 'u:80'}]}]
routes: [{name: a, upstream: u, match: {path: /a}}, {name: b, upstream: u, match: {path: /b}, cost: 5}]
policies:
  - {id: 11, name: per-consumer, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 100, period: 1s}], local: true}
  - {id: 12, name: orders-plan, level: consumer, match: {routes: ['*']}, key_template: '{consumer}:{route}', algorithm: gcra, from_plan: true}
  - {id: 13, name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
plans:
  - {id: 21, name: free, default_limits: [{policy: orders-plan, limits: [{rate: 10, period: 1s, burst: 20}]}]}
  - {id: 22, name: pro, default_limits: [{policy: orders-plan, limits: [{rate: 100, period: 1s, burst: 200}, {rate: 10000, period: 1h, burst: 500}]}, {policy: per-consumer, limits: [{rate: 500, period: 1s}]}]}
`

func compilePlanConfig(t *testing.T, yaml string, managed bool) (*Snapshot, error) {
	t.Helper()
	c, err := Load([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if managed {
		return CompileManaged(c)
	}
	return Compile(c)
}

// TestCompilePlans: each policy gets its plans' limits by plan ID, per
// replica for a local policy and cluster-wide for a Redis one (RL-08,
// RL-31, ADR-0041); a from_plan policy has no limits of its own; managed
// mode names policies by ID, static mode by name (ADR-0036).
func TestCompilePlans(t *testing.T) {
	s, err := compilePlanConfig(t, planConfig, true)
	if err != nil {
		t.Fatal(err)
	}
	pc, op, ip := s.Policies[0], s.Policies[1], s.Policies[2]
	if pc.Ref != "11" || op.Ref != "12" || ip.Ref != "13" {
		t.Errorf("managed refs %q %q %q, want the IDs", pc.Ref, op.Ref, ip.Ref)
	}
	if !pc.PerConsumer || !op.PerConsumer || ip.PerConsumer || !op.FromPlan || op.Limits != nil {
		t.Errorf("per-consumer %v %v %v, from_plan %v with limits %v", pc.PerConsumer, op.PerConsumer, ip.PerConsumer, op.FromPlan, op.Limits)
	}
	if got := pc.PlanLimits; len(got) != 1 || !slices.Equal(got[22], []Limit{{Rate: 250, Period: time.Second, Burst: 250}}) {
		t.Errorf("per-consumer plan limits %v, want pro's 500/s halved", got)
	}
	if got := op.PlanLimits; len(got) != 2 || !slices.Equal(got[21], []Limit{{Rate: 10, Period: time.Second, Burst: 20}}) ||
		!slices.Equal(got[22], []Limit{{Rate: 100, Period: time.Second, Burst: 200}, {Rate: 10000, Period: time.Hour, Burst: 500}}) {
		t.Errorf("orders-plan plan limits %v, want free's and pro's, cluster-wide", got)
	}
	static, err := compilePlanConfig(t, planConfig, false)
	if err != nil {
		t.Fatal(err)
	}
	if static.Policies[0].Ref != "per-consumer" || static.Policies[1].Ref != "orders-plan" {
		t.Errorf("static refs %q %q, want the names", static.Policies[0].Ref, static.Policies[1].Ref)
	}
	// Plans without IDs (a static file) are checked, but no consumer can
	// be on one, so none is kept.
	noIDs := strings.NewReplacer("id: 21, ", "", "id: 22, ", "").Replace(planConfig)
	if s, err := compilePlanConfig(t, noIDs, false); err != nil || s.Policies[1].PlanLimits != nil {
		t.Errorf("plans without IDs: %v, plan limits %v", err, s.Policies[1].PlanLimits)
	}
}

// TestPlanRejects: the plan rules of ADR-0041 (CP-03).
func TestPlanRejects(t *testing.T) {
	for _, tt := range []struct{ name, from, to, want string }{
		{"a plan without an entry for a from_plan policy", "{id: 21, name: free, default_limits: [{policy: orders-plan, limits: [{rate: 10, period: 1s, burst: 20}]}]}", "{id: 21, name: free}",
			`plan "free": no default_limits for policy "orders-plan", which takes its limits from plans (from_plan)`},
		{"an entry for an undefined policy", "{policy: per-consumer,", "{policy: nope,", `plan "pro": default_limits[1]: policy "nope" is not defined`},
		{"an entry for a global policy", "{policy: per-consumer,", "{policy: per-ip,", `plan "pro": default_limits[1]: policy "per-ip": plans and overrides apply only to`},
		{"an entry with a bad burst", "{rate: 10, period: 1s, burst: 20}", "{rate: 10, period: 1s, burst: -1}", "limits[0].burst: -1 is not 1 to"},
		{"an entry without a burst over 1m", "{rate: 10000, period: 1h, burst: 500}", "{rate: 10000, period: 1h}", `plan "pro": default_limits[0] (policy "orders-plan"): limits[1].burst: required for periods over 1m0s (RL-03)`},
		{"an entry below a route's cost", "{rate: 10, period: 1s, burst: 20}", "{rate: 4, period: 1s, burst: 4}", `plan "free": default_limits[0] (policy "orders-plan"): route "b" costs 5, more than the 4 that limits[0] admits`},
		{"an entry below a route's cost per replica", "{policy: per-consumer, limits: [{rate: 500, period: 1s}]}", "{policy: per-consumer, limits: [{rate: 9, period: 1s}]}", "more than the 4 that limits[0] admits per replica"},
		{"two plans with one ID", "{id: 22, name: pro", "{id: 21, name: pro", `plan "pro": id 21 is plan "free"'s`},
		{"a from_plan policy with limits", "algorithm: gcra, from_plan: true}", "algorithm: gcra, from_plan: true, limits: [{rate: 1, period: 1s}]}", "remove limits"},
		{"two entries for one policy", "{policy: per-consumer, limits: [{rate: 500, period: 1s}]}", "{policy: orders-plan, limits: [{rate: 1, period: 1s}]}, {policy: per-consumer, limits: [{rate: 500, period: 1s}]}",
			`plan "pro": default_limits[1]: policy "orders-plan" has another entry`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			yaml := strings.Replace(planConfig, tt.from, tt.to, 1)
			if yaml == planConfig {
				t.Fatal("the change does not apply")
			}
			if _, err := compilePlanConfig(t, yaml, true); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
	// The tenant-slot call counts a policy at its plans' most limits.
	redis := strings.Replace(planConfig, "algorithm: gcra, from_plan: true}", "algorithm: gcra, from_plan: true}\n  - {id: 14, name: r, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: "+limits(31)+"}", 1)
	if _, err := compilePlanConfig(t, redis, true); err == nil || !strings.Contains(err.Error(), `route "a" would carry 33 Redis limits in one call`) {
		t.Errorf("31 limits beside pro's 2: error = %v", err)
	}
}

// TestCheckOverride: an override's limits are checked against its policy
// as a plan entry's are, plus the call limit with the override's count
// (RL-08, ADR-0041).
func TestCheckOverride(t *testing.T) {
	s, err := compilePlanConfig(t, planConfig, true)
	if err != nil {
		t.Fatal(err)
	}
	lim := func(rate int64, period string, burst int64) *configpb.Limit {
		return &configpb.Limit{Rate: rate, Period: period, Burst: burst}
	}
	for _, tt := range []struct {
		name   string
		policy int64
		limits []*configpb.Limit
		want   string
	}{
		{"valid", 12, []*configpb.Limit{lim(30, "1s", 30)}, ""},
		{"valid on a local policy", 11, []*configpb.Limit{lim(50, "1s", 0), lim(1000, "1m", 0)}, ""},
		{"unknown policy", 99, []*configpb.Limit{lim(30, "1s", 0)}, "policy 99 is not defined"},
		{"a global policy", 13, []*configpb.Limit{lim(30, "1s", 0)}, `policy "per-ip": plans and overrides apply only to`},
		{"no limits", 12, nil, `policy "orders-plan": limits: at least one is required`},
		{"burst required", 12, []*configpb.Limit{lim(30, "1h", 0)}, "limits[0].burst: required"},
		{"below a route's cost", 12, []*configpb.Limit{lim(3, "1s", 3)}, `route "b" costs 5`},
		{"33 limits", 12, func() []*configpb.Limit {
			var ls []*configpb.Limit
			for i := range 33 {
				ls = append(ls, lim(10, (time.Duration(i+1)*time.Second).String(), 10))
			}
			return ls
		}(), "limits: a Redis policy has at most 32"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := s.CheckOverride(tt.policy, tt.limits)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
	// OverrideLimits, at request time: per replica for a local policy,
	// cluster-wide for a Redis one, false when they do not fit.
	if ls, ok := s.Policies[0].OverrideLimits([]*configpb.Limit{lim(50, "1s", 0)}, 2); !ok || !slices.Equal(ls, []Limit{{Rate: 25, Period: time.Second, Burst: 25}}) {
		t.Errorf("local override limits %v, %v", ls, ok)
	}
	if ls, ok := s.Policies[1].OverrideLimits([]*configpb.Limit{lim(30, "1s", 0)}, 2); !ok || !slices.Equal(ls, []Limit{{Rate: 30, Period: time.Second, Burst: 30}}) {
		t.Errorf("Redis override limits %v, %v", ls, ok)
	}
	for _, bad := range [][]*configpb.Limit{nil, {lim(30, "1h", 0)}, {lim(0, "1s", 0)}} {
		if _, ok := s.Policies[1].OverrideLimits(bad, 2); ok {
			t.Errorf("override limits %v accepted", bad)
		}
	}
	if _, ok := s.Policies[2].OverrideLimits([]*configpb.Limit{lim(30, "1s", 0)}, 2); ok {
		t.Error("override limits accepted for a global policy")
	}
}

// TestIdentityAccessAndOverrides: managed identity's statuses, access
// lists and overrides (RL-08, RL-09, ADR-0041).
func TestIdentityAccessAndOverrides(t *testing.T) {
	at := func(s string) *timestamppb.Timestamp {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return timestamppb.New(tm)
	}
	limit := func(rate int64) []*configpb.Limit { return []*configpb.Limit{{Rate: rate, Period: "1s"}} }
	id, err := CompileIdentity(&configpb.Identity{
		Version: 3, KeyFormat: KeyFormatManaged,
		Tenants: []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2, Status: "active"}, {Id: 2, Name: "gone", PlanId: 2, Status: "suspended"}},
		Consumers: []*configpb.Consumer{
			{Id: 3, TenantId: 1, Name: "bot", Status: "active", JwtSubject: "bot"},
			{Id: 4, TenantId: 1, Name: "off", Status: "suspended", JwtSubject: "off"},
			{Id: 5, TenantId: 2, Name: "orphan", Status: "active", JwtSubject: "orphan"},
		},
		AccessLists: []*configpb.AccessListEntry{
			{Id: 1, Action: "deny", SubjectType: "cidr", Cidr: "203.0.113.0/24"},
			{Id: 2, Action: "allow", SubjectType: "cidr", Cidr: "198.51.100.0/24"},
			{Id: 3, Action: "allow", SubjectType: "cidr", Cidr: "2001:db8::/32"},
			{Id: 4, Action: "deny", SubjectType: "key", ApiKeyId: 70},
			{Id: 5, Action: "allow", SubjectType: "key", ApiKeyId: 71},
			{Id: 6, Action: "allow", SubjectType: "consumer", ConsumerId: 3},
		},
		Overrides: []*configpb.Override{
			{Id: 10, ConsumerId: 3, PolicyId: 12, Limits: limit(30)},
			{Id: 11, ConsumerId: 3, PolicyId: 12, Limits: limit(50), StartsAt: at("2026-11-14T10:00:00Z"), EndsAt: at("2026-11-14T11:00:00Z")},
			{Id: 12, ConsumerId: 3, PolicyId: 12, Limits: limit(70), StartsAt: at("2026-11-14T10:30:00Z"), EndsAt: at("2026-11-14T10:40:00Z")},
			{Id: 13, ConsumerId: 3, PolicyId: 12, Limits: limit(60), EndsAt: at("2026-11-14T09:00:00Z")},
			{Id: 14, ConsumerId: 3, PolicyId: 11, Limits: limit(90), EndsAt: at("2026-11-14T12:00:00Z")},
			{Id: 15, ConsumerId: 3, PolicyId: 11, Limits: limit(91), EndsAt: at("2026-11-14T12:00:00Z")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string][2]bool{
		"203.0.113.9": {true, false}, "198.51.100.1": {false, true}, "2001:db8::1": {false, true}, "192.0.2.1": {false, false},
	} {
		if d, a := id.CIDR(netip.MustParseAddr(ip)); [2]bool{d, a} != want {
			t.Errorf("CIDR(%s) = deny %v, allow %v; want %v", ip, d, a, want)
		}
	}
	bot := &Consumer{ID: 3}
	for _, tt := range []struct {
		name string
		c    *Consumer
		key  int64
		want [2]bool
	}{
		{"consumer allow", bot, 0, [2]bool{false, true}},
		{"key deny beats consumer allow", bot, 70, [2]bool{true, true}},
		{"key allow", &Consumer{ID: 8}, 71, [2]bool{false, true}},
		{"neither", &Consumer{ID: 8}, 72, [2]bool{false, false}},
		{"suspended", &Consumer{ID: 4, Suspended: true}, 0, [2]bool{true, false}},
		{"anonymous", nil, 0, [2]bool{false, false}},
	} {
		if d, a := id.Access(tt.c, tt.key); [2]bool{d, a} != tt.want {
			t.Errorf("%s: Access = deny %v, allow %v; want %v", tt.name, d, a, tt.want)
		}
	}

	// Suspension, the consumer's own or its tenant's.
	for name, want := range map[string]bool{"bot": false, "off": true, "orphan": true} {
		if c, ok := id.Subject(name); !ok || c.Suspended != want {
			t.Errorf("%s: %+v, %v; want suspended %v", name, c, ok, want)
		}
	}

	// Which override applies, on the gateway's clock: a grant beats the
	// standing override, the latest start wins, and an ended grant gives
	// way to the standing override again.
	for _, tt := range []struct {
		at   string
		want int64 // the rate; 0 for none
	}{
		{"2026-11-14T08:00:00Z", 60}, // grant 13 has no start: active from the start
		{"2026-11-14T09:30:00Z", 30}, // only the standing override
		{"2026-11-14T10:15:00Z", 50}, // grant 11
		{"2026-11-14T10:35:00Z", 70}, // grant 12 started later
		{"2026-11-14T10:40:00Z", 50}, // grant 12 ended (ends_at excluded)
		{"2026-11-14T11:00:00Z", 30}, // grant 11 ended
	} {
		tm, _ := time.Parse(time.RFC3339, tt.at)
		ls, ok := id.Override(3, 12, tm)
		if got := map[bool]int64{true: ls[0].GetRate()}[ok]; got != tt.want {
			t.Errorf("at %s: override rate %d, want %d", tt.at, got, tt.want)
		}
	}
	// Two grants with the same start: the higher ID.
	if ls, ok := id.Override(3, 11, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)); !ok || ls[0].GetRate() != 91 {
		t.Errorf("tie: %v, %v; want override 15's", ls, ok)
	}
	if _, ok := id.Override(4, 12, time.Now()); ok {
		t.Error("an override for a consumer without one")
	}
	var none *Identity
	if d, a := none.CIDR(netip.MustParseAddr("203.0.113.9")); d || a {
		t.Error("a nil identity has access lists")
	}
}

// TestIdentityAccessErrors: malformed access-list entries and overrides
// reject the identity, which the gateway then keeps the last good one of.
func TestIdentityAccessErrors(t *testing.T) {
	base := func() *configpb.Identity {
		return &configpb.Identity{
			Version: 1, KeyFormat: KeyFormatManaged,
			Tenants:   []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2, Status: "active"}},
			Consumers: []*configpb.Consumer{{Id: 3, TenantId: 1, Name: "bot", Status: "active"}},
		}
	}
	for _, tt := range []struct {
		name   string
		change func(*configpb.Identity)
		want   string
	}{
		{"unknown action", func(c *configpb.Identity) {
			c.AccessLists = []*configpb.AccessListEntry{{Id: 1, Action: "block", SubjectType: "cidr", Cidr: "10.0.0.0/8"}}
		}, `access list entry 1: action "block"`},
		{"bad CIDR", func(c *configpb.Identity) {
			c.AccessLists = []*configpb.AccessListEntry{{Id: 2, Action: "deny", SubjectType: "cidr", Cidr: "10.0.0.1/8"}}
		}, `access list entry 2: cidr "10.0.0.1/8"`},
		{"key entry without a key", func(c *configpb.Identity) {
			c.AccessLists = []*configpb.AccessListEntry{{Id: 3, Action: "deny", SubjectType: "key"}}
		}, "access list entry 3: api_key_id is required"},
		{"unknown subject", func(c *configpb.Identity) {
			c.AccessLists = []*configpb.AccessListEntry{{Id: 4, Action: "deny", SubjectType: "tenant"}}
		}, `subject_type "tenant"`},
		{"override of an unknown consumer", func(c *configpb.Identity) {
			c.Overrides = []*configpb.Override{{Id: 5, ConsumerId: 9, PolicyId: 1}}
		}, "override 5: consumer 9 is not defined"},
		{"bad status", func(c *configpb.Identity) { c.Consumers[0].Status = "paused" }, `consumer 3: status "paused"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.change(c)
			if _, err := CompileIdentity(c); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
