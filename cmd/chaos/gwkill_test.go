package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// killWriter is the sender's stderr: on "kill <gateway>" it runs kill once.
type killWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
	kill func()
}

func (w *killWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.HasPrefix(string(p), "kill ") {
		w.once.Do(w.kill)
	}
	return w.buf.Write(p)
}

func (w *killWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// gwkillFixture is three gateways answering after delay, and an admin
// server reporting gw_config_version 74 for all (they share 127.0.0.1).
func gwkillFixture(t *testing.T, delay time.Duration, status func(gw int, n int64) int) ([]*httptest.Server, string) {
	t.Helper()
	var srvs []*httptest.Server
	for gw := range 3 {
		var n atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			w.WriteHeader(status(gw, n.Add(1)))
		}))
		t.Cleanup(srv.Close)
		srvs = append(srvs, srv)
	}
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "# TYPE gw_config_version gauge\ngw_config_version 74\n")
	}))
	t.Cleanup(admin.Close)
	_, port, _ := net.SplitHostPort(admin.Listener.Addr().String())
	return srvs, port
}

func gwkillOpts(srvs []*httptest.Server) gwkillOptions {
	var addrs []string
	for _, s := range srvs {
		addrs = append(addrs, s.Listener.Addr().String())
	}
	return gwkillOptions{gateways: addrs, kill: "127.0.0.1", path: "/bytes/1024", configVersion: 74,
		rate: 300, duration: 2 * time.Second, killAfter: time.Second, timeout: 2 * time.Second}
}

// TestGwkillPass kills the second gateway (its listener and connections
// closed) while requests to it are in flight: they and the first one sent
// after fail, the sender ejects it, and the others see no failure.
func TestGwkillPass(t *testing.T) {
	srvs, adminPort := gwkillFixture(t, 30*time.Millisecond, func(int, int64) int { return http.StatusOK })
	o := gwkillOpts(srvs)
	// All three listen on 127.0.0.1; the sender kills by index, so point
	// -kill at the second by giving each its own host name.
	o.gateways = []string{
		strings.Replace(o.gateways[0], "127.0.0.1", "localhost", 1),
		o.gateways[1],
		strings.Replace(o.gateways[2], "127.0.0.1", "localhost", 1),
	}
	stderr := &killWriter{kill: func() {
		_ = srvs[1].Listener.Close()
		srvs[1].CloseClientConnections()
	}}
	var stdout bytes.Buffer
	code := runGwkillWith(t.Context(), o, adminPort, &stdout, stderr)
	var rep gwkillReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	k := rep.Gateways[1]
	if code != 0 || !rep.Pass || k.Ejected == nil || k.Failed == 0 || k.LateFailures != 0 || rep.Gateways[0].Failed+rep.Gateways[2].Failed != 0 {
		t.Fatalf("code %d, report %+v\n%s", code, rep, stderr.String())
	}
	// Ejected after the kill, and nothing sent to it after.
	if !k.Ejected.After(rep.KillRequested) || k.OK+k.Failed != k.Sent {
		t.Errorf("killed gateway: %+v, kill requested %v", k, rep.KillRequested)
	}
	if rep.Gateways[0].Sent < 200 || rep.Gateways[2].Sent < 200 || rep.Requests.Sent < 594 {
		t.Errorf("sent %+v", rep)
	}
	for _, want := range []string{
		"kill 127.0.0.1\n",
		"PASS  sender sent >= 99% of its schedule, kill sent",
		"PASS  zero failed requests to the other gateways",
		"PASS  only 127.0.0.1's in-flight requests failed",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
}

func TestGwkillFail(t *testing.T) {
	t.Run("another gateway fails", func(t *testing.T) {
		srvs, adminPort := gwkillFixture(t, 0, func(gw int, n int64) int {
			if gw == 2 && n == 100 {
				return http.StatusServiceUnavailable
			}
			return http.StatusOK
		})
		o := gwkillOpts(srvs)
		o.gateways[1] = strings.Replace(o.gateways[1], "127.0.0.1", "localhost", 1)
		o.kill = "localhost"
		stderr := &killWriter{kill: func() { _ = srvs[1].Listener.Close(); srvs[1].CloseClientConnections() }}
		var stdout bytes.Buffer
		if code := runGwkillWith(t.Context(), o, adminPort, &stdout, stderr); code != 1 || !strings.Contains(stderr.String(), "FAIL  zero failed requests to the other gateways") {
			t.Errorf("code %d:\n%s", code, stderr.String())
		}
	})
	t.Run("nothing killed", func(t *testing.T) {
		srvs, adminPort := gwkillFixture(t, 0, func(int, int64) int { return http.StatusOK })
		o := gwkillOpts(srvs)
		o.gateways[1] = strings.Replace(o.gateways[1], "127.0.0.1", "localhost", 1)
		o.kill = "localhost"
		stderr := &killWriter{kill: func() {}}
		var stdout bytes.Buffer
		if code := runGwkillWith(t.Context(), o, adminPort, &stdout, stderr); code != 1 || !strings.Contains(stderr.String(), "no failure observed") {
			t.Errorf("code %d:\n%s", code, stderr.String())
		}
	})
}

func TestJudgeGwkillLateFailure(t *testing.T) {
	o := gwkillOptions{gateways: []string{"gateway-1:8000", "gateway-2:8000"}, kill: "gateway-2"}
	t0 := time.Unix(1000, 0)
	ej := t0.Add(time.Second)
	rep := judgeGwkill(o, 3, []gwkillSample{
		{gateway: 1, sentAt: t0, failure: "transport"},
		{gateway: 1, sentAt: ej, failure: "timeout"}, // sent at the ejection: late
		{gateway: 0, sentAt: ej},
	}, []*time.Time{nil, &ej}, t0)
	if rep.Pass || rep.Gateways[1].LateFailures != 1 || rep.Gateways[1].FailureKinds["timeout"] != 1 {
		t.Errorf("report %+v", rep)
	}
}

func TestGwkillFlags(t *testing.T) {
	o, err := parseGwkillFlags(nil, io.Discard)
	if err != nil || len(o.gateways) != 3 || o.killed() != 1 || o.rate != 300 || o.duration != 40*time.Second ||
		o.killAfter != 20*time.Second || o.timeout != 10*time.Second || o.configVersion != 74 {
		t.Errorf("defaults %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{"-gateways", "gateway-1:8000"}, {"-kill", "gateway-9"}, {"-rate", "0"}, {"-kill-after", "40s"},
		{"-timeout", "0s"}, {"extra"},
	} {
		if _, err := parseGwkillFlags(bad, io.Discard); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if code := runGwkill(t.Context(), []string{"-rate", "0"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("bad flags: exit %d", code)
	}
}
