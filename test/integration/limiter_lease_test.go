//go:build integration

package integration

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// Lease group script tests (RL-18, ADR-0069, its Tests section): a group
// with lease_k = k > 0 is decided for the cost as a strict one, and an
// allowed call writes its grant g = min(k, available on each of its limits).
// The limits use periods of a minute or more, so no test sees a unit decay
// back while it runs.

func leased(l redis.Limit, k int64, group int) redis.Limit {
	l.LeaseK, l.Group = k, group
	return l
}

// TestScriptLeaseGrant: a GCRA and a sliding-window lease group are granted
// min(k, available) until nothing is left, then denied with nothing written
// and the cost's retry-after, as a strict check of the cost.
func TestScriptLeaseGrant(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	awayFromEdge(time.Hour)
	p := scriptName("lease")
	g := leased(gcraLimit(redis.TenantKey(p, "g", time.Hour, redis.Digest("a")), "tenant", true, 100, time.Hour, 25), 10, 1)
	w := leased(windowLimit(redis.TenantKey(p, "w", time.Hour, redis.Digest("a")), "tenant", true, 25, time.Hour), 10, 1)
	for _, l := range []redis.Limit{g, w} {
		strict := l
		strict.LeaseK = 0
		for i, want := range []struct {
			granted, remaining int64
		}{{10, 15}, {10, 5}, {5, 0}} {
			res := scriptCall(t, c, 2, l)
			if !res.Allowed || res.Granted[0] != want.granted || res.Limits[0].Remaining != want.remaining {
				t.Errorf("%s grant %d: allowed %v, granted %d, remaining %d; want %d, %d", l.Algorithm, i+1, res.Allowed, res.Granted[0], res.Limits[0].Remaining, want.granted, want.remaining)
			}
		}
		before := state(t, rdb, l)
		res := scriptCall(t, c, 2, l)
		want := scriptCall(t, c, 2, strict) // the same deny, strictly, a moment later
		if res.Allowed || res.Granted[0] != 0 || res.DenyingLimit != 0 || want.Allowed || res.RetryAfter < want.RetryAfter || res.RetryAfter > want.RetryAfter+time.Second {
			t.Errorf("%s with nothing left: %+v; want a deny like the strict %+v", l.Algorithm, res, want)
		}
		if got := state(t, rdb, l); got != before {
			t.Errorf("%s: a denied lease wrote %s, was %s", l.Algorithm, got, before)
		}
		checkTTL(t, rdb, l)
	}
	if got := getHash(t, rdb, w.Key); got["cur"] != "25" {
		t.Errorf("sliding window after grants of 10, 10 and 5: %v", got)
	}
}

// state is a limit's stored state as one string.
func state(t *testing.T, rdb *goredis.ClusterClient, l redis.Limit) string {
	t.Helper()
	if l.Algorithm == "gcra" {
		return getKey(t, rdb, l.Key)
	}
	h := getHash(t, rdb, l.Key)
	return h["w"] + "/" + h["cur"] + "/" + h["prev"]
}

// TestScriptLeaseMultiWindow: a policy's limits form one group, granted the
// least available on any of them, and each is written with the grant.
func TestScriptLeaseMultiWindow(t *testing.T) {
	_, c, _ := scriptEnv(t)
	p := scriptName("lease-multi")
	hour := leased(gcraLimit(redis.TenantKey(p, "pol", time.Hour, redis.Digest("a")), "tenant", true, 100, time.Hour, 25), 10, 1)
	day := leased(gcraLimit(redis.TenantKey(p, "pol", 24*time.Hour, redis.Digest("a")), "tenant", true, 12, 24*time.Hour, 12), 10, 1)
	for i, want := range []int64{10, 2} {
		res := scriptCall(t, c, 1, hour, day)
		if !res.Allowed || !slices.Equal(res.Granted, []int64{want, want}) {
			t.Errorf("grant %d: allowed %v, granted %v; want %d on both", i+1, res.Allowed, res.Granted, want)
		}
	}
	if res := scriptCall(t, c, 1, hour, day); res.Allowed || res.DenyingLimit != 1 || !slices.Equal(res.Granted, []int64{0, 0}) {
		t.Errorf("third call: %+v; want the day limit to deny, nothing written", res)
	}
}

// TestScriptLeaseMixed: a lease group beside a strict enforce group and a
// shadow limit in one call. Allowed, the lease group writes its grant, the
// strict limits the cost; the shadow write rule is unchanged (written only
// if it admits alone). When the strict group denies, nothing is written.
func TestScriptLeaseMixed(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	p := scriptName("lease-mixed")
	lease := leased(gcraLimit(redis.TenantKey(p, "lease", time.Hour, redis.Digest("a")), "tenant", true, 100, time.Hour, 50), 10, 1)
	strict := gcraLimit(redis.TenantKey(p, "strict", time.Hour, redis.Digest("a")), "consumer", true, 100, time.Hour, 4)
	strict.Group = 2
	shadow := gcraLimit(redis.TenantKey(p, "shadow", time.Hour, redis.Digest("a")), "route", false, 100, time.Hour, 3)
	shadow.Group = 3

	res := scriptCall(t, c, 2, lease, strict, shadow)
	if !res.Allowed || !slices.Equal(res.Granted, []int64{10, 2, 2}) || res.Limits[0].Remaining != 40 || res.Limits[1].Remaining != 2 {
		t.Errorf("first call: %+v; want granted 10, 2, 2", res)
	}
	// The shadow limit would deny a cost of 2 now: allowed, not written.
	res = scriptCall(t, c, 2, lease, strict, shadow)
	if !res.Allowed || !slices.Equal(res.Granted, []int64{10, 2, 0}) || res.ShadowWouldDeny != 0b100 {
		t.Errorf("second call: %+v; want granted 10, 2, 0 and the shadow would-deny", res)
	}
	before := getKey(t, rdb, lease.Key)
	res = scriptCall(t, c, 2, lease, strict, shadow)
	if res.Allowed || res.DenyingLimit != 1 || !slices.Equal(res.Granted, []int64{0, 0, 0}) || !res.Limits[0].Allowed {
		t.Errorf("third call: %+v; want the strict limit to deny and nothing written", res)
	}
	if got := getKey(t, rdb, lease.Key); got != before {
		t.Errorf("a denied call moved the lease group's TAT from %s to %s", before, got)
	}

	// A refund names only strict limits (the gateway's lease keeps a
	// grant); the client sends lease_k 0 for it whatever the limit says.
	tat := getKey(t, rdb, lease.Key)
	scriptRefund(t, c, 2, 0, strict)
	if got := getKey(t, rdb, lease.Key); got != tat {
		t.Errorf("refunding the strict limit moved the lease group's TAT from %s to %s", tat, got)
	}
}

// TestScriptLeaseRejects: the script's argument rules for lease_k and group.
func TestScriptLeaseRejects(t *testing.T) {
	_, _, rdb := scriptEnv(t)
	src, err := os.ReadFile("../../internal/ratelimit/redis/limiter_v1.lua")
	if err != nil {
		t.Fatal(err)
	}
	p := scriptName("lease-rejects")
	keys := []string{redis.GlobalKey(p, time.Hour, redis.Digest("a")), redis.GlobalKey(p, 2*time.Hour, redis.Digest("a"))}
	limit := func(mode string, k, group int) []any {
		return []any{"global", mode, "gcra", 100, 3600000, 50, k, group}
	}
	args := func(cost int, limits ...[]any) []any {
		a := []any{cost}
		for _, l := range limits {
			a = append(a, l...)
		}
		return a
	}
	for _, tt := range []struct {
		name string
		keys int
		args []any
		want string
	}{
		{"shadow lease", 1, args(1, limit("shadow", 5, 1)), "lease_k > 0 needs an enforce limit"},
		{"cost above k", 1, args(6, limit("enforce", 5, 1)), "lease_k > 0 needs an enforce limit, a cost of at most lease_k"},
		{"refund of a lease", 1, append(args(-1, limit("enforce", 5, 1)), 0), "and no refund"},
		{"negative k", 1, args(1, limit("enforce", -1, 1)), "lease_k must be an integer >= 0"},
		{"first group not 1", 1, args(1, limit("enforce", 0, 2)), "group must be 1"},
		{"group skipped", 2, args(1, limit("enforce", 0, 1), limit("enforce", 0, 3)), "limit 2: group must be"},
		{"group going back", 2, args(1, limit("enforce", 0, 1), limit("enforce", 0, 0)), "limit 2: group must be"},
		{"k differs in a group", 2, args(1, limit("enforce", 5, 1), limit("enforce", 6, 1)), "lease_k differs within group 1"},
		{"six arguments per key", 1, []any{1, "global", "enforce", "gcra", 100, 3600000, 50}, "want 1 to 32 keys and 9 arguments"},
	} {
		err := rdb.Eval(context.Background(), string(src), keys[:tt.keys], tt.args...).Err()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
	if got := getKey(t, rdb, keys[0]); got != "<missing>" {
		t.Errorf("a rejected call wrote %s", got)
	}
}

// TestScriptOldAndNewSHA: the pre-lease script (M6's text, another SHA) and
// the current one share keys and state during a rollout (RL-29, ADR-0069):
// each continues from what the other wrote.
func TestScriptOldAndNewSHA(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	ctx := context.Background()
	src, err := os.ReadFile("testdata/limiter_v1_m6.lua")
	if err != nil {
		t.Fatal(err)
	}
	old := goredis.NewScript(string(src))
	p := scriptName("old-new")
	g := gcraLimit(redis.TenantKey(p, "g", time.Hour, redis.Digest("a")), "tenant", true, 100, time.Hour, 20)
	w := windowLimit(redis.TenantKey(p, "w", time.Hour, redis.Digest("a")), "tenant", true, 20, time.Hour)
	oldCall := func(cost int64, l redis.Limit) []any {
		t.Helper()
		burst := l.Burst
		if l.Algorithm != "gcra" {
			burst = 0
		}
		v, err := old.Run(ctx, rdb, []string{l.Key}, cost, l.Level, "enforce", l.Algorithm, l.Rate, l.Period.Milliseconds(), burst).Slice()
		if err != nil {
			t.Fatalf("old script: %v", err)
		}
		return v
	}
	awayFromEdge(time.Hour)
	for _, l := range []redis.Limit{g, w} {
		if v := oldCall(3, l); v[0] != int64(1) || v[3] != int64(17) {
			t.Fatalf("%s: old script's first call %v", l.Algorithm, v)
		}
		// The new script, batched, sees the old one's 3 units.
		if res := scriptCall(t, c, 1, leased(l, 10, 1)); !res.Allowed || res.Granted[0] != 10 || res.Limits[0].Remaining != 7 {
			t.Errorf("%s: new script after the old one: %+v", l.Algorithm, res)
		}
		// And the old one sees the new one's grant.
		if v := oldCall(1, l); v[0] != int64(1) || v[3] != int64(6) {
			t.Errorf("%s: old script after the new one: %v", l.Algorithm, v)
		}
		if l.Algorithm != "gcra" {
			if got := getHash(t, rdb, l.Key)["cur"]; got != strconv.Itoa(14) {
				t.Errorf("window cur %s, want 14", got)
			}
		}
	}
}
