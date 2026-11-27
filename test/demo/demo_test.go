//go:build demo

// Package demo holds the demo stack's tests (SC-01, ADR-0094), run by make
// test-demo against the running demo, from the host: make demo prints no
// secret, token or key; make demo-reset restores a modified demo to the
// seed and leaves the dev database spillway as it was.
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

// runMake runs a make target in the repository and returns its output.
func runMake(t *testing.T, target string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "make", "--no-print-directory", "-C", "../..", target) //nolint:gosec // G204: the tests' own targets
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("make %s: %v\n%s", target, err, out.String())
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
func TestDemoUp(t *testing.T) {
	out := runMake(t, "demo")
	noLeak(t, "make demo", out)
	if !strings.Contains(out, "demo-seed: done") || !strings.Contains(out, "no API keys") {
		t.Errorf("make demo did not report the seed:\n%s", out)
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
