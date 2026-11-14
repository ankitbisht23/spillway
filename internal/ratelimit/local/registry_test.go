package local

import (
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

func compile(t *testing.T, policies string) *config.Snapshot {
	t.Helper()
	c, err := config.Load([]byte(`
upstreams: [{name: u, endpoints: [{address: 'u:80'}]}]
routes: [{name: r, upstream: u, match: {path_prefix: /}}]
` + policies))
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func policy(name, algorithm, limits string) string {
	return "  - {name: " + name + ", level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: " + algorithm + ", limits: " + limits + ", local: true}\n"
}

// TestRegistryApply: IDs that remain keep their state and take the new rate
// and burst; new IDs start empty; removed ones are dropped, and one added
// back starts empty (GW-29).
func TestRegistryApply(t *testing.T) {
	clock := ratelimit.NewFakeClock(base)
	r := NewRegistry(clock)
	key := ratelimit.Key("k")
	gcra := ID{"g", config.GCRA, time.Second, false}
	window := ID{"w", config.FixedWindow, time.Minute, false}
	inflight := ID{"c", config.Concurrency, 0, false}

	r.Apply(compile(t, "policies:\n"+
		policy("g", "gcra", "[{rate: 10, period: 1s}]")+
		policy("w", "fixed_window", "[{rate: 5, period: 1m}]")+
		policy("c", "concurrency", "[{rate: 4}]")))
	take(r.Limiter(gcra), key, 4)
	take(r.Limiter(window), key, 2)
	c := r.Limiter(inflight).(*Concurrency)
	take(c, key, 3)

	// Rate, burst and max change; state stays.
	r.Apply(compile(t, "policies:\n"+
		policy("g", "gcra", "[{rate: 20, period: 1s, burst: 20}]")+
		policy("w", "fixed_window", "[{rate: 6, period: 1m}]")+
		policy("c", "concurrency", "[{rate: 8}]")))
	if d := take(r.Limiter(gcra), key, 0); d.Remaining != 12 { // TAT 400 ms; τ 1 s at T 50 ms
		t.Errorf("gcra after a rate change: %d left, want 12", d.Remaining)
	}
	if d := take(r.Limiter(window), key, 0); d.Remaining != 4 {
		t.Errorf("fixed window after a rate change: %d left, want 4", d.Remaining)
	}
	if d := take(r.Limiter(inflight), key, 0); d.Remaining != 5 {
		t.Errorf("concurrency after a max change: %d free, want 5", d.Remaining)
	}

	// A rate or max below the count leaves nothing, never less (ADR-0018:
	// RateLimit's r is never negative).
	r.Apply(compile(t, "policies:\n"+
		policy("g", "gcra", "[{rate: 20, period: 1s, burst: 20}]")+
		policy("w", "fixed_window", "[{rate: 1, period: 1m}]")+
		policy("c", "concurrency", "[{rate: 2}]")))
	if d := take(r.Limiter(window), key, 1); d.Allowed || d.Remaining != 0 {
		t.Errorf("fixed window after the rate fell below its count: %+v, want a deny with 0 left", d)
	}
	if d := take(r.Limiter(inflight), key, 1); d.Allowed || d.Remaining != 0 {
		t.Errorf("concurrency after the max fell below its count: %+v, want a deny with 0 free", d)
	}

	// A new period or algorithm is a new ID; the old ones go.
	r.Apply(compile(t, "policies:\n"+
		policy("g", "gcra", "[{rate: 20, period: 2s}]")+
		policy("w", "sliding_window", "[{rate: 5, period: 1m}]")))
	for _, id := range []ID{gcra, window, inflight} {
		if r.Limiter(id) != nil {
			t.Errorf("%v kept after its policy changed or went", id)
		}
	}
	if d := take(r.Limiter(ID{"g", config.GCRA, 2 * time.Second, false}), key, 0); d.Remaining != 20 {
		t.Errorf("new period: %d left, want a full 20", d.Remaining)
	}

	// A removed concurrency limiter still takes its requests' releases,
	// never below 0; added back, it starts empty.
	c.Release(key, 3)
	c.Release(key, 3)
	if d := take(c, key, 0); d.Remaining != 4 {
		t.Errorf("removed limiter after releases: %d free, want all 4", d.Remaining)
	}
	take(c, key, 2)
	r.Apply(compile(t, "policies:\n"+policy("c", "concurrency", "[{rate: 4}]")))
	if d := take(r.Limiter(inflight), key, 0); d.Remaining != 4 {
		t.Errorf("concurrency added back: %d free, want 4", d.Remaining)
	}

	// A policy that turns into a Redis policy loses its local limiter
	// (ADR-0021).
	r.Apply(compile(t, "policies:\n  - {name: g, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 20, period: 2s}]}\n"))
	if r.Limiter(ID{"g", config.GCRA, 2 * time.Second, false}) != nil {
		t.Error("a Redis policy has a local limiter")
	}
}

// TestRegistryDegraded: a Redis policy with failure_mode degraded has an
// in-memory limiter per limit at limit ÷ replicas, rounded down, minimum 1,
// under its own ID, whose state survives reloads; other Redis policies have
// none (RL-23, RL-31, ADR-0021).
func TestRegistryDegraded(t *testing.T) {
	r := NewRegistry(ratelimit.NewFakeClock(base))
	key := ratelimit.Key("k")
	redisPolicies := func(replicas string) string {
		return "ratelimit: {replicas: " + replicas + "}\npolicies:\n" +
			"  - {name: d, level: tenant, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 100, period: 1s, burst: 200}, {rate: 10000, period: 1h, burst: 500}], failure_mode: degraded}\n" +
			"  - {name: w, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: sliding_window, limits: [{rate: 6000, period: 1m}], failure_mode: degraded}\n" +
			"  - {name: o, level: global, match: {routes: ['*']}, key_template: '{ip}', algorithm: gcra, limits: [{rate: 10, period: 1s}]}\n"
	}
	r.Apply(compile(t, redisPolicies("3")))
	second := ID{"d", config.GCRA, time.Second, true}
	hour := ID{"d", config.GCRA, time.Hour, true}
	window := ID{"w", config.SlidingWindow, time.Minute, true}
	for id, want := range map[ID]int64{second: 66, hour: 166, window: 2000} {
		if d := take(r.Limiter(id), key, 0); d.Remaining != want {
			t.Errorf("%v: %d left, want %d", id, d.Remaining, want)
		}
	}
	for _, id := range []ID{{"d", config.GCRA, time.Second, false}, {"o", config.GCRA, time.Second, true}, {"o", config.GCRA, time.Second, false}} {
		if r.Limiter(id) != nil {
			t.Errorf("%v has a limiter", id)
		}
	}

	take(r.Limiter(hour), key, 100)
	r.Apply(compile(t, redisPolicies("2")))
	// The TAT stays: 100 taken at 3333/h is 108 s of the 180 s τ that
	// 5000/h with burst 250 has.
	if d := take(r.Limiter(hour), key, 0); d.Remaining != 99 {
		t.Errorf("after a reload to 2 replicas: %d left, want 99", d.Remaining)
	}
}

// TestRegistrySweep: a sweep drops idle keys of every limiter.
func TestRegistrySweep(t *testing.T) {
	clock := ratelimit.NewFakeClock(base)
	r := NewRegistry(clock)
	r.Apply(compile(t, "policies:\n"+
		policy("g", "gcra", "[{rate: 10, period: 1s}]")+
		policy("s", "sliding_window", "[{rate: 10, period: 1s}]")+
		policy("c", "concurrency", "[{rate: 4}]")))
	g := r.Limiter(ID{"g", config.GCRA, time.Second, false}).(*GCRA)
	sw := r.Limiter(ID{"s", config.SlidingWindow, time.Second, false}).(*SlidingWindow)
	take(g, ratelimit.Key("a"), 1)
	take(sw, ratelimit.Key("a"), 1)
	take(r.Limiter(ID{"c", config.Concurrency, 0, false}), ratelimit.Key("a"), 1)
	clock.Advance(2 * time.Second)
	r.sweep()
	if g.state.len() != 0 || sw.state.len() != 0 {
		t.Errorf("after a sweep: %d gcra and %d sliding-window entries, want none", g.state.len(), sw.state.len())
	}
	if n := r.Limiter(ID{"c", config.Concurrency, 0, false}).(*Concurrency).state.len(); n != 1 {
		t.Errorf("a sweep dropped an in-flight count")
	}
}

// TestRegistryPlansAndOverrides: limiters keep one period's state and take
// each call's limit (ForLimit); a plan's periods have limiters from Apply,
// an override's from For, which keeps them across reloads while the policy
// stays (RL-08, GW-29, ADR-0041); managed policies are named by ID.
func TestRegistryPlansAndOverrides(t *testing.T) {
	clock := ratelimit.NewFakeClock(base)
	r := NewRegistry(clock)
	key := ratelimit.Key("k")
	managed := func(planPeriod string) *config.Snapshot {
		t.Helper()
		c, err := config.Load([]byte(`
upstreams: [{name: u, endpoints: [{address: 'u:80'}]}]
routes: [{name: r, upstream: u, match: {path_prefix: /}}]
policies:
  - {id: 7, name: per-consumer, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, limits: [{rate: 10, period: 1s}], local: true}
plans: [{id: 9, name: pro, default_limits: [{policy: per-consumer, limits: [{rate: 100, period: ` + planPeriod + `, burst: 100}]}]}]
`))
		if err != nil {
			t.Fatal(err)
		}
		s, err := config.CompileManaged(c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	r.Apply(managed("1m"))
	second := ID{"7", config.GCRA, time.Second, false}
	minute := ID{"7", config.GCRA, time.Minute, false}
	if r.Limiter(second) == nil || r.Limiter(minute) == nil || r.Limiter(ID{"per-consumer", config.GCRA, time.Second, false}) != nil {
		t.Fatal("want limiters for the policy's and the plan's periods, named by the policy's ID")
	}

	// ForLimit: the base limit is the limiter itself; another limit sees
	// the same state.
	own := config.Limit{Rate: 10, Period: time.Second, Burst: 10}
	if ForLimit(r.Limiter(second), own) != r.Limiter(second) {
		t.Error("ForLimit with the limiter's own limit made a copy")
	}
	take(r.For(second, own), key, 5)
	if d := take(r.For(second, config.Limit{Rate: 20, Period: time.Second, Burst: 20}), key, 0); d.Remaining != 10 { // TAT 500 ms of 1 s: 10 more at 50 ms
		t.Errorf("at 20/s after 5 at 10/s: %d left, want 10", d.Remaining)
	}
	for _, tt := range []struct {
		id  ID
		alg string
	}{{ID{"w", config.FixedWindow, time.Minute, false}, "fixed_window"}, {ID{"s", config.SlidingWindow, time.Minute, false}, "sliding_window"}} {
		r.Apply(compile(t, "policies:\n"+policy(tt.id.Policy, tt.alg, "[{rate: 5, period: 1m}]")))
		take(r.Limiter(tt.id), key, 4)
		if d := take(r.For(tt.id, config.Limit{Rate: 10, Period: time.Minute}), key, 0); d.Remaining != 6 {
			t.Errorf("%s at 10 after 4 at 5: %d left, want 6", tt.alg, d.Remaining)
		}
	}
	r.Apply(compile(t, "policies:\n"+policy("c", "concurrency", "[{rate: 2}]")))
	inflight := ID{"c", config.Concurrency, 0, false}
	take(r.Limiter(inflight), key, 2)
	wide := r.For(inflight, config.Limit{Rate: 5})
	if d := take(wide, key, 1); !d.Allowed || d.Remaining != 2 {
		t.Errorf("concurrency at 5 with 2 in flight: %+v, want allowed with 2 free", d)
	}
	wide.(*Concurrency).Release(key, 3)
	if d := take(r.Limiter(inflight), key, 0); d.Remaining != 2 {
		t.Errorf("after the releases: %d free, want 2", d.Remaining)
	}

	// For adds a limiter for an override's period while the policy is
	// there, keeps it across reloads, and drops it with the policy.
	r.Apply(managed("1m"))
	hour := ID{"7", config.GCRA, time.Hour, false}
	if r.Limiter(hour) != nil {
		t.Fatal("a limiter for a period nothing declares")
	}
	perHour := config.Limit{Rate: 60, Period: time.Hour, Burst: 60}
	take(r.For(hour, perHour), key, 30)
	r.Apply(managed("2m")) // a publish: the plan's period changes
	if d := take(r.For(hour, perHour), key, 0); d.Remaining != 30 {
		t.Errorf("override period after a reload: %d left, want 30 (state kept)", d.Remaining)
	}
	if r.Limiter(minute) != nil || r.Limiter(ID{"7", config.GCRA, 2 * time.Minute, false}) == nil {
		t.Error("the plan's old period kept or its new one missing")
	}
	if r.For(ID{"8", config.GCRA, time.Hour, false}, perHour) != nil || r.For(ID{"7", config.SlidingWindow, time.Hour, false}, perHour) != nil {
		t.Error("For made a limiter for a policy the snapshot does not have")
	}
	r.Apply(compile(t, "policies:\n"+policy("g", "gcra", "[{rate: 10, period: 1s}]")))
	if r.Limiter(hour) != nil || r.For(hour, perHour) != nil {
		t.Error("an override's limiter outlived its policy")
	}
}
