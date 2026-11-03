package config

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// TestLoadMatchesProtojson checks that the YAML form and the protojson form
// (what Postgres snapshots store, CP-21) of the same config are equal.
func TestLoadMatchesProtojson(t *testing.T) {
	y, err := os.ReadFile("testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	j, err := os.ReadFile("testdata/example.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(y)
	if err != nil {
		t.Fatal(err)
	}
	want := &configpb.Config{}
	if err := protojson.Unmarshal(j, want); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) {
		t.Errorf("Load(example.yaml) = %v\nwant %v", got, want)
	}
}

func TestCompile(t *testing.T) {
	data, err := os.ReadFile("testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 42 || len(s.Routes) != 3 || len(s.Upstreams) != 2 {
		t.Fatalf("version %d, %d routes, %d upstreams", s.Version, len(s.Routes), len(s.Upstreams))
	}
	if got := s.Upstreams["orders"].Endpoints; len(got) != 2 || got[0] != "orders-1:8080" {
		t.Errorf("orders endpoints = %v", got)
	}
	tests := []struct {
		host, method, path string
		header             http.Header
		route, upstream    string
	}{
		{"api.example.com", "GET", "/v1/orders/42/items", nil, "orders-api", "orders"},
		{"api.example.com", "DELETE", "/v1/orders/42", nil, "catalog", "catalog"},
		{"other.example.com", "GET", "/v1/orders/42", http.Header{"X-Canary": {"on"}, "X-Version": {"v2"}}, "order-by-id", "orders"},
		{"other.example.com", "GET", "/v1/orders/42", http.Header{"X-Canary": {"on"}}, "catalog", "catalog"},
		{"", "GET", "/", nil, "catalog", "catalog"},
	}
	for _, tt := range tests {
		r, m, ok := s.Match(tt.host, tt.method, tt.path, tt.header)
		if !ok || r.Name != tt.route || r.Upstream != s.Upstreams[tt.upstream] {
			t.Errorf("Match(%s %s %s) = %+v, %v; want route %s", tt.method, tt.host, tt.path, r, ok, tt.route)
			continue
		}
		if tt.route == "order-by-id" && m.Param("id") != "42" {
			t.Errorf("Param(id) = %q", m.Param("id"))
		}
	}
}

const upstream = "upstreams: [{name: u, endpoints: [{address: 'u:80'}]}]\n"

func TestRejects(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"empty file", "", "config: empty"},
		{"not YAML", "routes: [", "config: yaml"},
		{"scalar key that is not a field", "1: x", `unknown field "1"`},
		{"complex key", "? [a, b]\n: c", "invalid map key"},
		{"infinity", "version: .inf", "not representable as JSON"},
		{"duplicate key", "version: 1\nversion: 2", `mapping key "version" already defined`},
		{"unknown field", "upstrem: []", `unknown field "upstrem"`},
		{"unknown nested field", upstream + "routes: [{name: r, upstream: u, match: {path: /, timeout: 1s}}]", `unknown field "timeout"`},
		{"wrong type", "version: abc", "invalid value for int64"},
		{"unquoted number for a string", upstream + "routes: [{name: r, upstream: u, match: {path: /, headers: [{name: x, exact: 1}]}}]", "invalid value for string"},
		{"upstream without a name", "upstreams: [{endpoints: [{address: 'u:80'}]}]", `upstream "": name is required`},
		{"upstream twice", "upstreams: [{name: u, endpoints: [{address: 'u:80'}]}, {name: u, endpoints: [{address: 'v:80'}]}]", `upstream "u": defined twice`},
		{"no endpoints", "upstreams: [{name: u}]", "at least one endpoint"},
		{"endpoint without a port", "upstreams: [{name: u, endpoints: [{address: u}]}]", "want host:port"},
		{"endpoint port 0", "upstreams: [{name: u, endpoints: [{address: 'u:0'}]}]", "port must be"},
		{"endpoint port too big", "upstreams: [{name: u, endpoints: [{address: 'u:70000'}]}]", "port must be"},
		{"endpoint twice", "upstreams: [{name: u, endpoints: [{address: 'u:80'}, {address: 'u:80'}]}]", "listed twice"},
		{"route without a name", upstream + "routes: [{upstream: u, match: {path: /}}]", "name is required"},
		{"route twice", upstream + "routes: [{name: r, upstream: u, match: {path: /a}}, {name: r, upstream: u, match: {path: /b}}]", `route "r": defined twice`},
		{"undefined upstream", upstream + "routes: [{name: r, upstream: nope, match: {path: /}}]", `upstream "nope" is not defined`},
		{"no path", upstream + "routes: [{name: r, upstream: u}]", "exactly one of match.path"},
		{"both paths", upstream + "routes: [{name: r, upstream: u, match: {path: /a, path_prefix: /a}}]", "exactly one of match.path"},
		{"router error names the route", upstream + "routes: [{name: ok, upstream: u, match: {path: /}}, {name: bad, upstream: u, match: {path: '/a//b'}}]", `route "bad": path "/a//b" is not clean`},
		{"bad regex", upstream + "routes: [{name: r, upstream: u, match: {path: /, headers: [{name: x, regex: '('}]}}]", `route "r": header "x"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

// YAML 1.2: off, on, yes and no are strings, not booleans.
func TestYAML12Strings(t *testing.T) {
	s, err := parse([]byte(upstream + "routes: [{name: r, upstream: u, match: {path: /, headers: [{name: x, exact: off}]}}]"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Match("", "GET", "/", http.Header{"X": {"off"}}); !ok {
		t.Error("exact: off did not stay the string \"off\"")
	}
}

func parse(data []byte) (*Snapshot, error) {
	c, err := Load(data)
	if err != nil {
		return nil, err
	}
	return Compile(c)
}

// FuzzLoad checks that loading and compiling never panic on hostile input.
func FuzzLoad(f *testing.F) {
	example, err := os.ReadFile("testdata/example.yaml")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(example)
	f.Add([]byte(upstream + "routes: [{name: r, upstream: u, match: {path: '/:a/:a'}}]"))
	f.Add([]byte("a: &x [*x]"))
	f.Add([]byte("version: 1e400\nroutes: ~"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parse(data)
	})
}
