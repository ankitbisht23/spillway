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
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ankitbisht23/spillway/internal/demo"
)

// TestSeededTenants: k4's expected tenants are deploy/demo/seed.yaml's,
// each with its plan's consumer-plan limit from deploy/demo/config.yaml.
func TestSeededTenants(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/demo/seed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := demo.ParseSeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("../../deploy/demo/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Plans []struct {
			Name          string `yaml:"name"`
			DefaultLimits []struct {
				Policy string `yaml:"policy"`
				Limits []struct {
					Rate   int64  `yaml:"rate"`
					Period string `yaml:"period"`
					Burst  int64  `yaml:"burst"`
				} `yaml:"limits"`
			} `yaml:"default_limits"`
		} `yaml:"plans"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(seed.Tenants) != len(seededTenants) {
		t.Fatalf("the seed has %d tenants, k4 expects %d", len(seed.Tenants), len(seededTenants))
	}
	for _, tn := range seed.Tenants {
		want, ok := seededTenants[tn.Name]
		if !ok || want.plan != tn.Plan {
			t.Errorf("seed tenant %s on plan %s; k4 expects %+v", tn.Name, tn.Plan, want)
			continue
		}
		found := false
		for _, p := range cfg.Plans {
			if p.Name != tn.Plan {
				continue
			}
			for _, dl := range p.DefaultLimits {
				if dl.Policy != "consumer-plan" || len(dl.Limits) == 0 {
					continue
				}
				l := dl.Limits[0]
				period, _ := time.ParseDuration(l.Period)
				found = l.Rate == want.rate && l.Burst == want.burst && period == want.period
			}
		}
		if !found {
			t.Errorf("plan %s's consumer-plan limit is not k4's %+v", tn.Plan, want)
		}
	}
}

// TestJudgeExpiry: k3's rule per replica.
func TestJudgeExpiry(t *testing.T) {
	exp := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return exp.Add(time.Duration(s * float64(time.Second))) }
	p := func(sent, done float64, status int) keyProbe {
		return keyProbe{sent: at(sent), done: at(done), status: status}
	}
	good := []keyProbe{p(-5, -4.99, 200), p(-2, -1.99, 429), p(-0.5, -0.49, 200), p(0.5, 0.51, 401), p(1.5, 1.6, 200), p(2.1, 2.11, 401), p(4, 4.01, 401)}
	for _, tc := range []struct {
		name          string
		ps            []keyProbe
		before, after int
		bad           string
	}{
		{"good: the margins are not judged", good, 2, 2, ""},
		{"a 401 while valid", append([]keyProbe{p(-3, -2.99, 401)}, good...), 3, 2, "401 3.0 s before expiry"},
		{"a 200 after the margin", append([]keyProbe{p(2.5, 2.51, 200)}, good...), 2, 3, "200 2.5 s after expiry"},
		{"a probe sent before but answered in the margin is not judged", append([]keyProbe{p(-1.2, -0.8, 503)}, good...), 2, 2, ""},
		{"no answer while valid", append([]keyProbe{p(-3, -2.99, 0)}, good...), 3, 2, "no answer to a probe sent 3.0 s before expiry"},
		{"no answer after", append([]keyProbe{p(3, 3.01, 0)}, good...), 2, 3, "no answer 3.0 s after expiry"},
		{"nothing after", good[:3], 2, 0, "no probe sent after expires_at + 2 s"},
		{"nothing before", good[3:], 0, 2, "no probe answered before expires_at − 1 s"},
	} {
		before, after, bad := judgeExpiry(tc.ps, exp)
		if before != tc.before || after != tc.after {
			t.Errorf("%s: %d before, %d after; want %d, %d", tc.name, before, after, tc.before, tc.after)
		}
		got := strings.Join(bad, "; ")
		if (tc.bad == "") != (got == "") || !strings.Contains(got, tc.bad) {
			t.Errorf("%s: problems %q, want %q", tc.name, got, tc.bad)
		}
	}
}

// demoBody is a getDemo answer with the seed's tenants and both faults.
func demoBody(mod func(map[string]any)) []byte {
	d := map[string]any{
		"tenants": []map[string]any{
			{"name": "free-co", "plan": "free", "limit": map[string]any{"rate": 10, "burst": 20, "period": "1s"}},
			{"name": "pro-co", "plan": "pro", "limit": map[string]any{"rate": 100, "burst": 200, "period": "1s"}},
			{"name": "enterprise-co", "plan": "enterprise", "limit": map[string]any{"rate": 1000, "burst": 1000, "period": "1s"}},
		},
		"routes": []any{}, "scenarios": []string{"d1", "d2", "d3"}, "client_ip": "172.18.0.1",
		"faults":     []map[string]any{{"name": "redis-cut", "active": false}, {"name": "upstream-slow", "active": false}},
		"playground": map[string]any{"max_rate": 200, "max_duration_s": 60, "active_runs": 0, "runs_left_this_hour": 10},
	}
	if mod != nil {
		mod(d)
	}
	b, _ := json.Marshal(d)
	return b
}

// TestJudgeDemoState: k4's rule.
func TestJudgeDemoState(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(map[string]any)
		bad  string
	}{
		{"the seed", nil, ""},
		{"a tenant missing", func(d map[string]any) { d["tenants"] = d["tenants"].([]map[string]any)[:2] }, "tenant enterprise-co missing"},
		{"an extra tenant", func(d map[string]any) {
			d["tenants"] = append(d["tenants"].([]map[string]any), map[string]any{"name": "acme", "plan": nil, "limit": nil})
		}, "tenant acme is not in the seed"},
		{"another limit", func(d map[string]any) {
			d["tenants"].([]map[string]any)[1]["limit"] = map[string]any{"rate": 50, "burst": 200, "period": "1s"}
		}, "pro-co: 50/1s burst 200, want 100/1s burst 200"},
		{"another plan", func(d map[string]any) { d["tenants"].([]map[string]any)[0]["plan"] = "pro" }, "free-co: plan pro, want free"},
		{"a fault on", func(d map[string]any) {
			d["faults"] = []map[string]any{{"name": "redis-cut", "active": true}, {"name": "upstream-slow", "active": false}}
		}, "fault redis-cut on, want off"},
		{"a fault unknown", func(d map[string]any) {
			d["faults"] = []map[string]any{{"name": "redis-cut", "active": false}, {"name": "upstream-slow", "active": nil}}
		}, "fault upstream-slow unknown, want off"},
	} {
		shown, bad := judgeDemoState(demoBody(tc.mod))
		got := strings.Join(bad, "; ")
		if (tc.bad == "") != (got == "") || !strings.Contains(got, tc.bad) {
			t.Errorf("%s: problems %q, want %q (shown %s)", tc.name, got, tc.bad, shown)
		}
	}
	if shown, _ := judgeDemoState(demoBody(nil)); shown != "200: free-co free 10/1s burst 20, pro-co pro 100/1s burst 200, enterprise-co enterprise 1000/1s burst 1000, redis-cut off, upstream-slow off" {
		t.Errorf("shown %q", shown)
	}
}

// fakeVerify is a token endpoint, a control plane, toxiproxy and three
// gateways for "verify": demo mode on or off, demo keys with the
// 30-minute rule, gateways that check a key's expiry on their clock.
type fakeVerify struct {
	mu       sync.Mutex
	demoMode bool
	leak     bool                 // the guard is broken: the demo routes answer 200 with a token
	proxies  string               // toxiproxy's GET /proxies
	keys     map[string]time.Time // key → expiry
	n        int64
	issued   []string
	cp, tp   *httptest.Server
	gws      []*httptest.Server
}

func newFakeVerify(t *testing.T, demoMode bool) *fakeVerify {
	f := &fakeVerify{demoMode: demoMode, keys: map[string]time.Time{}, proxies: `{"redis-0": {"toxics": []}}`}
	f.cp = httptest.NewServer(http.HandlerFunc(f.serveCP))
	f.tp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = w.Write([]byte(f.proxies))
	}))
	t.Cleanup(f.cp.Close)
	t.Cleanup(f.tp.Close)
	for range 3 {
		g := httptest.NewServer(http.HandlerFunc(f.serveGateway))
		t.Cleanup(g.Close)
		f.gws = append(f.gws, g)
	}
	return f
}

func (f *fakeVerify) env() func(string) string {
	var gws, admins []string
	for _, g := range f.gws {
		gws, admins = append(gws, g.URL), append(admins, g.URL)
	}
	return env(map[string]string{
		"SPILLWAY_DEMO_CP_URL": f.cp.URL, "SPILLWAY_DEMO_TOKEN_URL": f.cp.URL + "/token", "SPILLWAY_DEMO_CLIENT_SECRET": fakeSecret,
		"SPILLWAY_DEMO_TOXIPROXY_URL": f.tp.URL, "SPILLWAY_DEMO_GATEWAYS": strings.Join(gws, ","), "SPILLWAY_DEMO_ADMIN_URLS": strings.Join(admins, ","),
	})
}

func writeError(w http.ResponseWriter, status int, code, msg string, details ...map[string]string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "code": code, "request_id": "r", "details": details})
}

func (f *fakeVerify) serveCP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fakeToken})
		return
	}
	authed := r.Header.Get("Authorization") == "Bearer "+fakeToken
	if strings.HasPrefix(r.URL.Path, "/api/v1/demo") && !f.demoMode && (!f.leak || !authed) {
		writeError(w, 404, "not_found", "not found")
		return
	}
	if !authed {
		writeError(w, 401, "unauthenticated", "no token")
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/demo":
		_, _ = w.Write(demoBody(nil))
	case strings.HasPrefix(r.URL.Path, "/api/v1/demo"):
		_, _ = w.Write([]byte(`{"active": true}`))
	case r.Method == "GET" && r.URL.Path == "/api/v1/consumers":
		var items []map[string]any
		if r.URL.Query().Get("name") == keysConsumer {
			items = append(items, map[string]any{"id": 5, "name": keysConsumer})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == "POST" && r.URL.Path == "/api/v1/consumers/5/keys":
		var body struct {
			ExpiresAt *time.Time `json:"expires_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		now := time.Now().UTC()
		exp := now.Add(30 * time.Minute)
		if body.ExpiresAt != nil {
			if body.ExpiresAt.After(now.Add(30 * time.Minute)) {
				writeError(w, 422, "validation_failed", "the key breaks its rules", map[string]string{"path": "expires_at", "message": "at most 30 minutes ahead in demo mode"})
				return
			}
			exp = *body.ExpiresAt
		}
		f.n++
		key := fmt.Sprintf("spw_test%04d_%s", f.n, strings.Repeat("v", 43))
		f.keys[key] = exp
		f.issued = append(f.issued, key)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 700 + f.n, "consumer_id": 5, "prefix": "test", "key": key, "version": 1,
			"created_at": now, "updated_at": now, "expires_at": exp})
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeVerify) serveGateway(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	exp, ok := f.keys[r.Header.Get(apiKeyHeader)]
	f.mu.Unlock()
	if !ok || !time.Now().Before(exp) {
		w.WriteHeader(401)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// noSecret fails when s holds an issued key, the token or the secret.
func (f *fakeVerify) noSecret(t *testing.T, what, s string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range append(append([]string{}, f.issued...), fakeToken, fakeSecret, "Bearer", "eyJ", "spw_") {
		if strings.Contains(s, k) {
			t.Errorf("%s holds a key, token or secret", what)
		}
	}
}

// checkLines returns the PASS/FAIL lines of out by check ID.
func checkLines(out string) map[string]string {
	m := map[string]string{}
	for l := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && (f[0] == "PASS" || f[0] == "FAIL") {
			m[f[1]] = l
		}
	}
	return m
}

// TestVerifyGuards: G0 and G1 pass on a control plane with demo mode off
// and a toxiproxy without demo toxics; a broken guard, a demo toxic left
// on or (G1) a demo proxy left fails the check it breaks.
func TestVerifyGuards(t *testing.T) {
	for _, tc := range []struct {
		name, phase, proxies string
		demoMode, leak       bool
		fail                 []string
		code                 int
	}{
		{"G0 off", "G0", `{"redis-0": {"toxics": []}, "demo-redis-0": {"toxics": []}}`, false, false, nil, 0},
		{"G1 off", "G1", `{"redis-0": {"toxics": []}}`, false, false, nil, 0},
		{"G1 with a demo proxy left", "G1", `{"demo-redis-0": {"toxics": []}}`, false, false, []string{"G1.g5"}, 1},
		{"G0 with a demo toxic left", "G0", `{"demo-redis-0": {"toxics": [{"name": "demo-cut-up"}]}}`, false, false, []string{"G0.g5"}, 1},
		{"demo mode on", "G0", `{}`, true, false, []string{"G0.g1", "G0.g2", "G0.g3", "G0.g4", "G0.g5"}, 1},
		{"the guard answers a token", "G1", `{}`, false, true, []string{"G1.g1", "G1.g2", "G1.g3", "G1.g4", "G1.g5"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVerify(t, tc.demoMode)
			f.proxies, f.leak = tc.proxies, tc.leak
			report := filepath.Join(t.TempDir(), "g.json")
			var stderr bytes.Buffer
			code := run(context.Background(), []string{"verify", "guards", "--phase", tc.phase, "--out", report}, f.env(), &bytes.Buffer{}, &stderr)
			if code != tc.code {
				t.Errorf("exit %d, want %d:\n%s", code, tc.code, stderr.String())
			}
			lines := checkLines(stderr.String())
			if len(lines) != 5 {
				t.Errorf("%d check lines, want 5:\n%s", len(lines), stderr.String())
			}
			for id, l := range lines {
				if !strings.HasPrefix(id, tc.phase+".g") {
					t.Errorf("check ID %s, want %s.g1 … g5", id, tc.phase)
				}
				failed := strings.HasPrefix(l, "FAIL ")
				want := false
				for _, x := range tc.fail {
					want = want || x == id
				}
				if failed != want {
					t.Errorf("%s: %q", id, l)
				}
			}
			var rep struct {
				Part   string        `json:"part"`
				Pass   bool          `json:"pass"`
				Checks []checkResult `json:"checks"`
			}
			b, err := os.ReadFile(report) //nolint:gosec // G304: the test's own temp file
			if err != nil || json.Unmarshal(b, &rep) != nil || rep.Part != tc.phase || rep.Pass != (tc.code == 0) || len(rep.Checks) != 5 {
				t.Errorf("report %s (%v)", b, err)
			}
			f.noSecret(t, "output", stderr.String())
			f.noSecret(t, "report", string(b))
		})
	}
}

// TestVerifyKeys: K passes against a control plane with the 30-minute rule
// and gateways that check expiry on their clock; no output or report holds
// a key, the token or the secret.
func TestVerifyKeys(t *testing.T) {
	old := keyExpiry
	keyExpiry.lifetime, keyExpiry.tail, keyExpiry.every = 2500*time.Millisecond, 2500*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { keyExpiry = old })
	f := newFakeVerify(t, true)
	report := filepath.Join(t.TempDir(), "k.json")
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"verify", "keys", "--out", report}, f.env(), &bytes.Buffer{}, &stderr)
	out := stderr.String()
	if code != 0 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	lines := checkLines(out)
	for _, id := range []string{"k1", "k2", "k3", "k4"} {
		if !strings.HasPrefix(lines[id], "PASS ") {
			t.Errorf("%s: %q", id, lines[id])
		}
	}
	if !strings.Contains(lines["k1"], "expires_at − created_at = 30m0s") || !strings.Contains(lines["k3"], "key #702") {
		t.Errorf("k1 or k3 without its measured values:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "K: PASS (4 checks)") {
		t.Errorf("no verdict line:\n%s", out)
	}
	b, err := os.ReadFile(report) //nolint:gosec // G304: the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	f.noSecret(t, "output", out)
	f.noSecret(t, "report", string(b))
}

// TestVerifyKeysExpiryIgnored: a gateway that keeps accepting an expired
// key fails k3.
func TestVerifyKeysExpiryIgnored(t *testing.T) {
	old := keyExpiry
	keyExpiry.lifetime, keyExpiry.tail, keyExpiry.every = 1500*time.Millisecond, 2500*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { keyExpiry = old })
	f := newFakeVerify(t, true)
	for _, g := range f.gws[1:2] {
		g.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			_, ok := f.keys[r.Header.Get(apiKeyHeader)]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte("ok"))
		})
	}
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"verify", "keys"}, f.env(), &bytes.Buffer{}, &stderr)
	k3 := checkLines(stderr.String())["k3"]
	if code != 1 || !strings.HasPrefix(k3, "FAIL k3") || !strings.Contains(k3, strings.TrimPrefix(f.gws[1].URL, "http://")+": 200") {
		t.Errorf("exit %d, k3 %q", code, k3)
	}
}

// TestVerifyUsage: bad arguments exit 2.
func TestVerifyUsage(t *testing.T) {
	f := newFakeVerify(t, false)
	for _, args := range [][]string{{"verify"}, {"verify", "x"}, {"verify", "guards"}, {"verify", "guards", "--phase", "G2"}, {"verify", "keys", "--phase", "G0"}, {"verify", "keys", "extra"}} {
		if code := run(context.Background(), args, f.env(), &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
