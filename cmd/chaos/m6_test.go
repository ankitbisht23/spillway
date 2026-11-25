package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseM6Flags(t *testing.T) {
	o, err := parseM6Flags([]string{"probe"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.step != "probe" || o.heavyRate != 200 || o.lightRate != 10 || o.timeout != 20*time.Second || o.maxInFlight != 2000 ||
		o.windowStart != time.Second || o.window != 20*time.Second || o.minHeavyShare != 0.5 || o.limit != 5*time.Second ||
		o.publishes != 23 || len(o.gateways) != 5 {
		t.Errorf("defaults are not ADR-0061's: %+v", o)
	}
	for _, args := range [][]string{
		{}, {"ready"}, {"probe", "extra"}, {"probe", "-gateways", ""}, {"probe", "-heavy-rate", "0"},
		{"probe", "-window", "0s"}, {"probe", "-min-heavy-429", "1.5"}, {"probe", "-publishes", "0"},
	} {
		if _, err := parseM6Flags(args, io.Discard); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
}

func TestRateLimitExhausted(t *testing.T) {
	for _, c := range []struct {
		fields []string
		want   bool
	}{
		{[]string{`"m6-limit-1764000000";r=0;t=1`}, true},
		{[]string{`"other";r=5;t=1, "m6-limit-1764000000";r=0;t=1`}, true},
		{[]string{`"other";r=5;t=1`, `"m6-limit-1764000000";t=1;r=0`}, true},
		{[]string{`"m6-limit-1764000000";r=3;t=1`}, false},
		{[]string{`"m6-limit-1763999999";r=0;t=1`}, false}, // an earlier run's (ADR-0072)
		{[]string{`"m6-limit";r=0;t=1`}, false},
		{[]string{`m6-limit-1764000000;r=0`}, false},
		{nil, false},
	} {
		if got := rateLimitExhausted(c.fields, m6Policy("1764000000")); got != c.want {
			t.Errorf("%q: %v, want %v", c.fields, got, c.want)
		}
	}
}

// TestM6Policy: the policy's name is m6-limit-<run>, 19 characters for
// m6.sh's ten-digit run ID, within the policy-name rule (ADR-0072).
func TestM6Policy(t *testing.T) {
	if got := m6Policy("1764000000"); got != "m6-limit-1764000000" || len(got) != 19 || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(got) {
		t.Errorf("m6Policy = %q", got)
	}
}

func TestReadInputs(t *testing.T) {
	in, err := readInputs(strings.NewReader(`exit {"run":"r1","base_version":7,"publishes":[{"kind":"route","version":8,"t_click":1,"t_response":2}]}

guard {"checked":{"cookies":3},"secrets_seen":4,"findings":[],"console_errors":[]}
spec 0
`))
	if err != nil || in.Exit == nil || in.Exit.BaseVersion != 7 || len(in.Exit.Publishes) != 1 || in.Guard == nil || in.Guard.SecretsSeen != 4 || in.SpecCode != 0 {
		t.Fatalf("%+v, %v", in, err)
	}
	if in, err := readInputs(strings.NewReader("")); err != nil || in.Exit != nil || in.Guard != nil || in.SpecCode != -1 {
		t.Errorf("empty: %+v, %v", in, err)
	}
	for _, bad := range []string{"exit {", "spec x", "what 1"} {
		if _, err := readInputs(strings.NewReader(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// m6Run is a synthetic make verify-m6 run that passes every rule: 20 route
// publishes, then S, E and R 40 s apart, and the probe's records at a
// tenth of the real rates.
type m6Run struct {
	o        m6Options
	in       m6Inputs
	versions map[int64]published
	recs     []probeRecord
	stopped  time.Time
	stage    map[string]published // S, E, R
}

func newM6Run(t *testing.T) *m6Run {
	t.Helper()
	o, err := parseM6Flags([]string{"probe"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := &m6Run{o: o, versions: map[int64]published{}, stage: map[string]published{}}
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	uiHeavy := int64(2000)
	exit := &m6Exit{Run: "r1", BaseVersion: 100, Review: &m6Review{UIShowed: true, UIWouldDeny: "2,123", UIHeavy: &uiHeavy, HeavyDigest: "hd", LightDigest: "ld"}}
	wd, ev, sh := 2123.0, 4300.0, 0.49
	exit.Review.WouldDeny, exit.Review.Evaluated, exit.Review.Share = &wd, &ev, &sh
	exit.Review.Keys = append(exit.Review.Keys, struct {
		KeyDigest    string `json:"key_digest"`
		ShadowDenied int64  `json:"shadow_denied"`
		Denied       int64  `json:"denied"`
	}{"hd", 2000, 0})
	publish := func(kind string, v int64, at time.Time) {
		p := published{Version: v, PublishedAt: at}
		for i, g := range o.gateways {
			p.Applied = append(p.Applied, struct {
				Replica string    `json:"replica"`
				At      time.Time `json:"at"`
			}{g, at.Add(time.Duration(5+i) * time.Millisecond)})
		}
		r.versions[v] = p
		click := at.Add(-40 * time.Millisecond).UnixMilli()
		exit.Publishes = append(exit.Publishes, m6Publish{Kind: kind, Version: v, TClick: click, TResponse: click + 60})
		if kind != "route" {
			r.stage[kind] = p
		}
	}
	for i := range 20 {
		publish("route", int64(101+i), t0.Add(time.Duration(i)*3*time.Second))
	}
	publish("shadow", 121, t0.Add(80*time.Second))
	publish("enforce", 122, t0.Add(120*time.Second))
	publish("rollback", 123, t0.Add(160*time.Second))
	exit.Steps = []string{"publishes", "shadow", "reviewed", "enforced", "rolled back"}
	r.in = m6Inputs{Exit: exit, Guard: &m6Guard{Checked: map[string]int{"request_urls": 900, "cookies": 3}, SecretsSeen: 6}, SpecCode: 0}

	// The probe: heavy every 25 ms, light every 500 ms, round robin.
	eApplied, rApplied := r.stage["enforce"].Applied[4].At, r.stage["rollback"].Applied[4].At
	end := t0.Add(200 * time.Second)
	add := func(heavy bool, every time.Duration) {
		for i, due := 0, t0; due.Before(end); i, due = i+1, due.Add(every) {
			rec := probeRecord{due: due, recv: due.Add(2 * time.Millisecond), gateway: i % 5, heavy: heavy, status: 200}
			if heavy && !due.Before(eApplied) && due.Before(rApplied) && i%4 != 0 {
				rec.status, rec.limitOK = 429, true
			}
			r.recs = append(r.recs, rec)
		}
	}
	add(true, 25*time.Millisecond)
	add(false, 500*time.Millisecond)
	r.stopped = end
	return r
}

func (r *m6Run) judge() (m6Report, map[string]bool) {
	rep := m6Report{}
	judgeM6(&rep, r.o, r.in, r.versions, r.recs, r.stopped)
	got := map[string]bool{}
	for _, x := range rep.Rules {
		check, _, _ := strings.Cut(x.Check, " ")
		got[check] = x.Pass
	}
	return rep, got
}

// set changes the records for which match holds.
func (r *m6Run) set(match func(probeRecord) bool, change func(*probeRecord)) {
	for i := range r.recs {
		if match(r.recs[i]) {
			change(&r.recs[i])
		}
	}
}

func (r *m6Run) window(kind string) (from, to time.Time) {
	last := r.stage[kind].Applied[4].At
	return last.Add(time.Second), last.Add(21 * time.Second)
}

func (r *m6Run) during(kind string) func(probeRecord) bool {
	from, to := r.window(kind)
	return func(x probeRecord) bool { return !x.due.Before(from) && x.due.Before(to) }
}

func TestJudgeM6Passes(t *testing.T) {
	r := newM6Run(t)
	rep, got := r.judge()
	t.Log("\n" + strings.Join(rep.Lines, "\n"))
	for _, c := range []string{"(a)", "(b)", "(b2)", "(c)", "(d)", "(e)", "(f1)", "(f2)", "(g)"} {
		if !got[c] {
			t.Errorf("%s fails:\n%s", c, strings.Join(rep.Lines, "\n"))
		}
	}
	if !rep.Pass || len(rep.Lines) != 9 || len(rep.Samples) != 23 || len(rep.Stages) != 3 {
		t.Fatalf("pass %v, %d lines, %d samples, %d stages:\n%s", rep.Pass, len(rep.Lines), len(rep.Samples), len(rep.Stages), strings.Join(rep.Lines, "\n"))
	}
	// Each sample is the UI part plus the propagation part.
	if s := rep.Samples[0]; s.UIMS != 60 || s.PropagationMS != 9 || s.SampleMS != 69 {
		t.Errorf("sample %+v, want 60 + 9 ms", s)
	}
	for _, want := range []string{"23 of 23 UI publishes: p50 69 ms, p99 69 ms", "heavy 429 share 75.0 %", "would_deny 2123 of 4300", "heavy key 2000 would-denies, light key absent"} {
		if !strings.Contains(strings.Join(rep.Lines, "\n"), want) {
			t.Errorf("no %q in\n%s", want, strings.Join(rep.Lines, "\n"))
		}
	}
}

func TestJudgeM6Fails(t *testing.T) {
	for _, c := range []struct {
		name  string
		fails []string
		edit  func(r *m6Run)
	}{
		{"a publish takes 5 s from the click", []string{"(a)"}, func(r *m6Run) {
			r.in.Exit.Publishes[3].TResponse = r.in.Exit.Publishes[3].TClick + 4992 // + 9 ms of propagation
		}},
		{"a publish never applied on gateway-5", []string{"(a)"}, func(r *m6Run) {
			v := r.versions[104]
			v.Applied = v.Applied[:4]
			r.versions[104] = v
		}},
		{"22 publishes", []string{"(a)"}, func(r *m6Run) { r.in.Exit.Publishes = r.in.Exit.Publishes[1:] }},
		{"a 429 in shadow", []string{"(b)", "(e)"}, func(r *m6Run) {
			r.set(r.during("shadow"), func(x *probeRecord) {
				if x.heavy && x.gateway == 2 {
					x.status = 429
				}
			})
		}},
		{"a gateway never answers the light client in shadow", []string{"(b)", "(e)"}, func(r *m6Run) {
			r.set(r.during("shadow"), func(x *probeRecord) {
				if !x.heavy && x.gateway == 1 {
					x.status, x.fail = 0, "timeout"
				}
			})
		}},
		{"the light key would-denied", []string{"(b2)"}, func(r *m6Run) {
			r.in.Exit.Review.Keys = append(r.in.Exit.Review.Keys, r.in.Exit.Review.Keys[0])
			r.in.Exit.Review.Keys[1].KeyDigest, r.in.Exit.Review.Keys[1].ShadowDenied = "ld", 1
		}},
		{"no would-denies", []string{"(b2)"}, func(r *m6Run) { r.in.Exit.Review.WouldDeny = nil }},
		{"the UI showed none", []string{"(b2)"}, func(r *m6Run) { r.in.Exit.Review.UIShowed = false }},
		{"the UI showed no heavy key row", []string{"(b2)"}, func(r *m6Run) { r.in.Exit.Review.UIHeavy = nil }},
		{"the UI's heavy key row would-denied none", []string{"(b2)"}, func(r *m6Run) { zero := int64(0); r.in.Exit.Review.UIHeavy = &zero }},
		{"no review", []string{"(b2)"}, func(r *m6Run) { r.in.Exit.Review = nil }},
		{"40 % heavy 429s", []string{"(c)"}, func(r *m6Run) {
			r.set(r.during("enforce"), func(x *probeRecord) {
				if x.heavy && x.status == 429 && x.due.UnixMilli()%5 < 3 {
					x.status, x.limitOK = 200, false
				}
			})
		}},
		{"a 429 without the policy's RateLimit item", []string{"(c)"}, func(r *m6Run) {
			from, _ := r.window("enforce")
			r.set(func(x probeRecord) bool { return x.status == 429 && x.due.After(from) }, func(x *probeRecord) { x.limitOK = false })
		}},
		{"a 503 in enforce", []string{"(c)", "(e)"}, func(r *m6Run) {
			from, _ := r.window("enforce")
			r.set(func(x probeRecord) bool {
				return x.heavy && x.status == 200 && x.due.After(from) && x.due.Before(from.Add(time.Second))
			},
				func(x *probeRecord) { x.status = 503 })
		}},
		{"a light 429 after the rollback", []string{"(d)", "(e)"}, func(r *m6Run) {
			r.set(r.during("rollback"), func(x *probeRecord) {
				if !x.heavy && x.gateway == 0 {
					x.status = 429
				}
			})
		}},
		{"a heavy 429 due long after R applied, outside its window", []string{"(e)"}, func(r *m6Run) {
			_, to := r.window("rollback")
			r.set(func(x probeRecord) bool {
				return x.heavy && x.due.After(to) && x.due.Before(to.Add(30*time.Millisecond))
			},
				func(x *probeRecord) { x.status = 429 })
		}},
		{"a heavy 429 received before E's published_at", []string{"(e)"}, func(r *m6Run) {
			at := r.stage["enforce"].PublishedAt
			r.set(func(x probeRecord) bool {
				return x.heavy && x.due.Before(at) && x.due.After(at.Add(-30*time.Millisecond))
			},
				func(x *probeRecord) { x.status = 429 })
		}},
		{"a dropped request before the stages", []string{"(e)"}, func(r *m6Run) {
			r.recs[10].status, r.recs[10].fail = 0, "dropped"
		}},
		{"the load stopped inside R's window", []string{"(d)"}, func(r *m6Run) {
			_, to := r.window("rollback")
			r.stopped = to.Add(-time.Second)
		}},
		{"rollback not reached", []string{"(a)", "(d)"}, func(r *m6Run) {
			r.in.Exit.Publishes = r.in.Exit.Publishes[:22]
		}},
		{"a console error", []string{"(f1)"}, func(r *m6Run) { r.in.Guard.ConsoleErrors = []string{"boom"} }},
		{"a token in a URL", []string{"(f2)"}, func(r *m6Run) { r.in.Guard.Findings = []string{"a token in a request URL"} }},
		{"no token seen", []string{"(f2)"}, func(r *m6Run) { r.in.Guard.SecretsSeen = 0 }},
		{"no guard report", []string{"(f1)", "(f2)"}, func(r *m6Run) { r.in.Guard = nil }},
		{"the spec failed", []string{"(g)"}, func(r *m6Run) { r.in.SpecCode = 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newM6Run(t)
			c.edit(r)
			rep, got := r.judge()
			if rep.Pass {
				t.Errorf("passes:\n%s", strings.Join(rep.Lines, "\n"))
			}
			for check, pass := range got {
				want := true
				for _, f := range c.fails {
					want = want && f != check
				}
				if pass != want {
					t.Errorf("%s: pass %v, want %v:\n%s", check, pass, want, strings.Join(rep.Lines, "\n"))
				}
			}
		})
	}
}

func TestJudgeM6Nothing(t *testing.T) {
	o, _ := parseM6Flags([]string{"probe"}, io.Discard)
	rep := m6Report{}
	judgeM6(&rep, o, m6Inputs{SpecCode: -1}, nil, nil, time.Now())
	if rep.Pass || len(rep.Rules) != 9 {
		t.Fatalf("pass %v, %d rules", rep.Pass, len(rep.Rules))
	}
	for _, x := range rep.Rules {
		if x.Pass {
			t.Errorf("%s passes with nothing: %s", x.Check, x.Measured)
		}
	}
}

func m6Cp(t *testing.T, f *fakeCP) *cpRun {
	t.Helper()
	getenv := func(k string) string { return map[string]string{ciSecretEnv: testSecret}[k] }
	sec, _ := secretFrom(getenv)
	return newCPRun(cpOptions{controlPlane: f.srv.URL + "/api/v1", gateways: testGateways, runID: "t1", restTimeout: 5 * time.Second},
		newCPClient(f.srv.URL+"/api/v1", f.srv.URL+"/token", sec, 5*time.Second))
}

func TestM6Setup(t *testing.T) {
	f := newFakeCP(t)
	o, err := parseM6Flags([]string{"setup"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	yaml := "routes: []\n"
	for _, nothing := range []bool{false, true} {
		f.nothing = nothing // the second run: already the working set (409)
		var out, errs bytes.Buffer
		code := m6Step(t.Context(), o, m6Cp(t, f), nil, strings.NewReader(yaml), &out, &errs)
		var rep struct {
			ConfigVersion int64 `json:"config_version"`
		}
		if code != 0 || json.Unmarshal(out.Bytes(), &rep) != nil || rep.ConfigVersion != 101 || string(f.imported) != yaml {
			t.Fatalf("setup: exit %d, %s, %s", code, out.String(), errs.String())
		}
		if strings.Contains(out.String()+errs.String(), testToken) || strings.Contains(out.String()+errs.String(), testSecret) {
			t.Error("setup printed the token or the secret")
		}
	}
	if code := m6Step(t.Context(), o, m6Cp(t, f), nil, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
		t.Errorf("setup without a config: exit %d, want 2", code)
	}
}

// TestM6Probe drives fake gateways that deny every other heavy request
// with the policy's headers, until stdin ends, and reads its versions
// from the fake control plane.
func TestM6Probe(t *testing.T) {
	f := newFakeCP(t)
	var heavy, light atomic.Int64
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != m6ProbePath:
			w.WriteHeader(http.StatusNotFound)
		case r.Header.Get(m6ProbeHeader) == "heavy":
			if heavy.Add(1)%2 == 0 {
				w.Header().Set("RateLimit", `"m6-limit-t1";r=0;t=1`)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			}
		case r.Header.Get(m6ProbeHeader) == "light":
			light.Add(1)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(gw.Close)
	o, err := parseM6Flags([]string{"probe", "-heavy-rate", "100", "-light-rate", "20", "-run-id", "t1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	var stderr syncBuffer
	p := newProbe(o, func(string) string { return strings.TrimPrefix(gw.URL, "http://") })
	done := make(chan m6Report)
	go func() { done <- p.run(t.Context(), pr, &stderr, m6Cp(t, f)) }()
	time.Sleep(500 * time.Millisecond)
	exit := `{"run":"t1","base_version":100,"publishes":[{"kind":"route","version":100,"t_click":1,"t_response":31}]}`
	_, _ = io.WriteString(pw, "exit "+exit+"\nspec 1\n")
	_ = pw.Close()
	rep := <-done
	if !strings.Contains(stderr.String(), "probe started") {
		t.Errorf("stderr %q", stderr.String())
	}
	if rep.Requests < 40 || rep.Requests != int(heavy.Load()+light.Load()) {
		t.Errorf("%d requests recorded, the gateways saw %d heavy and %d light", rep.Requests, heavy.Load(), light.Load())
	}
	var ok429, other int
	for _, x := range p.recs {
		switch {
		case x.status == 429 && x.heavy && x.limitOK:
			ok429++
		case x.status != 200:
			other++
		}
		if x.recv.Before(x.due) {
			t.Errorf("received before due: %+v", x)
		}
	}
	if ok429 == 0 || other != 0 {
		t.Errorf("%d 429s with the policy's headers, %d other answers", ok429, other)
	}
	// The one publish's sample: 30 ms of UI and the fake's 3–7 ms apply delays.
	if len(rep.Samples) != 1 || rep.Samples[0].UIMS != 30 || len(rep.Samples[0].Missing) != 0 || rep.Samples[0].SampleMS < 30 || rep.Samples[0].SampleMS > 100 {
		t.Errorf("samples %+v", rep.Samples)
	}
	if rep.Pass {
		t.Error("a run with one publish and a failed spec passes")
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
