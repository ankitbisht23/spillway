package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/demo"
)

// TestBaseLoad: each tenant's base load is ADR-0096 §2's table, 258.5
// rps in all, under every limit, and only enterprise-co-web is above D4's
// 50/s on orders.
func TestBaseLoad(t *testing.T) {
	want := map[string]float64{
		"free-co-web orders": 4, "free-co-web catalog": 2, "free-co-web payments": 0.5,
		"pro-co-web orders": 35, "pro-co-web catalog": 15, "pro-co-web payments": 2,
		"enterprise-co-web orders": 140, "enterprise-co-web catalog": 50, "enterprise-co-web payments": 10,
	}
	plan := map[string]float64{"free-co": 10, "pro-co": 100, "enterprise-co": 1000}
	total := 0.0
	for _, s := range baseLoad() {
		n := s.consumer + " " + s.route
		if s.perSecond() != want[n] {
			t.Errorf("%s: %v rps, want %v", n, s.perSecond(), want[n])
		}
		delete(want, n)
		total += s.perSecond()
		if s.replica != 0 || s.from != 0 || s.to != 0 {
			t.Errorf("%s: not round robin for the whole run", n)
		}
		limit := map[string]float64{"orders": plan[tenantOf(s.consumer)], "catalog": 200, "payments": 100}[s.route]
		if s.perSecond() >= limit {
			t.Errorf("%s: %v rps, not under its limit %v", n, s.perSecond(), limit)
		}
		if s.route == "orders" && (s.perSecond() > 50) != (s.consumer == "enterprise-co-web") {
			t.Errorf("%s: %v rps on orders; only enterprise-co-web is above D4's 50/s", n, s.perSecond())
		}
	}
	if len(want) != 0 || total != 258.5 {
		t.Errorf("missing %v; total %v rps, want 258.5", want, total)
	}
}

// TestScenarioShapes: every scenario's streams, lengths and checks are
// ADR-0096 §2's and ADR-0098 §2's, and every consumer is in the seed.
func TestScenarioShapes(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/demo/seed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := demo.ParseSeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	seeded := map[string]bool{}
	for _, tn := range seed.Tenants {
		for _, c := range tn.Consumers {
			seeded[c.Name] = true
		}
	}
	type shape struct {
		demo, check time.Duration
		streams     []string // consumer route rate [replica] [from-to]
		checks      int
	}
	want := map[string]shape{
		"d1": {60 * time.Second, 45 * time.Second, []string{"free-co-mobile orders 30", "pro-co-mobile orders 50"}, 4},
		"d2": {60 * time.Second, 410 * time.Second, []string{"free-co-mobile orders 500 15s-45s", "pro-co-mobile orders 50", "enterprise-co-mobile orders 100"}, 4},
		"d3": {60 * time.Second, 60 * time.Second, []string{"pro-co-mobile payments 200"}, 4},
		"d4": {90 * time.Second, 150 * time.Second, nil, 6},
		"d5": {45 * time.Second, 50 * time.Second, []string{"pro-co-mobile catalog 100", "pro-co-mobile payments 10", "pro-co-mobile orders 66 @1", "pro-co-mobile orders 66 @2", "pro-co-mobile orders 66 @3"}, 9},
		"d6": {70 * time.Second, 75 * time.Second, []string{"pro-co-mobile orders 60"}, 4},
		"d7": {60 * time.Second, 120 * time.Second, []string{"free-co-partner orders 200"}, 4},
	}
	all := scenarios()
	if len(all) != 8 || all["base"] == nil {
		t.Fatalf("scenarios %v, want base and d1 … d7", slices.Sorted(func(yield func(string) bool) {
			for k := range all {
				if !yield(k) {
					return
				}
			}
		}))
	}
	for name, w := range want {
		sc := all[name]
		if sc == nil {
			t.Fatalf("no scenario %s", name)
		}
		if sc.length(false) != w.demo || sc.length(true) != w.check {
			t.Errorf("%s: %v and %v long, want %v and %v (check)", name, sc.length(false), sc.length(true), w.demo, w.check)
		}
		var got []string
		for _, s := range sc.streams(false) {
			d := fmt.Sprintf("%s %s %g", s.consumer, s.route, s.perSecond())
			if s.replica > 0 {
				d += fmt.Sprintf(" @%d", s.replica)
			}
			if s.from > 0 || s.to > 0 {
				d += fmt.Sprintf(" %v-%v", s.from, s.to)
			}
			got = append(got, d)
			if !seeded[s.consumer] {
				t.Errorf("%s: consumer %s is not in deploy/demo/seed.yaml", name, s.consumer)
			}
			if _, ok := routes[s.route]; !ok {
				t.Errorf("%s: no route %s", name, s.route)
			}
		}
		if !slices.Equal(got, w.streams) {
			t.Errorf("%s streams %q, want %q", name, got, w.streams)
		}
		if len(sc.ids) != w.checks || sc.checks == nil || sc.run == nil {
			t.Errorf("%s: %d checks, want %d", name, len(sc.ids), w.checks)
		}
		for i, d := range sc.ids {
			if !strings.HasPrefix(d.id, name+".") || d.what == "" || (i > 0 && d.id <= sc.ids[i-1].id) {
				t.Errorf("%s: check %+v", name, d)
			}
		}
	}
	for _, c := range baseConsumers {
		if !seeded[c] {
			t.Errorf("base consumer %s is not in the seed", c)
		}
	}
}

// TestD2CheckSlots: D2's check form is ADR-0098's: a 10 s warm-up, then
// 20 (off, on) pairs of 10 s slots, only the noisy tenant toggled.
func TestD2CheckSlots(t *testing.T) {
	sc := scenarios()["d2"]
	for _, s := range sc.streams(true) {
		if s.warmup != 10*time.Second || s.slot != 10*time.Second || s.toggle != (s.consumer == "free-co-mobile") || s.from != 0 || s.to != 0 {
			t.Errorf("%+v", s)
		}
		if d := sc.length(true) - s.warmup; d != 40*s.slot {
			t.Errorf("%s: %v measured, want 40 slots", s.consumer, d)
		}
	}
}

func TestParseRL(t *testing.T) {
	items, ok := parseRL(`"consumer-plan";q=10;w=1, "tenant-ceiling";q=1500;w=1`)
	if !ok || len(items) != 2 || items[0].name != "consumer-plan" || items[0].params["q"] != 10 || items[1].params["w"] != 1 {
		t.Errorf("%+v %v", items, ok)
	}
	if items, ok := parseRL(`"a"`); !ok || len(items) != 1 || len(items[0].params) != 0 {
		t.Errorf("a bare item: %+v %v", items, ok)
	}
	for _, bad := range []string{"", "a;q=1", `"a";q`, `"a";q=x`, `"a"q=1`, `"a";q=1,`, `"";q=1`} {
		if _, ok := parseRL(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// TestHeaderProblem: ADR-0098 d1.4 by each answer's own form.
func TestHeaderProblem(t *testing.T) {
	const (
		pol = `"consumer-plan";q=10;w=1, "tenant-ceiling";q=1500;w=1`
		lim = `"consumer-plan";r=3;t=1, "tenant-ceiling";r=1499;t=1`
	)
	for _, tt := range []struct {
		a    answer
		want string // a substring of the problem; "" for none
	}{
		{answer{status: 200, policy: pol, rateLimit: lim}, ""},
		{answer{status: 429, policy: pol, rateLimit: lim, retryAfter: "1"}, ""},
		{answer{status: 429, policy: pol, rateLimit: lim, retryAfter: "0"}, "Retry-After"},
		{answer{status: 429, policy: pol, rateLimit: lim}, "Retry-After"},
		{answer{status: 429, policy: pol, rateLimit: lim, retryAfter: "1.5"}, "Retry-After"},
		{answer{status: 200, policy: `"consumer-plan";q=100;w=1, "tenant-ceiling";q=1500;w=1`, rateLimit: lim}, "consumer-plan"},
		{answer{status: 200, policy: `"consumer-plan";q=10;w=1`, rateLimit: lim}, "tenant-ceiling"},
		{answer{status: 200, policy: pol, rateLimit: `"consumer-plan";r=3`}, "RateLimit"},
		{answer{status: 200, policy: "nope", rateLimit: lim}, "does not parse"},
		// A failure-mode answer: the degraded q, and tenant-ceiling (fail_open) may be missing.
		{answer{status: 200, redisErr: "timeout", policy: `"consumer-plan";q=3;w=1`, rateLimit: `"consumer-plan";r=1;t=1`}, ""},
		{answer{status: 200, redisErr: "timeout", policy: pol, rateLimit: lim}, "q=3"},
		{answer{status: 503}, ""},
	} {
		got := headerProblem(tt.a, 10, 3)
		if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
			t.Errorf("%+v: %q, want %q", tt.a, got, tt.want)
		}
	}
}

// TestWindowMax: ADR-0021's check over admission times.
func TestWindowMax(t *testing.T) {
	l := gcraLimit{rate: 100, period: time.Second, burst: 200}
	var ts []int64
	for i := range 200 { // the burst at once
		ts = append(ts, int64(i))
	}
	for i := 1; i <= 100; i++ { // then the rate for a second
		ts = append(ts, int64(i)*10_000)
	}
	most, over, _ := windowMax(ts, l)
	if over != 0 || most[time.Second] != 300 || most[10*time.Millisecond] != 201 {
		t.Errorf("over %d, most %v", over, most)
	}
	ts = append(ts, 999_999) // one more within the first second
	slices.Sort(ts)
	if _, over, w := windowMax(ts, l); over != 1 || w != time.Second {
		t.Errorf("one over: %d in %v", over, w)
	}
}

// scrapesAt makes one replica's scrapes, 1 s apart, with R and Q.
func scrapesAt(start time.Time, rq ...[2]float64) []scrape {
	var out []scrape
	for i, v := range rq {
		at := start.Add(time.Duration(i) * time.Second)
		out = append(out, scrape{sent: at, recv: at.Add(10 * time.Millisecond), ok: true, samples: []sample{
			{name: "gw_retries_total", labels: map[string]string{"upstream": "orders", "outcome": "sent"}, v: v[0]},
			{name: "gw_upstream_duration_seconds_count", labels: map[string]string{"upstream": "orders", "attempt": "1"}, v: v[1]},
		}})
	}
	return out
}

// TestBudgetRule: ADR-0101 §13's rule: retries paid for by first attempts
// that ended up to 10 s before the pair pass; more fail; coverage counts.
func TestBudgetRule(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	var steady [][2]float64
	for i := range 60 { // 30 first attempts a second, then none from 20 s; 3 retries a second
		q := float64(min(i, 20) * 30)
		steady = append(steady, [2]float64{float64(i * 3), q})
	}
	b := budgetRule(scrapesAt(start, steady...))
	if b.over != 0 || b.windows < 30 || b.gaps != 0 || b.failed != 0 {
		t.Errorf("steady: %+v", b)
	}
	burst := slices.Clone(steady)
	for i := 40; i < 60; i++ {
		burst[i][0] += 100 // 100 retries with no first attempt to pay for them
	}
	if b := budgetRule(scrapesAt(start, burst...)); b.over == 0 || b.tightest <= 1 {
		t.Errorf("over budget: %+v", b)
	}
	gappy := scrapesAt(start, steady...)
	gappy[10].ok = false
	gappy = append(gappy[:30], gappy[32:]...) // 3 s apart
	if b := budgetRule(gappy); b.gaps != 1 || b.failed != 1 {
		t.Errorf("coverage: %+v", b)
	}
}

func TestParseMetrics(t *testing.T) {
	m := scrape{ok: true, samples: parseMetrics(`# HELP x
gw_circuit_state{upstream="orders"} 1
gw_circuit_state{upstream="catalog"} 0
gw_ratelimit_check_duration_seconds_bucket{backend="redis",le="0.01"} 99
gw_requests_total{code_class="2xx",reason="",route="orders"} 12 1700000000000
up 1
bad{x="1"} nope
`)}
	if m.circuit() != 1 || m.sum("gw_ratelimit_check_duration_seconds_bucket", "backend", "redis", "le", "0.01") != 99 ||
		m.sum("gw_requests_total", "route", "orders", "reason", "") != 12 || m.sum("up") != 1 || m.sum("bad") != 0 {
		t.Errorf("%+v", m.samples)
	}
	if (scrape{}).circuit() != -1 {
		t.Error("an unread scrape has a circuit state")
	}
}

func TestGatewaysFrom(t *testing.T) {
	gs, err := gatewaysFrom("http://gateway-1:8000,http://gateway-2:8000/", "http://gateway-1:8001,http://gateway-2:8001")
	if err != nil || len(gs) != 2 || gs[1] != (gateway{"gateway-2", "http://gateway-2:8000", "http://gateway-2:8001"}) {
		t.Errorf("%+v %v", gs, err)
	}
	for _, bad := range [][2]string{{"http://a:1", ""}, {"http://a:1,http://b:1", "http://a:2"}, {"ftp://a", "http://a:2"}, {"http://a:1", "nope"}} {
		if _, err := gatewaysFrom(bad[0], bad[1]); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// fakeDemo is a token endpoint and a control plane for "run" and
// "probe": demo mode on, the seed's consumers, key issue, faults; and
// three gateways that know the issued keys and answer as plans would,
// with their admin ports.
type fakeDemo struct {
	t       *testing.T
	mu      sync.Mutex
	keys    map[string]string // key → consumer
	n       int64
	faults  []string
	sent    map[string]int // requests per consumer
	cp      *httptest.Server
	gws     []*httptest.Server
	admins  []*httptest.Server
	secrets []string // every key issued
}

func newFakeDemo(t *testing.T) *fakeDemo {
	f := &fakeDemo{t: t, keys: map[string]string{}, sent: map[string]int{}}
	f.cp = httptest.NewServer(http.HandlerFunc(f.serveCP))
	t.Cleanup(f.cp.Close)
	for range 3 {
		g := httptest.NewServer(http.HandlerFunc(f.serveGateway))
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("gw_circuit_state{upstream=\"orders\"} 0\n"))
		}))
		t.Cleanup(g.Close)
		t.Cleanup(a.Close)
		f.gws, f.admins = append(f.gws, g), append(f.admins, a)
	}
	return f
}

const fakeSecret = "dev-only-run-secret" //nolint:gosec // G101: a test value

func (f *fakeDemo) env() func(string) string {
	var gws, admins []string
	for i := range f.gws {
		gws, admins = append(gws, f.gws[i].URL), append(admins, f.admins[i].URL)
	}
	return env(map[string]string{
		"SPILLWAY_DEMO_CP_URL": f.cp.URL, "SPILLWAY_DEMO_TOKEN_URL": f.cp.URL + "/token", "SPILLWAY_DEMO_CLIENT_SECRET": fakeSecret,
		"SPILLWAY_DEMO_GATEWAYS": strings.Join(gws, ","), "SPILLWAY_DEMO_ADMIN_URLS": strings.Join(admins, ","),
	})
}

var fakeConsumers = []string{"free-co-web", "pro-co-web", "enterprise-co-web", "free-co-mobile", "pro-co-mobile", "enterprise-co-mobile", "free-co-partner"}

func (f *fakeDemo) serveCP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/token" && r.Header.Get("Authorization") != "Bearer "+fakeToken {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.URL.Path == "/token":
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fakeToken})
	case r.Method == "GET" && r.URL.Path == "/api/v1/demo":
		_, _ = w.Write([]byte(`{"tenants": [], "routes": [], "scenarios": [], "faults": []}`))
	case r.Method == "GET" && r.URL.Path == "/api/v1/consumers":
		var items []map[string]any
		if i := slices.Index(fakeConsumers, r.URL.Query().Get("name")); i >= 0 {
			items = append(items, map[string]any{"id": i + 1, "name": fakeConsumers[i]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/consumers/") && strings.HasSuffix(r.URL.Path, "/keys"):
		id, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/consumers/"), "/keys"))
		f.n++
		key := fmt.Sprintf("spw_test%04d_%s", f.n, strings.Repeat("s", 43))
		f.keys[key] = fakeConsumers[id-1]
		f.secrets = append(f.secrets, key)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 400 + f.n, "consumer_id": id, "key": key, "expires_at": time.Now().Add(30 * time.Minute)})
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/v1/demo/faults/"):
		var body struct {
			Active bool `json:"active"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.faults = append(f.faults, fmt.Sprintf("%s %v", strings.TrimPrefix(r.URL.Path, "/api/v1/demo/faults/"), body.Active))
		_ = json.NewEncoder(w).Encode(map[string]any{"active": body.Active})
	default:
		w.WriteHeader(404)
	}
}

// serveGateway answers a known key as its plan would, roughly: free
// consumers get every third request, the others all.
func (f *fakeDemo) serveGateway(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	c, ok := f.keys[r.Header.Get(apiKeyHeader)]
	f.sent[c]++
	n := f.sent[c]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(401)
		return
	}
	q := map[string]int{"free-co": 10, "pro-co": 100, "enterprise-co": 1000}[tenantOf(c)]
	w.Header().Set("RateLimit-Policy", fmt.Sprintf(`"consumer-plan";q=%d;w=1, "tenant-ceiling";q=1500;w=1`, q))
	w.Header().Set("RateLimit", `"consumer-plan";r=1;t=1, "tenant-ceiling";r=1400;t=1`)
	w.Header().Set("Spillway-Decision-Time", strconv.FormatInt(time.Now().UnixMicro(), 10))
	if strings.HasPrefix(c, "free-co-mobile") && n%3 != 0 {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// noSecret fails when s holds a key, the token or the client secret.
func (f *fakeDemo) noSecret(t *testing.T, what, s string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range append(slices.Clone(f.secrets), fakeToken, fakeSecret, "Bearer", "eyJ", "spw_") {
		if strings.Contains(s, k) {
			t.Errorf("%s holds a key, token or secret", what)
		}
	}
}

// shortened makes lookupScenario return name with its length set to d.
func shortened(t *testing.T, name string, d time.Duration) {
	t.Helper()
	old := lookupScenario
	lookupScenario = func(n string) *scenario {
		sc := scenarios()[n]
		if sc != nil && n == name {
			sc.length = fixed(d, d)
		}
		return sc
	}
	t.Cleanup(func() { lookupScenario = old })
}

// TestRunNoLeak runs the base load, a shortened D1 check and a probe
// against a fake control plane and gateways (ADR-0096 §1): keys are issued
// per consumer and sent only in X-API-Key, every output line names a key
// by ID, and no output, check line or report holds a key, the token or the
// client secret.
func TestRunNoLeak(t *testing.T) {
	f := newFakeDemo(t)
	var all bytes.Buffer
	runIt := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(t.Context(), args, f.env(), &out, &errOut)
		all.WriteString(out.String() + errOut.String())
		return code, out.String(), errOut.String()
	}

	code, out, errOut := runIt("run", "--scenario=base", "--duration=2s", "--health=")
	if code != 0 || !strings.Contains(out, "keys: free-co-web key #401, pro-co-web key #402, enterprise-co-web key #403") || !strings.Contains(out, "summary (base") {
		t.Fatalf("base: exit %d\n%s\n%s", code, out, errOut)
	}
	f.mu.Lock()
	if f.sent["enterprise-co-web"] < 300 || f.sent["free-co-web"] < 5 || f.sent[""] != 0 {
		t.Errorf("the gateways saw %v requests", f.sent)
	}
	f.mu.Unlock()

	shortened(t, "d1", 6*time.Second)
	reportPath := filepath.Join(t.TempDir(), "out", "d1.json")
	code, out, errOut = runIt("run", "--scenario=d1", "--check", "--out", reportPath, "--health=")
	for _, l := range []string{"PASS d1.1 ", "PASS d1.2 ", "FAIL d1.3 ", "PASS d1.4 ", "d1: FAIL (1 of 4 checks: d1.3)\n"} {
		if !strings.Contains(errOut, l) {
			t.Errorf("d1 check: no %q in\n%s", l, errOut)
		}
	}
	if code != 1 || !strings.Contains(out, "free-co-mobile orders: 2xx") || !strings.Contains(out, "RateLimit-Policy: \"consumer-plan\";q=10;w=1") {
		t.Errorf("d1: exit %d\n%s", code, out)
	}
	b, err := os.ReadFile(reportPath) //nolint:gosec // G304: the test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal(b, &rep); err != nil || rep.Scenario != "d1" || rep.Pass || len(rep.Checks) != 4 || len(rep.Streams) != 11 {
		t.Errorf("report %+v %v", rep, err)
	}
	f.noSecret(t, "the report", string(b))

	code, out, _ = runIt("probe", "--consumer", "free-co-mobile", "--route", "orders", "-n", "4")
	if code != 0 || strings.Count(out, "\n") != 5 || !strings.Contains(out, ": 429  RateLimit: ") || !strings.Contains(out, "Retry-After: 1") {
		t.Errorf("probe: exit %d\n%s", code, out)
	}
	f.noSecret(t, "the output", all.String())
}

// TestRunErrors: bad flags exit 2; with demo mode off nothing is sent.
func TestRunErrors(t *testing.T) {
	f := newFakeDemo(t)
	for _, args := range [][]string{
		{"--scenario=d9"}, {"--scenario=base", "--check"}, {"--scenario=d1", "--out", "x"}, {"--scenario=d1", "--block"},
		{"--scenario=d1", "--duration=1s"}, {"extra"},
	} {
		var out bytes.Buffer
		if code := run(t.Context(), append([]string{"run"}, args...), f.env(), &out, &out); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	var out bytes.Buffer
	if code := run(t.Context(), []string{"run", "--health="}, env(nil), &out, &out); code != 2 || !strings.Contains(out.String(), "SPILLWAY_DEMO_CLIENT_SECRET") {
		t.Errorf("no secret: %q", out.String())
	}
	if code := run(t.Context(), []string{"probe", "--route", "orders"}, f.env(), &out, &out); code != 2 {
		t.Errorf("probe without a consumer: %d", code)
	}

	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fakeToken})
			return
		}
		if r.URL.Path != "/api/v1/demo" {
			t.Errorf("%s %s with demo mode off", r.Method, r.URL.Path)
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(off.Close)
	out.Reset()
	getenv := env(map[string]string{"SPILLWAY_DEMO_CP_URL": off.URL, "SPILLWAY_DEMO_TOKEN_URL": off.URL + "/token", "SPILLWAY_DEMO_CLIENT_SECRET": fakeSecret})
	if code := run(t.Context(), []string{"run", "--scenario=d1", "--check", "--health="}, getenv, &out, &out); code != 1 ||
		!strings.Contains(out.String(), "FAIL d1.1 every answer 2xx or (free only) 429: setup: demo mode is off") || strings.Count(out.String(), "FAIL d1.") != 4 {
		t.Errorf("demo mode off: %d\n%s", code, out.String())
	}
}

// TestFaultsOffOnExit: a fault the run switched on is switched off when
// the run ends, also on SIGINT or SIGTERM (its context cancelled).
func TestFaultsOffOnExit(t *testing.T) {
	f := newFakeDemo(t)
	old := lookupScenario
	t.Cleanup(func() { lookupScenario = old })
	lookupScenario = func(string) *scenario {
		return &scenario{name: "d5", title: "test", length: fixed(time.Minute, time.Minute), streams: func(bool) []streamSpec { return nil },
			run: func(r *runner) error {
				if err := r.setFault(r.ctx, "redis-cut", true); err != nil {
					return err
				}
				<-r.ctx.Done() // SIGINT
				return nil
			}}
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(500*time.Millisecond, cancel)
	var out bytes.Buffer
	code := run(ctx, []string{"run", "--scenario=d5", "--health="}, f.env(), &out, &out)
	f.mu.Lock()
	defer f.mu.Unlock()
	if code != 1 || !slices.Equal(f.faults, []string{"redis-cut true", "redis-cut false"}) || !strings.Contains(out.String(), "redis-cut: off") {
		t.Errorf("exit %d, switches %q\n%s", code, f.faults, out.String())
	}
}

// TestHealth: the health listener answers 503 until the load runs, then
// 200, and "healthcheck" exits accordingly.
func TestHealth(t *testing.T) {
	var ready atomic.Bool
	stop, addr, err := serveHealth("127.0.0.1:0", &ready)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var out bytes.Buffer
	if code := run(t.Context(), []string{"healthcheck", "-addr", addr}, env(nil), &out, &out); code != 1 {
		t.Errorf("not ready: exit %d", code)
	}
	ready.Store(true)
	if code := run(t.Context(), []string{"healthcheck", "-addr", addr}, env(nil), &out, &out); code != 0 {
		t.Errorf("ready: exit %d: %s", code, out.String())
	}
}

// TestKeyring: keys are named by ID only, and due lists those with under
// 10 minutes left.
func TestKeyring(t *testing.T) {
	k := newKeyring(nil)
	now := time.Now()
	key := &apiKey{id: 7, consumer: "a", expires: now.Add(9 * time.Minute)}
	k.use(key)
	k.use(&apiKey{id: 8, consumer: "b", expires: now.Add(20 * time.Minute)})
	if got := fmt.Sprintf("%v %s %+v", key, key, key.secret); strings.Contains(got, "spw_") || !strings.HasPrefix(got, "key #7 key #7") {
		t.Errorf("printed %q", got)
	}
	if d := k.due(now); !slices.Equal(d, []string{"a"}) || k.current("b").id != 8 || k.current("c") != nil {
		t.Errorf("due %v", d)
	}
}
