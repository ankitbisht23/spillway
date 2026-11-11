//go:build integration

package integration

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// TestGatewayGlobalLimit checks gateway-1's Redis policies from the compose
// config on the compose cluster: with alice's key, shared-tenant runs in the
// {t:acme} tenant-slot call and shared-global after it; the 4th request for
// one X-Shared-Key is denied by shared-global with 429, and the tenant-slot
// deduction it made is refunded, as the key in Redis shows (RL-05, RL-13,
// RL-14, ADR-0021).
func TestGatewayGlobalLimit(t *testing.T) {
	_, _, rdb := scriptEnv(t)
	run := scriptName("shared")
	awayFromEdge(time.Hour) // shared-tenant's sliding window
	client := &http.Client{Timeout: 10 * time.Second}
	send := func(sharedKey string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/anything/shared", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", "alice-example-key-1")
		req.Header.Set("X-Shared-Key", sharedKey)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}

	const policy = `"per-ip";q=5000;w=1, "shared-tenant";q=100;w=3600, "shared-global";q=3;w=10`
	// Until a freshly started gateway has warmed up (ADR-0024), its first
	// call to a node loads the slot map, dials, checks the Redis version and
	// loads the script, which can overrun the budget and fail open. Both
	// calls' slots depend only on the tenant and the policy, so requests
	// with another X-Shared-Key wait for that.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, h, _ := send(run + "-warm")
		if h.Get("RateLimit-Policy") == policy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm-up: RateLimit-Policy %q, want %q", h.Get("RateLimit-Policy"), policy)
		}
	}
	for i := 1; i <= 3; i++ {
		status, h, _ := send(run)
		if status != http.StatusOK || h.Get("RateLimit-Policy") != policy || !strings.Contains(h.Get("RateLimit"), `"shared-global";r=`+strconv.Itoa(3-i)+";") {
			t.Fatalf("request %d: %d with RateLimit-Policy %q and RateLimit %q, want 200 with %q and shared-global r=%d", i, status, h.Get("RateLimit-Policy"), h.Get("RateLimit"), policy, 3-i)
		}
	}
	status, h, body := send(run)
	if status != http.StatusTooManyRequests || body != `{"error":"rate limited"}` || h.Get("Retry-After") == "" || h.Get("RateLimit-Policy") != policy ||
		!strings.Contains(h.Get("RateLimit"), `"shared-tenant";r=97;t=`) || !strings.Contains(h.Get("RateLimit"), `"shared-global";r=0;t=`) {
		t.Fatalf("4th request: %d %q, Retry-After %q, RateLimit %q; want 429 with shared-tenant r=97 (refunded) and shared-global r=0", status, body, h.Get("Retry-After"), h.Get("RateLimit"))
	}

	// The 4th tenant-slot call wrote a 4th unit; the refund, sent
	// asynchronously after the deny, takes it back.
	key := redis.TenantKey("acme", "shared-tenant", time.Hour, redis.Digest("acme:"+run))
	var state map[string]string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if state = getHash(t, rdb, key); state["cur"] == "3" {
			break
		}
	}
	if state["cur"] != "3" {
		t.Errorf("%s = %v, want cur 3: three admissions, the denied one refunded", key, state)
	}
	if v := getKey(t, rdb, redis.GlobalKey("shared-global", 10*time.Second, redis.Digest(run))); v == "<missing>" {
		t.Error("shared-global's key is missing")
	}

	_, metrics := get(t, "http://127.0.0.1:8001/metrics")
	for _, m := range []string{
		`
gw_ratelimit_decisions_total{decision="allow",mode="enforce",policy="shared-tenant"} `,
		`
gw_ratelimit_decisions_total{decision="deny",mode="enforce",policy="shared-global"} `,
	} {
		if !strings.Contains(metrics, m) {
			t.Errorf("admin /metrics has no %s", strings.TrimSpace(m))
		}
	}
}
