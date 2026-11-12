package config

import (
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompilePolicies(t *testing.T) {
	data, err := os.ReadFile("testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := parse(data)
	if err != nil {
		t.Fatal(err)
	}
	// ratelimit.replicas is 2, so every rate and burst is halved, rounded
	// down (RL-31).
	want := []Policy{
		{Name: "per-ip", Level: "global", Algorithm: GCRA, Mode: Enforce, Limits: []Limit{{Rate: 100, Period: time.Second, Burst: 100}}, Local: true},
		{Name: "per-consumer", Level: "consumer", Algorithm: GCRA, Mode: Enforce, Limits: []Limit{{Rate: 50, Period: time.Minute, Burst: 50}, {Rate: 5000, Period: time.Hour, Burst: 250}}, Local: true},
		{Name: "tenant-writes", Level: "tenant", Routes: map[string]bool{"orders-api": true}, Methods: []string{"POST", "DELETE"}, Algorithm: SlidingWindow, Mode: Enforce, Limits: []Limit{{Rate: 300, Period: time.Minute}}, Local: true},
		{Name: "order-inflight", Level: "route", Routes: map[string]bool{"order-by-id": true}, Algorithm: Concurrency, Mode: Shadow, Limits: []Limit{{Rate: 10}}, Local: true},
		{Name: "retired", Level: "global", Algorithm: FixedWindow, Mode: Off, Limits: []Limit{{Rate: 5, Period: time.Second}}, Local: true},
	}
	if len(s.Policies) != len(want) {
		t.Fatalf("%d policies, want %d", len(s.Policies), len(want))
	}
	v := &KeyValues{IP: "10.1.2.3", Consumer: "alice", Tenant: "acme", Route: "orders-api", Method: "GET", Header: http.Header{"X-Client": {"c1"}}}
	wantKeys := []string{"10.1.2.3", "alice", "acme:orders-api", "orders-api", "m:GET:c1"}
	for i, p := range s.Policies {
		if key, ok := p.Key.Fill(v); !ok || key != wantKeys[i] {
			t.Errorf("%s: key = %q, %v; want %q", p.Name, key, ok, wantKeys[i])
		}
		p.Key = KeyTemplate{}
		if !reflect.DeepEqual(p, want[i]) {
			t.Errorf("policy %d = %+v\nwant %+v", i, p, want[i])
		}
	}
}

// p is a policy in YAML flow form: a valid local GCRA policy on every route,
// changed by name/value pairs; an empty value drops the field.
func p(kv ...string) string {
	f := map[string]string{
		"name": "p", "level": "global", "match": "{routes: ['*']}", "key_template": "'{ip}'",
		"algorithm": "gcra", "limits": "[{rate: 10, period: 1s}]", "local": "true",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		f[kv[i]] = kv[i+1]
	}
	var fields []string
	for k, v := range f {
		if v != "" {
			fields = append(fields, k+": "+v)
		}
	}
	slices.Sort(fields)
	return "{" + strings.Join(fields, ", ") + "}"
}

// limits returns n GCRA limits of 10 per 1s … n s, in YAML flow form.
func limits(n int) string {
	var ls []string
	for i := 1; i <= n; i++ {
		ls = append(ls, "{rate: 10, period: "+strconv.Itoa(i)+"s}")
	}
	return "[" + strings.Join(ls, ", ") + "]"
}

// policies is a config with routes a and b, b costing 5, the extra top-level
// fields and the policies.
func policies(extra string, ps ...string) string {
	return upstream + "routes: [{name: a, upstream: u, match: {path: /a}}, {name: b, upstream: u, match: {path: /b}, cost: 5}]\n" +
		extra + "policies: [" + strings.Join(ps, ", ") + "]\n"
}

func TestPolicyDefaults(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		limits []Limit
		mode   string
	}{
		{"one replica by default", policies("", p()), []Limit{{Rate: 10, Period: time.Second, Burst: 10}}, Enforce},
		{"burst defaults to rate up to 1m", policies("", p("limits", "[{rate: 100, period: 1m}]")), []Limit{{Rate: 100, Period: time.Minute, Burst: 100}}, Enforce},
		{"explicit burst", policies("", p("limits", "[{rate: 10, period: 1s, burst: 25}]")), []Limit{{Rate: 10, Period: time.Second, Burst: 25}}, Enforce},
		{"burst set above 1m", policies("", p("limits", "[{rate: 1000, period: 1h, burst: 50}]")), []Limit{{Rate: 1000, Period: time.Hour, Burst: 50}}, Enforce},
		{"per replica, rounded down", policies("ratelimit: {replicas: 3}\n", p("limits", "[{rate: 10, period: 1s, burst: 20}]")), []Limit{{Rate: 3, Period: time.Second, Burst: 6}}, Enforce},
		{"per replica, minimum 1", policies("ratelimit: {replicas: 4}\n", p("match", "{routes: [a]}", "limits", "[{rate: 3, period: 1s, burst: 3}]")), []Limit{{Rate: 1, Period: time.Second, Burst: 1}}, Enforce},
		{"replicas 0 means 1", policies("ratelimit: {replicas: 0}\n", p()), []Limit{{Rate: 10, Period: time.Second, Burst: 10}}, Enforce},
		{"window without burst", policies("", p("algorithm", "sliding_window", "limits", "[{rate: 600, period: 1m}, {rate: 10000, period: 1h}]")), []Limit{{Rate: 600, Period: time.Minute}, {Rate: 10000, Period: time.Hour}}, Enforce},
		{"fixed window up to 8760h", policies("", p("algorithm", "fixed_window", "limits", "[{rate: 5, period: 8760h}]")), []Limit{{Rate: 5, Period: 8760 * time.Hour}}, Enforce},
		{"concurrency per replica", policies("ratelimit: {replicas: 2}\n", p("algorithm", "concurrency", "limits", "[{rate: 21}]")), []Limit{{Rate: 10}}, Enforce},
		{"concurrency is always local", policies("", p("algorithm", "concurrency", "limits", "[{rate: 5}]", "local", "")), []Limit{{Rate: 5}}, Enforce},
		{"shadow", policies("", p("mode", "shadow")), []Limit{{Rate: 10, Period: time.Second, Burst: 10}}, Shadow},
		{"off", policies("", p("mode", "off")), []Limit{{Rate: 10, Period: time.Second, Burst: 10}}, Off},
		// Route b costs 5; a policy that does not match it may admit less.
		{"cost checked only on matching routes", policies("", p("match", "{routes: [a]}", "limits", "[{rate: 2, period: 1s}]")), []Limit{{Rate: 2, Period: time.Second, Burst: 2}}, Enforce},
		{"cost equal to burst", policies("", p("limits", "[{rate: 1, period: 1s, burst: 5}]")), []Limit{{Rate: 1, Period: time.Second, Burst: 5}}, Enforce},
		{"burst tolerance of exactly 8760h", policies("", p("match", "{routes: [a]}", "limits", "[{rate: 1, period: 4380h, burst: 2}]")), []Limit{{Rate: 1, Period: 4380 * time.Hour, Burst: 2}}, Enforce},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Policies) != 1 {
				t.Fatalf("%d policies", len(s.Policies))
			}
			if got := s.Policies[0]; !reflect.DeepEqual(got.Limits, tt.limits) || got.Mode != tt.mode {
				t.Errorf("limits %+v, mode %s; want %+v, %s", got.Limits, got.Mode, tt.limits, tt.mode)
			}
		})
	}

	s, err := parse([]byte(policies("")))
	if err != nil {
		t.Fatal(err)
	}
	if s.Policies != nil || s.Routes[0].Cost != 1 || s.Routes[1].Cost != 5 {
		t.Errorf("policies %v, costs %d and %d; want none, 1 (the default) and 5", s.Policies, s.Routes[0].Cost, s.Routes[1].Cost)
	}
}

// TestRedisPolicies: a policy without local: true runs in Redis with the
// configured, cluster-wide rate and burst, fail_open unless it says
// otherwise; the snapshot carries the replica count and the Redis budget
// (ADR-0021).
func TestRedisPolicies(t *testing.T) {
	s, err := parse([]byte(policies("ratelimit: {replicas: 3, redis_budget: 20ms}\n",
		p("name", "open", "local", "", "limits", "[{rate: 10, period: 1s, burst: 20}]"),
		p("name", "closed", "local", "false", "algorithm", "sliding_window", "failure_mode", "fail_closed", "limits", "[{rate: 7, period: 1m}]"),
		p("name", "degraded", "local", "", "failure_mode", "degraded", "limits", "[{rate: 15, period: 1s}]"),
		p("name", "in-memory", "limits", "[{rate: 10, period: 1s, burst: 20}]"),
		// 32 limits on route a, and on b once the global policy is left out.
		p("name", "t", "level", "tenant", "local", "", "match", "{routes: [a]}", "limits", limits(29)),
		p("name", "g", "level", "global", "local", "", "match", "{routes: [b]}", "limits", limits(32)),
		p("name", "r", "level", "route", "local", "", "match", "{routes: [b]}", "limits", limits(29)),
		p("name", "r-off", "level", "route", "local", "", "match", "{routes: [b]}", "mode", "off", "limits", limits(32)),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if s.Replicas != 3 || s.RedisBudget != 20*time.Millisecond {
		t.Errorf("replicas %d, budget %v; want 3, 20ms", s.Replicas, s.RedisBudget)
	}
	for i, want := range []struct {
		local       bool
		failureMode string
		limit       Limit
	}{
		{false, FailOpen, Limit{Rate: 10, Period: time.Second, Burst: 20}},
		{false, FailClosed, Limit{Rate: 7, Period: time.Minute}},
		{false, Degraded, Limit{Rate: 15, Period: time.Second, Burst: 15}},
		{true, "", Limit{Rate: 3, Period: time.Second, Burst: 6}},
	} {
		got := s.Policies[i]
		if got.Local != want.local || got.FailureMode != want.failureMode || got.Limits[0] != want.limit {
			t.Errorf("%s: local %v, failure mode %q, limit %+v; want %v, %q, %+v", got.Name, got.Local, got.FailureMode, got.Limits[0], want.local, want.failureMode, want.limit)
		}
	}
	if s, err := parse([]byte(policies("", p()))); err != nil || s.Replicas != 1 || s.RedisBudget != 5*time.Millisecond {
		t.Errorf("defaults: %v; want 1 replica and a 5ms budget", err)
	}
}

// TestPerReplica: a degraded limiter's limit is rate and burst ÷ replicas,
// rounded down, minimum 1 (RL-23, RL-31).
func TestPerReplica(t *testing.T) {
	for _, tt := range []struct {
		in       Limit
		replicas int64
		want     Limit
	}{
		{Limit{Rate: 100, Period: time.Second, Burst: 200}, 3, Limit{Rate: 33, Period: time.Second, Burst: 66}},
		{Limit{Rate: 6000, Period: time.Minute}, 3, Limit{Rate: 2000, Period: time.Minute}},
		{Limit{Rate: 2, Period: time.Second, Burst: 2}, 5, Limit{Rate: 1, Period: time.Second, Burst: 1}},
		{Limit{Rate: 7, Period: time.Hour, Burst: 9}, 1, Limit{Rate: 7, Period: time.Hour, Burst: 9}},
	} {
		if got := tt.in.PerReplica(tt.replicas); got != tt.want {
			t.Errorf("%+v.PerReplica(%d) = %+v, want %+v", tt.in, tt.replicas, got, tt.want)
		}
	}
}

func TestPolicyRejects(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"negative replicas", policies("ratelimit: {replicas: -1}\n", p()), "ratelimit.replicas must not be negative"},
		{"unknown policy field", policies("", p("failure", "fail_open")), `unknown field "failure"`},
		{"no name", policies("", p("name", "")), "policy: name is required"},
		{"name twice", policies("", p(), p()), `policy "p": defined twice`},
		{"name with a space", policies("", p("name", "'a b'")), `policy "a b": name must be 1-64 characters`},
		{"name with a quote", policies("", p("name", `'a"b'`)), "name must be 1-64 characters"},
		{"name too long", policies("", p("name", strings.Repeat("n", 65))), "name must be 1-64 characters"},
		{"no level", policies("", p("level", "")), `level "": want global, tenant, consumer or route`},
		{"unknown level", policies("", p("level", "user")), `level "user"`},
		{"no algorithm", policies("", p("algorithm", "")), `algorithm "": want gcra`},
		{"unknown algorithm", policies("", p("algorithm", "token_bucket")), `algorithm "token_bucket"`},
		{"upper-case algorithm", policies("", p("algorithm", "GCRA")), `algorithm "GCRA"`},
		{"unknown mode", policies("", p("mode", "dry_run")), `mode "dry_run": want enforce, shadow or off`},
		{"upper-case mode", policies("", p("mode", "OFF")), `mode "OFF"`},
		{"fixed window in Redis", policies("", p("local", "", "algorithm", "fixed_window")), `policy "p": algorithm fixed_window must be local (local: true); Redis policies use gcra or sliding_window`},
		{"local false, fixed window", policies("", p("local", "false", "algorithm", "fixed_window")), "algorithm fixed_window must be local"},
		{"failure mode on a local policy", policies("", p("failure_mode", "fail_open")), `policy "p": failure_mode applies only to Redis policies; local policies never fail`},
		{"failure mode on concurrency", policies("", p("local", "", "algorithm", "concurrency", "limits", "[{rate: 5}]", "failure_mode", "degraded")), "failure_mode applies only to Redis policies"},
		{"unknown failure mode", policies("", p("local", "", "failure_mode", "fail_slow")), `failure_mode "fail_slow": want fail_open, fail_closed or degraded`},
		{"upper-case failure mode", policies("", p("local", "", "failure_mode", "FAIL_OPEN")), `failure_mode "FAIL_OPEN"`},
		{"redis budget 0", policies("ratelimit: {redis_budget: 0s}\n", p()), `ratelimit.redis_budget: "0s" is not a duration greater than 0 and at most 1s`},
		{"negative redis budget", policies("ratelimit: {redis_budget: -5ms}\n", p()), `ratelimit.redis_budget: "-5ms"`},
		{"redis budget over 1s", policies("ratelimit: {redis_budget: 1001ms}\n", p()), `ratelimit.redis_budget: "1001ms"`},
		{"redis budget without a unit", policies("ratelimit: {redis_budget: '5'}\n", p()), `ratelimit.redis_budget: "5"`},
		{"33 limits in a Redis policy", policies("", p("local", "", "match", "{routes: [a]}", "limits", limits(33))), `policy "p": limits: a Redis policy has at most 32`},
		{"33 Redis limits on a route", policies("",
			p("name", "t", "level", "tenant", "local", "", "match", "{routes: [a]}", "limits", limits(11)),
			p("name", "c", "level", "consumer", "local", "", "limits", limits(11)),
			p("name", "r", "level", "route", "local", "", "match", "{routes: [a]}", "limits", limits(11))),
			`route "a" would carry 33 Redis limits in one call; at most 32`},
		{"cost above a Redis burst", policies("", p("local", "", "limits", "[{rate: 4, period: 1s}]")), `route "b" costs 5, more than the 4 that limits[0] admits, so its requests could never be admitted`},
		{"cost above a degraded per-replica burst", policies("ratelimit: {replicas: 3}\n", p("local", "", "failure_mode", "degraded", "limits", "[{rate: 14, period: 1s}]")),
			`route "b" costs 5, more than the 4 that limits[0] admits per replica in degraded mode, so its requests could never be admitted during a Redis outage`},
		{"cost above a degraded per-replica window rate", policies("ratelimit: {replicas: 2}\n", p("local", "", "failure_mode", "degraded", "algorithm", "sliding_window", "limits", "[{rate: 9, period: 1m}]")),
			"more than the 4 that limits[0] admits per replica in degraded mode"},
		{"no match", policies("", p("match", "")), `match.routes is required; ["*"] matches every route`},
		{"no routes", policies("", p("match", "{methods: [GET]}")), "match.routes is required"},
		{"star with a route", policies("", p("match", "{routes: ['*', a]}")), `match.routes: "*" must be the only entry`},
		{"route twice", policies("", p("match", "{routes: [a, a]}")), `match.routes: "a" listed twice`},
		{"undefined route", policies("", p("match", "{routes: [nope]}")), `match.routes: route "nope" is not defined`},
		{"lower-case method", policies("", p("match", "{routes: ['*'], methods: [get]}")), `match.methods: "get" is not an upper-case method`},
		{"method with a space", policies("", p("match", "{routes: ['*'], methods: ['G T']}")), "match.methods"},
		{"empty method", policies("", p("match", "{routes: ['*'], methods: ['']}")), "match.methods"},
		{"method twice", policies("", p("match", "{routes: ['*'], methods: [GET, GET]}")), `match.methods: "GET" listed twice`},
		{"no key template", policies("", p("key_template", "")), "key_template is required"},
		{"unknown placeholder", policies("", p("key_template", "'{user}'")), `key_template "{user}": unknown placeholder {user}`},
		{"placeholder is case-sensitive", policies("", p("key_template", "'{IP}'")), "unknown placeholder {IP}"},
		{"unclosed brace", policies("", p("key_template", "'{ip'")), `'{' without '}'`},
		{"nested brace", policies("", p("key_template", "'{a{ip}}'")), "unknown placeholder {a{ip}"},
		{"stray closing brace", policies("", p("key_template", "'ip}'")), `'}' without '{'`},
		{"empty header name", policies("", p("key_template", "'{header:}'")), `header name "" is not valid`},
		{"invalid header name", policies("", p("key_template", "'{header:a b}'")), `header name "a b" is not valid`},
		{"no limits", policies("", p("limits", "")), "limits: at least one is required"},
		{"empty limits", policies("", p("limits", "[]")), "limits: at least one is required"},
		{"rate 0", policies("", p("limits", "[{period: 1s}]")), "limits[0].rate: 0 is not 1 to 1000000000"},
		{"negative rate", policies("", p("limits", "[{rate: -1, period: 1s}]")), "limits[0].rate: -1"},
		{"rate too large", policies("", p("limits", "[{rate: 1000000001, period: 1s}]")), "limits[0].rate: 1000000001"},
		{"no period", policies("", p("limits", "[{rate: 10}]")), `limits[0].period: "" is not a duration in whole seconds from 1s to 8760h`},
		{"period without a unit", policies("", p("limits", "[{rate: 10, period: '60'}]")), `limits[0].period: "60"`},
		{"sub-second period", policies("", p("limits", "[{rate: 10, period: 500ms}]")), `limits[0].period: "500ms"`},
		{"fractional seconds", policies("", p("limits", "[{rate: 10, period: 1500ms}]")), `limits[0].period: "1500ms"`},
		{"zero period", policies("", p("limits", "[{rate: 10, period: 0s}]")), `limits[0].period: "0s"`},
		{"negative period", policies("", p("limits", "[{rate: 10, period: -1s}]")), `limits[0].period: "-1s"`},
		{"period over 8760h", policies("", p("algorithm", "fixed_window", "limits", "[{rate: 10, period: 8761h}]")), `limits[0].period: "8761h"`},
		{"period twice", policies("", p("limits", "[{rate: 10, period: 60s}, {rate: 100, period: 1m}]")), "limits[1].period: another limit has period 1m0s"},
		{"burst on a sliding window", policies("", p("algorithm", "sliding_window", "limits", "[{rate: 10, period: 1s, burst: 10}]")), "limits[0].burst: only gcra has a burst"},
		{"burst on a fixed window", policies("", p("algorithm", "fixed_window", "limits", "[{rate: 10, period: 1s, burst: 10}]")), "only gcra has a burst"},
		{"burst missing above 1m", policies("", p("limits", "[{rate: 10, period: 1s}, {rate: 1000, period: 61s}]")), "limits[1].burst: required for periods over 1m0s"},
		{"negative burst", policies("", p("limits", "[{rate: 10, period: 1s, burst: -1}]")), "limits[0].burst: -1 is not 1 to 1000000000"},
		{"burst too large", policies("", p("limits", "[{rate: 10, period: 1s, burst: 1000000001}]")), "limits[0].burst: 1000000001"},
		{"burst tolerance over 8760h", policies("", p("match", "{routes: [a]}", "limits", "[{rate: 1, period: 4380h, burst: 3}]")), "limits[0].burst: 3 per replica at 1 per 4380h0m0s is more than 8760h0m0s of burst tolerance"},
		{"burst tolerance over 8760h per replica", policies("ratelimit: {replicas: 2}\n", p("match", "{routes: [a]}", "limits", "[{rate: 3, period: 4380h, burst: 6}]")), "limits[0].burst: 3 per replica at 1 per"},
		{"concurrency with two limits", policies("", p("algorithm", "concurrency", "limits", "[{rate: 5}, {rate: 6}]")), "limits: concurrency takes exactly one limit"},
		{"concurrency with a period", policies("", p("algorithm", "concurrency", "limits", "[{rate: 5, period: 1s}]")), "concurrency takes exactly one limit"},
		{"concurrency with a burst", policies("", p("algorithm", "concurrency", "limits", "[{rate: 5, burst: 5}]")), "concurrency takes exactly one limit"},
		{"concurrency max 0", policies("", p("algorithm", "concurrency", "limits", "[{rate: 0}]")), "limits[0].rate: 0"},
		{"cost above burst", policies("", p("limits", "[{rate: 4, period: 1s}]")), `policy "p": route "b" costs 5, more than the 4 that limits[0] admits per replica`},
		{"cost above a second limit's burst", policies("", p("limits", "[{rate: 10, period: 1s}, {rate: 100, period: 1h, burst: 4}]")), `route "b" costs 5, more than the 4 that limits[1] admits`},
		{"cost above per-replica burst", policies("ratelimit: {replicas: 2}\n", p("limits", "[{rate: 10, period: 1s, burst: 9}]")), `route "b" costs 5, more than the 4 that limits[0] admits per replica`},
		{"cost above a window's rate", policies("", p("algorithm", "sliding_window", "limits", "[{rate: 4, period: 1m}]")), `route "b" costs 5, more than the 4`},
		{"cost above per-replica window rate", policies("ratelimit: {replicas: 3}\n", p("algorithm", "fixed_window", "limits", "[{rate: 14, period: 1m}]")), `route "b" costs 5, more than the 4`},
		{"cost above the max in flight", policies("", p("algorithm", "concurrency", "match", "{routes: [b]}", "limits", "[{rate: 4}]")), `route "b" costs 5, more than the 4`},
		{"cost checked for off policies too", policies("", p("mode", "off", "limits", "[{rate: 4, period: 1s}]")), `route "b" costs 5`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q\n%s", err, tt.want, tt.yaml)
			}
		})
	}
}

func TestKeyTemplate(t *testing.T) {
	v := &KeyValues{
		IP: "203.0.113.7", APIKey: "keydigest", Consumer: "alice", Route: "orders", Method: "POST",
		Header: http.Header{"X-Client": {"c1", "c2"}, "X-Empty": {""}},
	}
	tests := []struct {
		tmpl         string
		key          string // "" means unfilled
		usesIdentity bool
	}{
		{"{ip}", "203.0.113.7", false},
		{"global", "global", false},
		{"{route}:{method}", "orders:POST", false},
		{"ip={ip}/{header:x-client}", "ip=203.0.113.7/c1", false},
		{"{header:X-CLIENT}", "c1", false},
		{"{header:x-missing}", "", false},
		{"{header:x-empty}", "", false},
		{"{api_key}", "keydigest", true},
		{"{consumer}:{route}", "alice:orders", true},
		{"{tenant}", "", true}, // alice has no tenant here
		{"{ip}:{tenant}", "", true},
	}
	for _, tt := range tests {
		k, err := parseKeyTemplate(tt.tmpl)
		if err != nil {
			t.Errorf("%s: %v", tt.tmpl, err)
			continue
		}
		key, ok := k.Fill(v)
		if key != tt.key || ok != (tt.key != "") || k.UsesIdentity != tt.usesIdentity {
			t.Errorf("%s: Fill = %q, %v; UsesIdentity %v; want %q, %v", tt.tmpl, key, ok, k.UsesIdentity, tt.key, tt.usesIdentity)
		}
	}
}

func TestPolicyMatches(t *testing.T) {
	s, err := parse([]byte(policies("",
		p("name", "all"),
		p("name", "a-writes", "match", "{routes: [a], methods: [POST, PUT]}"),
		p("name", "a-reads", "match", "{routes: [a], methods: [GET]}"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	all, writes, reads := &s.Policies[0], &s.Policies[1], &s.Policies[2]
	for _, tt := range []struct {
		p             *Policy
		route, method string
		want          bool
	}{
		{all, "a", "GET", true},
		{all, "b", "DELETE", true},
		{writes, "a", "POST", true},
		{writes, "a", "PUT", true},
		{writes, "a", "GET", false},
		{writes, "a", "post", false},
		{writes, "b", "POST", false},
		{writes, "a", "HEAD", false},
		// HEAD is GET without the body (D1, ADR-0030).
		{reads, "a", "GET", true},
		{reads, "a", "HEAD", true},
		{reads, "a", "OPTIONS", false},
	} {
		if got := tt.p.Matches(tt.route, tt.method); got != tt.want {
			t.Errorf("%s.Matches(%s, %s) = %v, want %v", tt.p.Name, tt.route, tt.method, got, tt.want)
		}
	}
}
