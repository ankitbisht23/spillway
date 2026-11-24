package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// quotaPolicies give plan free daily and monthly quotas and plan pro a
// monthly one (RL-10, ADR-0070), beside a Redis policy from the plans whose
// failure mode the tests set.
const quotaPolicies = `
policies:
  - {id: 14, name: redis-plan, level: consumer, match: {routes: ['*']}, key_template: '{consumer}', algorithm: gcra, from_plan: true, failure_mode: FAILURE}
plans:
  - {id: 21, name: free, default_limits: [{policy: redis-plan, limits: [{rate: 100, period: 1s}]}], quotas: {daily: 3, monthly: 100}}
  - {id: 22, name: pro, default_limits: [{policy: redis-plan, limits: [{rate: 100, period: 1s}]}], quotas: {monthly: 5}}
`

// quotaWorld is planWorld with quotaPolicies at failure mode failure; its
// fake Redis keeps the gateway's clock.
func quotaWorld(t *testing.T, failure string) *planWorld {
	t.Helper()
	w := newPlanWorld(t, "127.0.0.1/32")
	w.g.swap(t, managedSnapshot(t, w.routes+strings.Replace(quotaPolicies, "FAILURE", failure, 1)))
	w.redis.clock = w.g.clock.Now
	return w
}

// quotaResp is a response's status and headers.
type quotaResp struct {
	StatusCode int
	Header     http.Header
}

// sendQuota sends GET /orders as consumer and returns the response and its
// body.
func (w *planWorld) sendQuota(t *testing.T, consumer string) (quotaResp, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, w.g.URL+"/orders", nil)
	if err != nil {
		t.Fatal(err)
	}
	if consumer != "" {
		req.Header.Set("X-Api-Key", w.keys[consumer])
	}
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	resp, err := w.g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return quotaResp{resp.StatusCode, resp.Header}, string(body)
}

// TestQuotaCheck: a consumer's plan quotas ride in its tenant-slot call
// after its policies' limits, keyed by tenant, consumer and the UTC day
// and month of the gateway's clock with the tenant's epoch; they count
// admitted requests, deny with Retry-After to the period's end, and show as
// quota/daily and quota/monthly items after the policies' (RL-10,
// ADR-0070).
func TestQuotaCheck(t *testing.T) {
	w := quotaWorld(t, "fail_open")
	// t0 is 2026-11-10 12:00:00.25 UTC: 43,199.75 s to midnight; November
	// has 2,592,000 s.
	for i, wantR := range []string{"2", "1", "0"} {
		resp, body := w.sendQuota(t, "app")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d %s", i+1, resp.StatusCode, body)
		}
		wantPolicy := `"redis-plan";q=100;w=1, "quota/daily";q=3;w=86400, "quota/monthly";q=100;w=2592000`
		if got := resp.Header.Get("RateLimit-Policy"); got != wantPolicy {
			t.Errorf("request %d: RateLimit-Policy %q, want %q", i+1, got, wantPolicy)
		}
		if got := resp.Header.Get("RateLimit"); !strings.Contains(got, `"quota/daily";r=`+wantR+`;t=43200, "quota/monthly";r=`) {
			t.Errorf("request %d: RateLimit %q, want quota/daily r=%s t=43200", i+1, got, wantR)
		}
	}
	resp, body := w.sendQuota(t, "app")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "43200" {
		t.Errorf("fourth request: %d %s, Retry-After %q; want 429 until UTC midnight, 43200", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	calls := w.redis.take()
	want := "cost 1 | rl:v1:{t:2}:14:1000:" // redis-plan first
	if len(calls) == 0 || !strings.HasPrefix(calls[0], want) ||
		!strings.Contains(calls[0], " | q:v1:{t:2}:5:d20261110 consumer enforce quota 3/0s burst 0 [2026-11-10T00:00:00Z, 2026-11-11T00:00:00Z) epoch q:v1:{t:2}:epoch") ||
		!strings.HasSuffix(calls[0], " | q:v1:{t:2}:5:m202611 consumer enforce quota 100/0s burst 0 [2026-11-01T00:00:00Z, 2026-12-01T00:00:00Z) epoch q:v1:{t:2}:epoch") {
		t.Errorf("first call:\n%s\nwant redis-plan, then the daily and monthly quotas", strings.Join(calls, "\n"))
	}
	if got := w.redis.usedOf("q:v1:{t:2}:5:d20261110"); got != 3 {
		t.Errorf("daily count %d, want 3: the denied request is not counted", got)
	}
	for _, s := range []string{
		`gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="quota/daily"} 3`,
		`gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="quota/daily"} 1`,
	} {
		if !strings.Contains(scrape(t, w.g.metrics), "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}

	// Pro has a monthly quota only; the monthly deny waits for the month's
	// end, 20.5 days away.
	for range 5 {
		w.sendQuota(t, "bot")
	}
	resp, _ = w.sendQuota(t, "bot")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1771200" || strings.Contains(resp.Header.Get("RateLimit-Policy"), "quota/daily") {
		t.Errorf("bot's sixth request: %d, Retry-After %q, RateLimit-Policy %q; want 429 until December, 1771200, no daily item",
			resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("RateLimit-Policy"))
	}

	// Anonymous requests have no quotas.
	resp, _ = w.sendQuota(t, "")
	if strings.Contains(resp.Header.Get("RateLimit-Policy"), "quota/") {
		t.Errorf("anonymous: RateLimit-Policy %q has quota items", resp.Header.Get("RateLimit-Policy"))
	}

	// At UTC midnight the day's counter is new (the gateway's clock picks
	// the period; the script checks it against TIME).
	w.g.clock.Advance(12 * time.Hour)
	resp, body = w.sendQuota(t, "app")
	if resp.StatusCode != http.StatusOK || w.redis.usedOf("q:v1:{t:2}:5:d20261111") != 1 || w.redis.usedOf("q:v1:{t:2}:5:m202611") != 4 {
		t.Errorf("after midnight: %d %s; daily %d, monthly %d; want 200, 1, 4", resp.StatusCode, body,
			w.redis.usedOf("q:v1:{t:2}:5:d20261111"), w.redis.usedOf("q:v1:{t:2}:5:m202611"))
	}
}

// TestQuotaOverrides: a quota override replaces both plan quotas while it
// applies, a burst grant beating the standing one, on the gateway's clock;
// another consumer's does not apply (RL-10, ADR-0070).
func TestQuotaOverrides(t *testing.T) {
	w := quotaWorld(t, "fail_open")
	now := w.g.clock.Now()
	w.setIdentity(t, nil, []*configpb.Override{
		{Id: 1, ConsumerId: 5, Quotas: &configpb.Quotas{Daily: 10}},
		{Id: 2, ConsumerId: 5, Quotas: &configpb.Quotas{Daily: 20, Monthly: 50}, StartsAt: timestamppb.New(now.Add(time.Minute)), EndsAt: timestamppb.New(now.Add(time.Hour))},
		{Id: 3, ConsumerId: 3, Quotas: &configpb.Quotas{}}, // bot: no quotas at all
	})
	for _, tt := range []struct {
		name, consumer string
		advance        time.Duration
		want           string
	}{
		{"standing override: daily only", "app", 0, `"redis-plan";q=100;w=1, "quota/daily";q=10;w=86400`},
		{"burst grant from its start", "app", time.Minute, `"redis-plan";q=100;w=1, "quota/daily";q=20;w=86400, "quota/monthly";q=50;w=2592000`},
		{"standing again at the grant's end", "app", time.Hour, `"redis-plan";q=100;w=1, "quota/daily";q=10;w=86400`},
		{"an override of zeros: no quotas", "bot", 0, `"redis-plan";q=100;w=1`},
		{"another consumer: its plan's", "batch", 0, `"redis-plan";q=100;w=1, "quota/daily";q=3;w=86400, "quota/monthly";q=100;w=2592000`},
	} {
		w.g.clock.Advance(tt.advance)
		if resp, _ := w.sendQuota(t, tt.consumer); resp.Header.Get("RateLimit-Policy") != tt.want {
			t.Errorf("%s: RateLimit-Policy %q, want %q", tt.name, resp.Header.Get("RateLimit-Policy"), tt.want)
		}
	}
}

// TestQuotaFailureModes: when the tenant-slot call fails or answers
// quota_unseeded, the quota follows the failure modes of the call's
// policies: 503 if an enforce policy there is fail_closed, otherwise not
// enforced and not counted; a quota-only call is fail_open (ADR-0070).
func TestQuotaFailureModes(t *testing.T) {
	for _, tt := range []struct {
		name, failure string
		unseeded      bool // else the call fails
		quotaOnly     bool
		status        int
		decisions     string
	}{
		{"failed call, fail_open policy", "fail_open", false, false, http.StatusOK, `"policy":"quota/daily","decision":"fail_open"`},
		{"failed call, fail_closed policy", "fail_closed", false, false, http.StatusServiceUnavailable, `"policy":"quota/daily","decision":"fail_closed"`},
		{"failed call, degraded policy", "degraded", false, false, http.StatusOK, `"policy":"quota/daily","decision":"fail_open"`},
		{"failed quota-only call", "", false, true, http.StatusOK, `"policy":"quota/daily","decision":"fail_open"`},
		{"unseeded, fail_open policy", "fail_open", true, false, http.StatusOK, `"policy":"quota/daily","decision":"fail_open"`},
		{"unseeded, fail_closed policy", "fail_closed", true, false, http.StatusServiceUnavailable, `"policy":"quota/daily","decision":"fail_closed"`},
		{"unseeded quota-only call", "", true, true, http.StatusOK, `"policy":"quota/monthly","decision":"fail_open"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newPlanWorld(t, "127.0.0.1/32")
			yaml := strings.Replace(quotaPolicies, "FAILURE", tt.failure, 1)
			if tt.quotaOnly {
				yaml = "plans:\n  - {id: 21, name: free, quotas: {daily: 3, monthly: 100}}\n"
			}
			w.g.swap(t, managedSnapshot(t, w.routes+yaml))
			if tt.unseeded {
				w.redis.unseeded["{t:2}"] = true
			} else {
				w.redis.setFail("{t:2}", true)
			}
			for i := range 5 { // past the daily quota of 3
				resp, body := w.sendQuota(t, "app")
				if resp.StatusCode != tt.status {
					t.Fatalf("request %d: %d %s, want %d", i+1, resp.StatusCode, body, tt.status)
				}
				if strings.Contains(resp.Header.Get("RateLimit-Policy"), "quota/") {
					t.Errorf("request %d: RateLimit-Policy %q shows a quota that was not checked", i+1, resp.Header.Get("RateLimit-Policy"))
				}
			}
			if got := w.redis.usedOf("q:v1:{t:2}:5:d20261110"); got != 0 {
				t.Errorf("daily count %d, want 0: nothing is counted", got)
			}
			if !strings.Contains(w.g.logs.String(), tt.decisions) {
				t.Errorf("access log has no %s:\n%s", tt.decisions, w.g.logs.String())
			}
			if tt.unseeded && tt.status == http.StatusServiceUnavailable {
				// The policy's deduction is given back, as for a failed call.
				w.redis.takeRefunds(t, 5)
				if got := w.redis.usedOf(w.redisPlanKey(t)); got != 0 {
					t.Errorf("redis-plan holds %d after 503s, want 0", got)
				}
			}
		})
	}
}

// redisPlanKey is the key of app's redis-plan limit in the fake Redis.
func (w *planWorld) redisPlanKey(t *testing.T) string {
	t.Helper()
	w.redis.mu.Lock()
	defer w.redis.mu.Unlock()
	for k := range w.redis.used {
		if strings.HasPrefix(k, "rl:v1:{t:2}:14:") {
			return k
		}
	}
	return "rl:v1:{t:2}:14:none"
}

// TestQuotaRefund: a request the global level denies after its tenant-slot
// call allowed is refunded there too, its quota increment included, so a
// quota counts only admitted requests (RL-14, ADR-0070).
func TestQuotaRefund(t *testing.T) {
	w := newPlanWorld(t, "127.0.0.1/32")
	yaml := strings.Replace(quotaPolicies, "FAILURE", "fail_open", 1)
	at := strings.Index(yaml, "plans:")
	w.g.swap(t, managedSnapshot(t, w.routes+yaml[:at]+
		"  - {id: 15, name: global, level: global, match: {routes: ['*']}, key_template: '{route}', algorithm: gcra, limits: [{rate: 1, period: 1s}]}\n"+yaml[at:]))
	if resp, body := w.sendQuota(t, "app"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first: %d %s", resp.StatusCode, body)
	}
	if resp, _ := w.sendQuota(t, "app"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second: %d, want 429 from the global policy", resp.StatusCode)
	}
	refunds := w.redis.takeRefunds(t, 1)
	if !strings.Contains(refunds[0], "q:v1:{t:2}:5:d20261110") || !strings.Contains(refunds[0], "q:v1:{t:2}:5:m202611") {
		t.Errorf("refund %q does not give the quotas back", refunds[0])
	}
	if got := w.redis.usedOf("q:v1:{t:2}:5:d20261110"); got != 1 {
		t.Errorf("daily count %d, want 1: the request the global level denied is not counted", got)
	}
}
