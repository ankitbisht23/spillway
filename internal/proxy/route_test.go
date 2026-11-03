package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serveUpstream starts an upstream named name with handler h.
func serveUpstream(t *testing.T, name string, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{Server: httptest.NewServer(h), name: name}
	t.Cleanup(u.Close)
	return u
}

// routeTo is a config with one route on /v1/orders to u, with extra route
// fields.
func routeTo(u *upstream, fields string) string {
	return fmt.Sprintf(`
upstreams: [{name: %[1]s, endpoints: [{address: '%[1]s:%[2]s'}]}]
routes: [{name: r, upstream: %[1]s, match: {path_prefix: /v1/orders}, %[3]s}]
`, u.name, u.port(), fields)
}

// rawStatus sends raw on a new connection and returns the response status.
func rawStatus(t *testing.T, g *gateway, raw string) int {
	t.Helper()
	conn := dial(t, g.Listener.Addr().String())
	_, _ = io.WriteString(conn, raw)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestTransforms: the route rewrites the path prefix and changes request and
// response headers (GW-02).
func TestTransforms(t *testing.T) {
	u := serveUpstream(t, "orders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "orders/1.0")
		w.Header()["X-Set"] = []string{"old", "older"}
		w.Header().Set("X-Add", "old")
		_ = json.NewEncoder(w).Encode(echo{Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header})
	})
	g := newGateway(t, newResolver(u), routeTo(u, `
strip_prefix: /v1, add_prefix: /api/v2,
request_headers: {remove: [x-debug], set: [{name: x-set, value: new}], add: [{name: x-add, value: new}]},
response_headers: {remove: [server], set: [{name: x-set, value: new}], add: [{name: x-add, value: new}]}`))

	status, h, e := g.get(t, "/v1//orders/./42?x=1", http.Header{
		"X-Debug": {"1"},
		"X-Set":   {"old", "older"},
		"X-Add":   {"old"},
		"X-Kept":  {"yes"},
	})
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	for _, c := range []struct {
		what      string
		got, want []string
	}{
		{"upstream path", []string{e.Path}, []string{"/api/v2/orders/42"}},
		{"upstream query", []string{e.RawQuery}, []string{"x=1"}},
		{"removed request header", e.Header["X-Debug"], nil},
		{"set request header", e.Header["X-Set"], []string{"new"}},
		{"added request header", e.Header["X-Add"], []string{"old", "new"}},
		{"untouched request header", e.Header["X-Kept"], []string{"yes"}},
		{"X-Forwarded-For still the gateway's", e.Header["X-Forwarded-For"], []string{"127.0.0.1"}},
		{"removed response header", h["Server"], nil},
		{"set response header", h["X-Set"], []string{"new"}},
		{"added response header", h["X-Add"], []string{"old", "new"}},
	} {
		if strings.Join(c.got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s = %q, want %q", c.what, c.got, c.want)
		}
	}

	// The bare route prefix is rewritten too. TestUpstreamPath in
	// internal/config has the edge cases.
	if _, _, e := g.get(t, "/v1/orders", nil); e.Path != "/api/v2/orders" {
		t.Errorf("/v1/orders went upstream as %q, want /api/v2/orders", e.Path)
	}
}

// TestHeaderLimit: a route's max_header_bytes counts each header field as
// "Name: value\r\n", Host included, and answers 431 above it (GW-03).
func TestHeaderLimit(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), routeTo(u, "max_header_bytes: 100"))
	// "Host: x\r\n" is 9 bytes and "X-Pad: \r\n" 9 more, so 82 bytes of
	// padding reach the limit exactly.
	for pad, want := range map[int]int{82: http.StatusOK, 83: http.StatusRequestHeaderFieldsTooLarge} {
		status := rawStatus(t, g, "GET /v1/orders HTTP/1.1\r\nHost: x\r\nX-Pad: "+strings.Repeat("a", pad)+"\r\n\r\n")
		if status != want {
			t.Errorf("%d bytes of headers: status %d, want %d", 18+pad, status, want)
		}
	}
	if n := u.requests.Load(); n != 1 {
		t.Errorf("upstream served %d requests, want 1: the 431 must not reach it", n)
	}
}

// TestBodyLimit: a route's max_body_bytes answers 413 up front for a larger
// Content-Length, and while streaming for a larger chunked body (GW-03).
func TestBodyLimit(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), routeTo(u, "max_body_bytes: 10"))
	post := func(body io.Reader) (int, echo) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, g.URL+"/v1/orders", body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := g.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var e echo
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(resp.Body).Decode(&e)
		}
		return resp.StatusCode, e
	}
	chunked := func(s string) io.Reader { return io.MultiReader(strings.NewReader(s)) } // unknown length

	if status, e := post(strings.NewReader("0123456789")); status != http.StatusOK || e.Body != "0123456789" {
		t.Errorf("10 bytes with Content-Length: %d, upstream got %q", status, e.Body)
	}
	if status, e := post(chunked("0123456789")); status != http.StatusOK || e.Body != "0123456789" || e.Length != -1 {
		t.Errorf("10 bytes chunked: %d, upstream got %q with length %d", status, e.Body, e.Length)
	}
	before := u.requests.Load()
	if status, _ := post(strings.NewReader("0123456789a")); status != http.StatusRequestEntityTooLarge {
		t.Errorf("11 bytes with Content-Length: %d, want 413", status)
	}
	if u.requests.Load() != before {
		t.Error("a Content-Length over the limit reached the upstream")
	}
	if status, _ := post(chunked(strings.Repeat("a", 64<<10))); status != http.StatusRequestEntityTooLarge {
		t.Errorf("64 KiB chunked: %d, want 413", status)
	}

	// The limit holds even when the client claims a small Content-Length
	// and sends chunked: Go frames by Transfer-Encoding (ADR-0009).
	status := rawStatus(t, g, "POST /v1/orders HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\nb\r\n0123456789a\r\n0\r\n\r\n")
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("CL 5 + 11 chunked bytes: %d, want 413", status)
	}
}

// stall is an upstream handler that sends nothing until the request ends.
func stall(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

// getTimed returns a GET's status and how long it took.
func getTimed(t *testing.T, g *gateway, path string) (int, time.Duration) {
	t.Helper()
	start := time.Now()
	status, _, _ := g.get(t, path, nil)
	return status, time.Since(start)
}

// TestTimeouts: per_try and total each answer 504 when the upstream stalls
// (GW-11).
func TestTimeouts(t *testing.T) {
	u := serveUpstream(t, "slow", stall)
	for _, timeouts := range []string{"{per_try: 100ms, total: 10s}", "{total: 100ms}"} {
		g := newGateway(t, newResolver(u), routeTo(u, "timeouts: "+timeouts))
		status, took := getTimed(t, g, "/v1/orders")
		if status != http.StatusGatewayTimeout || took < 100*time.Millisecond || took > 5*time.Second {
			t.Errorf("timeouts %s: %d after %v, want 504 after about 100ms", timeouts, status, took)
		}
	}
}

// TestConnectTimeout: a dial that never completes is given up after the
// route's connect timeout and answered with 504 (GW-11).
func TestConnectTimeout(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), routeTo(u, "timeouts: {connect: 100ms, total: 10s}"))
	budget := make(chan time.Duration, 1)
	g.reg.dialer.ControlContext = func(ctx context.Context, _, _ string, _ syscall.RawConn) error {
		deadline, _ := ctx.Deadline()
		budget <- time.Until(deadline)
		<-ctx.Done() // a blackholed SYN
		return ctx.Err()
	}
	status, took := getTimed(t, g, "/v1/orders")
	if status != http.StatusGatewayTimeout || took > 5*time.Second {
		t.Errorf("%d after %v, want 504 after about 100ms", status, took)
	}
	if b := <-budget; b <= 0 || b > 100*time.Millisecond {
		t.Errorf("the dial had %v, want the route's 100ms connect timeout", b)
	}
}

// TestIdleTimeout: idle aborts a request once no body bytes have moved in
// either direction for its duration, however long the request has run
// (GW-11).
func TestIdleTimeout(t *testing.T) {
	type read struct {
		body string
		err  error
	}
	reads := make(chan read, 1)
	u := serveUpstream(t, "stream", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("upstream") {
		case "silent": // before the response headers
			<-r.Context().Done()
		case "trickle": // a byte every 50ms, for longer than the idle timeout
			for range 10 {
				_, _ = io.WriteString(w, ".")
				w.(http.Flusher).Flush()
				time.Sleep(50 * time.Millisecond)
			}
		case "stops": // part of the body, then nothing
			_, _ = io.WriteString(w, "part")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "reads":
			body, err := io.ReadAll(r.Body)
			reads <- read{string(body), err}
		}
	})
	g := newGateway(t, newResolver(u), routeTo(u, "timeouts: {idle: 200ms, total: 10s}"))

	if status, took := getTimed(t, g, "/v1/orders?upstream=silent"); status != http.StatusGatewayTimeout || took > 5*time.Second {
		t.Errorf("silent upstream: %d after %v, want 504 after about 200ms", status, took)
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/v1/orders?upstream=trickle", nil)
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || string(body) != ".........." {
		t.Errorf("trickling upstream: %d, body %q, %v; want all 10 bytes", resp.StatusCode, body, err)
	}

	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/v1/orders?upstream=stops", nil)
	start := time.Now()
	resp, err = g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "part" || err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("upstream that stops: body %q, %v after %v; want \"part\" then an abort after about 200ms", body, err, time.Since(start))
	}

	// A client that trickles its body keeps the request alive; one that
	// stops sending gets the upstream request aborted.
	const post = "POST /v1/orders?upstream=reads HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n"
	conn := dial(t, g.Listener.Addr().String())
	_, _ = io.WriteString(conn, post)
	for range 10 {
		_, _ = io.WriteString(conn, "1\r\n.\r\n")
		time.Sleep(50 * time.Millisecond)
	}
	_, _ = io.WriteString(conn, "0\r\n\r\n")
	if got := <-reads; got.body != ".........." || got.err != nil {
		t.Errorf("trickling client: upstream read %q, %v; want all 10 bytes", got.body, got.err)
	}

	conn = dial(t, g.Listener.Addr().String())
	_, _ = io.WriteString(conn, post+"4\r\npart\r\n")
	select {
	case got := <-reads:
		if got.err == nil {
			t.Error("stalled client: the upstream read a complete body")
		}
	case <-time.After(5 * time.Second):
		t.Error("stalled client: the upstream request was not aborted")
	}
}

// TestIdleTimeoutUpgrade: an upgraded connection is not cut by the idle
// timeout, and its body stays writable.
func TestIdleTimeoutUpgrade(t *testing.T) {
	u := serveUpstream(t, "ws", func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = rw.WriteString(line)
		_ = rw.Flush()
	})
	g := newGateway(t, newResolver(u), routeTo(u, "timeouts: {idle: 100ms}"))
	conn := dial(t, g.Listener.Addr().String())
	_, _ = io.WriteString(conn, "GET /v1/orders HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade answered %d", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond)
	_, _ = io.WriteString(conn, "ping\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := br.ReadString('\n'); line != "ping\n" {
		t.Errorf("after the idle timeout the upgraded connection answered %q, %v", line, err)
	}
}
