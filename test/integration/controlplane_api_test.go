//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/proxy"
)

// cpClient calls a control plane's REST API.
type cpClient struct {
	t    *testing.T
	base string // up to and without /api/v1
}

type cpAnswer struct {
	status int
	header http.Header
	body   []byte
}

func (a cpAnswer) etag() string { return a.header.Get("ETag") }

func (a cpAnswer) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(a.body, v); err != nil {
		t.Fatalf("%d %s: %v", a.status, a.body, err)
	}
}

func (a cpAnswer) apiErr(t *testing.T) apiv1.Error {
	t.Helper()
	var e apiv1.Error
	a.decode(t, &e)
	return e
}

func (c cpClient) call(method, path string, body any, header ...string) cpAnswer {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+api.BasePath+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return cpAnswer{resp.StatusCode, resp.Header, b}
}

// must calls and fails the test unless the answer has status want.
func (c cpClient) must(want int, method, path string, body any, header ...string) cpAnswer {
	c.t.Helper()
	a := c.call(method, path, body, header...)
	if a.status != want {
		c.t.Fatalf("%s %s: %d %s, want %d", method, path, a.status, a.body, want)
	}
	return a
}

// newControlPlane serves the API over a fresh, migrated database as the app
// role, and returns a client and the database's name.
func newControlPlane(t *testing.T) (cpClient, string) {
	t.Helper()
	db := migratedDatabase(t)
	allowed, err := proxy.ParseAllowlist("172.29.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(openStore(t, db), allowed, net.DefaultResolver, slog.New(slog.NewJSONHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return cpClient{t, srv.URL}, db
}

// TestControlPlaneConfigCRUD checks the config working set's REST API on
// Postgres (CP-01, CP-10, CP-14): create, read, replace and delete with
// ETags and If-Match, endpoint IDs kept across a PUT, drain, names unique,
// and versions that are working-set revisions.
func TestControlPlaneConfigCRUD(t *testing.T) {
	c, _ := newControlPlane(t)
	up := map[string]any{"name": "orders", "lb": "round_robin", "endpoints": []any{
		map[string]any{"address": "172.29.0.10:8080"}, map[string]any{"address": "172.29.0.11:8080"},
	}, "health_check": map[string]any{"path": "/healthz", "interval": "5s"}}
	created := c.must(201, "POST", "/upstreams", up)
	var u apiv1.Upstream
	created.decode(t, &u)
	if u.Id == nil || *u.Version != 2 || created.etag() != `"2"` || created.header.Get("Location") != fmt.Sprintf("/api/v1/upstreams/%d", *u.Id) {
		t.Fatalf("created %s, ETag %s, Location %s", created.body, created.etag(), created.header.Get("Location"))
	}
	if len(u.Endpoints) != 2 || u.Endpoints[0].Id == nil || *u.HealthCheck.Interval != "5s" || *u.Lb != "round_robin" {
		t.Fatalf("created upstream %s", created.body)
	}
	path := fmt.Sprintf("/upstreams/%d", *u.Id)
	if got := c.must(200, "GET", path, nil); got.etag() != `"2"` || !bytes.Equal(got.body, created.body) {
		t.Errorf("GET: ETag %s, body %s; want what POST answered", got.etag(), got.body)
	}
	if e := c.must(409, "POST", "/upstreams", up).apiErr(t); e.Code != apiv1.ErrorCodeConflict {
		t.Errorf("same name again: %+v", e)
	}

	// Replace: keep one address, drop one, add one.
	up["endpoints"] = []any{map[string]any{"address": "172.29.0.11:8080", "id": 999}, map[string]any{"address": "172.29.0.12:8080", "draining": true}}
	up["id"], up["version"] = 12345, 1 // read-only fields a client echoes back are ignored
	delete(up, "health_check")
	if e := c.must(428, "PUT", path, up).apiErr(t); e.Code != apiv1.ErrorCodePreconditionRequired {
		t.Errorf("PUT without If-Match: %+v", e)
	}
	stale := c.must(412, "PUT", path, up, "If-Match", `"1"`).apiErr(t)
	if stale.Code != apiv1.ErrorCodePreconditionFailed || stale.Details == nil || *(*stale.Details)[0].Version != 2 {
		t.Errorf("stale If-Match: %+v", stale)
	}
	c.must(412, "PUT", path, up, "If-Match", `W/"2"`)
	replaced := c.must(200, "PUT", path, up, "If-Match", `"2"`)
	var u2 apiv1.Upstream
	replaced.decode(t, &u2)
	if *u2.Id != *u.Id || *u2.Version != 3 || replaced.etag() != `"3"` || len(u2.Endpoints) != 2 {
		t.Fatalf("replaced %s", replaced.body)
	}
	if *u2.Endpoints[0].Id != *u.Endpoints[1].Id || *u2.Endpoints[1].Id <= *u.Endpoints[1].Id || !*u2.Endpoints[1].Draining {
		t.Errorf("endpoints %s: the kept address keeps its ID, the new one gets a new ID", replaced.body)
	}
	if u2.HealthCheck != nil {
		t.Errorf("PUT replaces the whole entity, but health_check survived: %s", replaced.body)
	}

	// Drain needs the upstream's ETag and bumps its version.
	drain := fmt.Sprintf("%s/endpoints/%d/drain", path, *u2.Endpoints[0].Id)
	c.must(412, "POST", drain, nil, "If-Match", `"2"`)
	c.must(404, "POST", fmt.Sprintf("%s/endpoints/%d/drain", path, 424242), nil, "If-Match", `"3"`)
	drained := c.must(200, "POST", drain, nil, "If-Match", `"3"`)
	var u3 apiv1.Upstream
	drained.decode(t, &u3)
	if *u3.Version != 4 || drained.etag() != `"4"` || !*u3.Endpoints[0].Draining {
		t.Errorf("drained: %s, want version 4: refused edits bump nothing", drained.body)
	}

	// A route and a policy on it; references are checked at publish, not here.
	route := c.must(201, "POST", "/routes", map[string]any{"name": "orders-api", "upstream": "missing", "match": map[string]any{"path_prefix": "/v1/orders", "methods": []string{"GET"}}, "cost": 2, "max_body_bytes": 1048576})
	var r apiv1.Route
	route.decode(t, &r)
	if *r.Cost != 2 || *r.MaxBodyBytes != 1048576 || r.Upstream != "missing" {
		t.Errorf("route %s", route.body)
	}
	c.must(201, "POST", "/policies", map[string]any{"name": "per-route", "level": "route", "match": map[string]any{"routes": []string{"orders-api"}}, "key_template": "{route}", "algorithm": "gcra", "limits": []any{map[string]any{"rate": 100, "period": "1s", "burst": 200}}})
	plan := c.must(201, "POST", "/plans", map[string]any{"name": "free"})
	if !strings.Contains(string(plan.body), `"name":"free"`) {
		t.Errorf("plan %s", plan.body)
	}

	c.must(412, "DELETE", path, nil, "If-Match", `"3"`)
	c.must(204, "DELETE", path, nil, "If-Match", `"4"`)
	c.must(404, "GET", path, nil)
	c.must(404, "DELETE", path, nil, "If-Match", `"4"`)
}

// TestControlPlanePages checks cursor pages and the name filter (UI-06,
// ADR-0036).
func TestControlPlanePages(t *testing.T) {
	c, _ := newControlPlane(t)
	var want []int64
	for i := range 5 {
		var p apiv1.Plan
		c.must(201, "POST", "/plans", map[string]any{"name": fmt.Sprintf("plan-%d", i)}).decode(t, &p)
		want = append(want, *p.Id)
	}
	var got []int64
	pages := 0
	cursor := ""
	for {
		var l apiv1.PlanList
		c.must(200, "GET", "/plans?limit=2"+cursor, nil).decode(t, &l)
		pages++
		for _, p := range l.Items {
			got = append(got, *p.Id)
		}
		if l.NextCursor == nil {
			break
		}
		cursor = "&cursor=" + *l.NextCursor
	}
	if pages != 3 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%d pages with %v, want 3 pages with %v", pages, got, want)
	}
	var l apiv1.PlanList
	c.must(200, "GET", "/plans?name=plan-3", nil).decode(t, &l)
	if len(l.Items) != 1 || l.Items[0].Name != "plan-3" || l.NextCursor != nil {
		t.Errorf("?name=plan-3: %+v", l)
	}
	var empty apiv1.PlanList
	a := c.must(200, "GET", "/plans?name=nope", nil)
	a.decode(t, &empty)
	if !strings.Contains(string(a.body), `"items":[]`) || !strings.Contains(string(a.body), `"next_cursor":null`) {
		t.Errorf("an empty page: %s", a.body)
	}
}

// TestControlPlaneIdentityCRUD checks the identity entities' REST API on
// Postgres (CP-01, CP-14, CP-20): references to the published config,
// tenants with consumers, access lists, overrides, and key listing.
func TestControlPlaneIdentityCRUD(t *testing.T) {
	c, db := newControlPlane(t)

	tenant := map[string]any{"name": "acme", "plan_id": 1}
	if e := c.must(422, "POST", "/tenants", tenant).apiErr(t); !strings.Contains(e.Error, "no config version is published yet") {
		t.Errorf("tenant before any publish: %+v", e)
	}
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	publish(t, owner, `{"plans": [{"id": 1, "name": "free"}, {"id": 2, "name": "pro"}], "policies": [{"id": 5, "name": "per-consumer"}]}`)

	var ten apiv1.Tenant
	created := c.must(201, "POST", "/tenants", tenant)
	created.decode(t, &ten)
	if ten.Status != "active" || *ten.Version != 2 || created.etag() != `"2"` {
		t.Errorf("tenant %s", created.body)
	}
	c.must(409, "POST", "/tenants", tenant)
	if e := c.must(422, "POST", "/tenants", map[string]any{"name": "beta", "plan_id": 3}).apiErr(t); !strings.Contains(e.Error, "plan 3 is not in the published config version 1") {
		t.Errorf("tenant on an unpublished plan: %+v", e)
	}

	consumer := map[string]any{"tenant_id": *ten.Id, "name": "bot", "plan_id": 2, "jwt_subject": "sub-1"}
	var con apiv1.Consumer
	c.must(201, "POST", "/consumers", consumer).decode(t, &con)
	if *con.PlanId != 2 || *con.JwtSubject != "sub-1" || *con.Version != 3 {
		t.Errorf("consumer %+v", con)
	}
	if e := c.must(422, "POST", "/consumers", map[string]any{"tenant_id": 999, "name": "x"}).apiErr(t); !strings.Contains(e.Error, "no such tenant") {
		t.Errorf("consumer of a missing tenant: %+v", e)
	}
	c.must(409, "POST", "/consumers", map[string]any{"tenant_id": *ten.Id, "name": "other", "jwt_subject": "sub-1"})
	var cl apiv1.ConsumerList
	c.must(200, "GET", fmt.Sprintf("/consumers?tenant_id=%d", *ten.Id), nil).decode(t, &cl)
	if len(cl.Items) != 1 {
		t.Errorf("consumers of acme: %+v", cl)
	}
	tenPath := fmt.Sprintf("/tenants/%d", *ten.Id)
	if e := c.must(409, "DELETE", tenPath, nil, "If-Match", `"2"`).apiErr(t); !strings.Contains(e.Error, "has consumers") {
		t.Errorf("tenant with consumers: %+v", e)
	}
	ten.Status = "suspended"
	c.must(412, "PUT", tenPath, ten, "If-Match", `"1"`)
	var ten2 apiv1.Tenant
	c.must(200, "PUT", tenPath, ten, "If-Match", `"2"`).decode(t, &ten2)
	if ten2.Status != "suspended" || *ten2.Version != 4 {
		t.Errorf("suspended tenant %+v", ten2)
	}

	// Keys: listed by prefix only, never the hash (ADR-0036).
	if _, err := owner.Exec(t.Context(), `INSERT INTO api_keys (consumer_id, prefix, hash, version) VALUES ($1, 'Ab3dE6gH', sha256('dev-only-not-secret'), 4)`, *con.Id); err != nil {
		t.Fatal(err)
	}
	keys := c.must(200, "GET", fmt.Sprintf("/consumers/%d/keys", *con.Id), nil)
	var kl apiv1.ApiKeyList
	keys.decode(t, &kl)
	if len(kl.Items) != 1 || kl.Items[0].Prefix != "Ab3dE6gH" || strings.Contains(string(keys.body), "hash") {
		t.Errorf("keys %s", keys.body)
	}
	c.must(404, "GET", "/consumers/999/keys", nil)

	// Access lists.
	cidr := map[string]any{"action": "deny", "subject_type": "cidr", "cidr": "203.0.113.0/24", "note": "abuse"}
	var al apiv1.AccessListEntry
	c.must(201, "POST", "/access-lists", cidr).decode(t, &al)
	if *al.Cidr != "203.0.113.0/24" {
		t.Errorf("access list %+v", al)
	}
	c.must(409, "POST", "/access-lists", cidr)
	c.must(201, "POST", "/access-lists", map[string]any{"action": "allow", "subject_type": "consumer", "consumer_id": *con.Id})
	c.must(201, "POST", "/access-lists", map[string]any{"action": "deny", "subject_type": "key", "api_key_id": kl.Items[0].Id})
	if e := c.must(422, "POST", "/access-lists", map[string]any{"action": "deny", "subject_type": "key", "api_key_id": 999}).apiErr(t); !strings.Contains(e.Error, "no such key") {
		t.Errorf("entry for a missing key: %+v", e)
	}
	var all apiv1.AccessListEntryList
	c.must(200, "GET", "/access-lists?action=deny", nil).decode(t, &all)
	if len(all.Items) != 2 {
		t.Errorf("deny entries: %+v", all)
	}

	// Overrides: the policy must be published; one standing override per
	// (consumer, policy), burst grants beside it.
	ov := map[string]any{"consumer_id": *con.Id, "policy_id": 5, "limits": []any{map[string]any{"rate": 30, "period": "1s"}}}
	var o apiv1.Override
	c.must(201, "POST", "/overrides", ov).decode(t, &o)
	if len(o.Limits) != 1 || *o.Limits[0].Rate != 30 || *o.Limits[0].Period != "1s" {
		t.Errorf("override %+v", o)
	}
	c.must(409, "POST", "/overrides", ov)
	ov["starts_at"], ov["ends_at"] = "2026-11-14T10:00:00Z", "2026-11-14T11:00:00Z"
	var grant apiv1.Override
	c.must(201, "POST", "/overrides", ov).decode(t, &grant)
	if grant.EndsAt == nil || !grant.EndsAt.Equal(time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("burst grant %+v", grant)
	}
	ov["policy_id"] = 6
	if e := c.must(422, "POST", "/overrides", ov).apiErr(t); !strings.Contains(e.Error, "policy 6 is not in the published config") {
		t.Errorf("override of an unpublished policy: %+v", e)
	}
	var ol apiv1.OverrideList
	c.must(200, "GET", fmt.Sprintf("/overrides?consumer_id=%d&policy_id=5", *con.Id), nil).decode(t, &ol)
	if len(ol.Items) != 2 {
		t.Errorf("overrides %+v", ol)
	}

	// Deleting the consumer removes its keys, entries and overrides.
	c.must(204, "DELETE", fmt.Sprintf("/consumers/%d", *con.Id), nil, "If-Match", fmt.Sprintf(`"%d"`, *con.Version))
	c.must(200, "GET", "/access-lists", nil).decode(t, &all)
	c.must(200, "GET", "/overrides", nil).decode(t, &ol)
	if len(all.Items) != 1 || len(ol.Items) != 0 {
		t.Errorf("after the consumer's delete: %d entries (want the CIDR one), %d overrides", len(all.Items), len(ol.Items))
	}
	c.must(204, "DELETE", tenPath, nil, "If-Match", `"4"`)
}

// TestComposeControlPlane checks the compose stack's control plane (CP-19):
// migrate ran before it, it answers on 127.0.0.1:8080 as the app role, and
// its admin port is ready.
func TestComposeControlPlane(t *testing.T) {
	if status, _ := get(t, "http://127.0.0.1:8081/readyz"); status != 200 {
		t.Fatalf("admin /readyz: %d", status)
	}
	c := cpClient{t, "http://127.0.0.1:8080"}
	name := "it-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + fmt.Sprint(time.Now().UnixNano()%1e6)
	var p apiv1.Plan
	created := c.must(201, "POST", "/plans", map[string]any{"name": name})
	created.decode(t, &p)
	if created.header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id")
	}
	c.must(200, "GET", "/plans?name="+name, nil)
	c.must(204, "DELETE", fmt.Sprintf("/plans/%d", *p.Id), nil, "If-Match", created.etag())
}
