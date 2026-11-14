package api

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/proxy"
)

// exampleConfigs are the repo's static config files (CP-05): the example
// in internal/config, the compose gateways', and the test and verify ones.
func exampleConfigs(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	var files []string
	for _, pattern := range []string{"internal/config/testdata/example.yaml", "deploy/compose/gateway/config.yaml", "test/*/gateway.yaml", "test/chaos/*.yaml", "test/verify/*.yaml"} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("found %d example configs: %v", len(files), files)
	}
	return files
}

// canonical is c as the control plane keeps it: no version, no ratelimit
// section (ADR-0044), entities sorted by name.
func canonical(c *configpb.Config) *configpb.Config {
	c = proto.Clone(c).(*configpb.Config)
	c.Version, c.Ratelimit = 0, nil
	slices.SortFunc(c.Upstreams, func(a, b *configpb.Upstream) int { return strings.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Routes, func(a, b *configpb.Route) int { return strings.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Policies, func(a, b *configpb.Policy) int { return strings.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Plans, func(a, b *configpb.Plan) int { return strings.Compare(a.GetName(), b.GetName()) })
	return c
}

// TestImportParsesTheExamples checks that every example config parses as an
// import, with what the gateway's loader reads (CP-05, CP-21): the same
// entities, version dropped, ratelimit reported as not kept; and that
// export's YAML of it reads back the same and is stable.
func TestImportParsesTheExamples(t *testing.T) {
	for _, f := range exampleConfigs(t) {
		t.Run(filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f) //nolint:gosec // G304: a config file of this repo
			if err != nil {
				t.Fatal(err)
			}
			want, err := config.Load(data)
			if err != nil {
				t.Fatal(err)
			}
			got, ignored, err := parseImport(data)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(canonical(got), canonical(want)) || got.Version != 0 || got.Ratelimit != nil {
				t.Errorf("parsed\n%v\nthe loader read\n%v", got, want)
			}
			if wantIgnored := want.Ratelimit != nil; wantIgnored != (len(ignored) == 1 && strings.HasPrefix(ignored[0], "ratelimit: not kept")) || !wantIgnored && len(ignored) != 0 {
				t.Errorf("ignored %q", ignored)
			}
			if errs := checkImportOf(t, got); len(errs) > 0 {
				t.Errorf("entity rules: %v", errs)
			}

			y, err := exportYAML(got)
			if err != nil {
				t.Fatal(err)
			}
			again, err := config.Load(y)
			if err != nil {
				t.Fatalf("the gateway cannot load the export: %v\n%s", err, y)
			}
			if !proto.Equal(again, canonical(want)) {
				t.Errorf("export reads back as\n%v\nwant\n%v", again, canonical(want))
			}
			if y2, err := exportYAML(again); err != nil || !bytes.Equal(y2, y) {
				t.Errorf("export is not stable: %v\n%s\nthen\n%s", err, y, y2)
			}
		})
	}
}

// checkImportOf runs checkImport with the test resolver and an allow-list
// that holds the examples' addresses.
func checkImportOf(t *testing.T, c *configpb.Config) []string {
	t.Helper()
	allowed, err := proxy.ParseAllowlist("10.0.0.0/8,172.29.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, allowed, fakeResolver{}, fakeAuth{}, nil)
	var out []string
	for _, d := range s.checkImport(t.Context(), c) {
		out = append(out, *d.Message)
	}
	return out
}

// TestExportWithIDs checks export's form (ADR-0037): ids, endpoint ids and a
// published version's number are written, entities sorted by name, fields
// in proto order, 64-bit integers as integers; an import reads it back
// with its ids and without the version; an empty config is {}.
func TestExportWithIDs(t *testing.T) {
	c := &configpb.Config{
		Version: 7,
		Upstreams: []*configpb.Upstream{{Id: 3, Name: "orders", Endpoints: []*configpb.Endpoint{
			{Id: 11, Address: "172.29.0.10:8080"}, {Id: 12, Address: "172.29.0.11:8080", Draining: true},
		}}},
		Routes: []*configpb.Route{
			{Id: 9, Name: "orders-b", Upstream: "orders", Match: &configpb.RouteMatch{Path: "/b"}, MaxBodyBytes: 1 << 40},
			{Id: 8, Name: "orders-a", Upstream: "orders", Match: &configpb.RouteMatch{Path: "/a", Headers: []*configpb.HeaderMatch{{Name: "x-canary", Exact: "on"}}}},
		},
	}
	y, err := exportYAML(c)
	if err != nil {
		t.Fatal(err)
	}
	want := `version: 7
upstreams:
  - name: orders
    endpoints:
      - address: 172.29.0.10:8080
        id: 11
      - address: 172.29.0.11:8080
        id: 12
        draining: true
    id: 3
routes:
  - name: orders-a
    match:
      path: /a
      headers:
        - name: x-canary
          exact: "on"
    upstream: orders
    id: 8
  - name: orders-b
    match:
      path: /b
    upstream: orders
    max_body_bytes: 1099511627776
    id: 9
`
	if string(y) != want {
		t.Errorf("export:\n%s\nwant:\n%s", y, want)
	}
	got, ignored, err := parseImport(y)
	if err != nil || len(ignored) != 0 {
		t.Fatal(err, ignored)
	}
	c.Version = 0
	if !proto.Equal(got, canonical(c)) {
		t.Errorf("import read\n%v", got)
	}
	if y, err := exportYAML(&configpb.Config{}); err != nil || string(y) != "{}\n" {
		t.Errorf("empty export %q, %v", y, err)
	}
	if got, _, err := parseImport([]byte("{}\n")); err != nil || proto.Size(got) != 0 {
		t.Errorf("empty import: %v, %v", got, err)
	}
}

// TestImportRejects checks the answers an import gives before it reaches
// the store (CP-05, ADR-0037): the media type and size, YAML and schema
// errors (400), identity, entity rules, duplicates and SEC-06 (422, every
// error in details, entities named by index and name), the note's length.
func TestImportRejects(t *testing.T) {
	h := newHandler(t, io.Discard)
	imp := func(body string, header ...string) answer {
		t.Helper()
		return do(t, h, http.MethodPost, BasePath+"/config/import", body, append([]string{"If-Match", `"1"`, "Content-Type", "application/yaml"}, header...)...)
	}
	upstream := "upstreams: [{name: orders, endpoints: [{address: '172.29.0.10:8080'}]}]\n"

	for _, c := range []struct {
		name, body string
		header     []string
		status     int
		code, msg  string
	}{
		{"json", "{}", []string{"Content-Type", "application/json"}, 415, "unsupported_media_type", "the body must be application/yaml"},
		{"no media type", "{}", []string{"Content-Type", ""}, 415, "unsupported_media_type", "application/yaml"},
		{"too large", strings.Repeat("#", MaxImportBody+1), nil, 413, "payload_too_large", "the body is over 10485760 bytes"},
		{"not YAML", "upstreams: [", nil, 400, "invalid_request", "body: not YAML: "},
		{"empty document", "# nothing\n", nil, 400, "invalid_request", "body: required"},
		{"a list", "- name: orders\n", nil, 400, "invalid_request", "want a YAML mapping"},
		{"unknown section", "routez: []\n", nil, 400, "invalid_request", `body: unknown field "routez"`},
		{"section not a list", "routes: {name: r}\n", nil, 400, "invalid_request", "body: routes: want a list"},
		{"unknown entity field", upstream + "routes: [{name: r, upstrem: orders}]\n", nil, 400, "invalid_request", `body: routes[0] ("r"): unknown field "upstrem"`},
		{"wrong type", "routes: [{name: r, priority: high}]\n", nil, 400, "invalid_request", `body: routes[0] ("r"): `},
		{"identity", upstream + "consumers: [{name: alice}]\napi_keys: []\n", nil, 422, "validation_failed", "consumers: identity cannot be imported"},
		{"identity section", "identity: {}\n", nil, 422, "validation_failed", "identity: identity cannot be imported"},
		{"entity rule", upstream + "routes: [{name: r, upstream: orders, match: {path: /a}, timeouts: {connect: soon}}]\n", nil, 422, "validation_failed", `route "r": `},
		{"bad name", upstream + "plans: [{name: 'a b'}]\n", nil, 422, "validation_failed", "name must be 1-64 characters"},
		{"name twice", upstream + "plans: [{name: free}, {name: free}]\n", nil, 422, "validation_failed", `name "free" is given twice`},
		{"id twice", "plans: [{name: free, id: 4}, {name: gold, id: 4}]\n", nil, 422, "validation_failed", "id 4 is given twice"},
		{"negative id", "plans: [{name: free, id: -1}]\n", nil, 422, "validation_failed", "ids are positive"},
		{"SEC-06", "upstreams: [{name: meta, endpoints: [{address: '169.254.169.254:80'}]}]\n", nil, 422, "validation_failed", "not in the allowed upstream networks"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := imp(c.body, c.header...)
			if a.status != c.status || string(a.err.Code) != c.code || !strings.Contains(a.err.Error, c.msg) {
				t.Errorf("%d %s %q, want %d %s %q", a.status, a.err.Code, a.err.Error, c.status, c.code, c.msg)
			}
		})
	}

	// Every error is a detail, with the entity's path in the file.
	a := imp("plans: [{name: free}, {name: free}, {name: gold, id: 2}]\nroutes: [{name: r, id: 2}]\n")
	if a.status != 422 || a.err.Details == nil {
		t.Fatalf("%d %+v", a.status, a.err)
	}
	var details []string
	for _, d := range *a.err.Details {
		details = append(details, *d.Entity+" "+*d.Path+": "+*d.Message)
	}
	if want := []string{
		"route routes[0]: set exactly one of match.path and match.path_prefix",
		`plan plans[1].name: name "free" is given twice`,
	}; !slices.Equal(details, want) {
		t.Errorf("details\n%s\nwant\n%s", strings.Join(details, "\n"), strings.Join(want, "\n"))
	}

	for _, n := range []string{"", strings.Repeat("n", maxNote+1)} {
		a := do(t, h, http.MethodPost, BasePath+"/config/import?note="+n, upstream, "If-Match", `"1"`, "Content-Type", "application/yaml")
		if a.status != 400 || !strings.Contains(a.err.Error, "note: want 1 to 2000 characters") {
			t.Errorf("note of %d characters: %d %q", len(n), a.status, a.err.Error)
		}
	}
	// A valid file reaches the store, which the test handler does not have.
	if a := imp(upstream + "version: 3\nratelimit: {replicas: 2}\n"); a.status != 599 {
		t.Errorf("a valid import: %d %q", a.status, a.err.Error)
	}
	// Other operations keep the 1 MiB cap and JSON.
	if a := do(t, h, http.MethodPost, BasePath+"/routes", strings.Repeat(" ", MaxBody+1)); a.status != 413 {
		t.Errorf("a JSON body over 1 MiB: %d", a.status)
	}
	if a := do(t, h, http.MethodPost, BasePath+"/routes", "name: r\n", "Content-Type", "application/yaml"); a.status != 415 {
		t.Errorf("YAML to a JSON operation: %d", a.status)
	}
}
