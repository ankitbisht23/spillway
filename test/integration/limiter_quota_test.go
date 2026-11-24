//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/quota"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// Quota limits in the limiter script (RL-10, ADR-0070), on the compose
// Redis Cluster. Each test uses a fresh tenant name, so runs never share
// counters or epochs.

// quotaLimit is consumer's quota of q for period p in tenant's slot.
func quotaLimit(tenant, consumer string, p quota.Period, q int64) redis.Limit {
	return redis.Limit{Key: redis.QuotaKey(tenant, consumer, p.ID), Level: "consumer", Enforce: true, Algorithm: redis.Quota, Rate: q,
		PeriodStart: p.Start, PeriodEnd: p.End, Epoch: redis.QuotaEpochKey(tenant)}
}

// seedEpoch sets tenant's quota epoch, as the flusher does.
func seedEpoch(t *testing.T, rdb *goredis.ClusterClient, tenant string) {
	t.Helper()
	if err := rdb.Set(context.Background(), redis.QuotaEpochKey(tenant), "1-test", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
}

// nearPeriodEdge waits past a UTC midnight that is less than 5 s away, so a
// test's calls share a day.
func nearPeriodEdge() {
	if left := time.Until(quota.Day(time.Now()).End); left < 5*time.Second {
		time.Sleep(left + 100*time.Millisecond)
	}
}

// TestScriptQuota: a quota counts cost units up to the quota and denies
// past it with retry-after to the period's end, writing a counter that
// lives for the rest of the period plus a day; a refund gives cost back
// (RL-10, RL-14, ADR-0070).
func TestScriptQuota(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	nearPeriodEdge()
	tenant := scriptName("quota")
	seedEpoch(t, rdb, tenant)
	day := quota.Day(time.Now())
	l := quotaLimit(tenant, "7", day, 5)
	for i, want := range []int64{3, 1} {
		res := scriptCall(t, c, 2, l)
		if !res.Allowed || res.QuotaUnseeded || res.Limits[0].Remaining != want || res.Granted[0] != 2 {
			t.Fatalf("call %d: %+v, want allowed with %d remaining", i+1, res, want)
		}
	}
	res := scriptCall(t, c, 2, l)
	left := time.Until(day.End)
	if res.Allowed || res.DenyingLimit != 0 || res.DenyingLevel != "consumer" || res.RetryAfter > left+time.Second || res.RetryAfter < left-5*time.Second ||
		res.Limits[0].ResetAfter != res.RetryAfter || res.Granted[0] != 0 {
		t.Errorf("past the quota: %+v, want a deny until the day ends, %v", res, left)
	}
	if got := getKey(t, rdb, l.Key); got != "4" {
		t.Errorf("counter %s, want 4: a deny writes nothing", got)
	}
	ttl, err := rdb.PTTL(context.Background(), l.Key).Result()
	if want := day.TTL(time.Now()); err != nil || ttl > want+time.Second || ttl < want-5*time.Second {
		t.Errorf("PTTL %v, %v; want the rest of the day plus a day, %v", ttl, err, want)
	}
	// One unit still fits.
	if res := scriptCall(t, c, 1, l); !res.Allowed || res.Limits[0].Remaining != 0 {
		t.Errorf("the last unit: %+v", res)
	}
	if res := scriptRefund(t, c, 2, res.NowUS, l); !res.Allowed {
		t.Errorf("refund: %+v", res)
	}
	if got := getKey(t, rdb, l.Key); got != "3" {
		t.Errorf("counter after a refund of 2: %s, want 3", got)
	}
	if ttl2, _ := rdb.PTTL(context.Background(), l.Key).Result(); ttl2 <= 0 || ttl2 > ttl {
		t.Errorf("PTTL after the refund %v: the refund keeps the TTL (%v)", ttl2, ttl)
	}
}

// TestScriptQuotaWriteRule: in one tenant-slot call a quota counts only
// when every limit allows, and a rate limit deducts only when the quota
// allows (RL-11, ADR-0070).
func TestScriptQuotaWriteRule(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	nearPeriodEdge()
	tenant := scriptName("quota-rule")
	seedEpoch(t, rdb, tenant)
	g := gcraLimit(redis.TenantKey(tenant, "p", time.Hour, redis.Digest("7")), "consumer", true, 1, time.Hour, 1)
	q := quotaLimit(tenant, "7", quota.Day(time.Now()), 1)
	m := quotaLimit(tenant, "7", quota.Month(time.Now()), 100)
	g.Group, q.Group, m.Group = 1, 0, 0
	if res := scriptCall(t, c, 1, g, q, m); !res.Allowed {
		t.Fatalf("first: %+v", res)
	}
	// The rate limit denies: the quotas are not counted.
	if res := scriptCall(t, c, 1, g, m); res.Allowed || res.DenyingLimit != 0 {
		t.Fatalf("second: %+v, want the GCRA limit to deny", res)
	}
	if got := getKey(t, rdb, m.Key); got != "1" {
		t.Errorf("monthly counter %s, want 1", got)
	}
	// The daily quota denies: neither the rate limit nor the monthly quota
	// is written.
	g2 := gcraLimit(redis.TenantKey(tenant, "p2", time.Hour, redis.Digest("7")), "consumer", true, 10, time.Hour, 10)
	if res := scriptCall(t, c, 1, g2, q, m); res.Allowed || res.DenyingLimit != 1 {
		t.Fatalf("third: %+v, want the daily quota to deny", res)
	}
	if got := getKey(t, rdb, g2.Key); got != "<missing>" {
		t.Errorf("GCRA state %s after a quota deny, want none", got)
	}
	if got := getKey(t, rdb, m.Key); got != "1" {
		t.Errorf("monthly counter %s, want still 1", got)
	}
}

// TestScriptQuotaUnseeded: without the tenant's epoch, or with Redis's TIME
// outside the period the gateway chose, quota limits are skipped — they
// neither decide nor are written — and the reply says quota_unseeded; the
// call's other limits still apply (ADR-0070).
func TestScriptQuotaUnseeded(t *testing.T) {
	_, c, rdb := scriptEnv(t)
	nearPeriodEdge()
	now := time.Now()
	for _, tt := range []struct {
		name   string
		seed   bool
		period quota.Period
	}{
		{"no epoch", false, quota.Day(now)},
		{"TIME after the period", true, quota.Day(now.AddDate(0, 0, -1))},
		{"TIME before the period", true, quota.Day(now.AddDate(0, 0, 1))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tenant := scriptName("unseeded")
			if tt.seed {
				seedEpoch(t, rdb, tenant)
			}
			q := quotaLimit(tenant, "7", tt.period, 1)
			w := windowLimit(redis.TenantKey(tenant, "w", time.Hour, redis.Digest("7")), "consumer", true, 2, time.Hour)
			w.Group = 1
			for i, allowed := range []bool{true, true, false} {
				res := scriptCall(t, c, 1, w, q)
				if res.Allowed != allowed || !res.QuotaUnseeded || res.Granted[1] != 0 {
					t.Errorf("call %d: %+v, want allowed %v by the window alone, quota_unseeded", i+1, res, allowed)
				}
			}
			if got := getKey(t, rdb, q.Key); got != "<missing>" {
				t.Errorf("counter %s, want none", got)
			}
		})
	}
	// A refund while the epoch is missing gives nothing back.
	tenant := scriptName("unseeded-refund")
	seedEpoch(t, rdb, tenant)
	q := quotaLimit(tenant, "7", quota.Day(now), 5)
	res := scriptCall(t, c, 2, q)
	if err := rdb.Del(context.Background(), redis.QuotaEpochKey(tenant)).Err(); err != nil {
		t.Fatal(err)
	}
	if r := scriptRefund(t, c, 2, res.NowUS, q); !r.QuotaUnseeded {
		t.Errorf("refund without the epoch: %+v, want quota_unseeded", r)
	}
	if got := getKey(t, rdb, q.Key); got != "2" {
		t.Errorf("counter %s after a refund without the epoch, want 2", got)
	}
}

// TestScriptQuotaRejects: the script's argument rules for quota limits.
func TestScriptQuotaRejects(t *testing.T) {
	_, _, rdb := scriptEnv(t)
	src, err := os.ReadFile("../../internal/ratelimit/redis/limiter_v1.lua")
	if err != nil {
		t.Fatal(err)
	}
	tenant := scriptName("quota-rejects")
	day := quota.Day(time.Now())
	start, end := day.Start.UnixMilli(), day.End.UnixMilli()
	key, epoch := redis.QuotaKey(tenant, "7", day.ID), redis.QuotaEpochKey(tenant)
	q := func(mode string, period, burst, k, s, e int64) []any {
		return []any{1, "consumer", mode, "quota", 5, period, burst, k, 1, s, e}
	}
	for _, tt := range []struct {
		name string
		keys []string
		args []any
		want string
	}{
		{"shadow", []string{key, epoch}, q("shadow", 0, 0, 0, start, end), "a quota is enforce"},
		{"a period_ms", []string{key, epoch}, q("enforce", 1000, 0, 0, start, end), "with period_ms, burst and lease_k 0"},
		{"leased", []string{key, epoch}, q("enforce", 0, 0, 5, start, end), "with period_ms, burst and lease_k 0"},
		{"empty period", []string{key, epoch}, q("enforce", 0, 0, 0, end, end), "period_start_ms < period_end_ms"},
		{"no epoch key", []string{key}, q("enforce", 0, 0, 0, start, end), "want 2 keys for 1 limits, got 1"},
		{"bounds on a GCRA limit", []string{key}, []any{1, "consumer", "enforce", "gcra", 5, 1000, 5, 0, 1, start, end}, "period_ms must be >= 1 and the period bounds 0"},
	} {
		err := rdb.Eval(context.Background(), string(src), tt.keys, tt.args...).Err()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
}
