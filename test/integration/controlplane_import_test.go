//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// sendYAML calls path with a YAML body, as an import is sent.
func (c cpClient) sendYAML(method, path string, body []byte, header ...string) cpAnswer {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+api.BasePath+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
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

// importYAML imports body at revision tag and fails unless the answer is
// want; it returns the answer.
func importYAML(t *testing.T, c cpClient, want int, body []byte, tag string, query string) cpAnswer {
	t.Helper()
	a := c.sendYAML("POST", "/config/import"+query, body, "If-Match", tag)
	if a.status != want {
		t.Fatalf("import: %d %s, want %d", a.status, a.body, want)
	}
	return a
}

func importResult(t *testing.T, a cpAnswer) apiv1.ConfigImport {
	t.Helper()
	var r apiv1.ConfigImport
	a.decode(t, &r)
	if a.etag() != fmt.Sprintf(`"%d"`, r.WorkingSetRevision) {
		t.Fatalf("import: ETag %s, revision %d", a.etag(), r.WorkingSetRevision)
	}
	return r
}

// exportYAML exports the working set (query "") or a version and returns
// the YAML and the ETag.
func exportYAML(t *testing.T, c cpClient, query string) (string, string) {
	t.Helper()
	a := c.must(200, "GET", "/config/export"+query, nil)
	if ct := a.header.Get("Content-Type"); ct != "application/yaml" {
		t.Fatalf("export: Content-Type %q", ct)
	}
	return string(a.body), a.etag()
}

func changeList(cs []apiv1.ConfigChange) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s %s %s", c.Change, c.Entity, c.Name))
	}
	return out
}

// TestControlPlaneImportExport checks YAML import and export through the
// API on Postgres (CP-05, ADR-0037, ADR-0044): export → import → export is
// byte-stable and a no-op import writes nothing; If-Match on the
// working-set revision; matching by id, then name; renames keep ids,
// unchanged rows keep their versions; endpoint ids; refused files write
// nothing; published versions export with their version; RBAC; one audit
// record per import with the changes and the note; no token in logs or
// records.
func TestControlPlaneImportExport(t *testing.T) {
	var logs syncBuffer
	c, iss, db := startControlPlane(t, &logs)
	r := newRoles(t, c, iss)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)

	y, tag := exportYAML(t, r.viewer, "")
	if y != "{}\n" || tag != `"1"` {
		t.Fatalf("empty export %q, ETag %s", y, tag)
	}

	file := []byte(`# a GitOps file
version: 9
ratelimit: { replicas: 3 }
upstreams:
  - name: orders
    endpoints: [{ address: '172.29.0.10:8080' }, { address: '172.29.0.11:8080', draining: true }]
  - name: catalog
    endpoints: [{ address: '172.29.0.12:8080' }]
routes:
  - { name: orders-api, match: { path_prefix: /v1/orders }, upstream: orders, cost: 2 }
  - { name: catalog-api, match: { path_prefix: /v1/catalog }, upstream: catalog }
policies:
  - { name: per-consumer, level: consumer, match: { routes: ['*'] }, key_template: '{consumer}', algorithm: gcra, limits: [{ rate: 100, period: 1s, burst: 200 }] }
plans:
  - name: free
`)
	// RBAC: editor and admin import; nobody else.
	for _, cl := range []cpClient{r.viewer, r.publisher, r.keyAdmin} {
		if a := cl.sendYAML("POST", "/config/import", file, "If-Match", tag); a.status != 403 {
			t.Errorf("import as a non-editor: %d %s", a.status, a.body)
		}
	}
	if a := r.editor.sendYAML("POST", "/config/import", file); a.status != 428 {
		t.Errorf("import without If-Match: %d %s", a.status, a.body)
	}
	if a := r.editor.sendYAML("POST", "/config/import", file, "If-Match", `"7"`); a.status != 412 || !strings.Contains(string(a.body), "the current version is 1") {
		t.Errorf("import with a stale If-Match: %d %s", a.status, a.body)
	}

	res := importResult(t, importYAML(t, r.editor, 200, file, tag, "?note=from+git+abc123"))
	if res.WorkingSetRevision != 2 || !slices.Equal(changeList(res.Changes), []string{
		"added upstream catalog", "added upstream orders", "added route catalog-api", "added route orders-api", "added policy per-consumer", "added plan free",
	}) || len(res.Ignored) != 1 || !strings.HasPrefix(res.Ignored[0], "ratelimit: not kept") {
		t.Fatalf("first import: %+v", res)
	}

	// Export → import → export: the same bytes, and the import writes
	// nothing (no revision, no record).
	e1, tag := exportYAML(t, r.viewer, "")
	if tag != `"2"` || strings.Contains(e1, "version") || strings.Contains(e1, "ratelimit") || !strings.HasPrefix(e1, "upstreams:\n  - name: catalog\n") ||
		!strings.Contains(e1, "        draining: true\n") {
		t.Fatalf("export, ETag %s:\n%s", tag, e1)
	}
	res = importResult(t, importYAML(t, r.editor, 200, []byte(e1), tag, ""))
	if res.WorkingSetRevision != 2 || len(res.Changes) != 0 || len(res.Ignored) != 0 {
		t.Errorf("re-import of the export: %+v", res)
	}
	if e2, tag2 := exportYAML(t, r.viewer, ""); e2 != e1 || tag2 != tag {
		t.Errorf("export after re-import differs, ETag %s:\n%s\nwas:\n%s", tag2, e2, e1)
	}
	// The original file again: matched by name, nothing changes.
	if res = importResult(t, importYAML(t, r.admin, 200, file, tag, "")); res.WorkingSetRevision != 2 || len(res.Changes) != 0 {
		t.Errorf("re-import of the file: %+v", res)
	}

	// Publish, then export the version: its number first, then the same
	// entities; importing it is a no-op too (version ignored).
	v, tag := validate(t, r.viewer)
	if !v.Valid {
		t.Fatalf("the imported working set does not validate: %+v", v.Errors)
	}
	r.publisher.must(200, "POST", "/config/publish", note("first"), "If-Match", tag)
	ev, vtag := exportYAML(t, r.viewer, "?version=1")
	if ev != "version: 1\n"+e1 || vtag != "" {
		t.Errorf("version export (ETag %q):\n%s", vtag, ev)
	}
	if res = importResult(t, importYAML(t, r.editor, 200, []byte(ev), tag, "")); len(res.Changes) != 0 {
		t.Errorf("import of the version export: %+v", res)
	}
	r.viewer.must(404, "GET", "/config/export?version=2", nil)

	// An edit of the export: rename orders-api (its id kept) and change its
	// cost, swap the two upstreams' names by id, drop the policy, add a plan;
	// catalog-api and free are untouched and keep their versions.
	ordersRoute, _ := lookup(t, r.viewer, "/routes", "orders-api")
	catalogRoute, catalogTag := lookup(t, r.viewer, "/routes", "catalog-api")
	ordersUp, _ := lookup(t, r.viewer, "/upstreams", "orders")
	catalogUp, _ := lookup(t, r.viewer, "/upstreams", "catalog")
	_, freeTag := lookup(t, r.viewer, "/plans", "free")
	var up struct{ Endpoints []struct{ Id int64 } }
	r.viewer.must(200, "GET", fmt.Sprint("/upstreams/", ordersUp), nil).decode(t, &up)
	edited := fmt.Sprintf(`upstreams:
  - name: catalog
    id: %d
    endpoints: [{ address: '172.29.0.10:8080' }, { address: '172.29.0.11:8080', draining: true }]
  - name: orders
    id: %d
    endpoints: [{ address: '172.29.0.12:8080' }]
routes:
  - { name: orders-v2, id: %d, match: { path_prefix: /v1/orders }, upstream: orders, cost: 3 }
  - { name: catalog-api, match: { path_prefix: /v1/catalog }, upstream: catalog }
plans:
  - name: free
  - name: gold
`, ordersUp, catalogUp, ordersRoute)
	res = importResult(t, importYAML(t, r.editor, 200, []byte(edited), tag, ""))
	if want := []string{"changed upstream catalog", "changed upstream orders", "changed route orders-v2", "removed policy per-consumer", "added plan gold"}; res.WorkingSetRevision != 3 ||
		!slices.Equal(changeList(res.Changes), want) {
		t.Fatalf("edited import: %+v\nwant %v\nfile:\n%s", res, want, edited)
	}
	if id, _ := lookup(t, r.viewer, "/routes", "orders-v2"); id != ordersRoute {
		t.Errorf("renamed route: id %d, was %d", id, ordersRoute)
	}
	if id, _ := lookup(t, r.viewer, "/upstreams", "catalog"); id != ordersUp {
		t.Errorf("upstream renamed to catalog: id %d, want %d (the old orders)", id, ordersUp)
	}
	if id, _ := lookup(t, r.viewer, "/upstreams", "orders"); id != catalogUp {
		t.Errorf("upstream renamed to orders: id %d, want %d", id, catalogUp)
	}
	if id, etag := lookup(t, r.viewer, "/routes", "catalog-api"); id != catalogRoute || etag != catalogTag {
		t.Errorf("untouched route: id %d ETag %s, was %d %s", id, etag, catalogRoute, catalogTag)
	}
	if _, etag := lookup(t, r.viewer, "/plans", "free"); etag != freeTag {
		t.Errorf("untouched plan: ETag %s, was %s", etag, freeTag)
	}
	var up2 struct{ Endpoints []struct{ Id int64 } }
	r.viewer.must(200, "GET", fmt.Sprint("/upstreams/", ordersUp), nil).decode(t, &up2)
	if len(up2.Endpoints) != 2 || up2.Endpoints[0].Id != up.Endpoints[0].Id || up2.Endpoints[1].Id != up.Endpoints[1].Id {
		t.Errorf("endpoint ids %+v, were %+v", up2, up)
	}
	_, tag = exportYAML(t, r.viewer, "")

	// Refused files write nothing: an id that does not exist, an endpoint
	// id that is not its address's, identity, an entity rule, SEC-06.
	for _, bad := range []struct{ body, msg string }{
		{"plans:\n  - name: free\n    id: 999999\n", `plan "free": id 999999 does not exist; ids are assigned by the control plane`},
		{fmt.Sprintf("upstreams:\n  - name: orders\n    endpoints: [{address: '172.29.0.99:8080', id: %d}]\n", up.Endpoints[0].Id), "is not the id of 172.29.0.99:8080 in this upstream"},
		{"upstreams:\n  - name: new\n    endpoints: [{address: '172.29.0.99:8080', id: 5}]\n", "endpoint id 5 is not the id"},
		{"plans: [{name: free}]\ntenants: [{name: acme}]\n", "tenants: identity cannot be imported"},
		{"routes: [{name: r, upstream: orders, match: {path: /a}, timeouts: {connect: soon}}]\n", `route "r": `},
		{"upstreams: [{name: x, endpoints: [{address: '10.9.9.9:80'}]}]\n", "not in the allowed upstream networks (SEC-06)"},
	} {
		a := r.editor.sendYAML("POST", "/config/import", []byte(bad.body), "If-Match", tag)
		if e := a.apiErr(t); a.status != 422 || e.Code != "validation_failed" || !strings.Contains(e.Error, bad.msg) {
			t.Errorf("import of\n%s: %d %s, want 422 %q", bad.body, a.status, a.body, bad.msg)
		}
	}
	if a := r.editor.sendYAML("POST", "/config/import", []byte("routes: [\n"), "If-Match", tag); a.status != 400 {
		t.Errorf("import of bad YAML: %d %s", a.status, a.body)
	}
	if e, tag2 := exportYAML(t, r.viewer, ""); tag2 != tag || !strings.Contains(e, "name: gold") {
		t.Errorf("a refused import changed the working set: ETag %s, was %s", tag2, tag)
	}

	// An import may leave references for validate and publish to catch.
	res = importResult(t, importYAML(t, r.editor, 200, []byte("routes: [{name: r, upstream: gone, match: {path_prefix: /}}]\n"), tag, ""))
	if len(res.Changes) != 7 {
		t.Errorf("import of a lone route: %+v", res)
	}
	if v, _ := validate(t, r.viewer); v.Valid || len(v.Errors) != 1 || !strings.Contains(*v.Errors[0].Message, `upstream "gone" does not exist`) {
		t.Errorf("validate after a dangling import: %+v", v)
	}

	// One audit record per import that changed something: the editor, the
	// revision, the note, the changes and what was ignored.
	records := auditPage(t, r.admin, "?entity=config")
	var imports []apiv1.AuditRecord
	for _, rec := range records.Items {
		if rec.Action == "config.import" {
			imports = append(imports, rec)
		}
	}
	if len(imports) != 3 {
		t.Fatalf("import records: %+v", imports)
	}
	first := imports[2]
	diff, _ := json.Marshal(first.Diff)
	if first.Actor.Sub != "sub-editor" || *first.EntityId != "2" || first.Note == nil || *first.Note != "from git abc123" || first.ConfigVersion != nil ||
		!strings.Contains(string(diff), `"from_revision":1`) || !strings.Contains(string(diff), `"to_revision":2`) ||
		!strings.Contains(string(diff), `{"change":"added","entity":"upstream","id":`) || !strings.Contains(string(diff), `"ignored":["ratelimit: not kept`) {
		t.Errorf("first import record %+v, diff %s", first, diff)
	}
	var n int
	if err := owner.QueryRow(t.Context(), "SELECT count(*) FROM audit_log WHERE action = 'config.import'").Scan(&n); err != nil || n != 3 {
		t.Errorf("%d import records, %v: refused and no-op imports write none", n, err)
	}

	var stored string
	if err := owner.QueryRow(t.Context(), `SELECT string_agg(row_to_json(a)::text, '') FROM audit_log a`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, cl := range []cpClient{r.editor, r.publisher, r.admin, r.viewer, r.keyAdmin} {
		if sig := cl.token[strings.LastIndexByte(cl.token, '.')+1:]; strings.Contains(logs.String(), sig) || strings.Contains(stored, sig) {
			t.Error("a bearer token appears in a log line or an audit record")
		}
	}
}

// TestControlPlaneImportsTheExamples imports every static config file in
// the repo, one after another (CP-05): each import succeeds, the export
// reads back through the gateway's loader as the file's config (entities
// by name, with ids, without version and ratelimit), and the working set
// validates as a publish would.
func TestControlPlaneImportsTheExamples(t *testing.T) {
	db := migratedDatabase(t)
	iss := newTestIssuer(t)
	c := serveOn(t, db, "172.29.0.0/16,10.0.0.0/8", iss, io.Discard)
	editor, viewer := c.as(iss.token(t, "sub-editor", "test-editor", "editor")), c.as(iss.token(t, "sub-viewer", "test-viewer", "viewer"))

	var files []string
	for _, pattern := range []string{"internal/config/testdata/example.yaml", "deploy/compose/gateway/config.yaml", "test/*/gateway.yaml", "test/chaos/*.yaml", "test/verify/*.yaml"} {
		m, err := filepath.Glob(filepath.Join("..", "..", pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("%d example configs", len(files))
	}
	_, tag := exportYAML(t, viewer, "")
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // G304: a config file of this repo
		if err != nil {
			t.Fatal(err)
		}
		want, err := config.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		res := importResult(t, importYAML(t, editor, 200, data, tag, ""))
		y, etag := exportYAML(t, viewer, "")
		tag = etag
		if etag != fmt.Sprintf(`"%d"`, res.WorkingSetRevision) {
			t.Errorf("%s: export ETag %s, import revision %d", f, etag, res.WorkingSetRevision)
		}
		got, err := config.Load([]byte(y))
		if err != nil {
			t.Fatalf("%s: the gateway cannot load the export: %v\n%s", f, err, y)
		}
		if !proto.Equal(withoutIDs(got), sortedByName(want)) {
			t.Errorf("%s: the export reads back as\n%v\nwant\n%v", f, got, want)
		}
		if v, _ := validate(t, viewer); !v.Valid {
			t.Errorf("%s: does not validate: %+v", f, v.Errors)
		}
	}
}

// sortedByName is c as the control plane keeps it: entities by name, no
// version and no ratelimit section (ADR-0044).
func sortedByName(c *configpb.Config) *configpb.Config {
	c = proto.Clone(c).(*configpb.Config)
	c.Version, c.Ratelimit = 0, nil
	slices.SortFunc(c.Upstreams, func(a, b *configpb.Upstream) int { return strings.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Routes, func(a, b *configpb.Route) int { return strings.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Policies, func(a, b *configpb.Policy) int { return strings.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Plans, func(a, b *configpb.Plan) int { return strings.Compare(a.GetName(), b.GetName()) })
	return c
}

// withoutIDs is c without the ids the control plane assigned.
func withoutIDs(c *configpb.Config) *configpb.Config {
	c = proto.Clone(c).(*configpb.Config)
	for _, u := range c.Upstreams {
		u.Id = 0
		for _, e := range u.Endpoints {
			e.Id = 0
		}
	}
	for _, r := range c.Routes {
		r.Id = 0
	}
	for _, p := range c.Policies {
		p.Id = 0
	}
	for _, p := range c.Plans {
		p.Id = 0
	}
	return c
}
