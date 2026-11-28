package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/demo"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// pgKeys is a demo.KeyStore with one consumer per tenant.
type pgKeys struct{}

const pgKey = "spw_playgroundtestkey" //nolint:gosec // G101: a test value

func (pgKeys) PlaygroundConsumer(context.Context, string) (int64, error) { return 7, nil }
func (pgKeys) IssueKey(context.Context, int64, *time.Time) (int64, secret.Value, error) {
	return 1, secret.New(pgKey), nil
}

// TestPlaygroundStartBody: a start's body is checked before anything
// else: an unknown kind or field is 400, a missing field, a value out of
// range or a field the kind does not take is 422 naming the field; a valid
// body reaches the Playground (here none: 503). Every role may start runs
// (P46).
func TestPlaygroundStartBody(t *testing.T) {
	h := newHandler(t, io.Discard)
	const url = "/api/v1/demo/playground/runs"
	for body, want := range map[string]string{
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 10}`:                    "503",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 200, "duration_s": 60}`: "503",
		`{"kind": "scenario", "scenario": "d2"}`:                                                    "503",
		`{"kind": "other"}`:                                                                         "400 invalid_request",
		`{}`:                                                                                        "400 invalid_request",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 10, "extra": 1}`:       "400 invalid_request",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 1.5}`:                  "400 invalid_request",
		`{"kind": "custom", "route": "orders", "rate": 10}`:                                        "422 tenant",
		`{"kind": "custom", "tenant": "acme", "route": "orders", "rate": 10}`:                      "422 tenant",
		`{"kind": "custom", "tenant": "free-co", "rate": 10}`:                                      "422 route",
		`{"kind": "custom", "tenant": "free-co", "route": "admin", "rate": 10}`:                    "422 route",
		`{"kind": "custom", "tenant": "free-co", "route": "orders"}`:                               "422 rate",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 0}`:                    "422 rate",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 201}`:                  "422 rate",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 10, "duration_s": 4}`:  "422 duration_s",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 10, "duration_s": 61}`: "422 duration_s",
		`{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 10, "scenario": "d1"}`: "422 scenario",
		`{"kind": "scenario"}`:                                        "422 scenario",
		`{"kind": "scenario", "scenario": "d4"}`:                      "422 scenario",
		`{"kind": "scenario", "scenario": "d1", "rate": 10}`:          "422 rate",
		`{"kind": "scenario", "scenario": "d1", "tenant": "free-co"}`: "422 tenant",
	} {
		a := do(t, h, "POST", url, body, "Authorization", "Bearer "+token("viewer"))
		got := ""
		switch a.status {
		case 503:
			got = "503"
		case 400:
			got = "400 " + string(a.err.Code)
		case 422:
			if a.err.Details != nil && (*a.err.Details)[0].Path != nil {
				got = "422 " + *(*a.err.Details)[0].Path
			}
		default:
			got = http.StatusText(a.status)
		}
		if got != want {
			t.Errorf("%s: %d %q (%s), want %s", body, a.status, a.err.Error, got, want)
		}
	}
	for _, role := range Roles {
		if a := do(t, h, "POST", url, `{"kind": "scenario", "scenario": "d1"}`, "Authorization", "Bearer "+token(role)); a.status != 503 {
			t.Errorf("%s: %d %s, want the Playground's 503", role, a.status, a.err.Error)
		}
	}
}

// playgroundServer is the API with a Playground over gateway and no store.
func playgroundServer(t *testing.T, gateway string, log io.Writer) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, nil, nil, fakeAuth{}, secret.Value{}, slog.New(slog.NewJSONHandler(log, nil))).WithDemo(true)
	s.playground = demo.NewPlayground(demo.PlaygroundConfig{Gateways: []string{gateway}, Keys: s.demoKeys, Store: pgKeys{}, Log: s.log})
	t.Cleanup(s.ShutdownPlayground)
	h := s.Handler()
	return s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				w.WriteHeader(599)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func okGateway(t *testing.T) string {
	t.Helper()
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("RateLimit", `"consumer-plan";r=9;t=1`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(g.Close)
	return g.URL
}

// TestPlaygroundOwnRunsOnly: only the caller who started a run may watch
// or stop it; to anyone else, as to a run that does not exist, it is 404
// (ADR-0096 §3). The owner's stream is SSE with ready first; the owner's
// stop reaches the audit log's store (599 here). A third stream of one
// run is 429 with Retry-After. Nothing logged carries the key or a token.
func TestPlaygroundOwnRunsOnly(t *testing.T) {
	var logs lockedBuffer
	s, h := playgroundServer(t, okGateway(t), &logs)
	run, err := s.playground.Start(t.Context(), demo.Caller{Sub: "user-viewer", IP: "198.51.100.1"}, demo.CustomShape("free-co", "orders", 5, demo.MaxDuration))
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/demo/playground/runs/"
	for _, path := range []string{base + run.ID, base + "00000000-0000-4000-8000-000000000000", base + "x"} {
		for _, role := range []string{"admin", "key-admin"} {
			if a := do(t, h, "DELETE", path, "", "Authorization", "Bearer "+token(role)); a.status != 404 || a.err.Error != "no such run" {
				t.Errorf("DELETE %s as %s: %d %q", path, role, a.status, a.err.Error)
			}
			if a := do(t, h, "GET", path+"/stream", "", "Authorization", "Bearer "+token(role)); a.status != 404 || a.err.Error != "no such run" {
				t.Errorf("GET %s/stream as %s: %d %q", path, role, a.status, a.err.Error)
			}
		}
	}
	if run.Ended() {
		t.Fatal("another caller's stop ended the run")
	}

	srv := httptest.NewServer(h)
	defer srv.Close()
	open := func() *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+base+run.ID+"/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token("viewer"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	r1, r2 := open(), open()
	defer func() { _ = r1.Body.Close(); _ = r2.Body.Close() }()
	if r1.StatusCode != 200 || r1.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || r1.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("owner's stream: %d %v", r1.StatusCode, r1.Header)
	}
	line, _ := bufio.NewReader(r1.Body).ReadString('\n')
	if line != "event: ready\n" {
		t.Errorf("first line %q", line)
	}
	r3 := open()
	body, _ := io.ReadAll(r3.Body)
	_ = r3.Body.Close()
	if r3.StatusCode != 429 || r3.Header.Get("Retry-After") == "" || !strings.Contains(string(body), `"code":"unavailable"`) {
		t.Errorf("a third stream: %d %v %s", r3.StatusCode, r3.Header, body)
	}

	if a := do(t, h, "DELETE", base+run.ID, "", "Authorization", "Bearer "+token("viewer")); a.status != 599 {
		t.Errorf("the owner's stop: %d, want it to reach the store", a.status)
	}
	for !run.Ended() {
		time.Sleep(10 * time.Millisecond)
	}
	for _, bad := range []string{pgKey, tokenSecret, "198.51.100.1"} {
		if strings.Contains(logs.String(), bad) {
			t.Errorf("the log has %q", bad)
		}
	}
}

// TestPlaygroundStartCap: a start past a cap is 429 unavailable with
// Retry-After, before any audit record (no store here): this caller's
// address already runs one.
func TestPlaygroundStartCap(t *testing.T) {
	s, h := playgroundServer(t, okGateway(t), io.Discard)
	// httptest's requests come from 192.0.2.1.
	if _, err := s.playground.Start(t.Context(), demo.Caller{Sub: "someone", IP: "192.0.2.1"}, demo.CustomShape("pro-co", "catalog", 5, demo.MaxDuration)); err != nil {
		t.Fatal(err)
	}
	a := do(t, h, "POST", "/api/v1/demo/playground/runs", `{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 10}`, "Authorization", "Bearer "+token("viewer"))
	if a.status != 429 || a.err.Code != "unavailable" || a.header.Get("Retry-After") == "" || !strings.Contains(a.err.Error, "from your address") {
		t.Errorf("%d %v %+v", a.status, a.header, a.err)
	}
}

// TestClientIP: the caps count IPv4 by address and IPv6 by /64; without
// trusted proxies the TCP peer is the client whatever CF-Connecting-IP
// says.
func TestClientIP(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.1:1234":             "192.0.2.1",
		"[2001:db8:1:2:3:4:5:6]:443": "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::1]:443": "2001:db8:1:2::/64",
		"[::ffff:192.0.2.7]:80":      "192.0.2.7",
		"[fe80::1%eth0]:80":          "fe80::/64",
		"not an address":             "not an address",
	} {
		if got := clientIP(nil, addr, nil); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", addr, got, want)
		}
		if got := clientIP(nil, addr, []string{"203.0.113.7"}); got != want {
			t.Errorf("clientIP(%q) with CF-Connecting-IP and no trusted proxy = %q, want the peer %q", addr, got, want)
		}
	}
}

// TestClientIPTrustedProxy: CF-Connecting-IP names the client only when
// the TCP peer is a trusted proxy and the header holds exactly one valid
// IP address; otherwise the peer is the client (SEC-05, ADR-0097 §2).
func TestClientIPTrustedProxy(t *testing.T) {
	trusted, err := proxy.ParseTrustedProxies("172.29.255.10/32,2001:db8:ff::/48")
	if err != nil {
		t.Fatal(err)
	}
	const caddy = "172.29.255.10:41000"
	for _, c := range []struct {
		name, peer string
		header     []string
		want       string
	}{
		{"trusted peer, valid IPv4", caddy, []string{"203.0.113.7"}, "203.0.113.7"},
		{"trusted peer, padded", caddy, []string{" 203.0.113.7 "}, "203.0.113.7"},
		{"trusted peer, IPv6 per /64", caddy, []string{"2001:db8:1:2:3:4:5:6"}, "2001:db8:1:2::/64"},
		{"trusted peer, IPv4-mapped", caddy, []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		{"trusted IPv6 peer", "[2001:db8:ff:1::5]:443", []string{"198.51.100.4"}, "198.51.100.4"},
		{"trusted mapped peer", "[::ffff:172.29.255.10]:41000", []string{"198.51.100.4"}, "198.51.100.4"},
		{"trusted peer, missing", caddy, nil, "172.29.255.10"},
		{"trusted peer, empty", caddy, []string{""}, "172.29.255.10"},
		{"trusted peer, invalid", caddy, []string{"not-an-ip"}, "172.29.255.10"},
		{"trusted peer, with a port", caddy, []string{"203.0.113.7:80"}, "172.29.255.10"},
		{"trusted peer, with a zone", caddy, []string{"fe80::1%eth0"}, "172.29.255.10"},
		{"trusted peer, a list", caddy, []string{"203.0.113.7, 198.51.100.4"}, "172.29.255.10"},
		{"trusted peer, two field lines", caddy, []string{"203.0.113.7", "198.51.100.4"}, "172.29.255.10"},
		{"untrusted peer with the header", "172.29.0.20:5000", []string{"203.0.113.7"}, "172.29.0.20"},
		{"untrusted neighbour of the proxy", "172.29.255.11:5000", []string{"203.0.113.7"}, "172.29.255.11"},
		{"untrusted IPv6 peer with the header", "[2001:db8:1:2::9]:443", []string{"203.0.113.7"}, "2001:db8:1:2::/64"},
	} {
		if got := clientIP(trusted, c.peer, c.header); got != c.want {
			t.Errorf("%s: clientIP(%q, %q) = %q, want %q", c.name, c.peer, c.header, got, c.want)
		}
	}
}

// TestGetDemoClientIP: getDemo's client_ip is the address the caps count
// the caller under: CF-Connecting-IP only from a trusted proxy. A forged
// header from anyone else is ignored (SEC-05, ADR-0097 §2).
func TestGetDemoClientIP(t *testing.T) {
	trusted, err := proxy.ParseTrustedProxies("172.29.255.10/32")
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	s := New(nil, nil, nil, fakeAuth{}, secret.Value{}, logger(io.Discard)).WithTrustedProxies(trusted)
	h := s.withClientIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = clientIPOf(r.Context()) }))
	for peer, want := range map[string]string{
		"172.29.255.10:41000": "203.0.113.7", // the demo proxy
		"172.29.0.31:52000":   "172.29.0.31", // anything else on the network
		"192.0.2.1:1234":      "192.0.2.1",
	} {
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/demo", nil)
		r.RemoteAddr = peer
		r.Header.Set("CF-Connecting-IP", "203.0.113.7")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if seen != want {
			t.Errorf("from %s with CF-Connecting-IP 203.0.113.7: client IP %q, want %q", peer, seen, want)
		}
	}
}
