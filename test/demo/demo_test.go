//go:build demo

// Package demo holds the demo stack's tests (SC-01, SC-08, ADR-0094,
// ADR-0095), run by make test-demo against the running demo, from the
// host: make demo prints no secret, token or key and runs in demo mode;
// make demo-reset restores a modified demo to the seed and leaves the dev
// database spillway as it was.
package demo

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankitbisht23/spillway/internal/demo"
	"github.com/ankitbisht23/spillway/internal/secret"
)

const (
	cpURL      = "http://127.0.0.1:8080/api/v1"
	tokenURL   = "http://127.0.0.1:8180/auth/realms/spillway/protocol/openid-connect/token" //nolint:gosec // G101: an endpoint, not a credential
	gatewayURL = "http://127.0.0.1:8000"
	devSecret  = "dev-only-not-secret" //nolint:gosec // G101: .env.example's dev-only value
)

// freeLimit matches a RateLimit-Policy with free's 10/s (ADR-0018).
var freeLimit = regexp.MustCompile(`"consumer-plan";q=10\b`)

// setting is the environment's value of name, else .env.example's.
func setting(t *testing.T, name string) secret.Value {
	t.Helper()
	s, err := secret.FromEnv(os.Getenv, name)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsSet() {
		s = secret.New(devSecret)
	}
	return s
}

func client(t *testing.T) *demo.Client {
	return demo.NewClient(cpURL, tokenURL, "spillway-ci", setting(t, "SPILLWAY_CI_CLIENT_SECRET"), 10*time.Second)
}

// must calls the control plane and fails the test on any other status.
func must(t *testing.T, c *demo.Client, want int, method, path string, body, out any, header ...string) demo.Answer {
	t.Helper()
	a, err := c.Must(t.Context(), want, method, path, body, out, header...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// runMake runs a make target in the repository, with variables such as
// ON=1 after it, and returns its output.
func runMake(t *testing.T, target string, vars ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "make", append([]string{"--no-print-directory", "-C", "../..", target}, vars...)...) //nolint:gosec // G204: the tests' own targets
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("make %s %s: %v\n%s", target, strings.Join(vars, " "), err, out.String())
	}
	return out.String()
}

// noLeak fails when output holds a secret, a bearer token or a key.
func noLeak(t *testing.T, what, output string, keys ...string) {
	t.Helper()
	for _, s := range append([]string{setting(t, "SPILLWAY_CI_CLIENT_SECRET").Reveal(), "Bearer ", "eyJ"}, keys...) {
		if s != "" && strings.Contains(output, s) {
			t.Errorf("%s printed a secret, token or key", what)
		}
	}
}

// devFingerprint summarises the dev database spillway: its audit log,
// versions, working-set revision, identity version and identity rows.
func devFingerprint(t *testing.T) string {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://spillway@127.0.0.1:5432/spillway?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = setting(t, "POSTGRES_PASSWORD").Reveal()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var fp string
	if err := conn.QueryRow(ctx, `SELECT concat_ws(' ',
		(SELECT count(*) || '/' || coalesce(max(id), 0) FROM audit_log),
		(SELECT count(*) || '/' || coalesce(max(version), 0) FROM config_versions),
		(SELECT revision FROM config_working_set),
		(SELECT version FROM identity_state),
		(SELECT count(*) FROM tenants), (SELECT count(*) FROM consumers), (SELECT count(*) FROM api_keys))`).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	return fp
}

type entity struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	PlanID   *int64  `json:"plan_id"`
	TenantID int64   `json:"tenant_id"`
	Version  int64   `json:"version"`
	Subject  *string `json:"jwt_subject"`
}

func list(t *testing.T, c *demo.Client, path string) []entity {
	t.Helper()
	var l struct{ Items []entity }
	must(t, c, 200, "GET", path, nil, &l)
	return l.Items
}

func byName(es []entity, name string) entity {
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	return entity{}
}

// TestDemoUp: make demo, run again on the running demo, prints no secret,
// token or key, and reports the seed.
// The second run seeds again and changes nothing (ADR-0094 §2, §3).
func TestDemoUp(t *testing.T) {
	for run := 1; run <= 2; run++ {
		out := runMake(t, "demo")
		noLeak(t, "make demo", out)
		if !strings.Contains(out, "demo-seed: done") || !strings.Contains(out, "no API keys") {
			t.Errorf("make demo run %d did not report the seed:\n%s", run, out)
		}
		if run == 2 && !strings.Contains(out, "(0 created, 0 updated, 14 unchanged)") {
			t.Errorf("make demo on the running demo changed the seed:\n%s", out)
		}
	}
}

// send sends GET /v1/orders to gateway-1 with key and returns the status,
// 0 for no answer.
func send(t *testing.T, key string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", gatewayURL+"/v1/orders", nil)
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// until waits up to 30 s for cond, failing with what.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after 30 s: %s", what)
		}
	}
}

// TestDemoMode: make demo runs the control plane in demo mode (SC-08,
// ADR-0095): GET /api/v1/demo answers the seeded tenants and routes; a key
// issued without an expiry expires 30 minutes after its creation and one
// asked for 31 minutes is 422; a free-co-playground key is limited by its
// own counter (free's 10/s, burst 20), so free-co-web's key is still
// admitted; an 8 s key is refused by gateway-1 with 401 once it has
// expired; and no key appears in the control plane's or a gateway's log.
// Waits allow 30 s, and the expiry check leaves 2 s after the expiry,
// both far above the host's 0.82 s stalls (ADR-0034).
func TestDemoMode(t *testing.T) {
	c := client(t)
	var d struct {
		Tenants []struct {
			Name string `json:"name"`
			Plan string `json:"plan"`
		} `json:"tenants"`
		Routes []struct {
			Name   string `json:"name"`
			Policy string `json:"policy"`
		} `json:"routes"`
	}
	must(t, c, 200, "GET", "/demo", nil, &d)
	if fmt.Sprint(d.Tenants) != "[{free-co free} {pro-co pro} {enterprise-co enterprise}]" ||
		fmt.Sprint(d.Routes) != "[{orders consumer-plan} {catalog catalog-limit} {payments payments-limit}]" {
		t.Errorf("getDemo: %+v", d)
	}

	consumers := list(t, c, "/consumers?limit=500")
	type issuedKey struct {
		ID        int64     `json:"id"`
		Key       string    `json:"key"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	var keys []string
	issue := func(consumer string, body map[string]any) issuedKey {
		t.Helper()
		var k issuedKey
		must(t, c, 201, "POST", fmt.Sprintf("/consumers/%d/keys", byName(consumers, consumer).ID), body, &k)
		keys = append(keys, k.Key)
		return k
	}
	pg := issue("free-co-playground", map[string]any{})
	if d := pg.ExpiresAt.Sub(pg.CreatedAt) - 30*time.Minute; d < -time.Second || d > time.Second {
		t.Errorf("a demo key: expires_at - created_at = %v, want 30 min ± 1 s", pg.ExpiresAt.Sub(pg.CreatedAt))
	}
	a, err := c.Call(t.Context(), "POST", fmt.Sprintf("/consumers/%d/keys", byName(consumers, "free-co-playground").ID),
		map[string]any{"expires_at": time.Now().Add(31 * time.Minute)})
	if err != nil || a.Status != 422 {
		t.Errorf("31 minutes ahead: %d %v", a.Status, err)
	}

	// Own limits: the playground key runs into free's limit; the web
	// consumer's key, of the same tenant, is still admitted.
	until(t, "the playground key works on gateway-1", func() bool { return send(t, pg.Key) == 200 })
	limited := 0
	for range 60 {
		switch code := send(t, pg.Key); code {
		case 429:
			limited++
		case 200:
		default:
			t.Fatalf("the playground key: %d", code)
		}
	}
	web := issue("free-co-web", map[string]any{})
	until(t, "the web key works on gateway-1", func() bool { return send(t, web.Key) == 200 })
	if limited == 0 {
		t.Error("61 playground requests in a row, none limited by free's 10/s, burst 20")
	}

	// An 8 s key: refused once it has expired.
	short := issue("free-co-mobile", map[string]any{"expires_at": time.Now().Add(8 * time.Second)})
	until(t, "the 8 s key works on gateway-1", func() bool { c := send(t, short.Key); return c == 200 || c == 429 })
	time.Sleep(time.Until(short.ExpiresAt.Add(2 * time.Second)))
	if code := send(t, short.Key); code != 401 {
		t.Errorf("2 s after its expiry the key answered %d, want 401", code)
	}

	for _, name := range []string{"spillway-controlplane-1", "spillway-gateway-1-1", "spillway-gateway-2-1", "spillway-gateway-3-1"} {
		out, err := exec.CommandContext(t.Context(), "docker", "logs", name).CombinedOutput() //nolint:gosec // G204: the demo's own containers
		if err != nil {
			t.Fatalf("docker logs %s: %v", name, err)
		}
		noLeak(t, name+"'s log", string(out), keys...)
	}
}

// TestDemoReset: a demo with an extra tenant, a tenant on another plan, a
// deleted consumer, an issued key and a published config change goes back
// to the seed with make demo-reset: three tenants on their plans, eleven
// consumers without keys, one published version that is the seed's, three
// live replicas running it; gateway-1 serves it, so a fresh key gets free's
// limit. The dev database spillway is as it was, and make demo-reset
// prints neither the key nor a secret or token.
func TestDemoReset(t *testing.T) {
	dev := devFingerprint(t)
	c := client(t)
	plans := list(t, c, "/plans")
	free, pro := byName(plans, "free"), byName(plans, "pro")
	tenants := list(t, c, "/tenants")
	if len(tenants) != 3 || free.ID == 0 || pro.ID == 0 {
		t.Fatalf("not the seeded demo: tenants %v, plans %v", tenants, plans)
	}

	// Modify it.
	must(t, c, 201, "POST", "/tenants", map[string]any{"name": "reset-probe", "plan_id": free.ID}, nil)
	proCo := byName(tenants, "pro-co")
	must(t, c, 200, "PUT", fmt.Sprintf("/tenants/%d", proCo.ID), map[string]any{"name": "pro-co", "plan_id": free.ID, "status": "active"}, nil, "If-Match", fmt.Sprintf(`"%d"`, proCo.Version))
	consumers := list(t, c, "/consumers?limit=500")
	web := byName(consumers, "free-co-web")
	must(t, c, 204, "DELETE", fmt.Sprintf("/consumers/%d", web.ID), nil, nil, "If-Match", fmt.Sprintf(`"%d"`, web.Version))
	var issued struct {
		Key string `json:"key"`
	}
	mobile := byName(consumers, "free-co-mobile")
	must(t, c, 201, "POST", fmt.Sprintf("/consumers/%d/keys", mobile.ID), map[string]any{}, &issued)
	limit := byName(list(t, c, "/policies"), "catalog-limit")
	must(t, c, 200, "PUT", fmt.Sprintf("/policies/%d", limit.ID), map[string]any{
		"name": "catalog-limit", "level": "route", "match": map[string]any{"routes": []string{"catalog"}}, "key_template": "{consumer}",
		"algorithm": "gcra", "limits": []any{map[string]any{"rate": 7, "period": "1s", "burst": 7}}, "mode": "enforce", "failure_mode": "fail_open",
	}, nil, "If-Match", fmt.Sprintf(`"%d"`, limit.Version))
	v := must(t, c, 200, "POST", "/config/validate", nil, nil)
	must(t, c, 200, "POST", "/config/publish", map[string]string{"note": "make test-demo: a change for demo-reset to undo"}, nil, "If-Match", v.Header.Get("ETag"))

	out := runMake(t, "demo-reset")
	noLeak(t, "make demo-reset", out, issued.Key)
	if !strings.Contains(out, "demo-reset: done in") {
		t.Errorf("output:\n%s", out)
	}

	// The seed again.
	c = client(t)
	plans = list(t, c, "/plans")
	planOf := map[int64]string{}
	for _, p := range plans {
		planOf[p.ID] = p.Name
	}
	tenants = list(t, c, "/tenants")
	got := map[string]string{}
	for _, tn := range tenants {
		got[tn.Name] = planOf[*tn.PlanID]
	}
	if len(got) != 3 || got["free-co"] != "free" || got["pro-co"] != "pro" || got["enterprise-co"] != "enterprise" {
		t.Errorf("tenants after the reset: %v", got)
	}
	consumers = list(t, c, "/consumers?limit=500")
	if len(consumers) != 11 || byName(consumers, "free-co-web").ID == 0 {
		t.Errorf("%d consumers after the reset", len(consumers))
	}
	for _, cs := range consumers {
		if keys := list(t, c, fmt.Sprintf("/consumers/%d/keys", cs.ID)); len(keys) != 0 {
			t.Errorf("consumer %s has %d keys after the reset", cs.Name, len(keys))
		}
	}
	var versions struct {
		Items []struct {
			Version int64  `json:"version"`
			Note    string `json:"note"`
		}
	}
	must(t, c, 200, "GET", "/config/versions", nil, &versions)
	if len(versions.Items) != 1 || versions.Items[0].Note != demo.SeedNote {
		t.Errorf("versions after the reset: %+v", versions.Items)
	}
	var policy struct {
		Limits []struct{ Rate int64 } `json:"limits"`
	}
	must(t, c, 200, "GET", fmt.Sprintf("/policies/%d", byName(list(t, c, "/policies"), "catalog-limit").ID), nil, &policy)
	if len(policy.Limits) != 1 || policy.Limits[0].Rate != 200 {
		t.Errorf("catalog-limit after the reset: %+v", policy)
	}

	// Three live replicas run it, and gateway-1 serves it.
	want := versions.Items[0].Version
	deadline := time.Now().Add(time.Minute)
	for {
		var rs struct {
			Items []struct {
				ID            string `json:"id"`
				ConfigVersion int64  `json:"config_version"`
				Live          bool   `json:"live"`
			}
		}
		must(t, c, 200, "GET", "/replicas", nil, &rs)
		var at []string
		for _, r := range rs.Items {
			if r.Live && r.ConfigVersion == want {
				at = append(at, r.ID)
			}
		}
		slices.Sort(at)
		if slices.Equal(at, []string{"gateway-1", "gateway-2", "gateway-3"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replicas at version %d after a minute: %v", want, at)
		}
		time.Sleep(time.Second)
	}
	mobile = byName(consumers, "free-co-mobile")
	must(t, c, 201, "POST", fmt.Sprintf("/consumers/%d/keys", mobile.ID), map[string]any{}, &issued)
	deadline = time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", gatewayURL+"/v1/orders", nil)
		req.Header.Set("X-API-Key", issued.Key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == 200 && freeLimit.MatchString(resp.Header.Get("RateLimit-Policy")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway-1 with a fresh free-co-mobile key: %d, RateLimit-Policy %q", resp.StatusCode, resp.Header.Get("RateLimit-Policy"))
		}
		time.Sleep(500 * time.Millisecond)
	}

	if after := devFingerprint(t); after != dev {
		t.Errorf("the dev database changed: %s, then %s", dev, after)
	}
}
