//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// Plans, overrides, burst grants and access lists (RL-08, RL-09, CP-11 -
// CP-13, SEC-08, ADR-0041, ADR-0050).

// errorsOf is the messages of a validation's errors, one per line.
func errorsOf(v apiv1.ConfigValidation) string {
	var b strings.Builder
	for _, e := range v.Errors {
		fmt.Fprintf(&b, "%s %s\n", *e.Entity, *e.Message)
	}
	return b.String()
}

// TestControlPlanePlansAndOverrides checks ADR-0041's rules on Postgres:
// plans must cover every from_plan policy and may set only per-consumer
// policies (validate and publish); overrides are checked against the
// latest published policy when written, and again at every publish and
// rollback; every change needs its role and writes an audit record with
// its note (CP-12, CP-13, SEC-08).
func TestControlPlanePlansAndOverrides(t *testing.T) {
	c, iss, _ := startControlPlane(t, io.Discard)
	r := newRoles(t, c, iss)
	created(t, r.editor, "/upstreams", ordersUpstream)
	created(t, r.editor, "/routes", ordersRoute) // cost 2
	pcID, _ := created(t, r.editor, "/policies", gcraPolicy("per-consumer", "orders-api"))
	fpID, _ := created(t, r.editor, "/policies", map[string]any{"name": "orders-plan", "level": "consumer", "match": map[string]any{"routes": []string{"*"}},
		"key_template": "{consumer}:{route}", "algorithm": "gcra", "from_plan": true})
	globalID, _ := created(t, r.editor, "/policies", map[string]any{"name": "per-ip", "level": "global", "match": map[string]any{"routes": []string{"*"}},
		"key_template": "{ip}", "algorithm": "gcra", "limits": []any{map[string]any{"rate": 1000, "period": "1s"}}, "local": true})
	planID, planTag := created(t, r.editor, "/plans", map[string]any{"name": "free"})
	limits := func(rate int64, period string, burst int64) []any {
		l := map[string]any{"rate": rate, "period": period}
		if burst > 0 {
			l["burst"] = burst
		}
		return []any{l}
	}

	// A plan without the from_plan policy's limits, then with an entry for
	// a global policy: reported by validate, refused by publish.
	for _, tt := range []struct {
		entries []any
		want    string
	}{
		{nil, `plan "free": no default_limits for policy "orders-plan", which takes its limits from plans (from_plan)`},
		{[]any{map[string]any{"policy": "orders-plan", "limits": limits(10, "1s", 20)}, map[string]any{"policy": "per-ip", "limits": limits(5, "1s", 0)}},
			`plan "free": default_limits[1]: policy "per-ip": plans and overrides apply only to consumer- and route-level policies`},
		{[]any{map[string]any{"policy": "orders-plan", "limits": limits(1, "1s", 1)}}, `route "orders-api" costs 2, more than the 1`},
	} {
		if tt.entries != nil {
			a := r.editor.must(200, "PUT", fmt.Sprintf("/plans/%d", planID), map[string]any{"name": "free", "default_limits": tt.entries}, "If-Match", planTag)
			planTag = a.etag()
		}
		v, tag := validate(t, r.viewer)
		if v.Valid || !strings.Contains(errorsOf(v), tt.want) {
			t.Errorf("validate: valid %v, errors:\n%s\nwant %q", v.Valid, errorsOf(v), tt.want)
		}
		if e := r.publisher.must(422, "POST", "/config/publish", note("x"), "If-Match", tag).apiErr(t); !strings.Contains(e.Error, "validation failed") {
			t.Errorf("publish: %+v", e)
		}
	}
	r.editor.must(200, "PUT", fmt.Sprintf("/plans/%d", planID), map[string]any{"name": "free", "default_limits": []any{
		map[string]any{"policy": "orders-plan", "limits": limits(10, "1s", 20)},
	}}, "If-Match", planTag)
	_, tag := validate(t, r.viewer)
	r.publisher.must(200, "POST", "/config/publish", note("v1: plan free"), "If-Match", tag)

	tenant, _ := created(t, r.admin, "/tenants", map[string]any{"name": "acme", "plan_id": planID})
	consumer, _ := created(t, r.keyAdmin, "/consumers", map[string]any{"tenant_id": tenant, "name": "bot"})

	// Overrides at write time, against version 1's policies.
	for _, tt := range []struct {
		policy int64
		limits []any
		want   string
	}{
		{globalID, limits(5, "1s", 0), `policy "per-ip": plans and overrides apply only to`},
		{fpID, limits(10, "1h", 0), "limits[0].burst: required for periods over 1m0s"},
		{fpID, limits(1, "1s", 0), `route "orders-api" costs 2, more than the 1`},
		{fpID, []any{map[string]any{"rate": 10}}, `limits[0].period: "" is not a duration`},
	} {
		e := r.keyAdmin.must(422, "POST", "/overrides", map[string]any{"consumer_id": consumer, "policy_id": tt.policy, "limits": tt.limits}).apiErr(t)
		if !strings.Contains(e.Error, tt.want) || e.Details == nil || *(*e.Details)[0].Path != "limits" {
			t.Errorf("override of policy %d with %v: %+v, want %q on limits", tt.policy, tt.limits, e, tt.want)
		}
	}
	r.viewer.must(403, "POST", "/overrides", map[string]any{"consumer_id": consumer, "policy_id": fpID, "limits": limits(30, "1s", 0)})
	r.editor.must(403, "POST", "/overrides", map[string]any{"consumer_id": consumer, "policy_id": fpID, "limits": limits(30, "1s", 0)})
	created(t, r.keyAdmin, "/overrides", map[string]any{"consumer_id": consumer, "policy_id": fpID, "limits": limits(30, "1s", 0), "note": "launch week"})
	grantEnd := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	created(t, r.keyAdmin, "/overrides", map[string]any{"consumer_id": consumer, "policy_id": fpID, "limits": limits(50, "1s", 0), "ends_at": grantEnd, "note": "burst"})
	pcOverride, pcTag := created(t, r.keyAdmin, "/overrides", map[string]any{"consumer_id": consumer, "policy_id": pcID, "limits": limits(30, "1s", 60)})

	// At publish: per-consumer turns sliding_window, so its override's
	// burst no longer fits; deleting the override lets the publish go.
	_, pt := lookup(t, r.viewer, "/policies", "per-consumer")
	sliding := gcraPolicy("per-consumer", "orders-api")
	sliding["algorithm"], sliding["limits"] = "sliding_window", limits(100, "1s", 0)
	sliding["local"] = true
	r.editor.must(200, "PUT", fmt.Sprintf("/policies/%d", pcID), sliding, "If-Match", pt)
	v, tag := validate(t, r.viewer)
	if want := fmt.Sprintf("override override %d no longer fits its policy: policy \"per-consumer\": limits[0].burst: only gcra has a burst", pcOverride); v.Valid || !strings.Contains(errorsOf(v), want) {
		t.Errorf("validate after the policy change: errors\n%s\nwant %q", errorsOf(v), want)
	}
	e := r.publisher.must(422, "POST", "/config/publish", note("sliding"), "If-Match", tag).apiErr(t)
	if e.Details == nil || *(*e.Details)[0].Entity != "override" || *(*e.Details)[0].Path != "limits" {
		t.Errorf("publish past an override that no longer fits: %+v", e)
	}
	r.keyAdmin.must(204, "DELETE", fmt.Sprintf("/overrides/%d", pcOverride), nil, "If-Match", pcTag)
	_, tag = validate(t, r.viewer)
	r.publisher.must(200, "POST", "/config/publish", note("v2: per-consumer sliding"), "If-Match", tag)

	// At rollback: an override that fits version 2's sliding window (no
	// burst over an hour) does not fit version 1's GCRA.
	hourly, hourlyTag := created(t, r.keyAdmin, "/overrides", map[string]any{"consumer_id": consumer, "policy_id": pcID, "limits": limits(3000, "1h", 0)})
	_, tag = validate(t, r.viewer)
	if e := r.publisher.must(422, "POST", "/config/versions/1/rollback", note("back"), "If-Match", tag).apiErr(t); !strings.Contains(e.Error, "burst: required") {
		t.Errorf("rollback past an override that would not fit: %+v", e)
	}
	r.keyAdmin.must(204, "DELETE", fmt.Sprintf("/overrides/%d", hourly), nil, "If-Match", hourlyTag)
	r.publisher.must(200, "POST", "/config/versions/1/rollback", note("back to gcra"), "If-Match", tag)

	// Access lists: key-admin only, audited with their notes.
	r.viewer.must(403, "POST", "/access-lists", map[string]any{"action": "deny", "subject_type": "cidr", "cidr": "203.0.113.0/24"})
	r.editor.must(403, "POST", "/access-lists", map[string]any{"action": "deny", "subject_type": "cidr", "cidr": "203.0.113.0/24"})
	created(t, r.keyAdmin, "/access-lists", map[string]any{"action": "deny", "subject_type": "cidr", "cidr": "203.0.113.0/24", "note": "abuse"})
	created(t, r.keyAdmin, "/access-lists", map[string]any{"action": "allow", "subject_type": "consumer", "consumer_id": consumer, "note": "partner"})
	notes := map[string][]string{} // newest first
	for _, entity := range []string{"override", "access_list", "plan"} {
		for _, rec := range auditPage(t, r.admin, "?entity="+entity+"&limit=500").Items {
			n := ""
			if rec.Note != nil {
				n = *rec.Note
			}
			notes[entity] = append(notes[entity], rec.Action+":"+n)
		}
	}
	for entity, want := range map[string]string{
		"override":    "override.delete: override.create: override.delete: override.create: override.create:burst override.create:launch week",
		"access_list": "access_list.create:partner access_list.create:abuse",
		"plan":        "plan.update: plan.update: plan.update: plan.create:",
	} {
		if got := strings.Join(notes[entity], " "); got != want {
			t.Errorf("%s audit records %q, want %q", entity, got, want)
		}
	}
}

// TestComposePlans runs plans, overrides, burst grants and a CIDR deny
// through gateway-1 in managed mode (RL-08, RL-09, CP-09, CP-20,
// ADR-0041): the limits a request gets show in RateLimit-Policy, and each
// change — a plan's limits (a config publish), the consumer's plan, a
// standing override and a burst grant (identity changes), a CIDR deny —
// takes effect on gateway-1 within 5 s of the API's answer (CP-09's and
// CP-17's bound; every wait allows 30 s, so a host stall of ADR-0034 alone
// fails nothing). A burst grant ends on gateway-1's clock with no identity
// change; a denied CIDR is answered 403 before authentication, so a bad
// key gets 403, not 401; the Redis keys name the policy by its ID
// (ADR-0036). The test's tenant and entry are deleted afterwards.
func TestComposePlans(t *testing.T) {
	c := composeControlPlane(t)
	c = c.as(keycloakToken(t, "client_credentials", "spillway-ci", ""))
	_, _, rdb := scriptEnv(t)
	root := filepath.Join("..", "..")

	// The compose config plus route it-plans, its from_plan policy and three
	// plans (it-keys too, which other compose tests name), published.
	data, err := os.ReadFile(filepath.Join(root, "deploy", "compose", "gateway", "config.yaml")) //nolint:gosec // G304: a config file of this repo
	if err != nil {
		t.Fatal(err)
	}
	const route = "  - { name: it-plans, match: { path_prefix: /anything/it-plans }, upstream: httpbin, auth_mode: api_key }\n"
	cfg := strings.Replace(string(data), "\nroutes:\n", "\nroutes:\n"+route, 1)
	if cfg == string(data) {
		t.Fatal("the compose config has no routes: line")
	}
	cfg += `  - { name: it-plans-limit, level: consumer, match: { routes: [it-plans] }, key_template: '{consumer}:{route}', algorithm: gcra, from_plan: true }
plans:
  - { name: it-keys, default_limits: [{ policy: it-plans-limit, limits: [{ rate: 10, period: 1s }] }] }
  - { name: it-plans-free, default_limits: [{ policy: it-plans-limit, limits: [{ rate: 10, period: 1s }] }] }
  - { name: it-plans-pro, default_limits: [{ policy: it-plans-limit, limits: [{ rate: 100, period: 1s }] }] }
`
	_, tag := exportYAML(t, c, "")
	importYAML(t, c, 200, []byte(cfg), tag, "?note=TestComposePlans")
	_, tag = validate(t, c)
	if a := c.call("POST", "/config/publish", note("TestComposePlans: route it-plans, its from_plan policy and plans"), "If-Match", tag); a.status != 200 && a.status != 409 {
		t.Fatalf("publish: %d %s", a.status, a.body)
	}
	free, freeTag := lookup(t, c, "/plans", "it-plans-free")
	pro, _ := lookup(t, c, "/plans", "it-plans-pro")
	policy, _ := lookup(t, c, "/policies", "it-plans-limit")

	removeTestTenants(t, c, "it-plans-") // left by a run that did not finish
	name := fmt.Sprintf("it-plans-%d", time.Now().UnixNano()%1e9)
	tenant, _ := created(t, c, "/tenants", map[string]any{"name": name, "plan_id": free})
	consumer, consumerTag := created(t, c, "/consumers", map[string]any{"tenant_id": tenant, "name": name})
	t.Cleanup(func() { removeTestTenants(t, c, "it-plans-") })
	var key apiv1.IssuedApiKey
	c.must(201, "POST", fmt.Sprintf("/consumers/%d/keys", consumer), nil).decode(t, &key)
	composeManaged(t)

	gw1 := &proc{name: "gateway-1", admin: "http://127.0.0.1:8001"}
	send := func(apiKey string) (int, string, string) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8000/anything/it-plans/orders", nil)
		req.Header.Set("X-API-Key", apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", ""
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		q := ""
		for p := range strings.SplitSeq(resp.Header.Get("RateLimit-Policy"), ", ") {
			if rest, ok := strings.CutPrefix(p, `"it-plans-limit";q=`); ok {
				q = rest
			}
		}
		return resp.StatusCode, q, string(body)
	}
	// within waits for gateway-1 to answer the key with status and quota q
	// ("" for none) and returns how long that took from since.
	var took []string
	within := func(what string, since time.Time, apiKey string, status int, q string) {
		t.Helper()
		var gotStatus int
		var gotQ string
		eventually(t, fmt.Sprintf("%s: gateway-1 answering %d with it-plans-limit q=%q (last %d, %q)", what, status, q, gotStatus, gotQ), func() bool {
			gotStatus, gotQ, _ = send(apiKey)
			return gotStatus == status && gotQ == q
		})
		d := time.Since(since)
		took = append(took, fmt.Sprintf("%s %v", what, d.Round(time.Millisecond)))
		if d >= 5*time.Second {
			t.Errorf("%s took %v on gateway-1, want < 5 s", what, d)
		}
	}
	eventually(t, "gateway-1 with the consumer's key", func() bool { s, _, _ := send(key.Key); return s == 200 })
	within("the start, plan free", time.Now(), key.Key, 200, "10;w=1")

	// The Redis key names the policy by its ID, not its name.
	digest := redis.Digest(fmt.Sprintf("%d:it-plans", consumer))
	byID := redis.TenantKey(fmt.Sprint(tenant), fmt.Sprint(policy), time.Second, digest)
	byName := redis.TenantKey(fmt.Sprint(tenant), "it-plans-limit", time.Second, digest)
	// A GCRA key lives as long as its TAT is ahead of now, 100 ms per
	// request here, so a few requests are sent right before each look.
	eventually(t, "Redis key "+byID, func() bool {
		for range 5 {
			send(key.Key)
		}
		n, err := rdb.Exists(t.Context(), byID).Result()
		return err == nil && n == 1
	})
	if n, _ := rdb.Exists(t.Context(), byName).Result(); n != 0 {
		t.Errorf("Redis key %s names the policy", byName)
	}

	// A plan's limits change: a config publish.
	c.must(200, "PUT", fmt.Sprintf("/plans/%d", free), map[string]any{"name": "it-plans-free", "default_limits": []any{
		map[string]any{"policy": "it-plans-limit", "limits": []any{map[string]any{"rate": 20, "period": "1s"}}},
	}}, "If-Match", freeTag)
	_, tag = validate(t, c)
	c.must(200, "POST", "/config/publish", note("TestComposePlans: free at 20/s"), "If-Match", tag)
	within("plan free's limits (publish)", time.Now(), key.Key, 200, "20;w=1")

	// The consumer's plan changes: an identity change.
	start := time.Now()
	c.must(200, "PUT", fmt.Sprintf("/consumers/%d", consumer), map[string]any{"tenant_id": tenant, "name": name, "plan_id": pro}, "If-Match", consumerTag)
	within("the consumer's plan (identity)", start, key.Key, 200, "100;w=1")

	// A standing override, then a burst grant that ends by itself.
	start = time.Now()
	created(t, c, "/overrides", map[string]any{"consumer_id": consumer, "policy_id": policy, "limits": []any{map[string]any{"rate": 30, "period": "1s"}}, "note": "TestComposePlans"})
	within("a standing override", start, key.Key, 200, "30;w=1")
	start = time.Now()
	ends := start.Add(8 * time.Second)
	created(t, c, "/overrides", map[string]any{"consumer_id": consumer, "policy_id": policy, "limits": []any{map[string]any{"rate": 50, "period": "1s"}}, "ends_at": ends.UTC(), "note": "TestComposePlans burst"})
	within("a burst grant", start, key.Key, 200, "50;w=1")
	iv := gw1.metric("gw_identity_version")
	eventually(t, "the burst grant's end on gateway-1", func() bool { _, q, _ := send(key.Key); return q == "30;w=1" })
	t.Logf("the grant ended on gateway-1 %v after its ends_at (host clock)", time.Since(ends).Round(time.Millisecond))
	if v := gw1.metric("gw_identity_version"); v != iv {
		t.Errorf("identity version %v, want %v: a grant ends with no identity change", v, iv)
	}

	// A CIDR deny on the client IP gateway-1 sees: 403 before
	// authentication, so a bad key gets 403 too.
	_, _, body := send(key.Key)
	var echo struct{ Headers map[string][]string }
	if err := json.Unmarshal([]byte(body), &echo); err != nil || len(echo.Headers["X-Forwarded-For"]) == 0 {
		t.Fatalf("the upstream's echo %q: %v", body, err)
	}
	ip := echo.Headers["X-Forwarded-For"][0]
	bad := key.Key[:len(key.Key)-1] + map[bool]string{true: "A", false: "B"}[key.Key[len(key.Key)-1] != 'A']
	if s, _, _ := send(bad); s != 401 {
		t.Fatalf("a bad key before the deny: %d, want 401", s)
	}
	start = time.Now()
	var entry apiv1.AccessListEntry
	c.must(201, "POST", "/access-lists", map[string]any{"action": "deny", "subject_type": "cidr", "cidr": ip + "/32", "note": "TestComposePlans"}).decode(t, &entry)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/api/v1/access-lists/%d", c.base, *entry.Id), nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("If-Match", fmt.Sprintf(`"%d"`, *entry.Version))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	})
	within("a CIDR deny", start, bad, 403, "")
	if s, q, b := send(key.Key); s != 403 || q != "" || b != `{"error":"forbidden"}` {
		t.Errorf("the good key from the denied IP: %d %q, q %q; want 403 forbidden with no limits", s, b, q)
	}
	start = time.Now()
	c.must(204, "DELETE", fmt.Sprintf("/access-lists/%d", *entry.Id), nil, "If-Match", fmt.Sprintf(`"%d"`, *entry.Version))
	within("the CIDR deny removed", start, key.Key, 200, "30;w=1")
	t.Logf("gateway-1 applied each change after: %s", strings.Join(took, "; "))
}
