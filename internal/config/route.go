package config

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/router"
)

// Route limits and timeout defaults (GW-03, GW-11, ADR-0011).
const (
	// MaxHeaderBytes is the traffic server's request header limit (GW-28); a
	// route's max_header_bytes can only lower it.
	MaxHeaderBytes = 64 << 10
	// MaxTotalTimeout keeps a whole request inside the traffic server's 60 s
	// write timeout (GW-28), with a second left to answer 504.
	MaxTotalTimeout = 59 * time.Second

	defaultConnectTimeout = 5 * time.Second
	defaultTotalTimeout   = 15 * time.Second
)

// HeaderTransform changes headers on the way to or from the upstream (GW-02).
type HeaderTransform struct {
	Remove []string // canonical names
	Set    []HeaderValue
	Add    []HeaderValue
}

// HeaderValue is one header value; Name is canonical.
type HeaderValue struct{ Name, Value string }

// Timeouts bound a route's upstream request (GW-11).
type Timeouts struct {
	Connect time.Duration // opening one upstream connection
	PerTry  time.Duration // one upstream attempt
	Total   time.Duration // the whole request, response body included
	Idle    time.Duration // no body bytes in either direction; 0 means none
}

// Retry is a route's compiled retry setting (GW-12, GW-13, ADR-0030).
type Retry struct {
	Attempts      int  // the first included; 1 means no retry
	NonIdempotent bool // retries every method, not only the idempotent ones (D4)
}

// MaxAttempts is the most attempts a route may set (ADR-0030).
const MaxAttempts = 5

// Apply removes, then sets, then adds headers in h.
func (t *HeaderTransform) Apply(h http.Header) {
	for _, name := range t.Remove {
		delete(h, name)
	}
	for _, v := range t.Set {
		h[v.Name] = []string{v.Value}
	}
	for _, v := range t.Add {
		h[v.Name] = append(h[v.Name], v.Value)
	}
}

// UpstreamPath rewrites a request's clean path for the upstream (GW-02):
// strip_prefix's segments come off the front, then add_prefix goes on; an
// empty result is "/".
func (r *Route) UpstreamPath(path string) string {
	for range r.strip {
		i := strings.IndexByte(path[1:], '/')
		if i < 0 {
			path = ""
			break
		}
		path = path[i+1:]
	}
	path = r.AddPrefix + path
	if path == "" {
		return "/"
	}
	return path
}

// compileTransforms fills in r's transforms, limits, timeouts, HSTS, cost and retry; path
// is the route's match.path or match.path_prefix.
func (r *Route) compileTransforms(c *configpb.Route, path string) error {
	if p := c.GetStripPrefix(); p != "" {
		if !isPrefix(p) || path != p && !strings.HasPrefix(path, p+"/") {
			return fmt.Errorf("strip_prefix %q: want whole leading segments of the route's path %q", p, path)
		}
		r.strip = strings.Count(p, "/")
	}
	if p := c.GetAddPrefix(); p != "" {
		if !isPrefix(p) {
			return fmt.Errorf("add_prefix %q: want a clean path such as /v2, not ending in /", p)
		}
		r.AddPrefix = p
	}
	var err error
	if r.RequestHeaders, err = compileHeaders(c.GetRequestHeaders()); err != nil {
		return fmt.Errorf("request_headers: %w", err)
	}
	if r.ResponseHeaders, err = compileHeaders(c.GetResponseHeaders()); err != nil {
		return fmt.Errorf("response_headers: %w", err)
	}
	if r.MaxBodyBytes = c.GetMaxBodyBytes(); r.MaxBodyBytes < 0 {
		return errors.New("max_body_bytes must not be negative")
	}
	if r.MaxHeaderBytes = int(c.GetMaxHeaderBytes()); r.MaxHeaderBytes < 0 || r.MaxHeaderBytes > MaxHeaderBytes {
		return fmt.Errorf("max_header_bytes must be 0 to %d, the server's limit", MaxHeaderBytes)
	}
	if r.Timeouts, err = compileTimeouts(c.GetTimeouts()); err != nil {
		return fmt.Errorf("timeouts.%w", err)
	}
	if v := c.GetHstsMaxAge(); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d%time.Second != 0 {
			return fmt.Errorf("hsts_max_age: %q is not a duration in whole seconds such as 8760h", v)
		}
		r.HSTS = "max-age=" + strconv.FormatInt(int64(d/time.Second), 10)
	}
	if r.Cost = c.GetCost(); r.Cost < 0 || r.Cost > maxQuantity {
		return fmt.Errorf("cost: %d is not 1 to %d; unset means 1", r.Cost, maxQuantity)
	}
	r.Cost = max(r.Cost, 1)
	r.Retry = Retry{Attempts: int(c.GetRetry().GetAttempts()), NonIdempotent: c.GetRetry().GetNonIdempotent()}
	if r.Retry.Attempts < 0 || r.Retry.Attempts > MaxAttempts {
		return fmt.Errorf("retry.attempts: want 1 to %d", MaxAttempts)
	}
	r.Retry.Attempts = max(r.Retry.Attempts, 1)
	return nil
}

// isPrefix reports whether p is a clean path of at least one segment that
// does not end in "/".
func isPrefix(p string) bool {
	return p != "/" && router.CleanPath(p) == p && !strings.HasSuffix(p, "/")
}

// gatewayHeaders are the headers a transform may not name, besides
// X-Forwarded-*: the gateway owns framing, hop-by-hop handling, forwarding
// headers (SEC-07), the request ID (GW-22) and the deadline header (GW-15),
// and a transform must not undo that.
var gatewayHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Host": true, "Content-Length": true, "Forwarded": true, "X-Request-Id": true, "Spillway-Deadline-Ms": true,
}

func compileHeaders(t *configpb.HeaderTransform) (HeaderTransform, error) {
	var out HeaderTransform
	for _, n := range t.GetRemove() {
		name, err := headerName(n)
		if err != nil {
			return out, fmt.Errorf("remove: %w", err)
		}
		out.Remove = append(out.Remove, name)
	}
	var err error
	if out.Set, err = headerValues(t.GetSet()); err != nil {
		return out, fmt.Errorf("set: %w", err)
	}
	if out.Add, err = headerValues(t.GetAdd()); err != nil {
		return out, fmt.Errorf("add: %w", err)
	}
	return out, nil
}

func headerValues(vs []*configpb.HeaderValue) ([]HeaderValue, error) {
	var out []HeaderValue
	for _, v := range vs {
		name, err := headerName(v.GetName())
		if err != nil {
			return nil, err
		}
		for _, c := range []byte(v.GetValue()) {
			if c < ' ' && c != '\t' || c == 0x7f {
				return nil, fmt.Errorf("header %q: value has a control character", v.GetName())
			}
		}
		out = append(out, HeaderValue{name, v.GetValue()})
	}
	return out, nil
}

// headerName returns the canonical form of a header name a transform may
// change.
func headerName(n string) (string, error) {
	if n == "" || strings.ContainsFunc(n, notTokenChar) {
		return "", fmt.Errorf("header name %q is not valid", n)
	}
	name := http.CanonicalHeaderKey(n)
	if gatewayHeaders[name] || strings.HasPrefix(name, "X-Forwarded-") {
		return "", fmt.Errorf("header %q is the gateway's own; transforms cannot change it", n)
	}
	return name, nil
}

func notTokenChar(c rune) bool {
	return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", c)
}

func compileTimeouts(t *configpb.Timeouts) (Timeouts, error) {
	out := Timeouts{Connect: defaultConnectTimeout, Total: defaultTotalTimeout}
	for _, f := range []struct {
		name, value string
		to          *time.Duration
	}{
		{"connect", t.GetConnect(), &out.Connect},
		{"per_try", t.GetPerTry(), &out.PerTry},
		{"total", t.GetTotal(), &out.Total},
		{"idle", t.GetIdle(), &out.Idle},
	} {
		if f.value == "" {
			continue
		}
		d, err := time.ParseDuration(f.value)
		if err != nil || d <= 0 {
			return out, fmt.Errorf("%s: %q is not a positive duration such as 200ms or 3s", f.name, f.value)
		}
		*f.to = d
	}
	if out.Total > MaxTotalTimeout {
		return out, fmt.Errorf("total: %v is more than %v, which the server's write timeout allows", out.Total, MaxTotalTimeout)
	}
	if out.PerTry == 0 {
		out.PerTry = out.Total
	}
	return out, nil
}
