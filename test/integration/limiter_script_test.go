//go:build integration

package integration

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: EVALSHA names scripts by SHA-1
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// Script tests (docs/design.md Testing "Script", ADR-0021): the limiter
// script on the compose Redis Cluster, from the host through the address map
// (ADR-0004). Every test uses fresh policy names, so runs never share state.

// scriptEnv returns the address map, a limiter client and a plain cluster
// client for looking at the keys.
func scriptEnv(t *testing.T) (map[string]string, *redis.Client, *goredis.ClusterClient) {
	t.Helper()
	addrMap, err := redis.ParseAddrMap(os.Getenv(redis.AddrMapEnv))
	if err != nil {
		t.Fatal(err)
	}
	if addrMap == nil {
		t.Fatalf("%s is not set; run make test-integration", redis.AddrMapEnv)
	}
	c := redis.NewClient([]string{"redis-0:6379"}, addrMap, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	rdb := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{"redis-0:6379"}, Dialer: redis.Dialer(addrMap)})
	t.Cleanup(func() { _ = c.Close(); _ = rdb.Close() })
	return addrMap, c, rdb
}

// scriptName returns a policy name unique to this test run.
func scriptName(s string) string {
	return "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + s
}

func scriptCall(t *testing.T, c *redis.Client, cost int64, limits ...redis.Limit) redis.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.Call(ctx, cost, limits)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return res
}

func scriptRefund(t *testing.T, c *redis.Client, cost, admittedUS int64, limits ...redis.Limit) redis.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.Refund(ctx, cost, admittedUS, limits)
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	return res
}

func gcraLimit(key, level string, enforce bool, rate int64, period time.Duration, burst int64) redis.Limit {
	return redis.Limit{Key: key, Level: level, Enforce: enforce, Algorithm: config.GCRA, Rate: rate, Period: period, Burst: burst}
}

func windowLimit(key, level string, enforce bool, rate int64, period time.Duration) redis.Limit {
	return redis.Limit{Key: key, Level: level, Enforce: enforce, Algorithm: config.SlidingWindow, Rate: rate, Period: period}
}

// gcraModel is the script's GCRA in Go: T = ⌈period/rate⌉ µs, τ = T·burst.
type gcraModel struct{ t, tau, tat int64 }

func newGCRAModel(l redis.Limit) *gcraModel {
	t := (l.Period.Microseconds() + l.Rate - 1) / l.Rate
	return &gcraModel{t: t, tau: t * l.Burst}
}

// admits reports whether the limit alone admits cost at now.
func (m *gcraModel) admits(now, cost int64) bool {
	return max(m.tat, now)+cost*m.t-now <= m.tau
}

// decide returns the script's result for this limit at now, writing the new
// TAT if write is set and the limit admits.
func (m *gcraModel) decide(now, cost int64, write bool) ratelimit.Decision {
	s := max(m.tat, now)
	next := s + cost*m.t
	d := ratelimit.Decision{Allowed: next-now <= m.tau}
	switch {
	case !d.Allowed:
		d.RetryAfter = msUp(next - m.tau - now)
	case write:
		m.tat, s = next, next
	}
	d.Remaining = max(0, (m.tau-(s-now))/m.t)
	d.ResetAfter = msUp(s - now)
	return d
}

func msUp(us int64) time.Duration { return time.Duration((us+999)/1000) * time.Millisecond }

// awayFromEdge waits, if the current window of period ends within 2 s, until
// the next one starts, so that a test's next few calls share a window.
func awayFromEdge(period time.Duration) {
	if left := period - time.Duration(time.Now().UnixNano()%int64(period)); left < 2*time.Second {
		time.Sleep(left + 50*time.Millisecond)
	}
}

func getKey(t *testing.T, rdb *goredis.ClusterClient, key string) string {
	t.Helper()
	v, err := rdb.Get(context.Background(), key).Result()
	if errors.Is(err, goredis.Nil) {
		return "<missing>"
	}
	if err != nil {
		t.Fatalf("GET %s: %v", key, err)
	}
	return v
}

func getHash(t *testing.T, rdb *goredis.ClusterClient, key string) map[string]string {
	t.Helper()
	v, err := rdb.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("HGETALL %s: %v", key, err)
	}
	return v
}

// checkTTL checks that every limit's key has a TTL in (max − 5 s, max]: the
// period plus the burst time for GCRA, two periods for a sliding window
// (RL-15).
func checkTTL(t *testing.T, rdb *goredis.ClusterClient, limits ...redis.Limit) {
	t.Helper()
	for _, l := range limits {
		want := 2 * l.Period
		if l.Algorithm == config.GCRA {
			want = l.Period + msUp(newGCRAModel(l).tau)
		}
		got, err := rdb.PTTL(context.Background(), l.Key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if got <= 0 || got > want || got < want-5*time.Second {
			t.Errorf("PTTL %s = %v, want (%v, %v]", l.Key, got, want-5*time.Second, want)
		}
	}
}

// TestScriptTime checks that decisions come from Redis TIME (RL-12) and
// follow the GCRA maths for those times (ADR-0021, ADR-0023).
func TestScriptTime(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	l := gcraLimit(redis.GlobalKey(scriptName("time"), time.Minute, redis.Digest("k")), "global", true, 7, time.Minute, 3) // T = 8,571,428.6 µs, rounded up (ADR-0023)
	m := newGCRAModel(l)
	var last int64
	for i := range 5 {
		res := scriptCall(t, c, 1, l)
		if res.NowUS <= last {
			t.Fatalf("call %d: now_us %d after %d", i, res.NowUS, last)
		}
		last = res.NowUS
		want := m.decide(res.NowUS, 1, true)
		if res.Limits[0] != want || res.Allowed != want.Allowed {
			t.Errorf("call %d at %d: %+v, want %+v", i, res.NowUS, res.Limits[0], want)
		}
		if got := getKey(t, rdb, l.Key); got != strconv.FormatInt(m.tat, 10) {
			t.Errorf("call %d: stored TAT %s, want %d", i, got, m.tat)
		}
	}
	if skew := time.Since(time.UnixMicro(last)).Abs(); skew > 5*time.Second {
		t.Errorf("Redis TIME is %v away from the host clock", skew)
	}
}

// TestScriptSlots checks that every key of a call shares one slot and that
// {g:} keys spread global policies across slots and primaries (RL-13).
func TestScriptSlots(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	ctx := context.Background()
	slot := func(key string) int64 {
		s, err := rdb.ClusterKeySlot(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	tenant := scriptName("tenant")
	limits := []redis.Limit{
		gcraLimit(redis.TenantKey(tenant, "tenant-default", time.Second, redis.Digest(tenant)), "tenant", true, 100, time.Second, 200),
		gcraLimit(redis.TenantKey(tenant, "tenant-default", time.Hour, redis.Digest(tenant)), "tenant", true, 10000, time.Hour, 500),
		windowLimit(redis.TenantKey(tenant, "consumer-minute", time.Minute, redis.Digest("alice")), "consumer", true, 1000, time.Minute),
		gcraLimit(redis.TenantKey(tenant, "route-x", time.Second, redis.Digest("x:1.2.3.4")), "route", false, 5, time.Second, 5),
	}
	digest := redis.Digest(scriptName("anon"))
	anon := []redis.Limit{
		windowLimit(redis.NoTenantKey(digest, "consumer-minute", time.Minute), "consumer", true, 1000, time.Minute),
		gcraLimit(redis.NoTenantKey(digest, "route-x", time.Second), "route", true, 5, time.Second, 5),
	}
	for _, call1 := range [][]redis.Limit{limits, anon} {
		for _, l := range call1[1:] {
			if slot(l.Key) != slot(call1[0].Key) {
				t.Errorf("%s and %s are in different slots", l.Key, call1[0].Key)
			}
		}
		if res := scriptCall(t, c, 1, call1...); !res.Allowed {
			t.Errorf("call %v denied: %+v", call1, res)
		}
	}

	slots, primaries := map[int64]bool{}, map[string]bool{}
	for i := range 10 {
		l := gcraLimit(redis.GlobalKey("global-"+strconv.Itoa(i), time.Second, redis.Digest(tenant)), "global", true, 100, time.Second, 100)
		slots[slot(l.Key)] = true
		node, err := rdb.MasterForKey(ctx, l.Key)
		if err != nil {
			t.Fatal(err)
		}
		primaries[node.Options().Addr] = true
		scriptCall(t, c, 1, l)
	}
	t.Logf("measure: 10 global policies over %d slots on %d primaries", len(slots), len(primaries))
	if len(slots) != 10 || len(primaries) < 2 {
		t.Errorf("10 global policies use %d slots on %d primaries; want 10 slots on 2 or more", len(slots), len(primaries))
	}
}

// TestScriptWriteRule checks the design's write rule: a denied call writes
// nothing; an allowed one writes every enforce limit and each shadow limit
// that admits alone.
func TestScriptWriteRule(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	awayFromEdge(time.Minute)
	p := scriptName("write")
	roomy := gcraLimit(redis.TenantKey(p, "roomy", time.Second, redis.Digest("a")), "tenant", true, 100, time.Second, 100)
	tight := gcraLimit(redis.TenantKey(p, "tight", time.Minute, redis.Digest("a")), "consumer", true, 1, time.Minute, 1)
	scriptCall(t, c, 1, tight)
	before := getKey(t, rdb, tight.Key)

	res := scriptCall(t, c, 1, roomy, tight)
	if res.Allowed || res.DenyingLimit != 1 || res.DenyingLevel != "consumer" || res.RetryAfter <= 0 || res.ShadowWouldDeny != 0 {
		t.Errorf("denied call = %+v", res)
	}
	if !res.Limits[0].Allowed || res.Limits[1].Allowed || res.Limits[0].Remaining != 100 {
		t.Errorf("per-limit results %+v", res.Limits)
	}
	if got := getKey(t, rdb, roomy.Key); got != "<missing>" {
		t.Errorf("a denied call wrote %s = %s", roomy.Key, got)
	}
	if got := getKey(t, rdb, tight.Key); got != before {
		t.Errorf("a denied call changed %s from %s to %s", tight.Key, before, got)
	}

	// The same tight limit as shadow does not deny, and is not written.
	shadow := tight
	shadow.Enforce = false
	fresh := windowLimit(redis.TenantKey(p, "fresh", time.Minute, redis.Digest("a")), "route", false, 10, time.Minute)
	res = scriptCall(t, c, 1, roomy, shadow, fresh)
	if !res.Allowed || res.ShadowWouldDeny != 0b010 || res.DenyingLimit != -1 || res.Remaining != 99 {
		t.Errorf("call with a shadow would-deny = %+v", res)
	}
	if got := getKey(t, rdb, tight.Key); got != before {
		t.Errorf("a shadow would-deny was written: %s from %s to %s", tight.Key, before, got)
	}
	if got := getKey(t, rdb, roomy.Key); got == "<missing>" {
		t.Error("the enforce limit was not written")
	}
	if got := getHash(t, rdb, fresh.Key); got["cur"] != "1" {
		t.Errorf("the shadow limit that admits was not written: %v", got)
	}
	checkTTL(t, rdb, roomy, tight, fresh)

	// A call with only shadow limits summarises limit 1.
	res = scriptCall(t, c, 1, shadow, fresh)
	if !res.Allowed || res.ShadowWouldDeny != 0b01 || res.Remaining != 0 {
		t.Errorf("shadow-only call = %+v", res)
	}
}

// TestScriptMultiWindowAndCost checks several windows of one policy in one
// call (RL-06) with a cost above 1 (RL-07).
func TestScriptMultiWindowAndCost(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	awayFromEdge(time.Minute)
	p := scriptName("multi")
	sec := gcraLimit(redis.TenantKey(p, "tenant-default", time.Second, redis.Digest(p)), "tenant", true, 10, time.Second, 10)
	minute := gcraLimit(redis.TenantKey(p, "tenant-default", time.Minute, redis.Digest(p)), "consumer", true, 12, time.Minute, 12)
	modelS, modelM := newGCRAModel(sec), newGCRAModel(minute)
	for i := range 3 {
		res := scriptCall(t, c, 5, sec, minute)
		both := modelS.admits(res.NowUS, 5) && modelM.admits(res.NowUS, 5)
		ds, dm := modelS.decide(res.NowUS, 5, both), modelM.decide(res.NowUS, 5, both)
		if res.Limits[0] != ds || res.Limits[1] != dm || res.Allowed != both {
			t.Errorf("call %d: %+v, want limits %+v and %+v", i, res, ds, dm)
		}
		checkTTL(t, rdb, sec, minute)
		if i == 2 {
			// Both deny; the minute limit waits longer, so it is the denying one.
			if res.Allowed || res.DenyingLimit != 1 || res.DenyingLevel != "consumer" || res.RetryAfter != dm.RetryAfter || res.RetryAfter < 14*time.Second {
				t.Errorf("third call = %+v", res)
			}
		} else if !res.Allowed {
			t.Errorf("call %d denied: %+v", i, res)
		}
	}

	// Sliding windows: 10/s and 12/min at cost 5 admit twice, then the minute
	// window denies.
	ws := windowLimit(redis.TenantKey(p, "w", time.Second, redis.Digest(p)), "route", true, 10, time.Second)
	wm := windowLimit(redis.TenantKey(p, "w", time.Minute, redis.Digest(p)), "route", true, 12, time.Minute)
	for i := range 3 {
		res := scriptCall(t, c, 5, ws, wm)
		if res.Allowed != (i < 2) || (i == 2 && res.Limits[1].Allowed) {
			t.Errorf("sliding window call %d: %+v", i, res)
		}
		checkTTL(t, rdb, ws, wm)
	}
	if got := getHash(t, rdb, wm.Key); got["cur"] != "10" {
		t.Errorf("minute window after two admissions of 5: %v", got)
	}
}

// TestScriptSlidingWindow compares sliding-window denials with the maths for
// the TIME the script used (ADR-0021), on state written into the current
// window: retry_after from both cases of ADR-0017's formula.
func TestScriptSlidingWindow(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	ctx := context.Background()
	awayFromEdge(time.Hour)
	const p, rate, cost = int64(3_600_000_000), int64(10), int64(5)
	wNow := time.Now().UnixMicro() / p * p
	for _, tt := range []struct {
		name      string
		prev, cur float64
	}{
		// cur + cost ≤ rate: waits for prev to decay. prev is large enough
		// to deny until 11 ms before the window ends.
		{"cur fits", 1_000_000, 2},
		// cur + cost > rate: waits for the next window.
		{"cur full", 0, 8},
	} {
		l := windowLimit(redis.GlobalKey(scriptName("sw"), time.Hour, redis.Digest(tt.name)), "global", true, rate, time.Hour)
		if err := rdb.HSet(ctx, l.Key, "w", wNow, "cur", tt.cur, "prev", tt.prev).Err(); err != nil {
			t.Fatal(err)
		}
		res := scriptCall(t, c, cost, l)
		if got := res.NowUS / p * p; got != wNow {
			t.Fatalf("%s: the script ran in window %d, not %d", tt.name, got, wNow)
		}
		// The script's arithmetic, in the same order, so the doubles match.
		e := float64(res.NowUS - wNow)
		fp, fr, fc := float64(p), float64(rate), float64(cost)
		r := 2*fp - e - (fr-fc)*fp/tt.cur
		if tt.cur+fc <= fr {
			r = fp - (fr-fc-tt.cur)*fp/tt.prev - e
		}
		want := ratelimit.Decision{
			Remaining:  max(0, int64(math.Floor(fr-(tt.prev*(fp-e)/fp+tt.cur)))),
			ResetAfter: msUp(p - int64(e)),
			RetryAfter: msUp(int64(math.Ceil(r))),
		}
		if res.Allowed || res.Limits[0] != want {
			t.Errorf("%s at e = %.0f µs: %+v, want %+v", tt.name, e, res.Limits[0], want)
		}
	}
}

// TestScriptRefund checks that refunds reverse only the limits they are
// given, never go below empty, and find the admission's sliding window.
func TestScriptRefund(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	p := scriptName("refund")
	a := gcraLimit(redis.GlobalKey(p+"-a", time.Minute, redis.Digest("k")), "global", true, 10, time.Minute, 10)
	b := gcraLimit(redis.GlobalKey(p+"-a", time.Hour, redis.Digest("k")), "global", true, 100, time.Hour, 100)
	scriptCall(t, c, 3, a, b)
	bBefore := getKey(t, rdb, b.Key)
	res := scriptRefund(t, c, 3, 0, a)
	if !res.Allowed || res.Limits[0].Remaining != 10 || res.Limits[0].ResetAfter != 0 {
		t.Errorf("refund = %+v", res)
	}
	if got := getKey(t, rdb, a.Key); got != "<missing>" {
		t.Errorf("a full refund left %s = %s", a.Key, got)
	}
	if got := getKey(t, rdb, b.Key); got != bBefore {
		t.Errorf("a refund of a changed %s from %s to %s", b.Key, bBefore, got)
	}
	scriptRefund(t, c, 3, 0, a) // below empty: stays missing
	if got := getKey(t, rdb, a.Key); got != "<missing>" {
		t.Errorf("a refund of an empty key wrote %s = %s", a.Key, got)
	}

	// A partial refund moves the TAT back by cost·T and keeps the TTL.
	m := newGCRAModel(a)
	scriptCall(t, c, 2, a)
	r2 := scriptCall(t, c, 2, a)
	tat, _ := strconv.ParseInt(getKey(t, rdb, a.Key), 10, 64)
	scriptRefund(t, c, 1, r2.NowUS, a)
	if got := getKey(t, rdb, a.Key); got != strconv.FormatInt(tat-m.t, 10) {
		t.Errorf("TAT after refunding 1 = %s, want %d", got, tat-m.t)
	}
	checkTTL(t, rdb, a)

	// Sliding window: an admission from the previous window is refunded from
	// prev, one from the current window from cur, an older one from nothing.
	w := windowLimit(redis.GlobalKey(p+"-w", time.Second, redis.Digest("k")), "global", true, 10, time.Second)
	first := scriptCall(t, c, 4, w)
	time.Sleep(time.Second - time.Duration(first.NowUS%1_000_000)*time.Microsecond + 50*time.Millisecond)
	second := scriptCall(t, c, 2, w)
	if second.NowUS/1_000_000 != first.NowUS/1_000_000+1 {
		t.Fatalf("second call at %d is not in the window after %d", second.NowUS, first.NowUS)
	}
	if got := getHash(t, rdb, w.Key); got["prev"] != "4" || got["cur"] != "2" {
		t.Fatalf("window state %v, want prev 4, cur 2", got)
	}
	scriptRefund(t, c, 4, first.NowUS, w)
	if got := getHash(t, rdb, w.Key); got["prev"] != "0" || got["cur"] != "2" {
		t.Errorf("after refunding the previous window's 4: %v, want prev 0, cur 2", got)
	}
	scriptRefund(t, c, 2, second.NowUS-5_000_000, w) // five windows ago: nothing
	scriptRefund(t, c, 5, second.NowUS, w)           // more than cur: clamped at 0
	if got := getHash(t, rdb, w.Key); got["prev"] != "0" || got["cur"] != "0" {
		t.Errorf("after refunding an old admission and 5 of cur's 2: %v, want prev 0, cur 0", got)
	}
	checkTTL(t, rdb, w)
}

// TestScriptWrongType checks that a key left with the other algorithm's type
// reads as empty and is replaced.
func TestScriptWrongType(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	ctx := context.Background()
	p := scriptName("type")
	w := windowLimit(redis.GlobalKey(p, time.Second, redis.Digest("k")), "global", true, 10, time.Second)
	g := gcraLimit(redis.GlobalKey(p, time.Minute, redis.Digest("k")), "global", true, 10, time.Minute, 10)
	if err := rdb.Set(ctx, w.Key, "123", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HSet(ctx, g.Key, "w", "1", "cur", "1", "prev", "1").Err(); err != nil {
		t.Fatal(err)
	}
	res := scriptCall(t, c, 1, w, g)
	if !res.Allowed || res.Limits[0].Remaining != 9 || res.Limits[1].Remaining != 9 {
		t.Errorf("call over wrong-typed keys = %+v", res)
	}
	for key, want := range map[string]string{w.Key: "hash", g.Key: "string"} {
		if got, err := rdb.Type(ctx, key).Result(); err != nil || got != want {
			t.Errorf("TYPE %s = %s, %v; want %s", key, got, err, want)
		}
	}
	checkTTL(t, rdb, w, g)
}

// TestScriptClockBehind checks that a sliding window written by a primary
// whose clock was ahead keeps its counts, which errs towards denying (RL-12).
func TestScriptClockBehind(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	w := windowLimit(redis.GlobalKey(scriptName("behind"), time.Second, redis.Digest("k")), "global", true, 10, time.Second)
	ahead := strconv.FormatInt((time.Now().Unix()+5)*1_000_000, 10)
	if err := rdb.HSet(context.Background(), w.Key, "w", ahead, "cur", "10", "prev", "0").Err(); err != nil {
		t.Fatal(err)
	}
	if res := scriptCall(t, c, 1, w); res.Allowed || res.Limits[0].Remaining != 0 {
		t.Errorf("call over a window from the future = %+v, want a deny", res)
	}
}

// TestScriptNoScript checks the EVAL fallback after SCRIPT FLUSH (RL-16).
func TestScriptNoScript(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	ctx := context.Background()
	src, err := os.ReadFile("../../internal/ratelimit/redis/limiter_v1.lua")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(src) //nolint:gosec // G401: as above
	sha := hex.EncodeToString(sum[:])

	l := gcraLimit(redis.GlobalKey(scriptName("noscript"), time.Minute, redis.Digest("k")), "global", true, 10, time.Minute, 10)
	scriptCall(t, c, 1, l)
	node, err := rdb.MasterForKey(ctx, l.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if ok, err := node.ScriptExists(ctx, sha).Result(); err != nil || ok[0] {
		t.Fatalf("SCRIPT EXISTS after SCRIPT FLUSH = %v, %v", ok, err)
	}
	if res := scriptCall(t, c, 1, l); !res.Allowed || res.Limits[0].Remaining != 8 {
		t.Errorf("call after SCRIPT FLUSH = %+v", res)
	}
	if ok, err := node.ScriptExists(ctx, sha).Result(); err != nil || !ok[0] {
		t.Errorf("SCRIPT EXISTS after the EVAL fallback = %v, %v", ok, err)
	}
}

// TestScriptTimeoutAfterExecution injects a timeout after the script ran:
// every node is reached through a proxy that can hold replies back past the
// call's deadline. The call fails as timeout and is never resent, so it
// deducts exactly once (RL-16).
func TestScriptTimeoutAfterExecution(t *testing.T) {
	addrMap, _, rdb := scriptEnv(t)
	var delay atomic.Int64
	proxied := make(map[string]string, len(addrMap))
	for from, to := range addrMap {
		proxied[from] = delayProxy(t, to, &delay)
	}
	c := redis.NewClient([]string{"redis-0:6379"}, proxied, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()

	name := scriptName("timeout")
	l := gcraLimit(redis.GlobalKey(name, time.Hour, redis.Digest("k")), "global", true, 1, time.Hour, 10)
	if res := scriptCall(t, c, 1, l); res.Limits[0].Remaining != 9 {
		t.Fatalf("first call = %+v", res)
	}

	delay.Store(int64(300 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err := c.Call(ctx, 1, []redis.Limit{l})
	cancel()
	var e *redis.Error
	if !errors.As(err, &e) || e.Kind != redis.KindTimeout || e.Node == "" {
		t.Fatalf("delayed call error = %v, want a timeout on a node", err)
	}
	time.Sleep(400 * time.Millisecond) // the held-back reply arrives, and is dropped
	delay.Store(0)
	// The delay held back the health checks' PONGs too, so the
	// node's breaker may be open (ADR-0025). Wait until a PING has closed
	// it, calling with another key in the same slot.
	poll := gcraLimit(redis.GlobalKey(name, time.Hour, redis.Digest("poll")), "global", true, 1000, time.Hour, 1000)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := c.Call(ctx, 1, []redis.Limit{poll})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node not back after the delay: %v", err)
		}
	}

	res := scriptCall(t, c, 1, l)
	t.Logf("measure: remaining after 3 calls with one timed out after execution = %d (want 7)", res.Limits[0].Remaining)
	if res.Limits[0].Remaining != 7 {
		t.Errorf("remaining = %d after three calls, want 7: the timed-out call deducted %d times", res.Limits[0].Remaining, 9-1-res.Limits[0].Remaining)
	}
	checkTTL(t, rdb, l)
}

// delayProxy forwards a local port to addr, holding each reply chunk back
// for the delay in effect when it arrives.
func delayProxy(t *testing.T, addr string, delay *atomic.Int64) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			var d net.Dialer
			server, err := d.DialContext(context.Background(), "tcp", addr)
			if err != nil {
				_ = client.Close()
				continue
			}
			go func() {
				_, _ = io.Copy(server, client)
				_ = server.Close()
			}()
			go func() {
				defer func() { _ = client.Close() }()
				buf := make([]byte, 32<<10)
				for {
					n, err := server.Read(buf)
					if n > 0 {
						time.Sleep(time.Duration(delay.Load()))
						if _, werr := client.Write(buf[:n]); werr != nil {
							_ = server.Close()
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// TestScriptLimiter checks the one-limit ratelimit.Limiter adapter (RL-01).
func TestScriptLimiter(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	awayFromEdge(time.Minute)
	ctx := context.Background()
	p := scriptName("limiter")
	lim := c.Limiter(p, config.SlidingWindow, config.Limit{Rate: 3, Period: time.Minute})
	key := ratelimit.Key("client-1")
	for i := range 4 {
		d, err := lim.Allow(ctx, key, 1)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed != (i < 3) || (i == 3 && d.RetryAfter <= 0) {
			t.Errorf("Allow %d = %+v", i, d)
		}
	}
	d, err := lim.Allow(ctx, key, -1)
	if err != nil || !d.Allowed || d.Remaining != 1 {
		t.Errorf("refund = %+v, %v", d, err)
	}
	if d, err := lim.Allow(ctx, key, 1); err != nil || !d.Allowed || d.Remaining != 0 {
		t.Errorf("Allow after the refund = %+v, %v", d, err)
	}
	if got := getHash(t, rdb, redis.GlobalKey(p, time.Minute, redis.Digest("client-1"))); got["cur"] != "3" {
		t.Errorf("adapter key state %v, want cur 3", got)
	}
	if _, err := lim.Allow(ctx, key, 0); err == nil {
		t.Error("Allow with cost 0 succeeded")
	}
}
