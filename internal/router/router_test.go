package router

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type req struct {
	host, method, path string
	header             http.Header
}

func get(path string) req { return req{host: "api.example.com", method: "GET", path: path} }

// TestPrecedence covers each rule of ADR-0007, in order.
func TestPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		routes []Route
		req    req
		want   int // route index, or -1 for no match
	}{
		{
			name:   "static segment beats parameter",
			routes: []Route{{Path: "/orders/:id"}, {Path: "/orders/new"}},
			req:    get("/orders/new"),
			want:   1,
		},
		{
			name:   "parameter matches when no static segment does",
			routes: []Route{{Path: "/orders/:id"}, {Path: "/orders/new"}},
			req:    get("/orders/42"),
			want:   0,
		},
		{
			name:   "static beats parameter at the first segment that differs",
			routes: []Route{{Path: "/:a/b/c"}, {Path: "/a/:b/:c"}},
			req:    get("/a/b/c"),
			want:   1,
		},
		{
			name:   "exact path beats a longer prefix",
			routes: []Route{{Path: "/a/b", Prefix: true}, {Path: "/a/:x"}},
			req:    get("/a/b"),
			want:   1,
		},
		{
			name:   "longest prefix wins",
			routes: []Route{{Path: "/", Prefix: true}, {Path: "/v1/orders", Prefix: true}, {Path: "/v1", Prefix: true}},
			req:    get("/v1/orders/42/items"),
			want:   1,
		},
		{
			name:   "longer prefix with a parameter beats a shorter static prefix",
			routes: []Route{{Path: "/v1", Prefix: true}, {Path: "/v1/:svc/items", Prefix: true}},
			req:    get("/v1/orders/items/9"),
			want:   1,
		},
		{
			name:   "static beats parameter between prefixes of equal length",
			routes: []Route{{Path: "/v1/:svc", Prefix: true}, {Path: "/v1/orders", Prefix: true}},
			req:    get("/v1/orders/42"),
			want:   1,
		},
		{
			name:   "prefix matches whole segments only",
			routes: []Route{{Path: "/v1/orders", Prefix: true}},
			req:    get("/v1/ordersx"),
			want:   -1,
		},
		{
			name:   "prefix matches its own path and a trailing slash",
			routes: []Route{{Path: "/v1/orders", Prefix: true}},
			req:    get("/v1/orders/"),
			want:   0,
		},
		{
			name:   "exact path does not match a trailing slash",
			routes: []Route{{Path: "/v1/orders"}},
			req:    get("/v1/orders/"),
			want:   -1,
		},
		{
			name:   "parameter does not match an empty segment",
			routes: []Route{{Path: "/orders/:id/"}},
			req:    get("/orders//"),
			want:   -1,
		},
		{
			name:   "host-specific route beats a more specific path without a host",
			routes: []Route{{Path: "/health"}, {Host: "api.example.com", Path: "/", Prefix: true}},
			req:    get("/health"),
			want:   1,
		},
		{
			name:   "host matches case-insensitively, without port or trailing dot",
			routes: []Route{{Host: "API.example.com", Path: "/"}},
			req:    req{host: "api.Example.com.:8443", method: "GET", path: "/"},
			want:   0,
		},
		{
			name:   "falls back to routes without a host",
			routes: []Route{{Host: "api.example.com", Path: "/a"}, {Path: "/b"}},
			req:    get("/b"),
			want:   1,
		},
		{
			name:   "other hosts never match a host-specific route",
			routes: []Route{{Host: "other.example.com", Path: "/", Prefix: true}},
			req:    get("/a"),
			want:   -1,
		},
		{
			name:   "method filters",
			routes: []Route{{Path: "/a", Methods: []string{"POST"}}},
			req:    get("/a"),
			want:   -1,
		},
		{
			name:   "method restriction beats no restriction",
			routes: []Route{{Path: "/a"}, {Path: "/a", Methods: []string{"GET", "HEAD"}}},
			req:    get("/a"),
			want:   1,
		},
		{
			name: "more predicates beat fewer",
			routes: []Route{
				{Path: "/a", Methods: []string{"GET"}},
				{Path: "/a", Methods: []string{"GET"}, Headers: []HeaderMatch{{Name: "x-canary", Exact: "1"}}},
			},
			req:  req{method: "GET", path: "/a", header: http.Header{"X-Canary": {"1"}}},
			want: 1,
		},
		{
			name: "predicates rank below path specificity",
			routes: []Route{
				{Path: "/a", Prefix: true, Headers: []HeaderMatch{{Name: "x-canary", Exact: "1"}}},
				{Path: "/a/b"},
			},
			req:  req{method: "GET", path: "/a/b", header: http.Header{"X-Canary": {"1"}}},
			want: 1,
		},
		{
			name:   "priority breaks ties",
			routes: []Route{{Path: "/a", Priority: 1}, {Path: "/a", Priority: 5}, {Path: "/a"}},
			req:    get("/a"),
			want:   1,
		},
		{
			name:   "config order breaks the rest",
			routes: []Route{{Path: "/a"}, {Path: "/a"}},
			req:    get("/a"),
			want:   0,
		},
		{
			name: "header regex matches the whole value",
			routes: []Route{
				{Path: "/a", Headers: []HeaderMatch{{Name: "X-Version", Regex: "v[12]"}}},
			},
			req:  req{method: "GET", path: "/a", header: http.Header{"X-Version": {"v12"}}},
			want: -1,
		},
		{
			name: "header predicate matches any of the header's values",
			routes: []Route{
				{Path: "/a", Headers: []HeaderMatch{{Name: "x-version", Regex: "v[12]"}}},
			},
			req:  req{method: "GET", path: "/a", header: http.Header{"X-Version": {"v9", "v2"}}},
			want: 0,
		},
		{
			name: "every header predicate must match",
			routes: []Route{
				{Path: "/a", Headers: []HeaderMatch{{Name: "a", Exact: "1"}, {Name: "b", Exact: "2"}}},
			},
			req:  req{method: "GET", path: "/a", header: http.Header{"A": {"1"}}},
			want: -1,
		},
		{
			name: "a failing predicate falls through to the next candidate",
			routes: []Route{
				{Path: "/a/b", Headers: []HeaderMatch{{Name: "x-canary", Exact: "1"}}},
				{Path: "/a/:x"},
				{Path: "/a", Prefix: true},
			},
			req:  get("/a/b"),
			want: 1,
		},
		{
			name:   "path without a leading slash never matches",
			routes: []Route{{Path: "/", Prefix: true}},
			req:    get("a"),
			want:   -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := New(tt.routes)
			if err != nil {
				t.Fatal(err)
			}
			m, ok := r.Match(tt.req.host, tt.req.method, tt.req.path, tt.req.header)
			got := -1
			if ok {
				got = m.Index
			}
			if got != tt.want {
				t.Errorf("Match(%q) = route %d, want %d", tt.req.path, got, tt.want)
			}
		})
	}
}

func TestParams(t *testing.T) {
	r, err := New([]Route{
		{Path: "/v1/:tenant/orders/:id"},
		{Path: "/files/:dir", Prefix: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := r.Match("", "GET", "/v1/acme/orders/42", nil)
	if !ok || m.Param("tenant") != "acme" || m.Param("id") != "42" || m.Param("missing") != "" {
		t.Errorf("got ok=%v tenant=%q id=%q missing=%q", ok, m.Param("tenant"), m.Param("id"), m.Param("missing"))
	}
	m, ok = r.Match("", "GET", "/files/docs/a/b.txt", nil)
	if !ok || m.Index != 1 || m.Param("dir") != "docs" {
		t.Errorf("got ok=%v index=%d dir=%q", ok, m.Index, m.Param("dir"))
	}
}

func TestNewRejects(t *testing.T) {
	tests := []struct {
		route Route
		want  string
	}{
		{Route{Path: ""}, "must start with /"},
		{Route{Path: "v1"}, "must start with /"},
		{Route{Path: "/a//b"}, "not clean"},
		{Route{Path: "/a/../b"}, "not clean"},
		{Route{Path: "/a/./b"}, "not clean"},
		{Route{Path: "/a/", Prefix: true}, "must not end in /"},
		{Route{Path: "/a/:"}, "needs a name"},
		{Route{Path: "/:id/:id"}, "appears twice"},
		{Route{Path: "/", Host: "api.example.com:443"}, "without a port"},
		{Route{Path: "/", Host: "."}, "without a port"},
		{Route{Path: "/", Methods: []string{"GE T"}}, "not a valid HTTP method"},
		{Route{Path: "/", Methods: []string{"post"}}, `method "post" is not upper case`},
		{Route{Path: "/", Headers: []HeaderMatch{{Name: "", Exact: "1"}}}, "not valid"},
		{Route{Path: "/", Headers: []HeaderMatch{{Name: "host", Exact: "a"}}}, "route's host"},
		{Route{Path: "/", Headers: []HeaderMatch{{Name: "x"}}}, "exactly one"},
		{Route{Path: "/", Headers: []HeaderMatch{{Name: "x", Exact: "1", Regex: "1"}}}, "exactly one"},
		{Route{Path: "/", Headers: []HeaderMatch{{Name: "x", Regex: "("}}}, "missing closing )"},
	}
	for _, tt := range tests {
		_, err := New([]Route{{Path: "/ok"}, tt.route})
		if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "route 1: ") {
			t.Errorf("New(%+v) error = %v, want route 1 and %q", tt.route, err, tt.want)
		}
	}
}

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		"":              "/",
		"/":             "/",
		"a/b":           "/a/b",
		"//a///b":       "/a/b",
		"/a/b/":         "/a/b/",
		"/a/./b/../c":   "/a/c",
		"/a/b/..":       "/a",
		"/a/b/../":      "/a/",
		"/../../etc":    "/etc",
		"/admin/..":     "/",
		"/v1/orders//":  "/v1/orders/",
		"/x/%2e%2e/y":   "/x/%2e%2e/y", // decoding is the caller's job (URL.Path is decoded)
		"/a/../../../b": "/b",
	} {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// reference is a linear scan that applies ADR-0007's order literally. The
// tree must always agree with it.
func reference(routes []Route, host, method, p string, h http.Header) (int, bool) {
	if p == "" || p[0] != '/' {
		return 0, false
	}
	reqSegs := strings.Split(p[1:], "/")
	type key struct {
		hostSpecific, exact bool
		depth               int
		statics             string // per segment: 1 static, 0 parameter; higher wins
		preds, priority     int
	}
	compare := func(a, b key) int {
		return cmp.Or(
			cmpBool(a.hostSpecific, b.hostSpecific),
			cmpBool(a.exact, b.exact),
			cmp.Compare(a.depth, b.depth),
			cmp.Compare(a.statics, b.statics),
			cmp.Compare(a.preds, b.preds),
			cmp.Compare(a.priority, b.priority),
		)
	}
	best, bestKey := -1, key{}
	for i, rt := range routes {
		if rt.Host != "" && !strings.EqualFold(strings.TrimSuffix(rt.Host, "."), hostKey(host)) {
			continue
		}
		if len(rt.Methods) > 0 && !slices.Contains(rt.Methods, method) {
			continue
		}
		ok := true
		for _, hm := range rt.Headers {
			re := regexp.MustCompile("^(?:" + hm.Regex + ")$")
			match := false
			for _, v := range h.Values(hm.Name) {
				if hm.Regex != "" && re.MatchString(v) || hm.Regex == "" && v == hm.Exact {
					match = true
				}
			}
			ok = ok && match
		}
		var pat []string
		if !rt.Prefix || rt.Path != "/" {
			pat = strings.Split(rt.Path[1:], "/")
		}
		if len(pat) > len(reqSegs) || !rt.Prefix && len(pat) != len(reqSegs) {
			continue
		}
		var statics strings.Builder
		for j, s := range pat {
			if strings.HasPrefix(s, ":") {
				ok = ok && reqSegs[j] != ""
				statics.WriteByte('0')
			} else {
				ok = ok && reqSegs[j] == s
				statics.WriteByte('1')
			}
		}
		if !ok {
			continue
		}
		preds := len(rt.Headers)
		if len(rt.Methods) > 0 {
			preds++
		}
		k := key{rt.Host != "", !rt.Prefix, len(pat), statics.String(), preds, rt.Priority}
		if best < 0 || compare(k, bestKey) > 0 {
			best, bestKey = i, k
		}
	}
	return best, best >= 0
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// gen builds routes and requests from a small alphabet, so random and fuzzed
// inputs collide often. next(n) returns a choice in [0, n).
type gen struct{ next func(n int) int }

func (g gen) pick(choices ...string) string { return choices[g.next(len(choices))] }

func (g gen) routes() []Route {
	routes := make([]Route, 1+g.next(12))
	for i := range routes {
		rt := Route{Prefix: g.next(2) == 0, Priority: g.next(3)}
		var segs []string
		for j := range g.next(4) {
			segs = append(segs, g.pick("a", "b", fmt.Sprintf(":p%d", j)))
		}
		rt.Path = "/" + strings.Join(segs, "/")
		if !rt.Prefix && len(segs) > 0 && g.next(3) == 0 {
			rt.Path += "/"
		}
		rt.Host = g.pick("", "", "h1")
		switch g.next(3) {
		case 1:
			rt.Methods = []string{"GET"}
		case 2:
			rt.Methods = []string{"POST", "GET"}
		}
		switch g.next(4) {
		case 1:
			rt.Headers = []HeaderMatch{{Name: "x-h", Exact: "1"}}
		case 2:
			rt.Headers = []HeaderMatch{{Name: "X-H", Regex: "[12]"}}
		case 3:
			rt.Headers = []HeaderMatch{{Name: "x-h", Regex: "[12]"}, {Name: "x-g", Exact: "1"}}
		}
		routes[i] = rt
	}
	return routes
}

func (g gen) request() req {
	var segs []string
	for range g.next(5) {
		segs = append(segs, g.pick("a", "b", "c", ""))
	}
	r := req{host: g.pick("h1", "H1:80", "h2", ""), method: g.pick("GET", "POST", "PUT"), path: "/" + strings.Join(segs, "/"), header: http.Header{}}
	if v := g.pick("", "1", "2", "3"); v != "" {
		r.header.Set("X-H", v)
	}
	if g.next(2) == 0 {
		r.header.Set("X-G", "1")
	}
	return r
}

func checkAgainstReference(t *testing.T, routes []Route, rq req) {
	t.Helper()
	r, err := New(routes)
	if err != nil {
		t.Fatalf("New(%+v): %v", routes, err)
	}
	m, ok := r.Match(rq.host, rq.method, rq.path, rq.header)
	want, wantOK := reference(routes, rq.host, rq.method, rq.path, rq.header)
	if ok != wantOK || ok && m.Index != want {
		t.Fatalf("routes %+v\nrequest %+v\ntree = (%d, %v), reference = (%d, %v)", routes, rq, m.Index, ok, want, wantOK)
	}
	if !ok {
		return
	}
	segs := strings.Split(rq.path[1:], "/")
	for j, s := range strings.Split(routes[m.Index].Path[1:], "/") {
		if name, isParam := strings.CutPrefix(s, ":"); isParam && m.Param(name) != segs[j] {
			t.Fatalf("route %q, path %q: Param(%q) = %q, want %q", routes[m.Index].Path, rq.path, name, m.Param(name), segs[j])
		}
	}
}

func TestTreeAgreesWithReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // fixed seed: reproducible test input, not security
	g := gen{next: rng.IntN}
	for range 20000 {
		checkAgainstReference(t, g.routes(), g.request())
	}
}

// FuzzRouter checks that the tree agrees with the reference matcher, and that
// Match and CleanPath never panic on arbitrary hosts and paths.
func FuzzRouter(f *testing.F) {
	f.Add([]byte{3, 1, 0, 2, 1, 1}, []byte{2, 0, 1, 0}, "api.example.com:8443", "/a/../b//c/")
	f.Add([]byte{0}, []byte{}, "", "")
	f.Add([]byte{9, 9, 9, 9, 9, 9, 9, 9}, []byte{4, 3, 3, 3, 3}, "[::1]:80", "/%2F/:/\x00")
	f.Fuzz(func(t *testing.T, routeBytes, reqBytes []byte, host, rawPath string) {
		routes := gen{next: byteSource(routeBytes)}.routes()
		checkAgainstReference(t, routes, gen{next: byteSource(reqBytes)}.request())

		r, err := New(routes)
		if err != nil {
			t.Fatal(err)
		}
		c := CleanPath(rawPath)
		if c[0] != '/' || CleanPath(c) != c {
			t.Fatalf("CleanPath(%q) = %q, not clean", rawPath, c)
		}
		r.Match(host, "GET", rawPath, http.Header{"X-H": {rawPath}})
		m, ok := r.Match(host, "GET", c, nil)
		if ok {
			m.Param("p0")
		}
	})
}

// byteSource turns fuzz input into choices; it returns 0 once the input runs out.
func byteSource(b []byte) func(n int) int {
	return func(n int) int {
		if len(b) == 0 {
			return 0
		}
		v := int(b[0]) % n
		b = b[1:]
		return v
	}
}

// BenchmarkMatch matches against 1,000 routes: 500 services, each with an
// exact route with a parameter and a prefix route.
func BenchmarkMatch(b *testing.B) {
	var routes []Route
	for i := range 500 {
		routes = append(routes,
			Route{Path: fmt.Sprintf("/svc%d/v1/items/:id", i), Methods: []string{"GET"}},
			Route{Path: fmt.Sprintf("/svc%d", i), Prefix: true, Headers: []HeaderMatch{{Name: "x-tenant", Regex: "t[0-9]+"}}},
		)
	}
	r, err := New(routes)
	if err != nil {
		b.Fatal(err)
	}
	h := http.Header{"X-Tenant": {"t42"}}
	for _, path := range []string{"/svc250/v1/items/42", "/svc250/v2/other"} {
		b.Run(path, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, ok := r.Match("api.example.com", "GET", path, h); !ok {
					b.Fatal("no match")
				}
			}
		})
	}
}
