//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/proxy"
)

// serveOn serves the API over database db as the app role with the
// upstream allow-list allow, logging to log; it returns a client without a
// token.
func serveOn(t *testing.T, db, allow string, iss *testIssuer, log io.Writer) cpClient {
	t.Helper()
	allowed, err := proxy.ParseAllowlist(allow)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(openStore(t, db), allowed, net.DefaultResolver, api.OIDC{Verifier: iss.verifier(t)}, slog.New(slog.NewJSONHandler(log, nil))).Handler())
	t.Cleanup(srv.Close)
	return cpClient{t: t, base: srv.URL}
}

// roles are clients with a token of each role.
type roles struct{ editor, publisher, admin, keyAdmin, viewer cpClient }

func newRoles(t *testing.T, c cpClient, iss *testIssuer) roles {
	return roles{
		editor:    c.as(iss.token(t, "sub-editor", "test-editor", "editor")),
		publisher: c.as(iss.token(t, "sub-publisher", "test-publisher", "publisher")),
		admin:     c.as(iss.token(t, "sub-admin", "test-admin", "admin")),
		keyAdmin:  c.as(iss.token(t, "sub-key-admin", "test-key-admin", "key-admin")),
		viewer:    c.as(iss.token(t, "sub-viewer", "test-viewer", "viewer")),
	}
}

func validate(t *testing.T, c cpClient) (apiv1.ConfigValidation, string) {
	t.Helper()
	a := c.must(200, "POST", "/config/validate", nil)
	var v apiv1.ConfigValidation
	a.decode(t, &v)
	if a.etag() != fmt.Sprintf(`"%d"`, v.WorkingSetRevision) {
		t.Fatalf("validate: ETag %s, revision %d", a.etag(), v.WorkingSetRevision)
	}
	return v, a.etag()
}

func note(s string) map[string]any { return map[string]any{"note": s} }

// created POSTs body to collection and returns the new entity's ID and
// ETag.
func created(t *testing.T, c cpClient, collection string, body any) (int64, string) {
	t.Helper()
	a := c.must(201, "POST", collection, body)
	var e struct{ Id int64 }
	a.decode(t, &e)
	return e.Id, a.etag()
}

var (
	ordersUpstream = map[string]any{"name": "orders", "endpoints": []any{map[string]any{"address": "172.29.0.10:8080"}}}
	ordersRoute    = map[string]any{"name": "orders-api", "upstream": "orders", "match": map[string]any{"path_prefix": "/v1/orders"}, "cost": 2}
)

func gcraPolicy(name, routes string) map[string]any {
	return map[string]any{"name": name, "level": "consumer", "match": map[string]any{"routes": []string{routes}}, "key_template": "{consumer}",
		"algorithm": "gcra", "limits": []any{map[string]any{"rate": 100, "period": "1s", "burst": 200}}}
}

// TestControlPlanePublish checks validate and publish on Postgres (CP-02,
// CP-03, ADR-0037): the preview and its ETag, If-Match on the working-set
// revision, the change note, nothing to publish, validation errors that
// write nothing, versions and their snapshots, diffs, the audit record and
// the NOTIFY; and no bearer token in a log line, a record or a version.
func TestControlPlanePublish(t *testing.T) {
	var logs syncBuffer
	c, iss, db := startControlPlane(t, &logs)
	r := newRoles(t, c, iss)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	listen := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	if _, err := listen.Exec(t.Context(), "LISTEN spillway_config"); err != nil {
		t.Fatal(err)
	}

	v, tag := validate(t, r.viewer)
	if v.WorkingSetRevision != 1 || tag != `"1"` || v.BaseVersion != nil || !v.Valid || len(v.Errors) != 0 || len(v.Changes) != 0 || v.Diff != "" {
		t.Fatalf("an empty working set: %+v", v)
	}

	created(t, r.editor, "/upstreams", ordersUpstream)
	created(t, r.editor, "/routes", ordersRoute)
	policyID, _ := created(t, r.editor, "/policies", gcraPolicy("per-consumer", "orders-api"))
	created(t, r.editor, "/plans", map[string]any{"name": "free"})
	v, tag = validate(t, r.viewer)
	if v.WorkingSetRevision != 5 || !v.Valid || len(v.Changes) != 4 || v.Changes[0].Entity != "upstream" || v.Changes[0].Change != "added" {
		t.Fatalf("preview: %+v", v)
	}
	if !strings.HasPrefix(v.Diff, "--- no published version\n+++ working set (revision 5)\n@@ -0,0 +1,") || !strings.Contains(v.Diff, "\n+  - name: orders-api\n") {
		t.Errorf("diff:\n%s", v.Diff)
	}

	// The publish rules, before anything is written.
	r.editor.must(403, "POST", "/config/publish", note("x"), "If-Match", tag)
	r.publisher.must(428, "POST", "/config/publish", note("x"))
	if e := r.publisher.must(412, "POST", "/config/publish", note("x"), "If-Match", `"4"`).apiErr(t); e.Details == nil || *(*e.Details)[0].Version != 5 {
		t.Errorf("stale If-Match: %+v", e)
	}
	r.publisher.must(400, "POST", "/config/publish", nil, "If-Match", tag)
	r.publisher.must(422, "POST", "/config/publish", note(""), "If-Match", tag)
	r.publisher.must(422, "POST", "/config/publish", note(strings.Repeat("é", 2001)), "If-Match", tag)
	r.publisher.must(400, "POST", "/config/publish", map[string]any{"note": "x", "version": 9}, "If-Match", tag)

	a := r.publisher.must(200, "POST", "/config/publish", note(strings.Repeat("é", 2000)), "If-Match", tag)
	var v1 apiv1.ConfigVersion
	a.decode(t, &v1)
	if v1.Version != 1 || v1.Kind != "publish" || v1.RollbackOf != nil || v1.WorkingSetRevision != 5 || v1.PublishedBy != "sub-publisher" ||
		v1.SchemaVersion != (apiv1.SchemaVersion{Major: 1, Minor: 0}) || v1.Snapshot != nil {
		t.Errorf("version 1: %s", a.body)
	}
	waitNotify(t, listen, "1")

	// The same preview again: nothing to publish.
	if e := r.admin.must(409, "POST", "/config/publish", note("again"), "If-Match", tag).apiErr(t); !strings.Contains(e.Error, "nothing to publish") {
		t.Errorf("unchanged working set: %+v", e)
	}
	v, _ = validate(t, r.viewer)
	if *v.BaseVersion != 1 || len(v.Changes) != 0 || v.Diff != "" {
		t.Errorf("after the publish: %+v", v)
	}

	// An invalid working set: validate reports, publish writes nothing.
	var route apiv1.Route
	routeID, routeTag := lookup(t, r.viewer, "/routes", "orders-api")
	broken := maps.Clone(ordersRoute)
	broken["upstream"] = "payments"
	r.editor.must(200, "PUT", fmt.Sprintf("/routes/%d", routeID), broken, "If-Match", routeTag).decode(t, &route)
	v, tag = validate(t, r.viewer)
	if v.Valid || len(v.Errors) != 1 || *v.Errors[0].Entity != "route" || *v.Errors[0].Id != routeID || *v.Errors[0].Path != "upstream" ||
		len(v.Changes) != 1 || v.Changes[0].Change != "changed" || !strings.Contains(v.Diff, "-    upstream: orders\n+    upstream: payments\n") {
		t.Errorf("invalid preview: %+v", v)
	}
	e := r.publisher.must(422, "POST", "/config/publish", note("broken"), "If-Match", tag).apiErr(t)
	if e.Code != apiv1.ErrorCodeValidationFailed || e.Details == nil || len(*e.Details) != 1 || !strings.Contains(e.Error, `route "orders-api": upstream "payments" does not exist`) {
		t.Errorf("invalid publish: %+v", e)
	}

	// Fixed: version 2.
	r.editor.must(200, "PUT", fmt.Sprintf("/routes/%d", routeID), ordersRoute, "If-Match", fmt.Sprintf(`"%d"`, *route.Version))
	_, tag = validate(t, r.viewer)
	var v2 apiv1.ConfigVersion
	r.admin.must(200, "POST", "/config/publish", note("route fixed"), "If-Match", tag).decode(t, &v2)
	if v2.Version != 2 || v2.WorkingSetRevision != 7 {
		t.Errorf("version 2: %+v", v2)
	}
	waitNotify(t, listen, "2")

	// Versions: a page, one with its snapshot, diffs.
	var l apiv1.ConfigVersionList
	r.viewer.must(200, "GET", "/config/versions?limit=1", nil).decode(t, &l)
	if len(l.Items) != 1 || l.Items[0].Version != 2 || l.NextCursor == nil || l.Items[0].Snapshot != nil {
		t.Fatalf("page 1: %+v", l)
	}
	r.viewer.must(200, "GET", "/config/versions?limit=1&cursor="+*l.NextCursor, nil).decode(t, &l)
	if len(l.Items) != 1 || l.Items[0].Version != 1 || l.NextCursor != nil || l.Items[0].Note != strings.Repeat("é", 2000) {
		t.Fatalf("page 2: %+v", l)
	}
	var one apiv1.ConfigVersion
	got := r.viewer.must(200, "GET", "/config/versions/1", nil)
	got.decode(t, &one)
	snap, _ := json.Marshal(one.Snapshot)
	if !strings.Contains(string(snap), `"version":1`) || !strings.Contains(string(snap), fmt.Sprintf(`"id":%d,"key_template":"{consumer}"`, policyID)) {
		t.Errorf("snapshot of version 1: %s", snap)
	}
	r.viewer.must(404, "GET", "/config/versions/9", nil)
	var d apiv1.ConfigDiff
	r.viewer.must(200, "GET", "/config/versions/2/diff", nil).decode(t, &d)
	if d.FromVersion != 1 || d.ToVersion != 2 || len(d.Changes) != 0 || d.Diff != "" {
		t.Errorf("2 against 1 (same config): %+v", d)
	}
	r.viewer.must(200, "GET", "/config/versions/1/diff", nil).decode(t, &d)
	if d.FromVersion != 0 || len(d.Changes) != 4 || !strings.HasPrefix(d.Diff, "--- version 0\n+++ version 1\n") {
		t.Errorf("1 against the empty config: %+v", d)
	}
	r.viewer.must(404, "GET", "/config/versions/2/diff?against=9", nil)
	r.viewer.must(400, "GET", "/config/versions/2/diff?against=-1", nil)
	r.viewer.must(404, "GET", "/config/versions/0/diff", nil)

	// The audit record: the note, the version, the changes; no snapshot.
	records := auditPage(t, r.admin, "?entity=config")
	if len(records.Items) != 2 {
		t.Fatalf("config records: %+v", records.Items)
	}
	rec := records.Items[1]
	diff, _ := json.Marshal(rec.Diff)
	if rec.Action != "config.publish" || rec.Actor.Sub != "sub-publisher" || *rec.EntityId != "1" || *rec.ConfigVersion != 1 ||
		*rec.Note != strings.Repeat("é", 2000) || !strings.Contains(string(diff), `"from_version":null`) ||
		!strings.Contains(string(diff), `"to_version":1`) || !strings.Contains(string(diff), `{"change":"added","entity":"route","id":`) || strings.Contains(string(diff), "endpoints") {
		t.Errorf("publish record %+v, diff %s", rec, diff)
	}
	var n int
	if err := owner.QueryRow(t.Context(), "SELECT count(*) FROM audit_log WHERE action = 'config.publish'").Scan(&n); err != nil || n != 2 {
		t.Errorf("%d publish records, %v: refused publishes write none", n, err)
	}

	var stored string
	if err := owner.QueryRow(t.Context(), `SELECT (SELECT string_agg(row_to_json(a)::text, '') FROM audit_log a) ||
		(SELECT string_agg(row_to_json(v)::text, '') FROM config_versions v)`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, cl := range []cpClient{r.editor, r.publisher, r.admin, r.viewer} {
		if sig := cl.token[strings.LastIndexByte(cl.token, '.')+1:]; strings.Contains(logs.String(), sig) || strings.Contains(stored, sig) {
			t.Error("a bearer token appears in a log line, an audit record or a config version")
		}
	}
}

// lookup returns the ID and ETag of the entity of collection named name.
func lookup(t *testing.T, c cpClient, collection, name string) (int64, string) {
	t.Helper()
	var l struct {
		Items []struct {
			Id      int64
			Version int64
		}
	}
	c.must(200, "GET", collection+"?name="+name, nil).decode(t, &l)
	if len(l.Items) != 1 {
		t.Fatalf("%s?name=%s: %+v", collection, name, l)
	}
	return l.Items[0].Id, fmt.Sprintf(`"%d"`, l.Items[0].Version)
}

// waitNotify waits for the NOTIFY of a publish on conn.
func waitNotify(t *testing.T, conn *pgx.Conn, version string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	n, err := conn.WaitForNotification(ctx)
	if err != nil || n.Channel != "spillway_config" || n.Payload != version {
		t.Errorf("notification %+v, %v; want spillway_config %s", n, err, version)
	}
}

// TestControlPlanePublishRaces checks If-Match on the working-set revision
// under concurrency (CP-02, ADR-0037): two publishers of one preview make
// one version; an edit after the preview stops its publish; and versions
// stay monotonic and gapless, committing in order, with many editors and
// publishers at once.
func TestControlPlanePublishRaces(t *testing.T) {
	c, iss, db := startControlPlane(t, io.Discard)
	r := newRoles(t, c, iss)
	created(t, r.editor, "/plans", map[string]any{"name": "p0"})
	_, tag := validate(t, r.viewer)

	// Two publishers of one preview.
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			statuses[i] = r.publisher.call("POST", "/config/publish", note("same preview"), "If-Match", tag).status
		})
	}
	wg.Wait()
	slices.Sort(statuses)
	if statuses[0] != 200 || statuses[1] != 409 {
		t.Errorf("two publishers of one preview: %v, want one 200 and one 409", statuses)
	}

	// An edit after the preview.
	created(t, r.editor, "/plans", map[string]any{"name": "p1"})
	_, tag = validate(t, r.viewer)
	created(t, r.editor, "/plans", map[string]any{"name": "p2"})
	if e := r.publisher.must(412, "POST", "/config/publish", note("stale preview"), "If-Match", tag).apiErr(t); e.Details == nil || fmt.Sprintf(`"%d"`, *(*e.Details)[0].Version-1) != tag {
		t.Errorf("edit after the preview: %+v", e)
	}

	// Many editors and publishers: each edits, previews and publishes until
	// its edit is published, by itself or another. Workers report with
	// Errorf, never Fatalf.
	const workers, rounds = 6, 5
	var mu sync.Mutex
	var published []int64
	for w := range workers {
		wg.Go(func() {
			for i := range rounds {
				if a := r.editor.call("POST", "/plans", map[string]any{"name": fmt.Sprintf("w%d-%d", w, i)}); a.status != 201 {
					t.Errorf("edit: %d %s", a.status, a.body)
					return
				}
				for {
					a := r.viewer.call("POST", "/config/validate", nil)
					if a.status != 200 {
						t.Errorf("validate: %d %s", a.status, a.body)
						return
					}
					a = r.publisher.call("POST", "/config/publish", note(fmt.Sprintf("w%d-%d", w, i)), "If-Match", a.etag())
					if a.status == 200 {
						var v apiv1.ConfigVersion
						if err := json.Unmarshal(a.body, &v); err != nil {
							t.Error(err)
						}
						mu.Lock()
						published = append(published, v.Version)
						mu.Unlock()
						break
					}
					if a.status == 409 {
						break // another publisher shipped this edit
					}
					if a.status != 412 {
						t.Errorf("publish: %d %s", a.status, a.body)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	if len(published) == 0 || len(published) > workers*rounds {
		t.Fatalf("%d publishes landed, want 1 to %d", len(published), workers*rounds)
	}
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	rows, err := owner.Query(t.Context(), "SELECT version, working_set_revision, published_at FROM config_versions ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	var want, lastRev int64 = 1, 0
	var lastAt time.Time
	for rows.Next() {
		var v, rev int64
		var at time.Time
		if err := rows.Scan(&v, &rev, &at); err != nil {
			t.Fatal(err)
		}
		if v != want || rev <= lastRev || at.Before(lastAt) {
			t.Errorf("version %d (want %d), revision %d after %d, published %v after %v", v, want, rev, lastRev, at, lastAt)
		}
		want, lastRev, lastAt = want+1, rev, at
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if want-1 != int64(1+len(published)) {
		t.Errorf("%d versions, want %d", want-1, 1+len(published))
	}
}

// TestControlPlaneRollback checks rollback on Postgres (CP-04, D5,
// ADR-0037): it publishes a copy of the old version as a new one, resets
// the working tables to it under the original IDs, never touches identity,
// is refused while identity refers to what the old version lacks, and
// needs If-Match and a note.
func TestControlPlaneRollback(t *testing.T) {
	c, iss, db := startControlPlane(t, io.Discard)
	r := newRoles(t, c, iss)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	listen := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	if _, err := listen.Exec(t.Context(), "LISTEN spillway_config"); err != nil {
		t.Fatal(err)
	}

	upID, _ := created(t, r.editor, "/upstreams", ordersUpstream)
	routeID, _ := created(t, r.editor, "/routes", ordersRoute)
	policyID, _ := created(t, r.editor, "/policies", gcraPolicy("per-consumer", "orders-api"))
	planID, _ := created(t, r.editor, "/plans", map[string]any{"name": "free"})
	_, tag := validate(t, r.viewer)
	r.publisher.must(200, "POST", "/config/publish", note("v1"), "If-Match", tag)
	waitNotify(t, listen, "1")
	var v1 apiv1.ConfigVersion
	r.viewer.must(200, "GET", "/config/versions/1", nil).decode(t, &v1)
	before := workingSet(t, r.viewer)

	// Identity on version 1's plan and policy.
	tenantID, _ := created(t, r.admin, "/tenants", map[string]any{"name": "acme", "plan_id": planID})
	consumerID, _ := created(t, r.keyAdmin, "/consumers", map[string]any{"tenant_id": tenantID, "name": "bot", "plan_id": planID})
	created(t, r.keyAdmin, "/overrides", map[string]any{"consumer_id": consumerID, "policy_id": policyID, "limits": []any{map[string]any{"rate": 5, "period": "1s"}}})

	// Version 2: a renamed route and the policy on it, a new upstream and
	// policy, an endpoint drained, an edit left unpublished.
	_, routeTag := lookup(t, r.viewer, "/routes", "orders-api")
	renamed := map[string]any{"name": "orders-v2", "upstream": "orders", "match": map[string]any{"path_prefix": "/v2/orders"}}
	r.editor.must(200, "PUT", fmt.Sprintf("/routes/%d", routeID), renamed, "If-Match", routeTag)
	_, policyTag := lookup(t, r.viewer, "/policies", "per-consumer")
	r.editor.must(200, "PUT", fmt.Sprintf("/policies/%d", policyID), gcraPolicy("per-consumer", "orders-v2"), "If-Match", policyTag)
	created(t, r.editor, "/upstreams", map[string]any{"name": "payments", "endpoints": []any{map[string]any{"address": "172.29.0.20:8080"}}})
	burstCap := gcraPolicy("burst-cap", "*")
	burstCap["mode"] = "shadow"
	newPolicyID, _ := created(t, r.editor, "/policies", burstCap)
	var up apiv1.Upstream
	r.viewer.must(200, "GET", fmt.Sprintf("/upstreams/%d", upID), nil).decode(t, &up)
	r.editor.must(200, "POST", fmt.Sprintf("/upstreams/%d/endpoints/%d/drain", upID, *up.Endpoints[0].Id), nil, "If-Match", fmt.Sprintf(`"%d"`, *up.Version))
	_, tag = validate(t, r.viewer)
	r.publisher.must(200, "POST", "/config/publish", note("v2"), "If-Match", tag)
	waitNotify(t, listen, "2")
	created(t, r.editor, "/plans", map[string]any{"name": "unpublished"})

	// The rules.
	_, tag = validate(t, r.viewer)
	r.editor.must(403, "POST", "/config/versions/1/rollback", note("x"), "If-Match", tag)
	r.publisher.must(428, "POST", "/config/versions/1/rollback", note("x"))
	r.publisher.must(412, "POST", "/config/versions/1/rollback", note("x"), "If-Match", `"3"`)
	r.publisher.must(422, "POST", "/config/versions/1/rollback", note(""), "If-Match", tag)
	r.publisher.must(404, "POST", "/config/versions/7/rollback", note("x"), "If-Match", tag)

	// Identity refers to burst-cap, which version 1 lacks: refused, and
	// nothing changes.
	created(t, r.keyAdmin, "/overrides", map[string]any{"consumer_id": consumerID, "policy_id": newPolicyID, "limits": []any{map[string]any{"rate": 5, "period": "1s"}}})
	identity := identityState(t, owner)
	e := r.publisher.must(422, "POST", "/config/versions/1/rollback", note("back"), "If-Match", tag).apiErr(t)
	if e.Details == nil || *(*e.Details)[0].Entity != "override" || *(*e.Details)[0].Path != "policy_id" ||
		!strings.Contains(e.Error, fmt.Sprintf("refers to policy %d, which this config does not have", newPolicyID)) {
		t.Errorf("rollback past an override: %+v", e)
	}
	if _, tag2 := validate(t, r.viewer); tag2 != tag {
		t.Errorf("a refused rollback moved the revision from %s to %s", tag, tag2)
	}
	if got := identityState(t, owner); got != identity {
		t.Errorf("a refused rollback changed identity:\n%s\nwant\n%s", got, identity)
	}
	var overrides apiv1.OverrideList
	r.viewer.must(200, "GET", fmt.Sprintf("/overrides?policy_id=%d", newPolicyID), nil).decode(t, &overrides)
	r.keyAdmin.must(204, "DELETE", fmt.Sprintf("/overrides/%d", *overrides.Items[0].Id), nil, "If-Match", fmt.Sprintf(`"%d"`, *overrides.Items[0].Version))
	identity = identityState(t, owner)

	// The rollback: version 3, a copy of version 1.
	a := r.publisher.must(200, "POST", "/config/versions/1/rollback", note("back to v1"), "If-Match", tag)
	var v3 apiv1.ConfigVersion
	a.decode(t, &v3)
	if v3.Version != 3 || v3.Kind != "rollback" || v3.RollbackOf == nil || *v3.RollbackOf != 1 || v3.Note != "back to v1" {
		t.Errorf("rollback: %s", a.body)
	}
	waitNotify(t, listen, "3")
	var got3 apiv1.ConfigVersion
	r.viewer.must(200, "GET", "/config/versions/3", nil).decode(t, &got3)
	(*got3.Snapshot)["version"] = float64(1)
	if s1, s3 := mustJSON(t, v1.Snapshot), mustJSON(t, got3.Snapshot); s1 != s3 {
		t.Errorf("version 3's snapshot is not version 1's:\n%s\n%s", s1, s3)
	}
	var d apiv1.ConfigDiff
	r.viewer.must(200, "GET", "/config/versions/3/diff?against=1", nil).decode(t, &d)
	if len(d.Changes) != 0 || d.Diff != "" {
		t.Errorf("3 against 1: %+v", d)
	}

	// The working set is version 1 again, under the original IDs, and the
	// unpublished edit is gone; the preview shows nothing to publish.
	after := workingSet(t, r.viewer)
	if before != after {
		t.Errorf("working set after the rollback:\n%s\nwant version 1's:\n%s", after, before)
	}
	v, tag3 := validate(t, r.viewer)
	if v.WorkingSetRevision != v3.WorkingSetRevision || *v.BaseVersion != 3 || len(v.Changes) != 0 || v.Diff != "" || tag3 == tag {
		t.Errorf("preview after the rollback: %+v, revision %d", v, v3.WorkingSetRevision)
	}
	r.publisher.must(409, "POST", "/config/publish", note("again"), "If-Match", tag3)

	// Identity untouched: same version, same rows.
	if got := identityState(t, owner); got != identity {
		t.Errorf("identity changed:\n%s\nwant\n%s", got, identity)
	}

	// The audit record.
	rec := auditPage(t, r.admin, "?entity=config&limit=1").Items[0]
	diff, _ := json.Marshal(rec.Diff)
	if rec.Action != "config.rollback" || *rec.ConfigVersion != 3 || *rec.Note != "back to v1" || !strings.Contains(string(diff), `"rollback_of":1`) ||
		!strings.Contains(string(diff), `"from_version":2`) || !strings.Contains(string(diff), `"change":"removed","entity":"upstream"`) {
		t.Errorf("rollback record %+v %s", rec, diff)
	}

	// Rolling back to the latest version is allowed.
	r.publisher.must(200, "POST", "/config/versions/3/rollback", note("again"), "If-Match", tag3)
}

// workingSet is every config entity as the API lists them, without version
// and timestamps.
func workingSet(t *testing.T, c cpClient) string {
	t.Helper()
	var out strings.Builder
	for _, col := range []string{"/upstreams", "/routes", "/policies", "/plans"} {
		var l struct{ Items []map[string]any }
		c.must(200, "GET", col+"?limit=500", nil).decode(t, &l)
		for _, e := range l.Items {
			delete(e, "version")
			delete(e, "created_at")
			delete(e, "updated_at")
			if eps, ok := e["endpoints"].([]any); ok {
				for _, ep := range eps {
					delete(ep.(map[string]any), "updated_at")
				}
			}
			out.WriteString(col + " " + mustJSON(t, e) + "\n")
		}
	}
	return out.String()
}

// identityState is the identity version and every identity row.
func identityState(t *testing.T, owner *pgx.Conn) string {
	t.Helper()
	var s string
	err := owner.QueryRow(t.Context(), `SELECT concat_ws(E'\n',
		(SELECT version::text FROM identity_state),
		(SELECT string_agg(row_to_json(x)::text, E'\n' ORDER BY id) FROM tenants x),
		(SELECT string_agg(row_to_json(x)::text, E'\n' ORDER BY id) FROM consumers x),
		(SELECT string_agg(row_to_json(x)::text, E'\n' ORDER BY id) FROM overrides x),
		(SELECT string_agg(row_to_json(x)::text, E'\n' ORDER BY id) FROM access_lists x),
		(SELECT string_agg(row_to_json(x)::text, E'\n' ORDER BY id) FROM api_keys x))`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestControlPlanePublishAllowList checks SEC-06 in API validation on the
// whole config (ADR-0037): an endpoint that a narrower allow-list, or a
// rollback to an old version, would let through is refused at validate,
// publish and rollback.
func TestControlPlanePublishAllowList(t *testing.T) {
	db := migratedDatabase(t)
	iss := newTestIssuer(t)
	wide := newRoles(t, serveOn(t, db, "172.29.0.0/16,10.0.0.0/8", iss, io.Discard), iss)
	narrow := newRoles(t, serveOn(t, db, "172.29.0.0/16", iss, io.Discard), iss)

	upID, upTag := created(t, wide.editor, "/upstreams", map[string]any{"name": "orders", "endpoints": []any{map[string]any{"address": "10.1.2.3:8080"}}})
	created(t, wide.editor, "/routes", ordersRoute)
	_, tag := validate(t, wide.viewer)
	wide.publisher.must(200, "POST", "/config/publish", note("on 10/8"), "If-Match", tag)

	// The narrower control plane refuses the same working set.
	created(t, wide.editor, "/plans", map[string]any{"name": "free"})
	v, tag := validate(t, narrow.viewer)
	if v.Valid || len(v.Errors) != 1 || *v.Errors[0].Entity != "upstream" || *v.Errors[0].Id != upID || *v.Errors[0].Path != "endpoints" ||
		!strings.Contains(*v.Errors[0].Message, `endpoint "10.1.2.3:8080" is not in the allowed upstream networks (SEC-06)`) {
		t.Errorf("narrow validate: %+v", v)
	}
	narrow.publisher.must(422, "POST", "/config/publish", note("x"), "If-Match", tag)

	// Moved inside: publishes; a rollback to version 1 is refused.
	up := map[string]any{"name": "orders", "endpoints": []any{map[string]any{"address": "172.29.0.10:8080"}}}
	narrow.editor.must(200, "PUT", fmt.Sprintf("/upstreams/%d", upID), up, "If-Match", upTag)
	_, tag = validate(t, narrow.viewer)
	narrow.publisher.must(200, "POST", "/config/publish", note("on the compose network"), "If-Match", tag)
	if e := narrow.publisher.must(422, "POST", "/config/versions/1/rollback", note("back"), "If-Match", tag).apiErr(t); !strings.Contains(e.Error, "10.1.2.3:8080") {
		t.Errorf("rollback to a blocked endpoint: %+v", e)
	}
	wide.publisher.must(200, "POST", "/config/versions/1/rollback", note("back"), "If-Match", tag)
}
