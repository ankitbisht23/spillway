package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// memoryChildEnv makes TestMemoryBudget act as the measurement's client and
// upstream, in a process of its own.
const memoryChildEnv = "SPILLWAY_MEMORY_CHILD"

// memoryClients is how many connections or requests each case opens.
const memoryClients = 1000

// TestMemoryBudget measures the heap and goroutine stacks a gateway holds
// per idle client connection and per request in flight, for the budget of
// GW-26's default caps (ADR-0030). It runs only with
// SPILLWAY_MEASURE_MEMORY=1:
//
//	SPILLWAY_MEASURE_MEMORY=1 go test -run '^TestMemoryBudget$' -v ./internal/proxy
//
// The clients and the upstream run in a second process (this test binary),
// so that only the gateway's memory is measured. A request in flight is
// held with its response headers and first body bytes passed on, so that
// it holds everything a request can: both connections, the reverse proxy's
// copy buffer and, for the PUT cases, a 64 KiB body buffered for retries.
func TestMemoryBudget(t *testing.T) {
	if os.Getenv(memoryChildEnv) != "" {
		memoryChild(t)
		return
	}
	if os.Getenv("SPILLWAY_MEASURE_MEMORY") == "" {
		t.Skip("set SPILLWAY_MEASURE_MEMORY=1 to measure (ADR-0030)")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMemoryBudget$") //nolint:gosec // this test binary, run again as the child
	cmd.Env = append(os.Environ(), memoryChildEnv+"=1")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	lines := bufio.NewScanner(stdout)
	expect := func(prefix string) string {
		t.Helper()
		for lines.Scan() {
			if word, v, _ := strings.Cut(lines.Text(), " "); word == prefix {
				return v
			}
			t.Log("child:", lines.Text())
		}
		t.Fatalf("child: no %q line", prefix)
		return ""
	}
	_, port, _ := net.SplitHostPort(expect("upstream"))

	// The gateway: the handler and both listeners, as cmd/gateway serves
	// them, with the default caps.
	discard := slog.New(slog.DiscardHandler)
	g := newGateway(t, newResolver(&upstream{name: "up"}), fmt.Sprintf(`
upstreams: [{name: up, endpoints: [{address: 'up:%s'}]}]
routes: [{name: r, upstream: up, match: {path_prefix: /}, retry: {attempts: 2}, timeouts: {total: 59s, per_try: 59s}}]
`, port))
	rec := telemetry.NewRecorder(prometheus.NewRegistry(), discard, 0)
	h := NewHandler(g.current.Load, g.identity.Load, nil, g.reg, g.limiters, nil, false, 4096, nil, rec, discard)
	conns := NewConnLimit(8192, rec, discard)
	certs, _, certFile, _ := newCerts(t, 1)
	tlsConfig := certs.TLSConfig()
	srv := NewServer(h, discard)
	srv.TLSConfig = tlsConfig
	plain, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secure, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(conns.Listener(plain, nil)) }()
	go func() { _ = srv.ServeTLS(conns.Listener(secure, tlsConfig), "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	inUse := func() float64 { // KiB
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapInuse+m.StackInuse) / 1024
	}
	cost := map[string]float64{} // KiB
	for _, c := range []string{"idle-plain", "idle-tls", "idle-h2", "hold-plain", "hold-tls", "hold-h2", "hold-plain-put", "hold-tls-put", "hold-h2-put"} {
		// One round first, so that the upstream connections and pools the
		// case needs exist before the baseline.
		var per float64
		for round := range 2 {
			before := inUse()
			_, _ = fmt.Fprintf(stdin, "%s %d %s %s %s\n", c, memoryClients, plain.Addr(), secure.Addr(), certFile)
			expect("ready")
			// A client that failed would make the case look cheap.
			if n, m := conns.Open(), h.InFlight(); n != memoryClients || strings.HasPrefix(c, "hold-") != (m == memoryClients) {
				t.Fatalf("%s: %d connections open and %d requests in flight, want %d", c, n, m, memoryClients)
			}
			per = (inUse() - before) / memoryClients
			_, _ = fmt.Fprintln(stdin, "release")
			expect("released")
			waitFor(t, c+" closed", func() bool { return conns.Open() == 0 && h.InFlight() == 0 })
			if round == 1 {
				t.Logf("%-15s %6.1f KiB each", c, per)
			}
		}
		cost[c] = per
	}
	idle := max(cost["idle-plain"], cost["idle-tls"], cost["idle-h2"])
	var request float64 // a request's cost on top of its idle connection
	for _, kind := range []string{"plain", "tls", "h2"} {
		request = max(request, cost["hold-"+kind+"-put"]-cost["idle-"+kind], cost["hold-"+kind]-cost["idle-"+kind])
	}
	worst := 8192*idle + 4096*request
	t.Logf("worst case at 8192 connections and 4096 requests in flight: 8192 × %.1f KiB + 4096 × %.1f KiB = %.2f GiB",
		idle, request, worst/(1<<20))
}

// memoryChild serves the upstream and runs the clients for TestMemoryBudget,
// one case per line on stdin.
func memoryChild(t *testing.T) {
	var mu sync.Mutex
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/hold" {
			return
		}
		mu.Lock()
		held := release
		mu.Unlock()
		_, _ = io.WriteString(w, "part")
		_ = http.NewResponseController(w).Flush()
		<-held
	}))
	defer up.Close()
	fmt.Println("upstream", up.Listener.Addr())
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var kind, plain, secure, certFile string
		var n int
		if _, err := fmt.Sscan(in.Text(), &kind, &n, &plain, &secure, &certFile); err != nil {
			t.Fatal(err)
		}
		pem, err := os.ReadFile(certFile) //nolint:gosec // the parent's test certificate
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(pem)
		closers := make([]func(), n)
		var wg sync.WaitGroup
		sem := make(chan struct{}, 64)
		for i := range n {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				closers[i] = memoryClient(t, kind, plain, secure, roots)
			})
		}
		wg.Wait()
		fmt.Println("ready")
		in.Scan()
		mu.Lock()
		close(release)
		release = make(chan struct{})
		mu.Unlock()
		for _, c := range closers {
			c()
		}
		fmt.Println("released")
	}
}

// memoryClient opens one connection of the kind TestMemoryBudget names and
// returns once it is idle after a request, or once its request's first body
// bytes have arrived; the returned function closes it.
func memoryClient(t *testing.T, kind, plain, secure string, roots *x509.CertPool) func() {
	method, path, body := http.MethodGet, "/", ""
	if strings.HasPrefix(kind, "hold-") {
		path = "/hold"
	}
	if strings.HasSuffix(kind, "-put") {
		method, body = http.MethodPut, strings.Repeat("a", maxRetryBody)
	}
	cfg := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	if strings.Contains(kind, "-h2") {
		tr := &http.Transport{TLSClientConfig: cfg.Clone(), ForceAttemptHTTP2: true}
		tr.TLSClientConfig.NextProtos = nil
		req, _ := http.NewRequestWithContext(t.Context(), method, "https://"+secure+path, strings.NewReader(body))
		resp, err := tr.RoundTrip(req) //nolint:bodyclose // closed by the returned function
		if err != nil || resp.ProtoMajor != 2 {
			t.Errorf("%s: %v", kind, err)
			return func() {}
		}
		readSome(t, kind, resp)
		return func() { _ = resp.Body.Close(); tr.CloseIdleConnections() }
	}
	var c net.Conn
	var err error
	if strings.Contains(kind, "-tls") {
		c, err = (&tls.Dialer{Config: cfg}).DialContext(t.Context(), "tcp", secure)
	} else {
		c, err = (&net.Dialer{}).DialContext(t.Context(), "tcp", plain)
	}
	if err != nil {
		t.Errorf("%s: %v", kind, err)
		return func() {}
	}
	_, _ = fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: up\r\nContent-Length: %d\r\n\r\n%s", method, path, len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Errorf("%s: %v", kind, err)
		return func() { _ = c.Close() }
	}
	readSome(t, kind, resp)
	return func() { _ = c.Close() }
}

// readSome reads a held response's first body bytes, or an idle one's whole
// body.
func readSome(t *testing.T, kind string, resp *http.Response) {
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%s: status %d", kind, resp.StatusCode)
	}
	if strings.HasPrefix(kind, "hold-") {
		_, _ = io.ReadFull(resp.Body, make([]byte, len("part")))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
