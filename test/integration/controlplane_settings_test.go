//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// TestControlPlaneSettings checks the config settings, the working set's
// ratelimit section (ADR-0045): read by every role, replaced by editors and
// admins with If-Match, checked like a config file's, a config edit that
// bumps the working-set revision and is audited, shown in the preview's
// diff (not its changes list, whose entity enum is closed), published in
// every snapshot, exported, and rolled back with config.
func TestControlPlaneSettings(t *testing.T) {
	c, iss, _ := startControlPlane(t, io.Discard)
	r := newRoles(t, c, iss)

	var st apiv1.ConfigSettings
	a := r.viewer.must(200, "GET", "/config/settings", nil)
	a.decode(t, &st)
	if a.etag() != `"1"` || st.Ratelimit.Replicas != nil || st.Ratelimit.RedisBudget != nil {
		t.Fatalf("new settings: ETag %s, %+v", a.etag(), st)
	}
	three := map[string]any{"ratelimit": map[string]any{"replicas": 3, "redis_budget": "8ms"}}
	for _, cl := range []cpClient{r.viewer, r.publisher, r.keyAdmin} {
		cl.must(403, "PUT", "/config/settings", three, "If-Match", `"1"`)
	}
	r.editor.must(428, "PUT", "/config/settings", three)
	r.editor.must(412, "PUT", "/config/settings", three, "If-Match", `"7"`)
	if e := r.editor.must(422, "PUT", "/config/settings", map[string]any{"ratelimit": map[string]any{"redis_budget": "2s"}}, "If-Match", `"1"`).apiErr(t); !strings.Contains(e.Error, "ratelimit.redis_budget") {
		t.Errorf("a budget over 1s: %+v", e)
	}
	r.editor.must(400, "PUT", "/config/settings", map[string]any{"ratelimit": map[string]any{"replica": 3}}, "If-Match", `"1"`)

	a = r.editor.must(200, "PUT", "/config/settings", three, "If-Match", `"1"`)
	a.decode(t, &st)
	if a.etag() != `"2"` || *st.Version != 2 || *st.Ratelimit.Replicas != 3 || *st.Ratelimit.RedisBudget != "8ms" {
		t.Fatalf("PUT: ETag %s, %+v", a.etag(), st)
	}
	v, tag := validate(t, r.viewer)
	if tag != `"2"` || !v.Valid || len(v.Changes) != 0 || !strings.Contains(v.Diff, "+ratelimit:\n+  replicas: 3\n") {
		t.Fatalf("validate after the PUT: %s %+v", tag, v)
	}
	r.publisher.must(200, "POST", "/config/publish", note("three replicas"), "If-Match", tag)
	if e, _ := exportYAML(t, r.viewer, "?version=1"); e != "version: 1\nratelimit:\n  replicas: 3\n  redis_budget: 8ms\n" {
		t.Errorf("version 1:\n%s", e)
	}

	// A second change, published, then a rollback to version 1 restores
	// the settings with the rest of config.
	r.admin.must(200, "PUT", "/config/settings", map[string]any{"ratelimit": map[string]any{}}, "If-Match", `"2"`)
	_, tag = validate(t, r.viewer)
	r.publisher.must(200, "POST", "/config/publish", note("defaults"), "If-Match", tag)
	if e, _ := exportYAML(t, r.viewer, ""); e != "{}\n" {
		t.Errorf("working set after the second publish:\n%s", e)
	}
	r.publisher.must(200, "POST", "/config/versions/1/rollback", note("back to three"), "If-Match", tag)
	a = r.viewer.must(200, "GET", "/config/settings", nil)
	a.decode(t, &st)
	if st.Ratelimit.Replicas == nil || *st.Ratelimit.Replicas != 3 || a.etag() != `"4"` {
		t.Errorf("after the rollback: ETag %s, %+v", a.etag(), st)
	}

	// One record per PUT, with the settings before and after.
	recs := auditPage(t, r.admin, "?entity=settings").Items
	if len(recs) != 2 || recs[1].Action != "settings.update" || recs[1].Actor.Sub != "sub-editor" || *recs[1].EntityId != "1" {
		t.Fatalf("settings records: %+v", recs)
	}
	diff, _ := json.Marshal(recs[1].Diff)
	if !strings.Contains(string(diff), `"after":{"ratelimit":{"redis_budget":"8ms","replicas":3}`) || !strings.Contains(string(diff), `"before":{"ratelimit":{}`) {
		t.Errorf("settings record diff %s", diff)
	}
}

// TestControlPlaneIdentityExport checks the identity snapshot and its
// export (CP-20, ADR-0037, ADR-0044): admin only; the identity version and
// every row a gateway receives, keys as prefix and SHA-256 only, revoked
// and expired keys and ended overrides left out; every identity change
// sends NOTIFY spillway_identity with its version at commit; no bearer
// token in a log line.
func TestControlPlaneIdentityExport(t *testing.T) {
	var logs syncBuffer
	c, iss, db := startControlPlane(t, &logs)
	r := newRoles(t, c, iss)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	listener := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	if _, err := listener.Exec(t.Context(), "LISTEN spillway_identity"); err != nil {
		t.Fatal(err)
	}

	importYAML(t, r.editor, 200, []byte("plans: [{name: free}]\n"), `"1"`, "")
	_, tag := validate(t, r.viewer)
	r.publisher.must(200, "POST", "/config/publish", note("a plan"), "If-Match", tag)
	plan, _ := lookup(t, r.viewer, "/plans", "free")
	tenant, _ := created(t, r.admin, "/tenants", map[string]any{"name": "acme", "plan_id": plan})
	notified := func(version string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		n, err := listener.WaitForNotification(ctx)
		if err != nil || n.Channel != "spillway_identity" || n.Payload != version {
			t.Errorf("notification %+v, %v; want spillway_identity %s", n, err, version)
		}
	}
	notified("2")
	consumer, _ := created(t, r.keyAdmin, "/consumers", map[string]any{"tenant_id": tenant, "name": "alice", "jwt_subject": "svc-alice"})
	notified("3")
	// Keys arrive with M5's keys-identity task; written directly here.
	if _, err := owner.Exec(t.Context(), `INSERT INTO api_keys (consumer_id, prefix, hash, expires_at, revoked_at, version) VALUES
		($1, 'AbCd1234', decode(repeat('ab', 32), 'hex'), NULL, NULL, 3),
		($1, 'Revoked1', decode(repeat('cd', 32), 'hex'), NULL, clock_timestamp(), 3),
		($1, 'Expired1', decode(repeat('ef', 32), 'hex'), clock_timestamp() - interval '1 hour', NULL, 3)`, consumer); err != nil {
		t.Fatal(err)
	}

	for _, cl := range []cpClient{r.viewer, r.editor, r.publisher, r.keyAdmin} {
		cl.must(403, "GET", "/identity/export", nil)
	}
	a := r.admin.must(200, "GET", "/identity/export", nil)
	y := string(a.body)
	if a.header.Get("Content-Type") != "application/yaml" || !strings.HasPrefix(y, "version: 3\n") ||
		!strings.Contains(y, "consumers:\n  - name: alice\n") || !strings.Contains(y, "    jwt_subject: svc-alice\n") ||
		!strings.Contains(y, "  - prefix: AbCd1234\n    hash: "+strings.Repeat("ab", 32)+"\n") ||
		strings.Contains(y, "Revoked1") || strings.Contains(y, "Expired1") ||
		!strings.Contains(y, "key_format: spw1\ntenants:\n  - id: ") || !strings.Contains(y, "    name: acme\n") {
		t.Errorf("identity export:\n%s", y)
	}
	for _, cl := range []cpClient{r.editor, r.publisher, r.admin, r.viewer, r.keyAdmin} {
		if sig := cl.token[strings.LastIndexByte(cl.token, '.')+1:]; strings.Contains(logs.String(), sig) {
			t.Error("a bearer token appears in a log line")
		}
	}
}
