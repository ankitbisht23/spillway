package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func latencyDefaults(t *testing.T) latencyOptions {
	t.Helper()
	o, err := parseLatencyFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// synthLatency makes samples for a 60 s run at 100 rps per route with the
// toxic on from 20 to 40 s: before it, every route answers in 2 ms with a
// Redis decision; under it, fail_open answers 2xx, fail_closed 503 fail
// closed, degraded admits every other request, all in 7 ms; after it,
// Redis decisions again (degraded's denies from the deny cache). edit may
// change each sample.
func synthLatency(edit func(s *latencySample, at time.Duration)) ([]latencySample, time.Time, time.Time) {
	start := time.Unix(10_000, 0)
	on, off := start.Add(20*time.Second), start.Add(40*time.Second)
	var out []latencySample
	for _, a := range schedule(3, 100, 60*time.Second) {
		s := latencySample{route: a.key, sent: true, due: start.Add(a.at), status: http.StatusOK, decided: true}
		lat := 2 * time.Millisecond
		k := int(a.at / (10 * time.Millisecond))
		if !s.due.Before(on) && s.due.Before(off) {
			lat, s.decided, s.redisError = 7*time.Millisecond, false, "timeout"
			switch a.key {
			case 1:
				s.status, s.failClosed = http.StatusServiceUnavailable, true
			case 2:
				if k%2 == 1 {
					s.status = http.StatusTooManyRequests
				}
			}
		} else if a.key == 2 && k%2 == 1 {
			s.status = http.StatusTooManyRequests
			if !s.due.Before(off) && k%4 == 1 {
				s.decided = false // from the deny cache
			}
		}
		s.done = s.due.Add(lat)
		if edit != nil {
			edit(&s, a.at)
		}
		out = append(out, s)
	}
	return out, on, off
}

func TestJudgeLatencyPass(t *testing.T) {
	o := latencyDefaults(t)
	samples, on, off := synthLatency(nil)
	rep := judgeLatency(o, samples, on, off)
	lines := strings.Join(rep.lines(o), "\n")
	if !rep.Pass {
		t.Fatalf("want PASS:\n%s", lines)
	}
	if rep.WindowS != 19 || rep.Expected.DegradedAdmits != 1000 {
		t.Errorf("window %v s, expected admits %v; want 19 and 1000", rep.WindowS, rep.Expected.DegradedAdmits)
	}
	open, closed, degraded := rep.Routes[0], rep.Routes[1], rep.Routes[2]
	if open.Window.Requests != 1900 || open.Window.OK != 1900 || closed.Window.FailClosed != 1900 || degraded.Window.OK != 950 {
		t.Errorf("windows: %+v %+v %+v", open.Window, closed.Window, degraded.Window)
	}
	if open.PreMedianMS != 2 || open.Window.MedianMS != 7 {
		t.Errorf("open medians: pre %v, window %v", open.PreMedianMS, open.Window.MedianMS)
	}
	if degraded.Recovered.CachedDenies == 0 || degraded.Recovered.Decided == 0 || degraded.Recovered.NotDecidedNow != 0 || open.Recovered.Requests != 1500 {
		t.Errorf("recovered: %+v %+v", degraded.Recovered, open.Recovered)
	}
	for _, want := range []string{
		"PASS  fail_open: >= 99.9% 2xx under +latency", " 1900/1900 2xx",
		"PASS  fail_closed: every answer 503 fail_closed",
		"PASS  degraded: admits within ±10% of expected", " 950 admitted of 1900; expected 1000 (50/s × 19.0 s + 50), allowed 900 – 1100",
		"PASS  fail_open: median <= pre-toxic + 10ms", " median 7.00 ms under the toxic, 2.00 ms before (+5.00 ms)",
		"PASS  degraded: Redis decides 5s after removal", " 1125 with Spillway-Decision-Time, 375 deny-cache 429s, 0 with Spillway-Redis-Error, 0 neither",
		"toxic on at 1970-01-01T02:47:00Z",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("lines lack %q:\n%s", want, lines)
		}
	}
}

func TestJudgeLatencyFail(t *testing.T) {
	inWindow := func(at time.Duration) bool { return at >= 21*time.Second && at < 40*time.Second }
	for _, tt := range []struct {
		name string
		edit func(s *latencySample, at time.Duration)
		want string
	}{
		{"fail_open errors", func(s *latencySample, at time.Duration) {
			if s.route == 0 && inWindow(at) && at < 21100*time.Millisecond {
				s.status = 0
			}
		}, "FAIL  fail_open"},
		{"fail_closed lets one through", func(s *latencySample, at time.Duration) {
			if s.route == 1 && at == 30*time.Second+3333333 {
				s.status, s.failClosed = http.StatusOK, false
			}
		}, "FAIL  fail_closed"},
		{"degraded admits everything", func(s *latencySample, at time.Duration) {
			if s.route == 2 && inWindow(at) {
				s.status = http.StatusOK
			}
		}, "FAIL  degraded: admits"},
		{"calls wait out the latency", func(s *latencySample, at time.Duration) {
			if s.route == 1 && inWindow(at) {
				s.done = s.due.Add(55 * time.Millisecond)
			}
		}, "FAIL  fail_closed: median"},
		{"Redis not back", func(s *latencySample, at time.Duration) {
			if s.route == 0 && at >= 50*time.Second {
				s.decided, s.redisError = false, "conn"
			}
		}, "FAIL  fail_open: Redis decides"},
		{"a 2xx without a decision after recovery", func(s *latencySample, at time.Duration) {
			if s.route == 2 && at >= 50*time.Second && s.status == http.StatusOK {
				s.decided = false
			}
		}, "FAIL  degraded: Redis decides"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := latencyDefaults(t)
			samples, on, off := synthLatency(tt.edit)
			rep := judgeLatency(o, samples, on, off)
			lines := strings.Join(rep.lines(o), "\n")
			if rep.Pass || !strings.Contains(lines, tt.want) {
				t.Errorf("want FAIL with %q:\n%s", tt.want, lines)
			}
		})
	}
}

// fakeToxiproxy records toxics per proxy.
type fakeToxiproxy struct {
	mu     sync.Mutex
	toxics map[string]map[string]any // proxy → its toxic's request body
	on     atomic.Bool
}

func (f *fakeToxiproxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // proxies/<p>/toxics[/<name>]
	if len(parts) < 3 || parts[0] != "proxies" || parts[2] != "toxics" {
		http.NotFound(w, r)
		return
	}
	p := parts[1]
	switch {
	case r.Method == http.MethodPost && len(parts) == 3:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.toxics[p] = body
		f.on.Store(true)
		_, _ = io.WriteString(w, "{}")
	case r.Method == http.MethodDelete && len(parts) == 4 && parts[3] == toxicName:
		if _, ok := f.toxics[p]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.toxics, p)
		if len(f.toxics) == 0 {
			f.on.Store(false)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

// TestLatencyRun drives a fake gateway that follows the failure modes while
// the fake toxiproxy has toxics, on a short schedule.
func TestLatencyRun(t *testing.T) {
	tox := &fakeToxiproxy{toxics: map[string]map[string]any{}}
	toxSrv := httptest.NewServer(tox)
	defer toxSrv.Close()
	var degraded atomic.Int64
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = io.WriteString(w, "gw_config_version 73\n")
			return
		}
		if !tox.on.Load() {
			w.Header().Set(decisionTimeHeader, "1795600000000000")
			return
		}
		w.Header().Set(redisErrorHeader, "timeout")
		switch r.URL.Path {
		case latencyPaths[1]:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"rate limit unavailable"}`)
		case latencyPaths[2]:
			if degraded.Add(1)%2 == 0 {
				w.WriteHeader(http.StatusTooManyRequests)
			}
		}
	}))
	defer gw.Close()
	o := latencyDefaults(t)
	o.gateway, o.metrics, o.toxiproxy = gw.URL, gw.URL+"/metrics", toxSrv.URL
	o.proxies = []string{"redis-0", "redis-1"}
	o.duration, o.toxicFrom, o.toxicTo = 5*time.Second, 1500*time.Millisecond, 3*time.Second
	o.settleIn, o.recoverIn, o.degradedBurst = 200*time.Millisecond, time.Second, 1
	var stdout, stderr bytes.Buffer
	code := runLatencyWith(t.Context(), o, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr.String())
	}
	var rep latencyReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Pass || rep.ToxicOff.Sub(rep.ToxicOn) < time.Second || rep.Routes[1].Window.FailClosed == 0 {
		t.Errorf("report %+v", rep)
	}
	tox.mu.Lock()
	defer tox.mu.Unlock()
	if len(tox.toxics) != 0 {
		t.Errorf("toxics left: %v", tox.toxics)
	}
}

func TestToxiproxyClient(t *testing.T) {
	tox := &fakeToxiproxy{toxics: map[string]map[string]any{}}
	srv := httptest.NewServer(tox)
	defer srv.Close()
	c := &toxiproxy{base: srv.URL, client: srv.Client()}
	if err := c.addAll(t.Context(), []string{"redis-0", "redis-5"}, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	b := tox.toxics["redis-5"]
	attrs, _ := b["attributes"].(map[string]any)
	if len(tox.toxics) != 2 || b["name"] != toxicName || b["type"] != "latency" || b["stream"] != "downstream" || attrs["latency"] != 50.0 {
		t.Errorf("toxics %v", tox.toxics)
	}
	for _, p := range []string{"redis-0", "redis-5", "redis-0"} { // the second removal finds none: no error
		if err := c.remove(t.Context(), p); err != nil {
			t.Errorf("remove %s: %v", p, err)
		}
	}
	if err := c.addAll(t.Context(), []string{"no/such"}, time.Millisecond); err == nil {
		t.Error("a failing add: no error")
	}
}

func TestLatencyFlags(t *testing.T) {
	o := latencyDefaults(t)
	if o.gateway != "http://gateway-4:8000" || len(o.proxies) != 6 || o.rate != 100 || o.duration != time.Minute ||
		o.toxicFrom != 20*time.Second || o.toxicTo != 40*time.Second || o.latency != 50*time.Millisecond ||
		o.degradedRate != 50 || o.degradedBurst != 50 || o.configVersion != 73 {
		t.Errorf("defaults %+v", o)
	}
	for _, bad := range [][]string{
		{"-proxies", ""}, {"-rate", "0"}, {"-toxic-to", "20s"}, {"-toxic-to", "56s"}, {"-latency", "0s"}, {"extra"},
	} {
		if _, err := parseLatencyFlags(bad, io.Discard); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
