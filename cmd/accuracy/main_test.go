package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-replicas", "3"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.replicas != 3 || o.keys != 10 || o.duration != time.Minute || o.configVersion != 31 || o.timeout != 10*time.Second || len(o.patterns) != 3 {
		t.Errorf("options = %+v", o)
	}
	if len(o.gateways) != 3 || o.gateways[2] != (gateway{"http://gateway-3:8000/bytes/64", "http://gateway-3:8001/metrics"}) {
		t.Errorf("gateways = %v", o.gateways)
	}
	for _, tt := range []struct {
		args []string
		want string // in the error
	}{
		{[]string{"-replicas", "0"}, "-replicas"},
		{[]string{"-replicas", "6"}, "-replicas"},
		{[]string{"-keys", "0"}, "-keys"},
		{[]string{"-duration", "500ms"}, "-duration"},
		{[]string{"-duration", "1500ms"}, "-duration"},
		{[]string{"-timeout", "0s"}, "-timeout"},
		{[]string{"extra"}, "unexpected arguments"},
	} {
		if _, err := parseFlags(tt.args, io.Discard); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error = %v, want one about %s", tt.args, err, tt.want)
		}
	}
	if code := run(t.Context(), []string{"-keys", "0"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("bad flags: exit code %d, want 2", code)
	}
	if code := run(t.Context(), []string{"-h"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("-h: exit code %d, want 0", code)
	}
}

// fakeGateway answers as a gateway running the harness policy in dev mode:
// GCRA per X-Accuracy-Key with its decision time in Spillway-Decision-Time.
// admitAll admits everything instead; noHeader leaves the header out except
// for the harness's probes. Its admin port reports the config version from
// the third scrape on, and counts every request as a Redis timeout.
type fakeGateway struct {
	admitAll, noHeader bool
	cut                bool // cut the body of every admission but the probes'

	mu       sync.Mutex
	tat      map[string]int64
	requests atomic.Int64
	scrapes  atomic.Int64
}

func (f *fakeGateway) start(t *testing.T) gateway {
	f.tat = map[string]int64{}
	traffic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		now := time.Now().UnixMicro()
		const T = 10_000 // µs, 1 s / 100
		tat := max(f.tat[r.Header.Get(keyHeader)], now)
		ok := f.admitAll || tat-now <= (policy.burst-1)*T
		if ok {
			f.tat[r.Header.Get(keyHeader)] = tat + T
		}
		f.mu.Unlock()
		if !f.noHeader || strings.HasPrefix(r.Header.Get(keyHeader), "probe-") {
			w.Header().Set(decisionTimeHeader, strconv.FormatInt(now, 10))
		}
		if !ok {
			w.WriteHeader(http.StatusTooManyRequests)
		} else if f.cut && !strings.HasPrefix(r.Header.Get(keyHeader), "probe-") {
			w.Header().Set("Content-Length", "2")
			_, _ = io.WriteString(w, "x") // the client reads an unexpected EOF
		}
	}))
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := 30
		if f.scrapes.Add(1) > 2 {
			version = 31
		}
		_, _ = fmt.Fprintf(w, "# TYPE gw_config_version gauge\ngw_config_version %d\ngw_redis_errors_total{kind=\"conn\"} 0\ngw_redis_errors_total{kind=\"timeout\"} %d\n", version, f.requests.Load())
	}))
	t.Cleanup(traffic.Close)
	t.Cleanup(admin.Close)
	return gateway{traffic.URL + "/bytes/64", admin.URL}
}

func harnessRun(t *testing.T, o options) (r report, out map[string]any, code int, stderr string) {
	t.Helper()
	o.configVersion, o.timeout = 31, 10*time.Second
	var stdout, errb bytes.Buffer
	code = runWith(t.Context(), o, &stdout, &errb)
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, errb.String())
	}
	_ = json.Unmarshal(stdout.Bytes(), &out)
	return r, out, code, errb.String()
}

// TestRun drives every pattern through two fake gateways that enforce the
// policy: a PASS, with each pattern's counts and the JSON report's shape.
func TestRun(t *testing.T) {
	a, b := &fakeGateway{}, &fakeGateway{}
	r, out, code, stderr := harnessRun(t, options{keys: 2, duration: time.Second, gateways: []gateway{a.start(t), b.start(t)}, patterns: patterns})
	if code != 0 || !r.Pass || r.Replicas != 2 || len(r.Patterns) != 3 {
		t.Fatalf("exit code %d, report %+v\n%s", code, r, stderr)
	}
	for i, p := range r.Patterns {
		if p.Pattern != patterns[i] || p.Requests != (requestCounts{Scheduled: 400, Sent: 400}) || p.Status.OK+p.Status.Limited != 400 ||
			p.FailureModeAdmissions != 0 || p.MaxOverAdmission.Count != 0 || !p.Pass {
			t.Errorf("%s: %+v", patterns[i], p)
		}
		// Every request reached one of the gateways, which count each as a
		// Redis timeout.
		if p.RedisErrors["timeout"] != 400 || p.RedisErrors["conn"] != 0 {
			t.Errorf("%s: redis errors %v, want 400 timeouts", patterns[i], p.RedisErrors)
		}
		if p.MeanAdmittedRPS < 100 || p.MeanAdmittedRPS > 200 || p.P99LatencyUS <= 0 {
			t.Errorf("%s: %.1f/s admitted per key, p99 %d µs", patterns[i], p.MeanAdmittedRPS, p.P99LatencyUS)
		}
		if line := strings.Split(stderr, "\n")[i]; !strings.HasPrefix(line, fmt.Sprintf("%-8s PASS  400/400 sent;", patterns[i])) {
			t.Errorf("summary line %q", line)
		}
	}
	if n := a.requests.Load() - b.requests.Load(); n < -3 || n > 3 {
		t.Errorf("gateways got %d and %d requests, want an even split", a.requests.Load(), b.requests.Load())
	}
	for _, k := range []string{"duration_s", "hardware", "keys", "pass", "patterns", "policy", "replicas"} {
		if _, ok := out[k]; !ok {
			t.Errorf("report has no %q", k)
		}
	}
	if pol := out["policy"].(map[string]any); pol["algorithm"] != "gcra" || pol["rate"] != 100.0 || pol["period_ms"] != 1000.0 || pol["burst"] != 100.0 {
		t.Errorf("policy %v", pol)
	}
}

// TestRunFails: an over-admitting gateway, one whose admissions carry no
// decision time and one whose answers the harness cannot read each FAIL the
// run.
func TestRunFails(t *testing.T) {
	// Boundary at 2 s: 200 within about 20 ms around the 1 s mark, against
	// 110 for a 100 ms window. How many more, and in which window, depends
	// on when the sender's timers fire.
	over := &fakeGateway{admitAll: true}
	r, _, code, stderr := harnessRun(t, options{keys: 1, duration: 2 * time.Second, gateways: []gateway{over.start(t)}, patterns: []string{"boundary"}})
	if p := r.Patterns[0]; code != 1 || r.Pass || p.Pass || p.MaxOverAdmission.Count < 50 || p.MaxOverAdmission.Key == "" {
		t.Errorf("over-admitting: exit code %d, %+v", code, r.Patterns)
	}
	if !strings.Contains(stderr, "boundary FAIL") || !strings.Contains(stderr, "(key boundary-") {
		t.Errorf("stderr %q", stderr)
	}

	noHeader := &fakeGateway{noHeader: true}
	r, _, code, _ = harnessRun(t, options{keys: 1, duration: time.Second, gateways: []gateway{noHeader.start(t)}, patterns: []string{"uniform"}})
	if p := r.Patterns[0]; code != 1 || p.Pass || p.FailureModeAdmissions == 0 || p.FailureModeAdmissions != p.Status.OK || p.MaxOverAdmission.Count != 0 {
		t.Errorf("no decision times: exit code %d, %+v", code, p)
	}

	// Admissions whose answers are lost are not judged, so they fail the
	// run (ADR-0028).
	cut := &fakeGateway{cut: true}
	r, _, code, _ = harnessRun(t, options{keys: 1, duration: time.Second, gateways: []gateway{cut.start(t)}, patterns: []string{"uniform"}})
	if p := r.Patterns[0]; code != 1 || p.Pass || p.Status.NoAnswer == 0 || p.MaxOverAdmission.Count != 0 || p.FailureModeAdmissions != 0 {
		t.Errorf("answers cut short: exit code %d, %+v", code, p)
	}
}

// TestWaitReady: a gateway that runs the config but answers without decision
// times, Redis not reached or dev mode off, is never ready.
func TestWaitReady(t *testing.T) {
	f := &fakeGateway{}
	g := f.start(t)
	cold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(cold.Close)
	g.url = cold.URL
	h := &harness{options: options{configVersion: 31, gateways: []gateway{g}}, client: http.DefaultClient}
	if err := h.waitReady(t.Context(), "run", time.Second); err == nil || !strings.Contains(err.Error(), "does not decide requests in Redis: last answer 503") {
		t.Errorf("error = %v", err)
	}
	h.configVersion = 32
	if err := h.waitReady(t.Context(), "run", 500*time.Millisecond); err == nil || !strings.Contains(err.Error(), "reports gw_config_version 31, want 32") {
		t.Errorf("error = %v", err)
	}
}
