package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Fault modes of the mock (ADR-0031).
const (
	modeNone    = "none"    // healthy: 200 with 1,024 bytes
	mode5xx     = "5xx"     // 503 with an empty body, to everything
	modeRefused = "refused" // traffic listener and its connections closed
)

// mockBody is what the mock answers with while healthy, as go-httpbin's
// /bytes/1024 would.
var mockBody = bytes.Repeat([]byte{'x'}, 1024)

// runMock is "chaos mock": the fault-injecting upstream of make verify-m4
// (ADR-0031), the compose service upstream-fault. Any path on -addr answers
// 200 with 1,024 bytes until the control port's POST /fault?mode=5xx|refused|none
// changes that: 5xx answers 503 to everything, refused closes the traffic
// listener and every connection on it, so the kernel refuses new ones, and
// none is healthy again. The control port also serves GET /healthz. With
// -set or -healthcheck it is a client of a running mock's control port
// instead, for the verify script and the compose healthcheck.
//
//	chaos mock [-addr :8080] [-control :8081] [-set mode | -healthcheck]
func runMock(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("chaos mock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8080", "traffic address")
	control := fs.String("control", ":8081", "control address (POST /fault?mode=, GET /healthz)")
	set := fs.String("set", "", "set a running mock's mode (none, 5xx or refused) through -control, then exit")
	healthcheck := fs.Bool("healthcheck", false, "check a running mock's control port, then exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "chaos mock: unexpected arguments %q\n", fs.Args())
		return 2
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "chaos mock:", err)
		return 1
	}
	local := "http://" + *control
	if strings.HasPrefix(*control, ":") {
		local = "http://127.0.0.1" + *control
	}
	client := &http.Client{Timeout: 5 * time.Second}
	switch {
	case *set != "":
		if err := setFault(ctx, client, local, *set); err != nil {
			return fail(err)
		}
		return 0
	case *healthcheck:
		if err := mockHealthy(ctx, client, local); err != nil {
			return fail(err)
		}
		return 0
	}
	m := &mock{addr: *addr}
	if err := m.listen(); err != nil {
		return fail(err)
	}
	ctl := &http.Server{Addr: *control, Handler: m.controlHandler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- ctl.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-errs:
		m.close()
		return fail(err)
	}
	_ = ctl.Close()
	m.close()
	return 0
}

// mock is the fault-injecting upstream.
type mock struct {
	addr string
	fail atomic.Bool // mode 5xx

	mu  sync.Mutex
	srv *http.Server // nil while refusing
}

// listen starts serving traffic on m.addr, unless it already is.
func (m *mock) listen() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listenLocked()
}

func (m *mock) listenLocked() error {
	if m.srv != nil {
		return nil
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", m.addr)
	if err != nil {
		return err
	}
	m.addr = ln.Addr().String() // the port, when -addr asked for any
	srv := &http.Server{Handler: http.HandlerFunc(m.serve), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() { _ = srv.Serve(ln) }()
	m.srv = srv
	return nil
}

func (m *mock) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv != nil {
		_ = m.srv.Close()
		m.srv = nil
	}
}

func (m *mock) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	if m.fail.Load() {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", "1024")
	_, _ = w.Write(mockBody)
}

// setMode puts mode in effect before it returns.
func (m *mock) setMode(mode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch mode {
	case modeNone, mode5xx:
		m.fail.Store(mode == mode5xx)
		if err := m.listenLocked(); err != nil {
			return err
		}
	case modeRefused:
		m.fail.Store(false)
		if m.srv != nil {
			_ = m.srv.Close() // the listener and every connection, idle or not
			m.srv = nil
		}
	default:
		return fmt.Errorf("mode %q: want none, 5xx or refused", mode)
	}
	return nil
}

func (m *mock) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fault", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if err := m.setMode(mode); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// setFault sets the mode of the mock whose control port is at base.
func setFault(ctx context.Context, client *http.Client, base, mode string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/fault?mode="+mode, nil) //nolint:gosec // G704: the mock of the compose network is the point
	if err != nil {
		return err
	}
	resp, err := client.Do(req) //nolint:gosec // G704: the mock of the compose network is the point
	if err != nil {
		return fmt.Errorf("setting mode %s: %w", mode, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("setting mode %s: %s %s", mode, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// mockHealthy checks the control port at base.
func mockHealthy(ctx context.Context, client *http.Client, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req) //nolint:gosec // G704: as setFault
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
