package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseUpstreamFlags(t *testing.T) {
	o, err := parseUpstreamFlags([]string{"-fault", "5xx"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.upstream != "verify-5xx" || o.configVersion != 51 || o.rate != 500 || o.duration != 40*time.Second ||
		o.faultAfter != 10*time.Second || o.timeout != 10*time.Second || o.traceWait != 60*time.Second || o.url != "http://gateway-1:8000/bytes/1024" {
		t.Errorf("options = %+v", o)
	}
	if o, _ := parseUpstreamFlags([]string{"-fault", "refused"}, io.Discard); o.upstream != "verify-refused" || o.configVersion != 52 {
		t.Errorf("refused: upstream %s, version %d", o.upstream, o.configVersion)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{nil, "-fault"},
		{[]string{"-fault", "slow"}, "-fault"},
		{[]string{"-fault", "5xx", "-rate", "0"}, "-rate"},
		{[]string{"-fault", "5xx", "-fault-after", "40s"}, "-fault-after"},
		{[]string{"-fault", "5xx", "-timeout", "0s"}, "-timeout"},
		{[]string{"-fault", "5xx", "extra"}, "unexpected arguments"},
	} {
		if _, err := parseUpstreamFlags(tt.args, io.Discard); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error %v, want one about %s", tt.args, err, tt.want)
		}
	}
	if code := runUpstream(t.Context(), []string{"-fault", "x"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("bad flags: exit code %d, want 2", code)
	}
}

func TestParseSample(t *testing.T) {
	for _, tt := range []struct {
		in     string
		name   string
		labels map[string]string
		v      float64
		ok     bool
	}{
		{"gw_config_version 51", "gw_config_version", map[string]string{}, 51, true},
		{`gw_retries_total{outcome="sent",upstream="verify-5xx"} 12`, "gw_retries_total", map[string]string{"outcome": "sent", "upstream": "verify-5xx"}, 12, true},
		{`gw_upstream_endpoint_healthy{endpoint="172.29.0.9:8080",upstream="u"} 0` + "\n", "gw_upstream_endpoint_healthy", map[string]string{"endpoint": "172.29.0.9:8080", "upstream": "u"}, 0, true},
		{`x{a="q\"uote,}",b="c"} 1.5e3`, "x", map[string]string{"a": `q"uote,}`, "b": "c"}, 1500, true},
		{"# TYPE x counter", "", nil, 0, false},
		{"", "", nil, 0, false},
		{`x{a="open 1`, "", nil, 0, false},
		{"x NaNa", "", nil, 0, false},
	} {
		name, labels, v, ok := parseSample(tt.in)
		if name != tt.name || !maps.Equal(labels, tt.labels) || v != tt.v || ok != tt.ok {
			t.Errorf("parseSample(%q) = %q, %v, %v, %v", tt.in, name, labels, v, ok)
		}
	}
}

// scrapesAt makes n answered scrapes 1 s apart, each taking 10 ms, with
// first attempts at q per second and retries from retries(k).
func scrapesAt(n int, q float64, retries func(k int) float64) []scrape {
	t0 := time.Unix(1000, 0)
	s := make([]scrape, n)
	for k := range s {
		sent := t0.Add(time.Duration(k) * time.Second)
		s[k] = scrape{Sent: sent, Recv: sent.Add(10 * time.Millisecond), OK: true, FirstAttempts: q * float64(k), Retries: retries(k), Circuit: 0}
	}
	return s
}

func TestCheckBudget(t *testing.T) {
	// 43 scrapes 1 s apart: windows of 10 scrape intervals (9.99 s on the
	// send/receive bounds is too long: recv_j − send_i = 10.01 s), so 9
	// intervals: scrapes 9 … 42, 34 windows.
	b := checkBudget(scrapesAt(43, 500, func(k int) float64 { return float64(k) }), defaultRules)
	if !b.Pass || b.Windows != 34 || b.Over != 0 || b.Sent != 42 || b.FirstAttempts != 21000 {
		t.Errorf("steady: %+v", b)
	}
	// 500 first attempts/s over 9 s allow 0.2 × 4500 + 30 = 930 retries.
	burst := func(n float64) func(int) float64 {
		return func(k int) float64 {
			if k >= 20 {
				return n
			}
			return 0
		}
	}
	if b := checkBudget(scrapesAt(43, 500, burst(930)), defaultRules); !b.Pass || b.TightRetries != 930 || b.TightAllowed != 930 {
		t.Errorf("exactly the allowance: %+v", b)
	}
	if b := checkBudget(scrapesAt(43, 500, burst(931)), defaultRules); b.Pass || b.Over != 9 {
		t.Errorf("one over the allowance in the 9 windows holding the burst: %+v", b)
	}
	// Without traffic, the floor alone: 30.
	if b := checkBudget(scrapesAt(43, 0, burst(31)), defaultRules); b.Pass {
		t.Errorf("31 retries without traffic: %+v", b)
	}
	// Coverage.
	s := scrapesAt(43, 500, func(int) float64 { return 0 })
	s[5].OK, s[5].Err = false, "timeout"
	if b := checkBudget(s, defaultRules); b.Pass || !strings.Contains(b.Coverage, "scrape 5 failed") {
		t.Errorf("a failed scrape: %+v", b)
	}
	s = scrapesAt(43, 500, func(int) float64 { return 0 })
	for k := 10; k < len(s); k++ {
		s[k].Sent, s[k].Recv = s[k].Sent.Add(2100*time.Millisecond), s[k].Recv.Add(2100*time.Millisecond)
	}
	if b := checkBudget(s, defaultRules); b.Pass || !strings.Contains(b.Coverage, "3.10 s apart") {
		t.Errorf("a 3.1 s gap: %+v", b)
	}
	if b := checkBudget(scrapesAt(38, 500, func(int) float64 { return 0 }), defaultRules); b.Pass || !strings.Contains(b.Coverage, "29 windows") {
		t.Errorf("too few windows: %+v", b)
	}
	// A slow scrape shortens the window it ends but cannot make one longer
	// than 10 s.
	s = scrapesAt(43, 500, func(int) float64 { return 0 })
	s[20].Recv = s[20].Sent.Add(1500 * time.Millisecond)
	if b := checkBudget(s, defaultRules); !b.Pass || b.TightSpanS > 10 {
		t.Errorf("a slow scrape: %+v", b)
	}
}

// TestAnalyzeUpstream: rules (0) and (a), and the information lines.
func TestAnalyzeUpstream(t *testing.T) {
	o, _ := parseUpstreamFlags([]string{"-fault", "5xx"}, io.Discard)
	start := time.Unix(2000, 0)
	tf := start.Add(10 * time.Second)
	at := func(ms int) time.Time { return tf.Add(time.Duration(ms) * time.Millisecond) }
	// 40 s at 100 rps around the fault; every request answered 200 in 2 ms.
	steady := func() []request {
		out := make([]request, 4000)
		for i := range out {
			due := start.Add(time.Duration(i) * 10 * time.Millisecond)
			out[i] = request{sent: true, due: due, sentAt: due, done: due.Add(2 * time.Millisecond), status: 200, attempts: 1}
		}
		return out
	}
	scrapes := scrapesAt(43, 100, func(int) float64 { return 0 })
	for k := range scrapes {
		scrapes[k].Sent, scrapes[k].Recv = scrapes[k].Sent.Add(start.Sub(time.Unix(1000, 0))-time.Second), scrapes[k].Recv.Add(start.Sub(time.Unix(1000, 0))-time.Second)
		v := 1.0
		if scrapes[k].Sent.After(tf) {
			v = 0
		}
		scrapes[k].Healthy = map[string]float64{"10.0.0.1:8080": 1, "10.0.0.2:8080": 1, "10.0.0.3:8080": v}
	}
	traces := []traceResult{{}, {}, {}, {}, {}}
	r := analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{start: start, faultAt: tf, out: steady(), scrapes: scrapes}, traces)
	if !r.Pass || !r.FaultSeen.Pass || r.FaultSeen.FirstOutS <= 0 || r.FaultSeen.FirstOutS > 1 || r.Success.Counted != 1000 || r.Success.Succeeded != 1000 || r.Success.Before != 1 || r.Success.After != 1 {
		t.Errorf("steady: %+v", r)
	}
	// Counted: in flight at t_f (sent before, answered after), and due up
	// to t_f + 10 s; not answered by t_f, not due from t_f + 10 s.
	out := steady()
	out[999].done = at(5) // due at -10 ms, in flight at t_f
	r = analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: out, scrapes: scrapes}, traces)
	if r.Success.Counted != 1001 {
		t.Errorf("counted %d, want 1001 with one request in flight at the fault", r.Success.Counted)
	}
	// Exactly 1% of 1000 fails: pass; 11 fail; fail.
	out = steady()
	for i := 1000; i < 1010; i++ {
		out[i].status = 503
	}
	if r := analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: out, scrapes: scrapes}, traces); !r.Success.Pass || r.Success.Failures["503"] != 10 {
		t.Errorf("10 failures of 1000: %+v", r.Success)
	}
	out[1010].status, out[1010].errKind = 0, "timeout"
	r = analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: out, scrapes: scrapes}, traces)
	if r.Success.Pass || r.Pass || r.Success.Failures["timeout"] != 1 {
		t.Errorf("11 failures of 1000: %+v", r.Success)
	}
	if l := r.lines("5xx", defaultRules)[1]; !strings.HasPrefix(l, "FAIL  5xx: (a)") || !strings.Contains(l, "503 10, timeout 1") {
		t.Errorf("(a) line: %s", l)
	}
	// A failure after t_f + 10 s is not counted.
	out = steady()
	out[2500].status = 503
	if r := analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: out, scrapes: scrapes}, traces); !r.Success.Pass || r.Success.After == 1 {
		t.Errorf("failure after the window: %+v", r.Success)
	}
	// More than 1% dropped: saturated.
	out = steady()
	for i := 0; i < 41; i++ {
		out[i].sent, out[i].status = false, 0
	}
	if r := analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: out, scrapes: scrapes}, traces); r.Success.Pass || !r.Success.Saturated {
		t.Errorf("41 of 4000 dropped: %+v", r.Success)
	}
	// (0): the endpoint never out, or out only after t_f + 10 s.
	late := slices.Clone(scrapes)
	for k := range late {
		late[k].Healthy = map[string]float64{"10.0.0.3:8080": 1}
		if late[k].Sent.After(tf.Add(10 * time.Second)) {
			late[k].Healthy = map[string]float64{"10.0.0.3:8080": 0}
		}
	}
	r = analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: steady(), scrapes: late}, traces)
	if r.FaultSeen.Pass || r.Pass || !strings.HasPrefix(r.lines("5xx", defaultRules)[0], "FAIL  5xx: (0)") {
		t.Errorf("out too late: %+v", r.FaultSeen)
	}
	// (c): fewer than 5 traces, or a mismatch.
	if r := analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: steady(), scrapes: scrapes}, traces[:4]); r.Traces.Pass {
		t.Errorf("4 traces: %+v", r.Traces)
	}
	bad := []traceResult{{}, {}, {}, {}, {TraceID: "t", Mismatch: "no trace yet"}}
	r = analyzeUpstream(o, "10.0.0.3:8080", scenarioRun{faultAt: tf, out: steady(), scrapes: scrapes}, bad)
	if r.Traces.Pass || !strings.Contains(r.lines("5xx", defaultRules)[3], "first mismatch t: no trace yet") {
		t.Errorf("a mismatch: %+v", r.Traces)
	}
}

func TestRetriedRequests(t *testing.T) {
	tf := time.Unix(100, 0)
	at := func(ms int) time.Time { return tf.Add(time.Duration(ms) * time.Millisecond) }
	out := []request{
		{sent: true, sentAt: at(-1), attempts: 2, traceID: "before"},
		{sent: true, sentAt: at(5), attempts: 2, traceID: "b"},
		{sent: true, sentAt: at(3), attempts: 3, traceID: "a"},
		{sent: true, sentAt: at(4), attempts: 1, traceID: "once"},
		{sent: true, sentAt: at(6), attempts: 2, traceID: "c"},
	}
	var ids []string
	for _, q := range retriedRequests(out, tf, 2) {
		ids = append(ids, q.traceID)
	}
	if !slices.Equal(ids, []string{"a", "b"}) {
		t.Errorf("retried = %v, want [a b]", ids)
	}
}

// fakeTrace builds Tempo's answer for a trace: the gateway's server span,
// child of parent, and one attempt span per status (0: no response), each
// failed but the last unless lastOK.
type fakeAttempt struct {
	n      int
	status int
	failed bool
}

func fakeTrace(traceID, parent string, attempts []fakeAttempt) []byte {
	b64 := func(h string) string {
		b, _ := hex.DecodeString(h)
		return base64.StdEncoding.EncodeToString(b)
	}
	str := func(k, v string) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
	}
	num := func(k string, v int) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"intValue": fmt.Sprint(v)}}
	}
	server := "00f067aa0ba902b7"
	spans := []any{map[string]any{"traceId": b64(traceID), "spanId": b64(server), "parentSpanId": b64(parent), "name": "GET verify", "kind": "SPAN_KIND_SERVER", "status": map[string]any{}}}
	for i, a := range attempts {
		attrs := []any{num("spillway.attempt", a.n), str("spillway.upstream", "verify-5xx")}
		if a.status != 0 {
			attrs = append(attrs, num("http.response.status_code", a.status))
		}
		status := map[string]any{}
		if a.failed {
			status["code"] = "STATUS_CODE_ERROR"
		}
		spans = append(spans, map[string]any{"traceId": b64(traceID), "spanId": b64(fmt.Sprintf("%016x", i+1)), "parentSpanId": b64(server),
			"name": "upstream attempt", "kind": "SPAN_KIND_CLIENT", "attributes": attrs, "status": status})
	}
	body, _ := json.Marshal(map[string]any{"trace": map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{str("service.name", "spillway-gateway")}},
		"scopeSpans": []any{map[string]any{"spans": spans}},
	}}}})
	return body
}

func TestMatchTrace(t *testing.T) {
	const tid, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "a3ce929d0e0e4736"
	q := request{traceID: tid, spanID: parent, attempts: 2, status: 200}
	ok503 := []fakeAttempt{{1, 503, true}, {2, 200, false}}
	for _, tt := range []struct {
		name     string
		q        request
		body     []byte
		mismatch string // in the result; "" for a match
	}{
		{"503 then 200", q, fakeTrace(tid, parent, ok503), ""},
		{"refused then 200", q, fakeTrace(tid, parent, []fakeAttempt{{1, 0, true}, {2, 200, false}}), ""},
		{"three attempts, the last a 502 without response", request{traceID: tid, spanID: parent, attempts: 3, status: 502},
			fakeTrace(tid, parent, []fakeAttempt{{1, 503, true}, {2, 0, true}, {3, 0, true}}), ""},
		{"last 503 answered", request{traceID: tid, spanID: parent, attempts: 2, status: 503},
			fakeTrace(tid, parent, []fakeAttempt{{1, 503, true}, {2, 503, true}}), ""},
		{"not yet in Tempo", q, []byte(`{"trace":{},"metrics":{}}`), "no trace yet"},
		{"other parent", q, fakeTrace(tid, "1111111111111111", ok503), "0 gateway server spans"},
		{"one attempt span missing", q, fakeTrace(tid, parent, ok503[1:]), "1 attempt spans, Spillway-Attempts 2"},
		{"attempts misnumbered", q, fakeTrace(tid, parent, []fakeAttempt{{1, 503, true}, {3, 200, false}}), "no span of attempt 2"},
		{"duplicate attempt", q, fakeTrace(tid, parent, []fakeAttempt{{1, 503, true}, {1, 200, false}}), "two spans of attempt 1"},
		{"first attempt not an error", q, fakeTrace(tid, parent, []fakeAttempt{{1, 503, false}, {2, 200, false}}), "attempt 1 of 2 is not an error"},
		{"last status differs", q, fakeTrace(tid, parent, []fakeAttempt{{1, 503, true}, {2, 204, false}}), "last attempt has status 204"},
		{"no response but the client got 200", q, fakeTrace(tid, parent, []fakeAttempt{{1, 503, true}, {2, 0, true}}), "last attempt has no response"},
		{"bad JSON", q, []byte("{"), "tempo:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, got := matchTrace(tt.body, tt.q)
			if tt.mismatch == "" && got != "" || tt.mismatch != "" && !strings.Contains(got, tt.mismatch) {
				t.Errorf("mismatch %q, want %q", got, tt.mismatch)
			}
		})
	}
	if spanID("WhA0YP6DRjw=") != "5a103460fe83463c" || spanID("A3CE929D0E0E4736") != "a3ce929d0e0e4736" {
		t.Errorf("spanID: %s, %s", spanID("WhA0YP6DRjw="), spanID("A3CE929D0E0E4736"))
	}
}

// fakeM4Gateway answers as gateway-1 running a verify config over the mock
// m: while m fails, every fourth request is retried once (Spillway-Attempts: 2)
// and its trace, with the failed attempt, goes to the fake Tempo; its
// metrics show the mock's endpoint out and count the retries.
type fakeM4Gateway struct {
	m         *mock
	faultAddr string
	firsts    atomic.Int64
	retries   atomic.Int64
	mu        sync.Mutex
	traces    map[string][]byte // trace ID → Tempo's answer
}

func (g *fakeM4Gateway) start(t *testing.T) (url, metrics, tempo string) {
	g.traces = map[string][]byte{}
	traffic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts := 1
		if n := g.firsts.Add(1); g.m.fail.Load() && n%4 == 0 {
			attempts = 2
			g.retries.Add(1)
			parts := strings.Split(r.Header.Get("Traceparent"), "-")
			g.mu.Lock()
			g.traces[parts[1]] = fakeTrace(parts[1], parts[2], []fakeAttempt{{1, 503, true}, {2, 200, false}})
			g.mu.Unlock()
		}
		w.Header().Set(attemptsHeader, fmt.Sprint(attempts))
		_, _ = w.Write(mockBody)
	}))
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		out := 1
		if g.m.fail.Load() {
			out = 0
		}
		_, _ = fmt.Fprintf(w, "gw_config_version 51\ngw_circuit_state{upstream=\"verify-5xx\"} 0\n"+
			"gw_retries_total{outcome=\"sent\",upstream=\"verify-5xx\"} %d\n"+
			"gw_upstream_duration_seconds_count{attempt=\"1\",upstream=\"verify-5xx\"} %d\n"+
			"gw_upstream_endpoint_healthy{endpoint=\"10.0.0.1:8080\",upstream=\"verify-5xx\"} 1\n"+
			"gw_upstream_endpoint_healthy{endpoint=\"10.0.0.2:8080\",upstream=\"verify-5xx\"} 1\n"+
			"gw_upstream_endpoint_healthy{endpoint=\"%s\",upstream=\"verify-5xx\"} %d\n"+
			"gw_upstream_endpoint_healthy{endpoint=\"10.0.0.9:8080\",upstream=\"other\"} 0\n",
			g.retries.Load(), g.firsts.Load(), g.faultAddr, out)
	}))
	tempoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		body, ok := g.traces[strings.TrimPrefix(r.URL.Path, "/api/v2/traces/")]
		g.mu.Unlock()
		if !ok {
			body = []byte(`{"trace":{},"metrics":{}}`)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(traffic.Close)
	t.Cleanup(admin.Close)
	t.Cleanup(tempoSrv.Close)
	return traffic.URL, admin.URL, tempoSrv.URL
}

// TestRunUpstream runs the sender end to end against the mock, a fake
// gateway and a fake Tempo, with the rules scaled down to a 4 s run.
func TestRunUpstream(t *testing.T) {
	m, _, ctl := startMock(t)
	g := &fakeM4Gateway{m: m, faultAddr: m.addr}
	url, metrics, tempo := g.start(t)
	o, err := parseUpstreamFlags([]string{"-fault", "5xx", "-url", url, "-metrics", metrics, "-control", ctl.URL, "-tempo", tempo,
		"-fault-endpoint", m.addr, "-rate", "50", "-duration", "4s", "-fault-after", "1500ms", "-trace-wait", "3s", "-setup-wait", "1s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o.rules.success, o.rules.maxWindow, o.rules.minWindow, o.rules.minWindows, o.rules.tracePoll = time.Second, 2500*time.Millisecond, 1500*time.Millisecond, 3, 100*time.Millisecond
	var stdout, stderr bytes.Buffer
	code := runUpstreamWith(t.Context(), o, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code %d; stderr:\n%s", code, stderr.String())
	}
	var r upstreamReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("report: %v\n%s", err, stdout.String())
	}
	if r.Requests.Scheduled != 200 || r.FaultEndpoint != m.addr || r.Traces.Checked != 20 || r.Traces.AttemptSpans != 40 || r.Traces.Retried < 20 ||
		r.Success.Failures == nil || len(r.Scrapes) < 6 || r.Circuit.Max != 0 || r.Budget.Sent == 0 {
		t.Errorf("report: %+v", r)
	}
	if m.fail.Load() {
		t.Error("the mock still fails after the run")
	}
	if n := strings.Count(stderr.String(), "PASS  5xx: "); n != 4 {
		t.Errorf("%d PASS lines, want 4:\n%s", n, stderr.String())
	}

	// Setup: the gateway runs another config version.
	o.configVersion = 52
	stderr.Reset()
	if code := runUpstreamWith(t.Context(), o, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "FAIL  5xx: setup") {
		t.Errorf("wrong version: exit code %d, stderr:\n%s", code, stderr.String())
	}
}
