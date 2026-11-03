// Package router matches requests to routes on host, method, path and headers
// (GW-01). Each host has a radix tree whose edges are whole path segments, plus
// one tree for routes without a host; ADR-0007 gives the precedence rules.
package router

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Route is one route's match criteria. New compiles a list of routes; Match
// returns the index of the winning route in that list.
type Route struct {
	Host     string        // exact host, case-insensitive, without a port; "" matches any host
	Methods  []string      // empty matches any method
	Path     string        // a clean path; a segment ":name" matches any non-empty segment
	Prefix   bool          // also match every path below Path, on segment boundaries
	Headers  []HeaderMatch // all must match
	Priority int           // higher wins between otherwise equal routes
}

// HeaderMatch matches when any value of header Name equals Exact or fully
// matches Regex. Exactly one of Exact and Regex is set.
type HeaderMatch struct {
	Name  string
	Exact string
	Regex string
}

// Router is an immutable set of compiled routes, safe for concurrent use.
type Router struct {
	hosts map[string]*node
	any   *node
}

// node is one path segment. Static children are tried before the parameter
// child, so a depth-first walk meets candidates in precedence order.
type node struct {
	static map[string]*node
	param  *node
	exact  []*entry // routes whose path ends here
	prefix []*entry // prefix routes whose path ends here
}

type entry struct {
	index    int
	methods  []string
	headers  []headerMatch
	params   []param
	preds    int // method restriction (0 or 1) plus header predicates
	priority int
}

type headerMatch struct {
	name  string // canonical form, as http.Header keys are
	exact string
	re    *regexp.Regexp
}

type param struct {
	name string
	seg  int // segment index in the path
}

// RouteError is the error New returns for an invalid route.
type RouteError struct {
	Index int // index of the route in the list given to New
	Err   error
}

func (e *RouteError) Error() string { return fmt.Sprintf("route %d: %v", e.Index, e.Err) }
func (e *RouteError) Unwrap() error { return e.Err }

// New compiles routes into a Router.
func New(routes []Route) (*Router, error) {
	r := &Router{hosts: map[string]*node{}, any: &node{}}
	for i, rt := range routes {
		e, segs, err := compile(i, rt)
		if err != nil {
			return nil, &RouteError{Index: i, Err: err}
		}
		n := r.any
		if rt.Host != "" {
			h := hostKey(rt.Host)
			if r.hosts[h] == nil {
				r.hosts[h] = &node{}
			}
			n = r.hosts[h]
		}
		for _, s := range segs {
			if strings.HasPrefix(s, ":") {
				if n.param == nil {
					n.param = &node{}
				}
				n = n.param
				continue
			}
			if n.static[s] == nil {
				if n.static == nil {
					n.static = map[string]*node{}
				}
				n.static[s] = &node{}
			}
			n = n.static[s]
		}
		if rt.Prefix {
			n.prefix = append(n.prefix, e)
		} else {
			n.exact = append(n.exact, e)
		}
	}
	r.any.sort()
	for _, n := range r.hosts {
		n.sort()
	}
	return r, nil
}

func compile(index int, rt Route) (*entry, []string, error) {
	if rt.Host != "" && (strings.ContainsAny(rt.Host, ":/ ") || hostKey(rt.Host) == "") {
		return nil, nil, fmt.Errorf("host %q must be a host name without a port", rt.Host)
	}
	segs, err := splitPattern(rt.Path, rt.Prefix)
	if err != nil {
		return nil, nil, err
	}
	e := &entry{index: index, methods: rt.Methods, priority: rt.Priority}
	for i, s := range segs {
		name, ok := strings.CutPrefix(s, ":")
		if !ok {
			continue
		}
		if !isToken(name) {
			return nil, nil, fmt.Errorf("path %q: parameter %q needs a name", rt.Path, s)
		}
		for _, p := range e.params {
			if p.name == name {
				return nil, nil, fmt.Errorf("path %q: parameter %q appears twice", rt.Path, name)
			}
		}
		e.params = append(e.params, param{name: name, seg: i})
	}
	for _, m := range rt.Methods {
		if !isToken(m) {
			return nil, nil, fmt.Errorf("method %q is not a valid HTTP method", m)
		}
	}
	if len(rt.Methods) > 0 {
		e.preds++
	}
	for _, hm := range rt.Headers {
		if !isToken(hm.Name) {
			return nil, nil, fmt.Errorf("header name %q is not valid", hm.Name)
		}
		name := textproto.CanonicalMIMEHeaderKey(hm.Name)
		if name == "Host" {
			return nil, nil, errors.New("match the Host header with the route's host, not a header predicate")
		}
		if (hm.Exact == "") == (hm.Regex == "") {
			return nil, nil, fmt.Errorf("header %q: set exactly one of exact and regex", hm.Name)
		}
		m := headerMatch{name: name, exact: hm.Exact}
		if hm.Regex != "" {
			if m.re, err = regexp.Compile("^(?:" + hm.Regex + ")$"); err != nil {
				return nil, nil, fmt.Errorf("header %q: %w", hm.Name, err)
			}
		}
		e.headers = append(e.headers, m)
		e.preds++
	}
	return e, segs, nil
}

// splitPattern returns a route path's segments. A prefix of "/" has none, so
// it matches every path.
func splitPattern(p string, prefix bool) ([]string, error) {
	if p == "" || p[0] != '/' {
		return nil, fmt.Errorf("path %q must start with /", p)
	}
	if CleanPath(p) != p {
		return nil, fmt.Errorf("path %q is not clean: no //, . or .. segments", p)
	}
	if prefix {
		if p == "/" {
			return nil, nil
		}
		if strings.HasSuffix(p, "/") {
			return nil, fmt.Errorf("path prefix %q must not end in /: prefixes match whole segments", p)
		}
	}
	return strings.Split(p[1:], "/"), nil
}

// sort orders each node's routes by predicate count, then priority; the stable
// sort keeps config order for the rest.
func (n *node) sort() {
	byPrecedence := func(a, b *entry) int {
		return cmp.Or(cmp.Compare(b.preds, a.preds), cmp.Compare(b.priority, a.priority))
	}
	slices.SortStableFunc(n.exact, byPrecedence)
	slices.SortStableFunc(n.prefix, byPrecedence)
	for _, c := range n.static {
		c.sort()
	}
	if n.param != nil {
		n.param.sort()
	}
}

// Match is the winning route and the request path it matched.
type Match struct {
	Index  int // index of the route in the list given to New
	path   string
	params []param
}

// Param returns the path segment matched by the parameter name, or "".
func (m Match) Param(name string) string {
	for _, p := range m.params {
		if p.name == name {
			return segment(m.path, p.seg)
		}
	}
	return ""
}

// Match returns the route for a request. path must already be clean
// (CleanPath); host may carry a port. Host-specific routes win over routes
// without a host; within each, ADR-0007's order applies.
func (r *Router) Match(host, method, path string, h http.Header) (Match, bool) {
	if path == "" || path[0] != '/' {
		return Match{}, false
	}
	if len(r.hosts) > 0 {
		if t := r.hosts[hostKey(host)]; t != nil {
			if e := t.match(path, method, h); e != nil {
				return Match{Index: e.index, path: path, params: e.params}, true
			}
		}
	}
	if e := r.any.match(path, method, h); e != nil {
		return Match{Index: e.index, path: path, params: e.params}, true
	}
	return Match{}, false
}

type search struct {
	path      string
	method    string
	header    http.Header
	best      *entry // longest prefix match so far
	bestDepth int
}

func (n *node) match(path, method string, h http.Header) *entry {
	s := search{path: path, method: method, header: h, bestDepth: -1}
	if e := s.walk(n, 1, 0); e != nil {
		return e
	}
	return s.best
}

// walk visits n, which matched depth segments; pos is where the next segment
// starts, or -1 when the path is used up. It returns the first exact match,
// which is the best one; prefix matches are kept in s.best, and only a deeper
// one replaces it.
func (s *search) walk(n *node, pos, depth int) *entry {
	if depth > s.bestDepth {
		if e := s.first(n.prefix); e != nil {
			s.best, s.bestDepth = e, depth
		}
	}
	if pos < 0 {
		return s.first(n.exact)
	}
	end, next := len(s.path), -1
	if i := strings.IndexByte(s.path[pos:], '/'); i >= 0 {
		end = pos + i
		next = end + 1
	}
	seg := s.path[pos:end]
	if c := n.static[seg]; c != nil {
		if e := s.walk(c, next, depth+1); e != nil {
			return e
		}
	}
	if n.param != nil && seg != "" {
		return s.walk(n.param, next, depth+1)
	}
	return nil
}

func (s *search) first(es []*entry) *entry {
	for _, e := range es {
		if e.matches(s.method, s.header) {
			return e
		}
	}
	return nil
}

func (e *entry) matches(method string, h http.Header) bool {
	if len(e.methods) > 0 && !slices.Contains(e.methods, method) {
		return false
	}
	for _, hm := range e.headers {
		if !slices.ContainsFunc(h[hm.name], hm.match) {
			return false
		}
	}
	return true
}

func (hm headerMatch) match(v string) bool {
	if hm.re != nil {
		return hm.re.MatchString(v)
	}
	return v == hm.exact
}

// CleanPath returns p with duplicate slashes and . and .. segments removed,
// keeping a trailing slash. The gateway routes on the cleaned path and
// forwards that path, so an upstream never sees one that would have matched a
// different route.
func CleanPath(p string) string {
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	c := path.Clean(p)
	if c != "/" && strings.HasSuffix(p, "/") {
		c += "/"
	}
	return c
}

// hostKey drops the port and a trailing dot and lower-cases the host.
func hostKey(h string) string {
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	h = strings.TrimSuffix(h, ".")
	for i := range len(h) {
		if 'A' <= h[i] && h[i] <= 'Z' {
			return strings.ToLower(h)
		}
	}
	return h
}

// segment returns segment i of a path that starts with "/".
func segment(p string, i int) string {
	p = p[1:]
	for ; i > 0; i-- {
		j := strings.IndexByte(p, '/')
		if j < 0 {
			return ""
		}
		p = p[j+1:]
	}
	if j := strings.IndexByte(p, '/'); j >= 0 {
		return p[:j]
	}
	return p
}

// isToken reports whether s is a non-empty RFC 9110 token, as header names
// and methods must be.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isTokenChar(s[i]) {
			return false
		}
	}
	return true
}

func isTokenChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
