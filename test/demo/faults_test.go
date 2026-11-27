//go:build demo

package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fault switches on the running demo (SC-03, ADR-0095 §5), from the
// host: make demo-fault-redis and make demo-fault-upstream switch each fault
// through the control plane, and the demo gateways show D5's failure modes
// (RL-22 – RL-24) and D6's breaker and retry budget (GW-13, GW-14). Only
// gateway-1 is published on the host, so the checks are gateway-1's; its
// admin port (8001) gives its metrics.
//
// Host and Docker Desktop stalls (usually 4–9 ms, sometimes 100–820 ms, up
// to two bursts in a run; ADR-0026, ADR-0027, ADR-0034) cannot fail a check
// by themselves: answers are placed in windows by their due time, not their
// send time; D5's degraded count is ± 10% (± 1 s of the per-replica rate)
// and every GCRA burst holds 2 s of its rate, so requests a stall holds
// back are admitted when they arrive; latency checks compare medians;
// gateway-side, 1% of the Redis checks may exceed 10 ms (a stall lengthens
// only the few checks in flight); D6's budget windows are ADR-0031's rule,
// bounded by each scrape's send and receive times; every wait (5 s for
// Redis to decide again, 15 s for the breaker to open, 30 s for it to
// close) is many times the longest stall.

const (
	gatewayAdmin = "http://127.0.0.1:8001/metrics"
	toxiproxyAPI = "http://127.0.0.1:8474"
)

// routes are the demo routes' requests (ADR-0094 §3).
var routes = map[string][2]string{
	"orders":   {"GET", "/v1/orders"},
	"catalog":  {"GET", "/v1/catalog"},
	"payments": {"POST", "/v1/payments"},
}

// answer is one request's answer from gateway-1.
type answer struct {
	due      time.Duration // when it was due, from the phase's start
	latency  time.Duration
	status   int    // 0: no answer
	decision bool   // Spillway-Decision-Time: Redis decided it (ADR-0021)
	redisErr string // Spillway-Redis-Error (ADR-0034)
	attempts int    // Spillway-Attempts (ADR-0030)
}

// load is one stream of a phase: rate requests per second on route.
type load struct {
	route string
	rate  float64
}

// stream sends l's requests to gateway-1 with key for d (open model:
// request i is due at i / rate, sent then or at once if overdue), on its
// own connections, and returns every answer once all are in (15 s client
// timeout, above the routes' 6 s).
func stream(t *testing.T, key string, d time.Duration, l load) []answer {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 200}}
	defer client.CloseIdleConnections()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []answer
	start := time.Now()
	for i := range int(l.rate * d.Seconds()) {
		due := time.Duration(float64(i) / l.rate * float64(time.Second))
		time.Sleep(time.Until(start.Add(due)))
		wg.Go(func() {
			a := hit(client, key, l.route)
			a.due = due
			mu.Lock()
			out = append(out, a)
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// phase runs the streams of loads at once for d and returns their answers
// by route.
func phase(t *testing.T, key string, d time.Duration, loads ...load) map[string][]answer {
	t.Helper()
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := map[string][]answer{}
	for _, l := range loads {
		wg.Go(func() {
			got := stream(t, key, d, l)
			mu.Lock()
			out[l.route] = got
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// hit sends one request for route to gateway-1 with key.
func hit(client *http.Client, key, route string) answer {
	r := routes[route]
	req, _ := http.NewRequest(r[0], gatewayURL+r[1], nil) //nolint:noctx // bounded by the client's timeout
	req.Header.Set("X-API-Key", key)
	sent := time.Now()
	resp, err := client.Do(req)
	a := answer{latency: time.Since(sent)}
	if err != nil {
		return a
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	a.status = resp.StatusCode
	a.decision = resp.Header.Get("Spillway-Decision-Time") != ""
	a.redisErr = resp.Header.Get("Spillway-Redis-Error")
	a.attempts, _ = strconv.Atoi(resp.Header.Get("Spillway-Attempts"))
	return a
}

// share is the share of as for which ok holds.
func share(as []answer, ok func(answer) bool) float64 {
	if len(as) == 0 {
		return 0
	}
	n := 0
	for _, a := range as {
		if ok(a) {
			n++
		}
	}
	return float64(n) / float64(len(as))
}

func count(as []answer, ok func(answer) bool) int {
	n := 0
	for _, a := range as {
		if ok(a) {
			n++
		}
	}
	return n
}

func is2xx(a answer) bool { return a.status >= 200 && a.status < 300 }

func median(as []answer) time.Duration {
	ls := make([]time.Duration, 0, len(as))
	for _, a := range as {
		ls = append(ls, a.latency)
	}
	slices.Sort(ls)
	if len(ls) == 0 {
		return 0
	}
	return ls[len(ls)/2]
}

// issueKey issues a demo key for consumer (30 minutes, ADR-0095 §3) and
// waits until gateway-1 knows it; the key is never printed.
func issueKey(t *testing.T, consumer string) string {
	t.Helper()
	c := client(t)
	var k struct {
		Key string `json:"key"`
	}
	must(t, c, 201, "POST", fmt.Sprintf("/consumers/%d/keys", byName(list(t, c, "/consumers?limit=500"), consumer).ID), map[string]any{}, &k)
	probe := &http.Client{Timeout: 10 * time.Second}
	until(t, "the new key works on gateway-1", func() bool { s := hit(probe, k.Key, "catalog").status; return s >= 200 && s < 300 })
	return k.Key
}

// demoToxics returns the toxics on toxiproxy's demo-* proxies, by proxy.
func demoToxics(ctx context.Context) (map[string][]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", toxiproxyAPI+"/proxies", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]struct {
		Toxics []struct {
			Name string `json:"name"`
		} `json:"toxics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for name, p := range m {
		if !strings.HasPrefix(name, "demo-") {
			continue
		}
		for _, x := range p.Toxics {
			out[name] = append(out[name], x.Name)
		}
		slices.Sort(out[name])
	}
	return out, nil
}

// switchFault runs make target ON=on and checks its output.
func switchFault(t *testing.T, target, fault string, on bool) {
	t.Helper()
	state, arg := "off", "ON=0"
	if on {
		state, arg = "on", "ON=1"
	}
	out := runMake(t, target, arg)
	noLeak(t, "make "+target, out)
	if !strings.Contains(out, fault+": "+state+"\n") {
		t.Fatalf("make %s %s:\n%s", target, arg, out)
	}
}

// clearFaultsAfter switches both faults off when the test ends, also when
// it failed (make demo-fault-clear, the API path), then checks that no
// demo proxy keeps a toxic; any left is removed through toxiproxy's API
// and fails the test.
func clearFaultsAfter(t *testing.T) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // t.Context() has ended
		defer cancel()
		out, err := exec.CommandContext(ctx, "make", "--no-print-directory", "-C", "../..", "demo-fault-clear").CombinedOutput() //nolint:gosec // G204: the tests' own target
		if err != nil {
			t.Errorf("make demo-fault-clear: %v\n%s", err, out)
		}
		left, err := demoToxics(ctx)
		if err != nil {
			t.Errorf("toxiproxy: %v", err)
			return
		}
		for p, names := range left {
			for _, n := range names {
				t.Errorf("toxic %s left on %s after make demo-fault-clear; removing it", n, p)
				req, _ := http.NewRequestWithContext(ctx, "DELETE", toxiproxyAPI+"/proxies/"+p+"/toxics/"+n, nil)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}
		}
	})
}

// metrics is a scrape of gateway-1's admin port.
type metrics struct {
	sent, recv time.Time
	ok         bool
	samples    []sample
}

type sample struct {
	name   string
	labels map[string]string
	v      float64
}

// scrapeGateway reads gateway-1's metrics with a 2 s timeout.
func scrapeGateway() metrics {
	m := metrics{sent: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", gatewayAdmin, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.recv = time.Now()
		return m
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m.recv = time.Now()
	if err != nil || resp.StatusCode != 200 {
		return m
	}
	m.ok = true
	for l := range strings.Lines(string(body)) {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' {
			continue
		}
		head, val, ok := strings.Cut(l, " ")
		v, err := strconv.ParseFloat(strings.Fields(val)[0], 64)
		if !ok || err != nil {
			continue
		}
		s := sample{labels: map[string]string{}, v: v}
		name, lbls, _ := strings.Cut(head, "{")
		s.name = name
		for kv := range strings.SplitSeq(strings.TrimSuffix(lbls, "}"), `",`) {
			if k, v, ok := strings.Cut(kv, `="`); ok {
				s.labels[k] = strings.TrimSuffix(v, `"`)
			}
		}
		m.samples = append(m.samples, s)
	}
	return m
}

// sum is the sum of name's samples whose labels include match.
func (m metrics) sum(name string, match ...string) float64 {
	total := 0.0
	for _, s := range m.samples {
		if s.name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(match); i += 2 {
			ok = ok && s.labels[match[i]] == match[i+1]
		}
		if ok {
			total += s.v
		}
	}
	return total
}

// TestFaultRedisCut is D5 on gateway-1 (RL-22 – RL-24, the chaos table's
// full outage through toxiproxy): pro-co-mobile sends catalog 50 rps
// (fail_open), payments 10 rps (fail_closed) and orders (degraded, its
// per-replica 33/s, burst 66 with three live replicas). Before: Redis
// decides at least 90% of catalog and payments. make demo-fault-redis ON=1
// puts the cut on all six demo-redis proxies; for 10 s from its return:
// at least 99.9% of catalog is 2xx, every payments answer is 503, orders
// at 66 rps admits (2xx − 66) ÷ 10 s within ± 10% of 33/s and denies the
// rest with 429, no answer is a Redis decision, every Redis error is
// timeout or conn (gw_redis_errors_total{kind="script"} does not grow),
// each route's median latency is at most the median before + 10 ms, and at
// least 99% of gateway-1's Redis checks end within 10 ms (the 5 ms budget,
// RL-22, plus ADR-0027's slack). make demo-fault-redis ON=0 removes it;
// Redis decides again within 5 s, and at least 90% of the next 5 s.
func TestFaultRedisCut(t *testing.T) {
	clearFaultsAfter(t)
	key := issueKey(t, "pro-co-mobile")

	before := phase(t, key, 5*time.Second, load{"catalog", 50}, load{"payments", 10}, load{"orders", 20})
	for _, r := range []string{"catalog", "payments"} {
		if s := share(before[r], func(a answer) bool { return a.decision }); s < 0.9 {
			t.Errorf("before the cut: %.1f%% of %s decided by Redis, want at least 90%%", 100*s, r)
		}
	}
	for r, as := range before {
		if s := share(as, is2xx); s != 1 {
			t.Errorf("before the cut: %.1f%% of %s 2xx, want all", 100*s, r)
		}
	}

	m0 := scrapeGateway()
	switchFault(t, "demo-fault-redis", "redis-cut", true)
	toxics, err := demoToxics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for n := range 6 {
		if p := fmt.Sprintf("demo-redis-%d", n); !slices.Equal(toxics[p], []string{"demo-cut-down", "demo-cut-up"}) {
			t.Errorf("%s toxics %v", p, toxics[p])
		}
	}
	cut := phase(t, key, 10*time.Second, load{"catalog", 50}, load{"payments", 10}, load{"orders", 66})
	m1 := scrapeGateway()
	switchFault(t, "demo-fault-redis", "redis-cut", false)
	off := time.Now()
	if left, err := demoToxics(t.Context()); err != nil || len(left) != 0 {
		t.Errorf("toxics after make demo-fault-redis ON=0: %v %v", left, err)
	}

	catalog, payments, orders := cut["catalog"], cut["payments"], cut["orders"]
	t.Logf("cut: catalog %d answers, %.2f%% 2xx; payments %d, %d × 503; orders %d, %d × 2xx, %d × 429",
		len(catalog), 100*share(catalog, is2xx), len(payments), count(payments, func(a answer) bool { return a.status == 503 }),
		len(orders), count(orders, is2xx), count(orders, func(a answer) bool { return a.status == 429 }))
	if s := share(catalog, is2xx); s < 0.999 {
		t.Errorf("fail-open: %.2f%% of catalog 2xx, want at least 99.9%%", 100*s)
	}
	if s := share(payments, func(a answer) bool { return a.status == 503 }); s != 1 {
		t.Errorf("fail-closed: %.1f%% of payments 503, want all", 100*s)
	}
	admitted := count(orders, is2xx)
	rate := float64(admitted-66) / 10
	t.Logf("degraded: orders admitted %d in 10 s: %.1f/s after the burst, against 33/s", admitted, rate)
	if rate < 0.9*33 || rate > 1.1*33 {
		t.Errorf("degraded: (%d − 66) ÷ 10 s = %.1f/s, want 33/s ± 10%%", admitted, rate)
	}
	if n := count(orders, func(a answer) bool { return !is2xx(a) && a.status != 429 }); n > 0 {
		t.Errorf("degraded: %d orders answers neither 2xx nor 429", n)
	}
	all := slices.Concat(catalog, payments, orders)
	if n := count(all, func(a answer) bool { return a.decision }); n > 0 {
		t.Errorf("%d answers in the cut were decided by Redis", n)
	}
	kinds := map[string]int{}
	for _, a := range all {
		for k := range strings.SplitSeq(a.redisErr, ",") {
			if k != "" {
				kinds[k]++
			}
		}
	}
	t.Logf("Redis errors in the cut by kind: %v", kinds)
	if len(kinds) == 0 {
		t.Error("no answer in the cut carried a Redis error")
	}
	for k := range kinds {
		if k != "timeout" && k != "conn" {
			t.Errorf("a Redis error of kind %q in the cut", k)
		}
	}
	if !m0.ok || !m1.ok {
		t.Fatal("gateway-1's metrics could not be read")
	}
	if d := m1.sum("gw_redis_errors_total", "kind", "script") - m0.sum("gw_redis_errors_total", "kind", "script"); d != 0 {
		t.Errorf("gw_redis_errors_total{kind=\"script\"} grew by %v in the cut", d)
	}
	for r, as := range cut {
		if b, c := median(before[r]), median(as); c > b+10*time.Millisecond {
			t.Errorf("%s: median latency %v in the cut, %v before: more than 10 ms over", r, c, b)
		}
	}
	checks := m1.sum("gw_ratelimit_check_duration_seconds_count", "backend", "redis") - m0.sum("gw_ratelimit_check_duration_seconds_count", "backend", "redis")
	fast := m1.sum("gw_ratelimit_check_duration_seconds_bucket", "backend", "redis", "le", "0.01") - m0.sum("gw_ratelimit_check_duration_seconds_bucket", "backend", "redis", "le", "0.01")
	t.Logf("gateway-1's Redis checks in the cut: %.0f, %.2f%% within 10 ms", checks, 100*fast/checks)
	if checks == 0 || fast/checks < 0.99 {
		t.Errorf("within budget: %.0f of %.0f Redis checks within 10 ms, want at least 99%%", fast, checks)
	}

	// Recovery.
	probe := &http.Client{Timeout: 10 * time.Second}
	for !hit(probe, key, "catalog").decision {
		if time.Since(off) > 5*time.Second {
			t.Fatalf("Redis does not decide again within 5 s of make demo-fault-redis ON=0")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("recovery: Redis decides again %.2f s after make demo-fault-redis ON=0 returned", time.Since(off).Seconds())
	after := phase(t, key, 5*time.Second, load{"catalog", 50}, load{"payments", 10})
	for r, as := range after {
		if s := share(as, func(a answer) bool { return a.decision }); s < 0.9 {
			t.Errorf("after the cut: %.1f%% of %s decided by Redis, want at least 90%%", 100*s, r)
		}
	}
}

// TestFaultUpstreamSlow is D6 on gateway-1 (GW-13, GW-14): pro-co-mobile
// sends orders at 30 rps, gateway-1's metrics are scraped every second.
// Before: every answer 2xx, circuit closed. make demo-fault-upstream ON=1
// adds 4 s to every orders endpoint (over its 2.5 s per-try timeout); the
// circuit opens within 15 s and gw_requests_total{reason="circuit_open"}
// grows; retries stay within the budget in every window of ADR-0031's rule
// (b) (ΔR ≤ 0.2 × first attempts + 30 over 8–10 s windows, at least 30
// windows, every scrape answered, at most 2 s apart). make
// demo-fault-upstream ON=0 removes it after 20 s; the circuit closes within
// 30 s and at least 99% of the answers due 20–30 s after are 2xx.
func TestFaultUpstreamSlow(t *testing.T) {
	clearFaultsAfter(t)
	key := issueKey(t, "pro-co-mobile")

	var scrapes []metrics
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		first := time.Now()
		for k := 0; ; k++ {
			time.Sleep(time.Until(first.Add(time.Duration(k) * time.Second)))
			select {
			case <-stop:
				return
			default:
			}
			m := scrapeGateway()
			mu.Lock()
			scrapes = append(scrapes, m)
			mu.Unlock()
		}
	}()
	stopScrapes := sync.OnceFunc(func() { close(stop); <-done })
	t.Cleanup(stopScrapes)
	circuit := func(m metrics) float64 { return m.sum("gw_circuit_state", "upstream", "orders") }

	before := phase(t, key, 5*time.Second, load{"orders", 30})["orders"]
	if s := share(before, is2xx); s != 1 {
		t.Errorf("before the fault: %.1f%% of orders 2xx, want all", 100*s)
	}
	if m := scrapeGateway(); !m.ok || circuit(m) != 0 {
		t.Errorf("before the fault: circuit %v (read %v), want 0", circuit(m), m.ok)
	}
	switchFault(t, "demo-fault-upstream", "upstream-slow", true)
	on := time.Now()
	toxics, err := demoToxics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		if p := fmt.Sprintf("demo-upstream-%d", n); !slices.Equal(toxics[p], []string{"demo-slow"}) {
			t.Errorf("%s toxics %v", p, toxics[p])
		}
	}
	fault := phase(t, key, 20*time.Second, load{"orders", 30})["orders"]
	switchFault(t, "demo-fault-upstream", "upstream-slow", false)
	off := time.Now()
	recovery := phase(t, key, 30*time.Second, load{"orders", 30})["orders"]
	stopScrapes()

	statuses := map[int]int{}
	retried := 0
	for _, a := range fault {
		statuses[a.status]++
		if a.attempts >= 2 {
			retried++
		}
	}
	t.Logf("fault: %d answers by status %v, %d with two attempts", len(fault), statuses, retried)

	var opened time.Time
	var atOn, atOff metrics
	for _, m := range scrapes {
		if !m.ok {
			continue
		}
		if m.sent.Before(on) {
			atOn = m
		}
		if m.sent.Before(off) {
			atOff = m
		}
		if opened.IsZero() && !m.sent.Before(on) && circuit(m) == 1 {
			opened = m.sent
		}
	}
	if opened.IsZero() || opened.Sub(on) > 15*time.Second {
		t.Errorf("the circuit did not open within 15 s of the fault (opened %v after)", opened.Sub(on))
	} else {
		t.Logf("the circuit opened %.1f s after the fault", opened.Sub(on).Seconds())
	}
	open := func(m metrics) float64 {
		return m.sum("gw_requests_total", "route", "orders", "reason", "circuit_open")
	}
	if d := open(atOff) - open(atOn); d <= 0 {
		t.Errorf("gw_requests_total{route=\"orders\", reason=\"circuit_open\"} did not grow in the fault (%v)", d)
	}

	// ADR-0031's rule (b), as d6.3 of ADR-0098.
	retries := func(m metrics) float64 { return m.sum("gw_retries_total", "upstream", "orders", "outcome", "sent") }
	firsts := func(m metrics) float64 {
		return m.sum("gw_upstream_duration_seconds_count", "upstream", "orders", "attempt", "1")
	}
	windows, over, tightest := 0, 0, 0.0
	for k, m := range scrapes {
		if !m.ok {
			t.Errorf("scrape %d failed", k)
		}
		if k > 0 && m.sent.Sub(scrapes[k-1].sent) > 2*time.Second {
			t.Errorf("scrapes %d and %d sent %.2f s apart", k-1, k, m.sent.Sub(scrapes[k-1].sent).Seconds())
		}
	}
	for j, mj := range scrapes {
		for _, mi := range scrapes[:j] {
			if !mi.ok || !mj.ok || mj.recv.Sub(mi.sent) > 10*time.Second || mj.sent.Sub(mi.recv) < 8*time.Second {
				continue
			}
			r, q := retries(mj)-retries(mi), firsts(mj)-firsts(mi)
			windows++
			if 5*r > q+150 {
				over++
			}
			tightest = max(tightest, r/(q/5+30))
			break
		}
	}
	last, first := scrapes[len(scrapes)-1], scrapes[0]
	sent := retries(last) - retries(first)
	exhausted := last.sum("gw_retries_total", "upstream", "orders", "outcome", "budget_exhausted") - first.sum("gw_retries_total", "upstream", "orders", "outcome", "budget_exhausted")
	t.Logf("retry budget: %d windows, %d over, tightest at %.0f%% of its budget; retries sent %.0f, budget_exhausted %.0f", windows, over, 100*tightest, sent, exhausted)
	if windows < 30 || over > 0 {
		t.Errorf("retry budget: %d windows, %d over; want at least 30 and none over", windows, over)
	}
	if sent == 0 {
		t.Error("no retry was sent in the fault")
	}

	var closed time.Time
	for _, m := range scrapes {
		if m.ok && !m.sent.Before(off) && circuit(m) == 0 {
			closed = m.sent
			break
		}
	}
	if closed.IsZero() || closed.Sub(off) > 30*time.Second {
		t.Errorf("the circuit did not close within 30 s of the fault's end")
	} else {
		t.Logf("the circuit closed %.1f s after the fault's end", closed.Sub(off).Seconds())
	}
	var late []answer
	for _, a := range recovery {
		if a.due >= 20*time.Second {
			late = append(late, a)
		}
	}
	if s := share(late, is2xx); s < 0.99 {
		t.Errorf("recovery: %.1f%% of the answers due 20–30 s after the fault 2xx, want at least 99%%", 100*s)
	}
}
