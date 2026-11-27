//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/demo"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// Demo mode (SC-08, ADR-0095): its wiring in compose and Helm, the 404s
// with it off and getDemo with it on against Postgres, demo keys on a
// managed gateway, and the expired-key janitor.

// TestDemoModeWiring: only deploy/compose/demo.compose.yml sets the
// control plane's SPILLWAY_CP_DEMO_MODE, from DEMO_MODE with false as its
// default; .env.example has DEMO_MODE=false; compose.yaml, the Helm chart
// and the kind values never set it, and no gateway reads it (ADR-0095 §1).
func TestDemoModeWiring(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "compose", "demo.compose.yml")) //nolint:gosec // G304: a file of this repository
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for name, svc := range f.Services {
		v, ok := svc.Environment["SPILLWAY_CP_DEMO_MODE"]
		if want := name == "controlplane"; ok != want || want && v != "${DEMO_MODE:-false}" {
			t.Errorf("demo.compose.yml: %s sets SPILLWAY_CP_DEMO_MODE %v (%q)", name, ok, v)
		}
	}
	env, err := os.ReadFile(filepath.Join(root, ".env.example")) //nolint:gosec // G304: a file of this repository
	if err != nil || !strings.Contains(string(env), "\nDEMO_MODE=false\n") {
		t.Errorf(".env.example: no DEMO_MODE=false (%v)", err)
	}
	for _, dir := range []string{"compose.yaml", "deploy/helm", "deploy/kind", "internal/config", "cmd/gateway"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path) //nolint:gosec // G304: files of this repository
			if err == nil && strings.Contains(string(b), "DEMO_MODE") {
				t.Errorf("%s mentions DEMO_MODE", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGetDemo: with demo mode off a control plane on Postgres answers every
// demo path 404 exactly as an unknown path, to an admin and to no token;
// with it on and the demo seeded, getDemo answers the three tenants with
// their plans' limits and the three Playground routes with their policies
// (ADR-0095 §2).
func TestGetDemo(t *testing.T) {
	off, iss, _ := startControlPlaneMode(t, io.Discard, false)
	admin := iss.token(t, "service-account-spillway-ci", "service-account-spillway-ci", "admin")
	unknown := off.as(admin).call("GET", "/no-such-path", nil)
	var unknownErr apiv1.Error
	unknown.decode(t, &unknownErr)
	for _, path := range []string{"/demo", "/demo/", "/demo/playground/runs", "/demo/faults/redis-cut"} {
		for _, c := range []cpClient{off, off.as(admin)} {
			for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
				a := c.call(method, path, nil)
				e := a.apiErr(t)
				if a.status != 404 || e.Code != apiv1.ErrorCodeNotFound || e.Error != unknownErr.Error || a.header.Get("WWW-Authenticate") != "" {
					t.Errorf("demo mode off, %s %s: %d %s", method, path, a.status, a.body)
				}
			}
		}
	}

	c, iss, _ := startControlPlaneMode(t, io.Discard, true)
	admin = iss.token(t, "service-account-spillway-ci", "service-account-spillway-ci", "admin")
	c.must(401, "GET", "/demo", nil)
	viewer := c.as(iss.token(t, "sub-viewer", "test-viewer", "viewer"))
	var empty apiv1.Demo
	viewer.must(200, "GET", "/demo", nil).decode(t, &empty)
	if len(empty.Tenants) != 0 || len(empty.Routes) != 0 || len(empty.Scenarios) != 3 {
		t.Errorf("before the seed: %+v", empty)
	}
	seedDemo(t, c, admin)
	var d apiv1.Demo
	viewer.must(200, "GET", "/demo", nil).decode(t, &d)
	got, _ := json.Marshal(d)
	// No toxiproxy here: both fault states unknown (TestDemoFaults has one).
	const want = `{"faults":[{"active":null,"name":"redis-cut"},{"active":null,"name":"upstream-slow"}],` +
		`"routes":[{"limit":null,"name":"orders","policy":"consumer-plan"},` +
		`{"limit":{"burst":200,"period":"1s","rate":200},"name":"catalog","policy":"catalog-limit"},` +
		`{"limit":{"burst":200,"period":"1s","rate":100},"name":"payments","policy":"payments-limit"}],` +
		`"scenarios":["d1","d2","d3"],` +
		`"tenants":[{"limit":{"burst":20,"period":"1s","rate":10},"name":"free-co","plan":"free"},` +
		`{"limit":{"burst":200,"period":"1s","rate":100},"name":"pro-co","plan":"pro"},` +
		`{"limit":{"burst":1000,"period":"1s","rate":1000},"name":"enterprise-co","plan":"enterprise"}]}`
	if string(got) != want {
		t.Errorf("getDemo:\n%s\nwant\n%s", got, want)
	}
}

// seedDemo runs demo-traffic seed's logic against c with deploy/demo's
// files, with token as the spillway-ci service account's.
func seedDemo(t *testing.T, c cpClient, token string) {
	t.Helper()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.PostForm.Get("client_secret") != demoClientSecret {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token})
	}))
	t.Cleanup(tokens.Close)
	cfg, err := os.ReadFile("../../deploy/demo/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../deploy/demo/seed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := demo.ParseSeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	client := demo.NewClient(c.base+api.BasePath, tokens.URL, "spillway-ci", secret.New(demoClientSecret), 10*time.Second)
	if _, err := demo.Seed(t.Context(), client, cfg, seed, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// demoKeyConfig is a config like the demo's for a host gateway: route
// orders (API keys only) limited per consumer by its plan, free (5 per
// hour, burst 5), on the gateway itself.
func demoKeyConfig(addr string) []byte {
	return fmt.Appendf(nil, `upstreams: [{name: u, endpoints: [{address: '%s'}]}]
routes: [{name: orders, upstream: u, match: {path_prefix: /v1/orders}, auth_mode: api_key}]
policies:
  - name: consumer-plan
    level: consumer
    algorithm: gcra
    local: true
    from_plan: true
    key_template: '{consumer}'
    match: {routes: [orders]}
plans:
  - name: free
    default_limits: [{policy: consumer-plan, limits: [{rate: 5, period: 1h, burst: 5}]}]
`, addr)
}

// TestDemoKeys: a control plane in demo mode and a managed gateway (SC-08,
// ADR-0095 §3). A key issued without an expiry expires 30 minutes after
// its creation; one asked for 31 minutes is 422; a rotation gives the new
// key 30 minutes and the old one no longer; a key asked for 8 s works
// until then and the gateway rejects it with 401 after, on its own clock;
// a playground consumer's key is limited by its own counter under its
// tenant's plan, so another consumer of the tenant keeps its whole limit;
// and no key or secret appears in either process's log.
//
// The expiry check tolerates the host's stalls (ADR-0027, ADR-0034: up to
// 0.82 s): probes answered before expires_at - 1 s must be accepted and
// probes sent after expires_at + 2 s refused, so a stall moves either side
// by less than its margin; every wait allows 30 s.
func TestDemoKeys(t *testing.T) {
	iss := newTestIssuer(t)
	database := migratedDatabase(t)
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", database, grpcAddr, iss, "SPILLWAY_CP_DEMO_MODE=true")
	v := publishYAML(t, cp.rest, demoKeyConfig(upstreamAnswering(t, "ok")), "demo keys")
	tenant, _ := created(t, cp.rest, "/tenants", map[string]any{"name": "free-co", "plan_id": planID(t, cp.rest)})
	consumer := func(name string) int64 {
		id, _ := created(t, cp.rest, "/consumers", map[string]any{"tenant_id": tenant, "name": name})
		return id
	}
	web, mobile, playground := consumer("free-co-web"), consumer("free-co-mobile"), consumer("free-co-playground")
	gw := startManaged(t, "gateway-1", grpcAddr, t.TempDir())
	gw.waitVersion(t, int(v))

	var keys []keyParts
	issue := func(c int64, body any) apiv1.IssuedApiKey {
		t.Helper()
		var k apiv1.IssuedApiKey
		cp.rest.must(201, "POST", fmt.Sprintf("/consumers/%d/keys", c), body).decode(t, &k)
		keys = append(keys, partsOf(t, k.Key))
		return k
	}
	send := func(key string) int {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, gw.url+"/v1/orders", nil)
		req.Header.Set("X-API-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// No expires_at: exactly 30 minutes after its creation.
	k1 := issue(playground, nil)
	if k1.ExpiresAt == nil {
		t.Fatal("a key issued in demo mode has no expiry")
	}
	if d := k1.ExpiresAt.Sub(k1.CreatedAt) - 30*time.Minute; d < -time.Second || d > time.Second {
		t.Errorf("expires_at - created_at = %v, want 30 min ± 1 s", k1.ExpiresAt.Sub(k1.CreatedAt))
	}
	// 31 minutes: refused, naming expires_at.
	a := cp.rest.must(422, "POST", fmt.Sprintf("/consumers/%d/keys", playground), map[string]any{"expires_at": time.Now().Add(31 * time.Minute)})
	if e := a.apiErr(t); e.Code != apiv1.ErrorCodeValidationFailed || e.Details == nil || *(*e.Details)[0].Path != "expires_at" ||
		*(*e.Details)[0].Message != "expires_at: at most 30 minutes ahead in demo mode" {
		t.Errorf("31 minutes: %s", a.body)
	}
	// A rotation with the default 24 h grace: the new key gets 30 minutes,
	// the old one ends at the latest then.
	var rot apiv1.KeyRotation
	before := time.Now()
	cp.rest.must(200, "POST", fmt.Sprintf("/keys/%d/rotate", k1.Id), nil).decode(t, &rot)
	keys = append(keys, partsOf(t, rot.New.Key))
	if rot.New.ExpiresAt == nil || rot.Old.ExpiresAt == nil || rot.New.ExpiresAt.Before(before.Add(30*time.Minute-time.Second)) ||
		rot.New.ExpiresAt.After(time.Now().Add(30*time.Minute+time.Second)) || rot.Old.ExpiresAt.After(*rot.New.ExpiresAt) {
		t.Errorf("rotation: new key expires %v, old key %v", rot.New.ExpiresAt, rot.Old.ExpiresAt)
	}

	// Their own limits: the playground key's counter runs out after the
	// plan's burst; the web consumer's key is still admitted.
	pg := rot.New.Key
	eventually(t, "the playground key works", func() bool { return send(pg) == 200 })
	admitted, limited := 1, false
	for range 10 {
		switch code := send(pg); code {
		case 200:
			admitted++
		case 429:
			limited = true
		default:
			t.Fatalf("the playground key: %d", code)
		}
	}
	if !limited || admitted > 5 {
		t.Errorf("the playground key: %d admitted, limited %v; want at most the plan's burst of 5, then 429", admitted, limited)
	}
	webKey := issue(web, nil)
	eventually(t, "the web key works", func() bool { return send(webKey.Key) == 200 })
	for i := range 3 {
		if code := send(webKey.Key); code != 200 {
			t.Errorf("the web key's request %d: %d; the playground key spent only its own counter", i+2, code)
		}
	}

	// An 8 s key: accepted until it expires, then 401 on the gateway.
	short := issue(mobile, map[string]any{"expires_at": time.Now().Add(8 * time.Second)})
	expires := *short.ExpiresAt
	eventually(t, "the 8 s key works", func() bool { c := send(short.Key); return c == 200 || c == 429 })
	if time.Until(expires) < 2*time.Second {
		t.Fatalf("setup: the 8 s key applied only %v before its expiry", time.Until(expires))
	}
	var probes int
	for time.Now().Before(expires.Add(3 * time.Second)) {
		sent := time.Now()
		code := send(short.Key)
		answered := time.Now()
		probes++
		switch {
		case answered.Before(expires.Add(-time.Second)) && code != 200 && code != 429:
			t.Errorf("before its expiry the key answered %d", code)
		case sent.After(expires.Add(2*time.Second)) && code != 401:
			t.Errorf("%v after its expiry the key answered %d, want 401", sent.Sub(expires), code)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code := send(short.Key); code != 401 {
		t.Errorf("after its expiry: %d", code)
	}
	t.Logf("%d probes of the 8 s key", probes)

	// getDemo on this config.
	var d apiv1.Demo
	cp.rest.must(200, "GET", "/demo", nil).decode(t, &d)
	if len(d.Tenants) != 1 || d.Tenants[0].Limit == nil || *d.Tenants[0].Limit != (apiv1.DemoLimit{Rate: 5, Burst: 5, Period: "1h"}) ||
		len(d.Routes) != 1 || d.Routes[0].Policy == nil || *d.Routes[0].Policy != "consumer-plan" || d.Routes[0].Limit != nil {
		t.Errorf("getDemo: %+v", d)
	}

	// No key material in either log.
	gw.stop(syscall.SIGTERM)
	cp.stop(syscall.SIGTERM)
	noKeyIn(t, "the gateway's log", gw.out.String(), true, keys...)
	noKeyIn(t, "the control plane's log", cp.out.String(), true, keys...)
	if !strings.Contains(cp.out.String(), "demo mode is on") {
		t.Error("the control plane did not log that demo mode is on")
	}
}

// TestDemoJanitor: the expired-key janitor's sweep on a scratch database
// (ADR-0095 §4) deletes the keys that expired over an hour ago, with the
// access-list entries naming them, and keeps keys that expired less long
// ago, live keys, keys without an expiry and an expired key another key
// was rotated from; one identity version for the sweep, one audit record
// per deleted key (actor system:demo-janitor, request ID demo-janitor-…,
// no hash or key); a second sweep finds nothing and changes nothing.
func TestDemoJanitor(t *testing.T) {
	ctx := t.Context()
	database := migratedDatabase(t)
	st := openStore(t, database)
	owner := pgConnect(t, pgURL(t, "spillway", database), pgOwnerPassword)
	a := testAudit()
	publish(t, owner, `{"plans": [{"id": 7, "name": "free"}]}`)
	tenant, err := st.CreateTenant(ctx, a, tenantParams("free-co", 7))
	if err != nil {
		t.Fatal(err)
	}
	cons, err := st.CreateConsumer(ctx, a, db.CreateConsumerParams{TenantID: tenant.ID, Name: "free-co-web", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	var parts []keyParts
	newKey := func() int64 {
		k, key, err := st.IssueKey(ctx, a, apikey.Generate, cons.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, partsOf(t, key.Secret.Reveal()))
		return k.ID
	}
	expire := func(id int64, ago time.Duration) {
		if _, err := owner.Exec(ctx, "UPDATE api_keys SET expires_at = now() - make_interval(secs => $2) WHERE id = $1", id, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	old1, old2, young, live, never, target := newKey(), newKey(), newKey(), newKey(), newKey(), newKey()
	expire(old1, 2*time.Hour)
	expire(old2, 61*time.Minute)
	expire(young, 30*time.Minute)
	expire(live, -10*time.Minute)
	entry, err := st.CreateAccessList(ctx, a, db.CreateAccessListParams{Action: "deny", SubjectType: "key", ApiKeyID: &old2})
	if err != nil {
		t.Fatal(err)
	}
	_, child, _, err := st.RotateKey(ctx, a, apikey.Generate, target, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	expire(target, 3*time.Hour)

	identity := func() int64 {
		var v int64
		if err := owner.QueryRow(ctx, "SELECT version FROM identity_state").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	exists := func(table string, id int64) bool {
		var n int
		if err := owner.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE id = $1", id).Scan(&n); err != nil { //nolint:gosec // G202: the test's own table names
			t.Fatal(err)
		}
		return n == 1
	}
	v0 := identity()
	srv := api.New(st, nil, nil, nil, secret.New(hitterSecret), slog.New(slog.NewJSONHandler(io.Discard, nil))).WithDemo(true)
	n, err := srv.SweepExpiredKeys(ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweep: %d deleted, %v; want 2", n, err)
	}
	if identity() != v0+1 {
		t.Errorf("identity version %d, want one change from %d", identity(), v0)
	}
	for id, want := range map[int64]bool{old1: false, old2: false, young: true, live: true, never: true, target: true, child.ID: true} {
		if exists("api_keys", id) != want {
			t.Errorf("key %d: present %v, want %v", id, !want, want)
		}
	}
	if exists("access_lists", entry.ID) {
		t.Error("the deleted key's access-list entry is still there")
	}
	rows, err := owner.Query(ctx, "SELECT actor_sub, action, entity, entity_id, request_id, identity_version, diff::text FROM audit_log WHERE actor_sub = 'system:demo-janitor' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var records []string
	for rows.Next() {
		var sub, action, entity, entityID, requestID, diff string
		var iv int64
		if err := rows.Scan(&sub, &action, &entity, &entityID, &requestID, &iv, &diff); err != nil {
			t.Fatal(err)
		}
		records = append(records, entityID)
		if action != "key.delete" || entity != "key" || !strings.HasPrefix(requestID, "demo-janitor-") || iv != v0+1 || !strings.Contains(diff, `"prefix"`) {
			t.Errorf("audit record %s %s %s %s %d %s", action, entity, entityID, requestID, iv, diff)
		}
		if (entityID == fmt.Sprint(old2)) != strings.Contains(diff, fmt.Sprintf(`"access_list_ids": [%d]`, entry.ID)) {
			t.Errorf("key %s's record: %s", entityID, diff)
		}
		noKeyIn(t, "an audit record", diff, true, parts...)
	}
	rows.Close()
	if strings.Join(records, ",") != fmt.Sprintf("%d,%d", old1, old2) {
		t.Errorf("audit records for keys %v, want %d and %d", records, old1, old2)
	}

	// Nothing left to delete: no identity version, no record.
	if n, err := srv.SweepExpiredKeys(ctx); err != nil || n != 0 || identity() != v0+1 {
		t.Errorf("second sweep: %d deleted, %v, identity version %d", n, err, identity())
	}
	var count int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE actor_sub = 'system:demo-janitor'").Scan(&count); err != nil || count != 2 {
		t.Errorf("%d janitor records after the second sweep, %v", count, err)
	}
}
