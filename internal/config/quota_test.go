package config

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// TestCompileQuotas: managed mode keeps each plan's quotas by plan ID;
// static mode rejects them; each is 0 to 10^15 (RL-10, ADR-0070).
func TestCompileQuotas(t *testing.T) {
	withQuotas := strings.Replace(planConfig, "{id: 21, name: free, ", "{id: 21, name: free, quotas: {daily: 1000, monthly: 20000}, ", 1)
	withQuotas = strings.Replace(withQuotas, "{id: 22, name: pro, ", "{id: 22, name: pro, quotas: {monthly: 0}, ", 1)
	s, err := compilePlanConfig(t, withQuotas, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.PlanQuotas) != 1 || s.PlanQuotas[21] != (Quotas{Daily: 1000, Monthly: 20000}) {
		t.Errorf("PlanQuotas = %v, want free's only", s.PlanQuotas)
	}
	if _, err := compilePlanConfig(t, withQuotas, false); err == nil || err.Error() != `plan "free": quotas need managed mode (RL-10)` {
		t.Errorf("static: error = %v", err)
	}
	if _, err := compilePlanConfig(t, strings.Replace(withQuotas, "monthly: 20000", "monthly: 1000000000000001", 1), true); err == nil ||
		!strings.Contains(err.Error(), `plan "free": quotas.monthly: 1000000000000001 is not 0 (none) to 1000000000000000`) {
		t.Errorf("over 10^15: error = %v", err)
	}
	if err := CheckPlan(&configpb.Plan{Name: "x", Quotas: &configpb.Quotas{Daily: -1}}); err == nil || !strings.Contains(err.Error(), "quotas.daily: -1") {
		t.Errorf("CheckPlan with a negative quota: %v", err)
	}
}

// TestQuotaCallReserve: managed mode leaves two limits of every tenant-slot
// call for a consumer's quotas; static mode does not (ADR-0070).
func TestQuotaCallReserve(t *testing.T) {
	limits := func(n int) string {
		var b []string
		for i := range n {
			b = append(b, "{rate: 1000, period: "+time.Duration((i+2)*int(time.Second)).String()+"}")
		}
		return "[" + strings.Join(b, ", ") + "]"
	}
	// pro's orders-plan has 2 limits on every route.
	redis := func(n int) string {
		return strings.Replace(planConfig, "algorithm: gcra, from_plan: true}", "algorithm: gcra, from_plan: true}\n  - {id: 14, name: r, level: route, match: {routes: ['*']}, key_template: '{route}', algorithm: sliding_window, limits: "+limits(n)+"}", 1)
	}
	if _, err := compilePlanConfig(t, redis(28), true); err != nil {
		t.Errorf("28 + 2 limits in managed mode: %v", err)
	}
	if _, err := compilePlanConfig(t, redis(29), true); err == nil || !strings.Contains(err.Error(), `route "a" would carry 31 Redis limits in one call; at most 30, beside the 2 a consumer's quotas take (RL-10)`) {
		t.Errorf("29 + 2 limits in managed mode: %v", err)
	}
	static := regexp.MustCompile(`id: \d+, `).ReplaceAllString(redis(30), "")
	if _, err := compilePlanConfig(t, static, false); err != nil {
		t.Errorf("30 + 2 limits in static mode: %v", err)
	}
}

// TestQuotaResolution: a consumer's quotas are its quota override that
// applies at the gateway's time, a burst grant beating the standing one,
// else its plan's (its own, else its tenant's); anonymous requests and
// consumers without a plan with quotas have none (RL-10, ADR-0070).
func TestQuotaResolution(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	s := &Snapshot{PlanQuotas: map[int64]Quotas{21: {Daily: 100, Monthly: 1000}, 22: {Monthly: 5000}}}
	id, err := CompileIdentity(&configpb.Identity{
		Version: 1, KeyFormat: KeyFormatManaged,
		Tenants: []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 22}, {Id: 2, Name: "tiny", PlanId: 21}},
		Consumers: []*configpb.Consumer{
			{Id: 3, TenantId: 1, Name: "bot"}, {Id: 4, TenantId: 1, Name: "batch", PlanId: 21}, {Id: 5, TenantId: 2, Name: "app"}, {Id: 6, TenantId: 2, Name: "other"},
		},
		Overrides: []*configpb.Override{
			{Id: 1, ConsumerId: 5, Quotas: &configpb.Quotas{Daily: 7}},
			{Id: 2, ConsumerId: 5, Quotas: &configpb.Quotas{Daily: 50, Monthly: 60}, StartsAt: timestamppb.New(t0.Add(time.Hour)), EndsAt: timestamppb.New(t0.Add(2 * time.Hour))},
			{Id: 3, ConsumerId: 5, PolicyId: 12, Limits: []*configpb.Limit{{Rate: 1, Period: "1s"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]*Consumer{
		3: {Name: "3", Tenant: "1", ID: 3, Plan: 22}, 4: {Name: "4", Tenant: "1", ID: 4, Plan: 21},
		5: {Name: "5", Tenant: "2", ID: 5, Plan: 21}, 6: {Name: "6", Tenant: "2", ID: 6, Plan: 21},
	}
	for _, tt := range []struct {
		name string
		c    *Consumer
		at   time.Time
		want Quotas
	}{
		{"tenant's plan", byID[3], t0, Quotas{Monthly: 5000}},
		{"own plan beats the tenant's", byID[4], t0, Quotas{Daily: 100, Monthly: 1000}},
		{"standing quota override replaces both", byID[5], t0, Quotas{Daily: 7}},
		{"burst grant from its start", byID[5], t0.Add(time.Hour), Quotas{Daily: 50, Monthly: 60}},
		{"standing again at the grant's end", byID[5], t0.Add(2 * time.Hour), Quotas{Daily: 7}},
		{"no override: the plan's", byID[6], t0, Quotas{Daily: 100, Monthly: 1000}},
		{"anonymous", nil, t0, Quotas{}},
		{"static consumer", &Consumer{Name: "alice", Tenant: "acme"}, t0, Quotas{}},
		{"plan without quotas", &Consumer{Name: "9", Tenant: "1", ID: 9, Plan: 23}, t0, Quotas{}},
	} {
		if got := s.Quotas(tt.c, id, tt.at); got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
	// A quota override never stands in for a policy's limits.
	if _, ok := id.Override(5, 0, t0); ok {
		t.Error("Override(policy 0) found the quota override")
	}
	if ls, ok := id.Override(5, 12, t0); !ok || len(ls) != 1 {
		t.Errorf("the policy override of the same consumer: %v %v", ls, ok)
	}
}

// TestQuotaOverrideErrors: an override has a policy and no quotas, or
// quotas, no policy and no limits; quotas within range.
func TestQuotaOverrideErrors(t *testing.T) {
	for _, tt := range []struct {
		o    *configpb.Override
		want string
	}{
		{&configpb.Override{Id: 1, ConsumerId: 3}, "override 1: policy_id is required, or quotas for a quota override"},
		{&configpb.Override{Id: 2, ConsumerId: 3, PolicyId: 12, Quotas: &configpb.Quotas{Daily: 1}}, "override 2: a policy override has no quotas"},
		{&configpb.Override{Id: 3, ConsumerId: 3, Quotas: &configpb.Quotas{Daily: 1}, Limits: []*configpb.Limit{{Rate: 1, Period: "1s"}}}, "override 3: a quota override has no limits"},
		{&configpb.Override{Id: 4, ConsumerId: 3, Quotas: &configpb.Quotas{Monthly: -5}}, "override 4: quotas.monthly: -5 is not 0 (none)"},
	} {
		_, err := CompileIdentity(&configpb.Identity{
			Version: 1, KeyFormat: KeyFormatManaged, Tenants: []*configpb.Tenant{{Id: 1, Name: "acme"}},
			Consumers: []*configpb.Consumer{{Id: 3, TenantId: 1, Name: "bot"}}, Overrides: []*configpb.Override{tt.o},
		})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("override %d: error = %v, want %q", tt.o.GetId(), err, tt.want)
		}
	}
}
