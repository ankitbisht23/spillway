package config

import (
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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
	orders, catalog := s.Routes[0], s.Routes[2]
	wantOrders := Route{
		Name:            "orders-api",
		Upstream:        s.Upstreams["orders"],
		AddPrefix:       "/api",
		RequestHeaders:  HeaderTransform{Remove: []string{"X-Debug"}, Set: []HeaderValue{{"X-Gateway", "spillway"}}, Add: []HeaderValue{{"X-Tag", "a"}}},
		ResponseHeaders: HeaderTransform{Remove: []string{"Server"}},
		MaxBodyBytes:    1 << 20,
		MaxHeaderBytes:  8192,
		Timeouts:        Timeouts{Connect: 200 * time.Millisecond, PerTry: time.Second, Total: 3 * time.Second, Idle: 500 * time.Millisecond},
		HSTS:            "max-age=31536000",
		strip:           1,
	}
	if !reflect.DeepEqual(orders, wantOrders) {
		t.Errorf("orders-api = %+v\nwant %+v", orders, wantOrders)
	}
	if want := (Timeouts{Connect: 5 * time.Second, PerTry: 15 * time.Second, Total: 15 * time.Second}); catalog.Timeouts != want {
		t.Errorf("catalog timeouts = %+v, want the defaults %+v", catalog.Timeouts, want)
	}
	if catalog.MaxBodyBytes != 0 || catalog.MaxHeaderBytes != 0 || catalog.HSTS != "" {
		t.Errorf("catalog limits = %d, %d, HSTS %q; want none", catalog.MaxBodyBytes, catalog.MaxHeaderBytes, catalog.HSTS)
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
		{"strip_prefix not a prefix", route("strip_prefix: /v2"), `route "r": strip_prefix "/v2": want whole leading segments of the route's path "/v1/orders"`},
		{"strip_prefix inside a segment", route("strip_prefix: /v1/ord"), "want whole leading segments"},
		{"strip_prefix longer than the path", route("strip_prefix: /v1/orders/x"), "want whole leading segments"},
		{"strip_prefix with trailing slash", route("strip_prefix: /v1/"), "want whole leading segments"},
		{"strip_prefix /", route("strip_prefix: /"), "want whole leading segments"},
		{"strip_prefix not clean", route("strip_prefix: /v1/../v1"), "want whole leading segments"},
		{"strip_prefix without a slash", route("strip_prefix: v1"), "want whole leading segments"},
		{"add_prefix with trailing slash", route("add_prefix: /api/"), `add_prefix "/api/": want a clean path`},
		{"add_prefix /", route("add_prefix: /"), "want a clean path"},
		{"add_prefix not clean", route("add_prefix: /a//b"), "want a clean path"},
		{"set Host", route("request_headers: {set: [{name: host, value: x}]}"), `request_headers: set: header "host" is the gateway's own`},
		{"remove Connection", route("response_headers: {remove: [Connection]}"), `response_headers: remove: header "Connection" is the gateway's own`},
		{"add Transfer-Encoding", route("request_headers: {add: [{name: transfer-encoding, value: chunked}]}"), "is the gateway's own"},
		{"set Content-Length", route("response_headers: {set: [{name: content-length, value: '1'}]}"), "is the gateway's own"},
		{"set TE", route("request_headers: {set: [{name: TE, value: trailers}]}"), "is the gateway's own"},
		{"set Upgrade", route("request_headers: {set: [{name: upgrade, value: h2c}]}"), "is the gateway's own"},
		{"remove X-Forwarded-For", route("request_headers: {remove: [x-forwarded-for]}"), "is the gateway's own"},
		{"set any X-Forwarded-*", route("request_headers: {set: [{name: X-Forwarded-Prefix, value: /v1}]}"), "is the gateway's own"},
		{"set Forwarded", route("request_headers: {set: [{name: forwarded, value: for=1.2.3.4}]}"), "is the gateway's own"},
		{"invalid header name", route("request_headers: {set: [{name: 'x y', value: z}]}"), `header name "x y" is not valid`},
		{"empty header name", route("response_headers: {remove: ['']}"), `header name "" is not valid`},
		{"header value with CR LF", route("request_headers: {add: [{name: x, value: \"a\\r\\nInjected: 1\"}]}"), "control character"},
		{"negative max_body_bytes", route("max_body_bytes: -1"), "max_body_bytes must not be negative"},
		{"negative max_header_bytes", route("max_header_bytes: -1"), "max_header_bytes must be 0 to 65536"},
		{"max_header_bytes over the server's", route("max_header_bytes: 65537"), "max_header_bytes must be 0 to 65536"},
		{"timeout without a unit", route("timeouts: {connect: '2'}"), `timeouts.connect: "2" is not a positive duration`},
		{"timeout not a duration", route("timeouts: {per_try: fast}"), `timeouts.per_try: "fast"`},
		{"zero timeout", route("timeouts: {idle: 0s}"), `timeouts.idle: "0s" is not a positive duration`},
		{"negative timeout", route("timeouts: {total: -1s}"), "timeouts.total"},
		{"total over the write timeout", route("timeouts: {total: 60s}"), "timeouts.total: 1m0s is more than 59s"},
		{"unknown timeout", route("timeouts: {read: 1s}"), `unknown field "read"`},
		{"hsts_max_age not a duration", route("hsts_max_age: '31536000'"), `hsts_max_age: "31536000" is not a duration in whole seconds`},
		{"negative hsts_max_age", route("hsts_max_age: -1s"), "hsts_max_age"},
		{"hsts_max_age not whole seconds", route("hsts_max_age: 1500ms"), "hsts_max_age"},
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

// route is a config with one route on /v1/orders and extra route fields.
func route(fields string) string {
	return upstream + "routes: [{name: r, upstream: u, match: {path_prefix: /v1/orders}, " + fields + "}]"
}

func TestUpstreamPath(t *testing.T) {
	tests := []struct {
		match, strip, add string
		in, want          string
	}{
		{"path_prefix: /v1/orders", "", "", "/v1/orders/42", "/v1/orders/42"},
		{"path_prefix: /v1/orders", "/v1", "", "/v1/orders/42", "/orders/42"},
		{"path_prefix: /v1/orders", "/v1", "", "/v1/orders/", "/orders/"},
		{"path_prefix: /v1/orders", "/v1/orders", "", "/v1/orders", "/"},
		{"path_prefix: /v1/orders", "/v1/orders", "", "/v1/orders/", "/"},
		{"path_prefix: /v1/orders", "/v1/orders", "", "/v1/orders/42/items", "/42/items"},
		{"path_prefix: /v1/orders", "/v1", "/api/v2", "/v1/orders/42", "/api/v2/orders/42"},
		{"path_prefix: /v1/orders", "/v1/orders", "/orders", "/v1/orders", "/orders"},
		{"path_prefix: /v1/orders", "/v1/orders", "/orders", "/v1/orders/", "/orders/"},
		{"path_prefix: /v1/orders", "", "/api", "/v1/orders", "/api/v1/orders"},
		{"path_prefix: /", "", "/api", "/", "/api/"},
		{"path_prefix: /", "", "/api", "/x", "/api/x"},
		{"path: /t/:tenant/orders", "/t/:tenant", "", "/t/acme/orders", "/orders"},
		{"path_prefix: /t/:tenant", "/t/:tenant", "/tenants", "/t/acme/a/b", "/tenants/a/b"},
	}
	for _, tt := range tests {
		yaml := fmt.Sprintf("%sroutes: [{name: r, upstream: u, match: {%s}, strip_prefix: '%s', add_prefix: '%s'}]", upstream, tt.match, tt.strip, tt.add)
		s, err := parse([]byte(yaml))
		if err != nil {
			t.Fatalf("%s: %v", yaml, err)
		}
		r, _, ok := s.Match("", "GET", tt.in, nil)
		if !ok {
			t.Fatalf("%s: %s does not match", tt.match, tt.in)
		}
		if got := r.UpstreamPath(tt.in); got != tt.want {
			t.Errorf("{%s} strip %q add %q: UpstreamPath(%q) = %q, want %q", tt.match, tt.strip, tt.add, tt.in, got, tt.want)
		}
	}
}

// TestHeaderTransform: remove, then set, then add, by canonical name.
func TestHeaderTransform(t *testing.T) {
	s, err := parse([]byte(route(`request_headers: {remove: [x-a, x-b], set: [{name: x-b, value: set}, {name: x-c, value: set}], add: [{name: x-c, value: added}, {name: x-d, value: added}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{"X-A": {"1"}, "X-B": {"1", "2"}, "X-C": {"1"}, "X-D": {"1"}, "X-E": {"kept"}}
	s.Routes[0].RequestHeaders.Apply(h)
	want := http.Header{"X-B": {"set"}, "X-C": {"set", "added"}, "X-D": {"1", "added"}, "X-E": {"kept"}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("headers = %v, want %v", h, want)
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
