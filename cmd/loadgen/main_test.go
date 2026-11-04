package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-url", "http://gateway-1:8000/bytes/1024", "-rate", "10000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := options{url: "http://gateway-1:8000/bytes/1024", rate: 10000, warmup: 30 * time.Second, duration: 3 * time.Minute,
		maxInFlight: 10000, conns: 100, timeout: 10 * time.Second}
	if o != want {
		t.Errorf("options = %+v, want %+v", o, want)
	}

	ok := []string{"-url", "https://127.0.0.1:8443/", "-rate", "1"}
	for _, tt := range []struct {
		args []string
		want string // in the error
	}{
		{[]string{"-rate", "1"}, "-url"},
		{[]string{"-url", "gateway-1:8000", "-rate", "1"}, "-url"},
		{[]string{"-url", "ftp://gateway-1/", "-rate", "1"}, "-url"},
		{[]string{"-url", "http:///bytes/1024", "-rate", "1"}, "-url"},
		{[]string{"-url", "http://gateway-1:8000/"}, "-rate"},
		{append(ok, "-rate", "0"), "-rate"},
		{append(ok, "-warmup", "-1s"), "-warmup"},
		{append(ok, "-duration", "0s"), "-duration"},
		{append(ok, "-max-in-flight", "0"), "-max-in-flight"},
		{append(ok, "-conns", "0"), "-conns"},
		{append(ok, "-timeout", "0s"), "-timeout"},
		{append(ok, "extra"), "unexpected arguments"},
		{append(ok, "-rate", "fast"), "-rate"},
	} {
		if _, err := parseFlags(tt.args, io.Discard); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error = %v, want one about %s", tt.args, err, tt.want)
		}
	}
	if code := run(t.Context(), []string{"-rate", "1"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("bad flags: exit code %d, want 2", code)
	}
	if code := run(t.Context(), []string{"-h"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("-h: exit code %d, want 0", code)
	}
}

func TestCPUModel(t *testing.T) {
	for _, tt := range []struct{ name, cpuinfo, want string }{
		{"x64", "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz\n\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz\n",
			"Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz"},
		{"arm64", "processor\t: 0\nBogoMIPS\t: 50.00\nCPU implementer\t: 0x41\nCPU architecture: 8\nCPU variant\t: 0x3\nCPU part\t: 0xd0c\nCPU revision\t: 1\n\nprocessor\t: 1\nCPU implementer\t: 0x41\nCPU part\t: 0xd0c\n",
			"implementer 0x41 part 0xd0c"},
		{"none", "", ""},
	} {
		if got := cpuModel([]byte(tt.cpuinfo)); got != tt.want {
			t.Errorf("%s: cpuModel = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// loadgen runs the command and decodes its JSON result.
func loadgen(t *testing.T, args ...string) (r result, out map[string]any, code int, stderr string) {
	t.Helper()
	var stdout, errb bytes.Buffer
	code = run(t.Context(), args, &stdout, &errb)
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("stdout is not a result: %v\n%s\nstderr: %s", err, stdout.String(), errb.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return r, out, code, errb.String()
}

// TestResult runs against a plain-HTTP and a TLS server: HTTP/1.1 and HTTP/2
// are used, warm-up requests are sent but left out of the result, and the
// JSON has every field the benchmark needs (ADR-0014).
func TestResult(t *testing.T) {
	var got atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		_, _ = w.Write(make([]byte, 1024))
	})
	plain := httptest.NewServer(h)
	defer plain.Close()
	tlsServer := httptest.NewUnstartedServer(h)
	tlsServer.EnableHTTP2 = true
	tlsServer.StartTLS()
	defer tlsServer.Close()
	caCert := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name, proto string
		args        []string
	}{
		{"http", "HTTP/1.1", []string{"-url", plain.URL + "/bytes"}},
		{"https", "HTTP/2.0", []string{"-url", tlsServer.URL + "/bytes", "-cacert", caCert}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got.Store(0)
			r, out, code, stderr := loadgen(t, append(tt.args, "-rate", "50", "-warmup", "200ms", "-duration", "500ms")...)
			if code != 0 {
				t.Errorf("exit code %d: %s", code, stderr)
			}
			if r.Protocol != tt.proto || r.URL != tt.args[1] || r.TargetRPS != 50 || r.AchievedRPS != 50 || !r.Valid || r.WarmupS != 0.2 || r.DurationS != 0.5 {
				t.Errorf("result = %+v, want %s, 50 rps target and achieved, valid", r, tt.proto)
			}
			if r.Requests != (requestCounts{Scheduled: 25, Sent: 25}) || r.Status["2xx"] != 25 || len(r.Errors) != 0 {
				t.Errorf("requests = %+v, status = %v, errors = %v; want 25 sent and 2xx", r.Requests, r.Status, r.Errors)
			}
			if n := got.Load(); n != 35 {
				t.Errorf("server got %d requests, want 35: 10 warm-up and 25 measured", n)
			}
			if l := r.LatencyUS; l.P50 <= 0 || l.P50 > l.P99 || l.P99 > l.P999 || l.P999 > l.Max {
				t.Errorf("latency = %+v µs, want 0 < p50 ≤ p99 ≤ p99.9 ≤ max", l)
			}
			if h := r.Hardware; h.GOARCH == "" || h.NumCPU <= 0 || h.GOMAXPROCS <= 0 {
				t.Errorf("hardware = %+v", h)
			}

			// The JSON shape bench.yml reads: these keys, no others.
			for _, k := range []struct {
				obj  map[string]any
				keys []string
			}{
				{out, []string{"achieved_rps", "duration_s", "errors", "hardware", "latency_us", "protocol", "requests", "status", "target_rps", "url", "valid", "warmup_s"}},
				{out["requests"].(map[string]any), []string{"dropped", "scheduled", "sent"}},
				{out["latency_us"].(map[string]any), []string{"max", "p50", "p99", "p99_9"}},
				{out["hardware"].(map[string]any), []string{"cpu_model", "goarch", "gomaxprocs", "num_cpu"}},
			} {
				if keys := slices.Sorted(maps.Keys(k.obj)); !slices.Equal(keys, k.keys) {
					t.Errorf("JSON keys %v, want %v", keys, k.keys)
				}
			}
		})
	}
}

// TestStalledServer is the coordinated-omission check: the server answers
// nothing for its first 500 ms. Requests keep going out at their due times
// while it stalls, so the latencies include the stall: the first request
// waits about 500 ms, and so does the 99th percentile. A generator that
// waited for each answer would record one slow request and a fast p99.
func TestStalledServer(t *testing.T) {
	stallUntil := time.Now().Add(500 * time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(time.Until(stallUntil))
	}))
	defer srv.Close()

	r, _, code, stderr := loadgen(t, "-url", srv.URL, "-rate", "100", "-warmup", "0s", "-duration", "1s")
	// Valid allows the last due request to be dropped if the timer fires late.
	if code != 0 || !r.Valid || r.Status["2xx"] != r.Requests.Sent {
		t.Fatalf("exit code %d, result %+v: %s", code, r, stderr)
	}
	// 50 of the 100 requests are due during the stall, up to 500 ms before it ends.
	if l := r.LatencyUS; l.Max < 450_000 || l.P99 < 400_000 {
		t.Errorf("latency = %+v µs, want max and p99 to include the 500 ms stall", l)
	}
}

// TestSaturated caps requests in flight at 50 against a server that answers
// none within the run: the remaining 150 due requests are dropped, the
// achieved rate is 25% of the target, and the run fails with exit code 1
// while still printing its result.
func TestSaturated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(1200 * time.Millisecond)
	}))
	defer srv.Close()

	r, _, code, stderr := loadgen(t, "-url", srv.URL, "-rate", "200", "-warmup", "0s", "-duration", "1s", "-max-in-flight", "50")
	if code != 1 || r.Valid || r.Requests != (requestCounts{Scheduled: 200, Sent: 50, Dropped: 150}) || r.AchievedRPS != 50 {
		t.Errorf("exit code %d, result %+v; want 1, invalid, 50 of 200 sent", code, r)
	}
	if !strings.Contains(stderr, "below 99% of the target 200 rps") {
		t.Errorf("stderr = %q, want the reason", stderr)
	}
}
