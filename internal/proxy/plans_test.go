package proxy

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// planPolicies are a managed config's policies and plans (IDs as the
// control plane sets them, ADR-0036), after ADR-0041's worked example:
// per-ip runs before authentication; per-consumer has its own limit;
// orders-plan and redis-plan take theirs from plans; free and pro are the
// plans.
const planPolicies = `
policies:
  - {id: 13, name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
  - {id: 11, name: per-consumer, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 100, period: 1s}], local: true}
  - {id: 12, name: orders-plan, level: consumer, match: {routes: ['*']}, key_template: '{consumer}:{route}', algorithm: gcra, from_plan: true, local: true}
  - {id: 14, name: redis-plan, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, from_plan: true}
plans:
  - {id: 21, name: free, default_limits: [{policy: orders-plan, limits: [{rate: 10, period: 1s, burst: 20}]}, {policy: redis-plan, limits: [{rate: 5, period: 1s}]}]}
  - {id: 22, name: pro, default_limits: [{policy: orders-plan, limits: [{rate: 100, period: 1s, burst: 200}]}, {policy: per-consumer, limits: [{rate: 500, period: 1s}]}, {policy: redis-plan, limits: [{rate: 50, period: 1m}]}]}
`

// managedSnapshot compiles yaml as a managed gateway does (ADR-0050).
func managedSnapshot(t *testing.T, yaml string) *config.Snapshot {
	t.Helper()
	c, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.CompileManaged(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// planWorld is a managed gateway with planPolicies, a fake Redis, trusted
// proxies on loopback (so X-Forwarded-For sets the client IP) and a key per
// consumer.
type planWorld struct {
	g     *gateway
	redis *fakeRedis
	keys  map[string]string // by consumer name
	ids   map[string]int64  // key IDs by consumer name
	base  *configpb.Identity
}

// The consumers: bot (acme, so pro), batch (acme, its own plan free), app
// (tiny, free), off (suspended), orphan (of suspended tenant gone), noplan
// (of a tenant whose plan the config lacks).
var planConsumers = []*configpb.Consumer{
	{Id: 3, TenantId: 1, Name: "bot", Status: "active"},
	{Id: 4, TenantId: 1, Name: "batch", PlanId: 21, Status: "active"},
	{Id: 5, TenantId: 2, Name: "app", Status: "active"},
	{Id: 6, TenantId: 1, Name: "off", Status: "suspended"},
	{Id: 7, TenantId: 3, Name: "orphan", Status: "active"},
	{Id: 8, TenantId: 4, Name: "noplan", Status: "active"},
}

func newPlanWorld(t *testing.T, trusted string) *planWorld {
	t.Helper()
	u := newUpstream(t, "orders")
	tp, err := ParseTrustedProxies(trusted)
	if err != nil {
		t.Fatal(err)
	}
	w := &planWorld{redis: newFakeRedis(), keys: map[string]string{}, ids: map[string]int64{}}
	w.g = newGatewayWith(t, newResolver(u), oneUpstream(u), tp, w.redis)
	w.g.swap(t, managedSnapshot(t, oneUpstream(u)+planPolicies))
	w.base = &configpb.Identity{
		Version: 1, KeyFormat: config.KeyFormatManaged,
		Tenants: []*configpb.Tenant{
			{Id: 1, Name: "acme", PlanId: 22, Status: "active"}, {Id: 2, Name: "tiny", PlanId: 21, Status: "active"},
			{Id: 3, Name: "gone", PlanId: 21, Status: "suspended"}, {Id: 4, Name: "lost", PlanId: 99, Status: "active"},
		},
		Consumers: planConsumers,
	}
	for i, c := range planConsumers {
		k, err := apikey.Generate()
		if err != nil {
			t.Fatal(err)
		}
		id := int64(70 + i)
		w.keys[c.GetName()], w.ids[c.GetName()] = k.Secret.Reveal(), id
		w.base.ApiKeys = append(w.base.ApiKeys, &configpb.ApiKey{Id: id, ConsumerId: c.GetId(), Prefix: k.Prefix, Hash: hex.EncodeToString(k.Hash[:])})
	}
	w.setIdentity(t, nil, nil)
	return w
}

// setIdentity makes the base identity, with these access lists and
// overrides, current, as an identity update does (CP-20).
func (w *planWorld) setIdentity(t *testing.T, lists []*configpb.AccessListEntry, overrides []*configpb.Override) {
	t.Helper()
	c := &configpb.Identity{
		Version: w.base.GetVersion() + 1, KeyFormat: w.base.GetKeyFormat(), Tenants: w.base.GetTenants(),
		Consumers: w.base.GetConsumers(), ApiKeys: w.base.GetApiKeys(), AccessLists: lists, Overrides: overrides,
	}
	w.base.Version = c.Version
	id, err := config.CompileIdentity(c)
	if err != nil {
		t.Fatal(err)
	}
	w.g.identity.Store(id)
}

// send sends GET /orders as consumer (its key; "" for none) from client IP
// ip; it returns the status, the body and the quotas RateLimit-Policy
// shows by policy name.
func (w *planWorld) send(t *testing.T, consumer, ip string) (int, string, map[string]string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, w.g.URL+"/orders", nil)
	if err != nil {
		t.Fatal(err)
	}
	if consumer != "" {
		req.Header.Set("X-Api-Key", w.keys[consumer])
	}
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := w.g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	quotas := map[string]string{}
	for p := range strings.SplitSeq(resp.Header.Get("RateLimit-Policy"), ", ") {
		if name, rest, ok := strings.Cut(p, ";q="); ok {
			quotas[strings.Trim(name, `"`)] = rest
		}
	}
	return resp.StatusCode, string(body), quotas
}

// TestLimitPrecedence is D3's matrix (RL-08, RL-09, ADR-0041): deny > allow
// > (consumer, policy) override > plan default_limits > the policy's own
// limit, each row seen in the limits RateLimit-Policy reports and in the
// Redis call's limits.
func TestLimitPrecedence(t *testing.T) {
	w := newPlanWorld(t, "127.0.0.1/32")
	const ip = "192.0.2.10"
	at := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(w.g.clock.Now().Add(d)) }
	limits := func(rate int64) []*configpb.Limit { return []*configpb.Limit{{Rate: rate, Period: "1s"}} }
	standing := &configpb.Override{Id: 1, ConsumerId: 4, PolicyId: 12, Limits: limits(30)}
	grant := &configpb.Override{Id: 2, ConsumerId: 4, PolicyId: 12, Limits: limits(50), StartsAt: at(-time.Minute), EndsAt: at(time.Hour)}
	redisOverride := &configpb.Override{Id: 3, ConsumerId: 4, PolicyId: 14, Limits: []*configpb.Limit{{Rate: 7, Period: "10s"}}}
	globalOverride := &configpb.Override{Id: 4, ConsumerId: 4, PolicyId: 13, Limits: limits(1)} // not per-consumer: ignored
	entry := func(id int64, action, subject string, v any) *configpb.AccessListEntry {
		e := &configpb.AccessListEntry{Id: id, Action: action, SubjectType: subject}
		switch subject {
		case "cidr":
			e.Cidr = v.(string)
		case "key":
			e.ApiKeyId = w.ids[v.(string)]
		case "consumer":
			e.ConsumerId = v.(int64)
		}
		return e
	}

	type want struct {
		status int
		quotas string // name=q;w=… of each limit, sorted by name; "-" for no RateLimit-Policy
	}
	for _, tt := range []struct {
		name      string
		consumer  string
		ip        string
		lists     []*configpb.AccessListEntry
		overrides []*configpb.Override
		want      want
	}{
		// Policy and plan (steps 4 and 5).
		{"free tenant: policy limit, plan limit", "app", ip, nil, nil,
			want{200, "orders-plan=10;w=1 per-consumer=100;w=1 per-ip=1000;w=1 redis-plan=5;w=1"}},
		{"pro tenant: both from the plan", "bot", ip, nil, nil,
			want{200, "orders-plan=100;w=1 per-consumer=500;w=1 per-ip=1000;w=1 redis-plan=50;w=60"}},
		{"own plan beats the tenant's", "batch", ip, nil, nil,
			want{200, "orders-plan=10;w=1 per-consumer=100;w=1 per-ip=1000;w=1 redis-plan=5;w=1"}},
		{"from_plan with no plan limits: the policy does not apply", "noplan", ip, nil, nil,
			want{200, "per-consumer=100;w=1 per-ip=1000;w=1"}},
		{"anonymous: per-consumer policies do not apply", "", ip, nil, nil,
			want{200, "per-ip=1000;w=1"}},
		// Overrides (step 3).
		{"standing override beats the plan", "batch", ip, nil, []*configpb.Override{standing, redisOverride, globalOverride},
			want{200, "orders-plan=30;w=1 per-consumer=100;w=1 per-ip=1000;w=1 redis-plan=7;w=10"}},
		{"burst grant beats the standing override", "batch", ip, nil, []*configpb.Override{standing, grant},
			want{200, "orders-plan=50;w=1 per-consumer=100;w=1 per-ip=1000;w=1 redis-plan=5;w=1"}},
		{"another consumer's override does not apply", "app", ip, nil, []*configpb.Override{standing, grant},
			want{200, "orders-plan=10;w=1 per-consumer=100;w=1 per-ip=1000;w=1 redis-plan=5;w=1"}},
		// Allow (step 2): no policy that has not run yet.
		{"CIDR allow: no policy runs", "batch", ip, []*configpb.AccessListEntry{entry(1, "allow", "cidr", "192.0.2.0/24")}, []*configpb.Override{standing},
			want{200, "-"}},
		{"key allow: only the policies before authentication", "batch", ip, []*configpb.AccessListEntry{entry(1, "allow", "key", "batch")}, nil,
			want{200, "per-ip=1000;w=1"}},
		{"consumer allow", "batch", ip, []*configpb.AccessListEntry{entry(1, "allow", "consumer", int64(4))}, nil,
			want{200, "per-ip=1000;w=1"}},
		{"CIDR allow for an anonymous request", "", ip, []*configpb.AccessListEntry{entry(1, "allow", "cidr", "192.0.2.10/32")}, nil,
			want{200, "-"}},
		// Deny (step 1) beats everything.
		{"CIDR deny", "bot", ip, []*configpb.AccessListEntry{entry(1, "deny", "cidr", "192.0.2.0/24")}, nil, want{403, "-"}},
		{"CIDR deny beats CIDR allow", "bot", ip, []*configpb.AccessListEntry{entry(1, "deny", "cidr", "192.0.2.10/32"), entry(2, "allow", "cidr", "192.0.2.0/24")}, nil, want{403, "-"}},
		{"key deny beats consumer allow", "bot", ip, []*configpb.AccessListEntry{entry(1, "deny", "key", "bot"), entry(2, "allow", "consumer", int64(3))}, nil,
			want{403, "per-ip=1000;w=1"}},
		{"consumer deny beats key and CIDR allow", "bot", ip, []*configpb.AccessListEntry{entry(1, "deny", "consumer", int64(3)), entry(2, "allow", "key", "bot"), entry(3, "allow", "cidr", "192.0.2.0/24")}, nil,
			want{403, "-"}},
		{"suspended consumer, even allowed", "off", ip, []*configpb.AccessListEntry{entry(1, "allow", "consumer", int64(6))}, nil, want{403, "per-ip=1000;w=1"}},
		{"consumer of a suspended tenant", "orphan", ip, nil, nil, want{403, "per-ip=1000;w=1"}},
		{"another IP is not denied", "bot", "198.51.100.1", []*configpb.AccessListEntry{entry(1, "deny", "cidr", "192.0.2.0/24")}, nil,
			want{200, "orders-plan=100;w=1 per-consumer=500;w=1 per-ip=1000;w=1 redis-plan=50;w=60"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w.setIdentity(t, tt.lists, tt.overrides)
			status, body, quotas := w.send(t, tt.consumer, tt.ip)
			got := "-"
			if len(quotas) > 0 {
				var parts []string
				for name, q := range quotas {
					parts = append(parts, name+"="+q)
				}
				slices.Sort(parts)
				got = strings.Join(parts, " ")
			}
			if status != tt.want.status || got != tt.want.quotas {
				t.Errorf("%d %s, limits %s; want %d, limits %s", status, body, got, tt.want.status, tt.want.quotas)
			}
			if status == http.StatusForbidden && body != `{"error":"forbidden"}` {
				t.Errorf("403 body %q", body)
			}
		})
	}

	// The Redis calls carry the policy's ID in their keys and the chosen
	// limits as ARGV (ADR-0036, ADR-0041): app's free 5/s, then batch's
	// override 7/10s.
	w.redis.mu.Lock()
	calls := strings.Join(w.redis.calls, "\n")
	w.redis.mu.Unlock()
	for _, want := range []string{"rl:v1:{t:2}:14:1000:", "rl:v1:{t:1}:14:10000:", "5/1s burst 5", "7/10s burst 7", "rl:v1:{t:1}:14:60000:"} {
		if !strings.Contains(calls, want) {
			t.Errorf("no Redis call has %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "redis-plan") {
		t.Errorf("a managed Redis key names the policy, not its ID:\n%s", calls)
	}
}

// TestBurstGrantExpiry: a burst grant applies from its start to its end on
// the gateway's clock, then the standing override returns, with no
// identity change (RL-08, ADR-0041); the limiter keeps its state across the
// switch, as for a reload (GW-29).
func TestBurstGrantExpiry(t *testing.T) {
	w := newPlanWorld(t, "127.0.0.1/32")
	start, end := w.g.clock.Now().Add(10*time.Second), w.g.clock.Now().Add(70*time.Second)
	w.setIdentity(t, nil, []*configpb.Override{
		{Id: 1, ConsumerId: 4, PolicyId: 12, Limits: []*configpb.Limit{{Rate: 2, Period: "1m"}}},
		{Id: 2, ConsumerId: 4, PolicyId: 12, Limits: []*configpb.Limit{{Rate: 6, Period: "1m"}}, StartsAt: timestamppb.New(start), EndsAt: timestamppb.New(end)},
	})
	version := w.g.identity.Load().Version
	quota := func() (int, string) {
		status, _, q := w.send(t, "batch", "192.0.2.10")
		return status, q["orders-plan"]
	}
	// Before the start: the standing override, 2 a minute.
	for i, want := range []int{200, 200, 429} {
		if status, q := quota(); status != want || q != "2;w=60" {
			t.Fatalf("before the grant, request %d: %d, q %s; want %d at 2;w=60", i, status, q, want)
		}
	}
	// From the start: the grant's 6 a minute, on the same limiter state.
	w.g.clock.Advance(10 * time.Second)
	n := 0
	for {
		status, q := quota()
		if q != "6;w=60" {
			t.Fatalf("during the grant: q %s, want 6;w=60", q)
		}
		if status != http.StatusOK {
			break
		}
		n++
	}
	// GCRA keeps time, not counts: the two requests moved the TAT 60 s
	// ahead (30 s each); 10 s later 50 s of the grant's 60 s tolerance is
	// used, room for one request at 10 s each.
	if n != 1 {
		t.Errorf("during the grant: %d more admitted, want 1", n)
	}
	w.g.clock.Advance(20 * time.Second)
	if status, _ := quota(); status != http.StatusOK {
		t.Errorf("during the grant, 20 s later: %d, want 200", status)
	}
	// At the end (ends_at excluded): the standing override again, with no
	// identity change.
	w.g.clock.Advance(40 * time.Second)
	if _, q := quota(); q != "2;w=60" {
		t.Errorf("after the grant: q %s, want 2;w=60", q)
	}
	if v := w.g.identity.Load().Version; v != version {
		t.Errorf("identity version %d, want %d: the grant ends by itself", v, version)
	}
}

// TestCIDRDenyBeforeAuthn: a request from a denied CIDR is answered 403
// before authentication, so no key or token is checked (no authn span; a
// bad key still gets 403, not 401) and no policy runs (RL-09, ADR-0041);
// a request from elsewhere with the same bad key gets 401. The 403s are
// access-logged as gateway answers, and no key appears in the logs,
// spans or bodies (SEC-03).
func TestCIDRDenyBeforeAuthn(t *testing.T) {
	u := newUpstream(t, "orders")
	sr, tracer := newTracer(t, sdktrace.AlwaysSample())
	tp, err := ParseTrustedProxies("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	g := newTracedGateway(t, newResolver(u), oneUpstream(u), tp, nil, false, tracer)
	g.swap(t, managedSnapshot(t, strings.Replace(oneUpstream(u), "path_prefix: /}", "path_prefix: /}, auth_mode: api_key", 1)+
		"policies:\n  - {id: 13, name: per-ip, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}\n"))
	good, goodEntry := newManagedKey(t, 1, time.Time{})
	denied, deniedEntry := newManagedKey(t, 2, time.Time{})
	id, err := config.CompileIdentity(&configpb.Identity{
		Version: 1, KeyFormat: config.KeyFormatManaged,
		Tenants:   []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2, Status: "active"}},
		Consumers: []*configpb.Consumer{{Id: 3, TenantId: 1, Name: "alice", Status: "active"}},
		ApiKeys:   []*configpb.ApiKey{goodEntry, deniedEntry},
		AccessLists: []*configpb.AccessListEntry{
			{Id: 1, Action: "deny", SubjectType: "cidr", Cidr: "203.0.113.0/24"},
			{Id: 2, Action: "deny", SubjectType: "key", ApiKeyId: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.identity.Store(id)
	bad := good.Secret.Reveal()[:len(good.Secret.Reveal())-1] + "x"

	var bodies []string
	for i, tt := range []struct {
		name, ip, key string
		status        int
		authn         bool // an authn span is recorded
	}{
		{"denied CIDR, valid key", "203.0.113.7", good.Secret.Reveal(), 403, false},
		{"denied CIDR, bad key", "203.0.113.7", bad, 403, false},
		{"denied CIDR, no key", "203.0.113.7", "", 403, false},
		{"other IP, bad key", "192.0.2.1", bad, 401, true},
		{"other IP, denied key", "192.0.2.1", denied.Secret.Reveal(), 403, true},
		{"other IP, valid key", "192.0.2.1", good.Secret.Reveal(), 200, true},
	} {
		rid := "cidr-" + strconv.Itoa(i)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/orders", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", tt.ip)
		req.Header.Set("X-Request-Id", rid)
		if tt.key != "" {
			req.Header.Set("X-Api-Key", tt.key)
		}
		before := len(sr.Ended())
		resp, err := g.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tt.status {
			t.Errorf("%s: status %d %s, want %d", tt.name, resp.StatusCode, body, tt.status)
		}
		if tt.status == 403 {
			bodies = append(bodies, string(body))
			if string(body) != `{"error":"forbidden"}` || resp.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s: 403 %q %q", tt.name, resp.Header.Get("Content-Type"), body)
			}
			if line := g.logs.access(t, rid); line["status"] != float64(403) {
				t.Errorf("%s: access log %v", tt.name, line)
			}
		}
		if tt.ip == "203.0.113.7" && resp.Header.Get("RateLimit-Policy") != "" {
			t.Errorf("%s: RateLimit-Policy %q; no policy runs for a denied CIDR", tt.name, resp.Header.Get("RateLimit-Policy"))
		}
		authn := false
		for _, s := range waitSpans(t, sr, before) {
			authn = authn || s.Name() == "authn"
		}
		if authn != tt.authn {
			t.Errorf("%s: authn span %v, want %v", tt.name, authn, tt.authn)
		}
	}
	// Gateway answers, reason none (ADR-0039): the 3 denied CIDRs and the
	// 401 have no tenant; the denied key's request counts under its
	// consumer's tenant.
	m := scrape(t, g.metrics)
	for _, want := range []string{
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="none",route="r",tenant="none"} 4`,
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="none",route="r",tenant="acme"} 1`,
	} {
		if !strings.Contains(m, "\n"+want+"\n") {
			t.Errorf("/metrics has no %s", want)
		}
	}
	var spans strings.Builder
	for _, s := range sr.Ended() {
		fmt.Fprintf(&spans, "%s %v %v\n", s.Name(), s.Attributes(), s.Events())
	}
	outputs := g.logs.String() + g.events.String() + spans.String() + strings.Join(bodies, "\n")
	for _, k := range []string{good.Secret.Reveal(), denied.Secret.Reveal(), bad} {
		_, sec, _ := apikey.Parse(k)
		if strings.Contains(outputs, k) || sec != "" && strings.Contains(outputs, sec) {
			t.Error("a key or its secret appears in the logs, spans or bodies")
		}
	}
}

// waitSpans returns the spans ended after the first n, once the request's
// server span has ended.
func waitSpans(t *testing.T, sr *tracetest.SpanRecorder, n int) []sdktrace.ReadOnlySpan {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		ended := sr.Ended()
		for _, s := range ended[n:] {
			if s.SpanKind().String() == "server" {
				return ended[n:]
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no server span")
		}
	}
}
