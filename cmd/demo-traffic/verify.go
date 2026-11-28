package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ankitbisht23/spillway/internal/demo"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// "demo-traffic verify guards --phase G0|G1" and "demo-traffic verify
// keys" are make verify-m8's G and K checks (SC-08, ADR-0098 §2), run in a
// one-off container of the demo-traffic service: G0 and G1 against the dev
// stack (demo mode off) before and after the demo, K on the running demo.
// g6 and g7, which run make targets and look at containers, are
// test/verify/m8.sh's. Each check prints one PASS or FAIL line on stderr,
// as "run --check" does, then one verdict line; --out writes the JSON
// report. No key, token or secret is ever printed: keys are named by ID.

// verifyTimeout bounds every HTTP call of these checks: over 12 times the
// worst stall (ADR-0098, Stall tolerance).
const verifyTimeout = 15 * time.Second

// verifier collects one part's checks.
type verifier struct {
	part     string
	out      io.Writer
	mu       sync.Mutex
	checks   []checkResult
	measured map[string]any
}

func (v *verifier) judge(id, what string, pass bool, want, measured string, args ...any) {
	c := checkResult{ID: id, What: what, Pass: pass, Want: want, Measured: fmt.Sprintf(measured, args...)}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.checks = append(v.checks, c)
	if pass {
		_, _ = fmt.Fprintf(v.out, "PASS %s %s: %s\n", c.ID, c.What, c.Measured)
	} else {
		_, _ = fmt.Fprintf(v.out, "FAIL %s %s: %s (want %s)\n", c.ID, c.What, c.Measured, c.Want)
	}
}

func (v *verifier) measure(name string, x any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.measured[name] = x
}

// verdict prints the last line and returns the exit code.
func (v *verifier) verdict() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	var failed []string
	for _, c := range v.checks {
		if !c.Pass {
			failed = append(failed, c.ID)
		}
	}
	if len(failed) == 0 && len(v.checks) > 0 {
		_, _ = fmt.Fprintf(v.out, "%s: PASS (%d checks)\n", v.part, len(v.checks))
		return 0
	}
	_, _ = fmt.Fprintf(v.out, "%s: FAIL (%d of %d checks: %s)\n", v.part, len(failed), len(v.checks), strings.Join(failed, ", "))
	return 1
}

// writeReport writes the part's checks and measured values as JSON (0644:
// no secret in it).
func (v *verifier) writeReport(path string) error {
	v.mu.Lock()
	rep := struct {
		Part     string         `json:"part"`
		Pass     bool           `json:"pass"`
		Checks   []checkResult  `json:"checks"`
		Measured map[string]any `json:"measured"`
	}{Part: v.part, Pass: len(v.checks) > 0, Checks: slices.Clone(v.checks), Measured: v.measured}
	for _, c := range rep.Checks {
		rep.Pass = rep.Pass && c.Pass
	}
	v.mu.Unlock()
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: results, no secret
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644) //nolint:gosec // G306: results, no secret
}

// runVerify is "demo-traffic verify <guards|keys>".
func runVerify(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "guards" && args[0] != "keys") {
		_, _ = fmt.Fprintln(stderr, "usage: demo-traffic verify guards --phase G0|G1 [--out file], or demo-traffic verify keys [--out file]")
		return 2
	}
	fs := flag.NewFlagSet("demo-traffic verify "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	phase := fs.String("phase", "", "guards: G0 (before the demo) or G1 (after make demo-down)")
	out := fs.String("out", "", "write the JSON report to this file")
	if code, done := parse(fs, args[1:]); done {
		return code
	}
	cp, err := cpClient(getenv, verifyTimeout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic verify:", err)
		return 2
	}
	base := strings.TrimSuffix(setting(getenv, "SPILLWAY_DEMO_CP_URL", "http://controlplane:8080"), "/") + "/api/v1"
	v := &verifier{out: stderr, measured: map[string]any{}}
	switch args[0] {
	case "guards":
		if *phase != "G0" && *phase != "G1" {
			_, _ = fmt.Fprintln(stderr, "demo-traffic verify guards: --phase must be G0 or G1")
			return 2
		}
		v.part = *phase
		tp := demo.NewToxiproxy(setting(getenv, "SPILLWAY_DEMO_TOXIPROXY_URL", "http://toxiproxy:8474"), verifyTimeout)
		verifyGuards(ctx, v, *phase, base, cp, tp)
	case "keys":
		if *phase != "" {
			_, _ = fmt.Fprintln(stderr, "demo-traffic verify keys: --phase is guards'")
			return 2
		}
		gws, err := gatewaysFrom(setting(getenv, "SPILLWAY_DEMO_GATEWAYS", "http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000"),
			setting(getenv, "SPILLWAY_DEMO_ADMIN_URLS", "http://gateway-1:8001,http://gateway-2:8001,http://gateway-3:8001"))
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-traffic verify keys:", err)
			return 2
		}
		v.part = "K"
		verifyKeys(ctx, v, cp, gws)
	}
	code := v.verdict()
	if *out != "" {
		if err := v.writeReport(*out); err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-traffic verify:", err)
			code = 1
		}
	}
	return code
}

// --- G0 and G1: demo mode off ----------------------------------------------

// guardAnswer is one answer of a guarded operation: its status and error
// code, or the transport error.
type guardAnswer struct {
	status int
	code   string
	err    string
}

func (a guardAnswer) String() string {
	switch {
	case a.err != "":
		return "no answer (" + a.err + ")"
	case a.code != "":
		return strconv.Itoa(a.status) + " " + a.code
	}
	return strconv.Itoa(a.status)
}

// errorCode is an error answer's code, "" for none.
func errorCode(body []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Code
}

// guardCall sends method path (under base, /api/v1) once without a token
// and once with the spillway-ci token.
func guardCall(ctx context.Context, hc *http.Client, cp *demo.Client, base, method, path string, body any) (anon, ci guardAnswer) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(raw)) //nolint:gosec // G704: the compose control plane is the point
	if err == nil {
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		var resp *http.Response
		if resp, err = hc.Do(req); err == nil { //nolint:gosec // G704: as above
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			anon = guardAnswer{status: resp.StatusCode, code: errorCode(b)}
		}
	}
	if err != nil {
		anon = guardAnswer{err: "transport error or timeout"}
	}
	var payload any
	if body != nil {
		payload = body
	}
	a, err := cp.Call(ctx, method, path, payload)
	if err != nil {
		ci = guardAnswer{err: err.Error()}
	} else {
		ci = guardAnswer{status: a.Status, code: errorCode(a.Body)}
	}
	return anon, ci
}

// guardProblems lists the answers that are not 404 (with code not_found when
// wantCode).
func guardProblems(wantCode bool, as map[string]guardAnswer) []string {
	var bad []string
	keys := make([]string, 0, len(as))
	for k := range as {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		a := as[k]
		if a.status != http.StatusNotFound || (wantCode && a.code != "not_found") {
			bad = append(bad, k+": "+a.String())
		}
	}
	return bad
}

// guardMeasured is the answers as "no token 404 not_found, spillway-ci 404
// not_found" (sorted by label).
func guardMeasured(as map[string]guardAnswer) string {
	keys := make([]string, 0, len(as))
	for k := range as {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+as[k].String())
	}
	return strings.Join(parts, ", ")
}

// verifyGuards runs g1 – g5 of phase G0 or G1 (ADR-0098 §2).
func verifyGuards(ctx context.Context, v *verifier, phase, base string, cp *demo.Client, tp *demo.Toxiproxy) {
	hc := &http.Client{Timeout: verifyTimeout}
	defer hc.CloseIdleConnections()
	run := uuid.NewString()
	custom := map[string]any{"kind": "custom", "tenant": "pro-co", "route": "catalog", "rate": 10, "duration_s": 5}
	steps := []struct {
		id, what     string
		wantCode     bool
		method, path string
		body         any
	}{
		{"g1", "GET /api/v1/demo answers 404 not_found", true, http.MethodGet, "/demo", nil},
		{"g2", "POST /api/v1/demo/playground/runs answers 404", false, http.MethodPost, "/demo/playground/runs", custom},
		{"g3", "DELETE /api/v1/demo/playground/runs/{id} answers 404", false, http.MethodDelete, "/demo/playground/runs/" + run, nil},
		{"g4", "GET /api/v1/demo/playground/runs/{id}/stream answers 404", false, http.MethodGet, "/demo/playground/runs/" + run + "/stream", nil},
	}
	for _, s := range steps {
		anon, ci := guardCall(ctx, hc, cp, base, s.method, s.path, s.body)
		as := map[string]guardAnswer{"no token": anon, "spillway-ci": ci}
		bad := guardProblems(s.wantCode, as)
		want := "404 with and without a token"
		if s.wantCode {
			want = "404 not_found with and without a token"
		}
		v.judge(phase+"."+s.id, s.what, len(bad) == 0, want, "%s", guardMeasured(as))
	}

	// g5: both fault switches answer 404, and toxiproxy carries no demo
	// toxic (G1: and no demo proxy, which make demo-down removed).
	as := map[string]guardAnswer{}
	for _, f := range []string{"redis-cut", "upstream-slow"} {
		anon, ci := guardCall(ctx, hc, cp, base, http.MethodPut, "/demo/faults/"+f, map[string]any{"active": true})
		as[f+" no token"], as[f+" spillway-ci"] = anon, ci
	}
	bad := guardProblems(false, as)
	toxics, err := tp.ProxyToxics(ctx)
	var demoToxics, demoProxies []string
	if err != nil {
		bad = append(bad, "toxiproxy: "+err.Error())
	}
	for proxy, names := range toxics {
		for _, n := range names {
			if strings.HasPrefix(n, demo.DemoPrefix) {
				demoToxics = append(demoToxics, proxy+"/"+n)
			}
		}
		if strings.HasPrefix(proxy, demo.DemoPrefix) {
			demoProxies = append(demoProxies, proxy)
		}
	}
	slices.Sort(demoToxics)
	slices.Sort(demoProxies)
	if len(demoToxics) > 0 {
		bad = append(bad, "demo toxics "+strings.Join(demoToxics, " "))
	}
	what, want := "fault switches answer 404, no demo toxic in toxiproxy", "404 for both switches with and without a token; no demo-* toxic"
	measured := guardMeasured(as) + "; demo toxics: none"
	if len(demoToxics) > 0 {
		measured = guardMeasured(as) + "; demo toxics: " + strings.Join(demoToxics, " ")
	}
	if phase == "G1" {
		what, want = "fault switches answer 404, no demo toxic or proxy in toxiproxy", want+" and no demo-* proxy"
		if len(demoProxies) > 0 {
			bad = append(bad, "demo proxies "+strings.Join(demoProxies, " "))
			measured += "; demo proxies: " + strings.Join(demoProxies, " ")
		} else {
			measured += "; demo proxies: none"
		}
	}
	if err != nil {
		measured += "; toxiproxy: " + err.Error()
	}
	v.judge(phase+".g5", what, len(bad) == 0, want, "%s", measured)
}

// --- K: demo keys ----------------------------------------------------------

// seededTenant is a tenant of deploy/demo/seed.yaml with its plan's
// consumer-plan limit from deploy/demo/config.yaml (TestSeededTenants
// checks both files).
type seededTenant struct {
	plan        string
	rate, burst int64
	period      time.Duration
}

var seededTenants = map[string]seededTenant{
	"free-co":       {"free", 10, 20, time.Second},
	"pro-co":        {"pro", 100, 200, time.Second},
	"enterprise-co": {"enterprise", 1000, 1000, time.Second},
}

// keysConsumer is the consumer K issues its keys for (ADR-0098 k1).
const keysConsumer = "pro-co-mobile"

// issued is an issueKey answer, the key itself as a secret.
type issued struct {
	key       *apiKey
	createdAt time.Time
	expiresAt *time.Time
}

// issue sends issueKey for consumer id with body and returns the answer,
// with the key when it is a 201.
func issue(ctx context.Context, cp *demo.Client, consumer string, id int64, body map[string]any) (demo.Answer, *issued, error) {
	a, err := cp.Call(ctx, http.MethodPost, "/consumers/"+strconv.FormatInt(id, 10)+"/keys", body)
	if err != nil || a.Status != http.StatusCreated {
		return a, nil, err
	}
	var out struct {
		ID        int64      `json:"id"`
		Key       string     `json:"key"`
		CreatedAt time.Time  `json:"created_at"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(a.Body, &out); err != nil {
		return a, nil, fmt.Errorf("issueKey: %w", err)
	}
	k := &apiKey{id: out.ID, consumerID: id, consumer: consumer, secret: secret.New(out.Key)}
	if out.ExpiresAt != nil {
		k.expires = *out.ExpiresAt
	}
	return a, &issued{key: k, createdAt: out.CreatedAt, expiresAt: out.ExpiresAt}, nil
}

// namesField reports whether an error answer names field.
func namesField(body []byte, field string) bool {
	var e struct {
		Error   string `json:"error"`
		Details []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	for _, d := range e.Details {
		if d.Path == field || strings.Contains(d.Message, field) {
			return true
		}
	}
	return strings.Contains(e.Error, field)
}

// keyProbe is one k3 probe: when it was sent and answered, and its status
// (0: no answer).
type keyProbe struct {
	sent, done time.Time
	status     int
}

// judgeExpiry is k3's rule for one replica: every probe answered before
// expires − 1 s is 2xx or 429, every probe sent after expires + 2 s is 401,
// and there is at least one of each. It returns the counts and the
// problems.
func judgeExpiry(ps []keyProbe, expires time.Time) (before, after int, bad []string) {
	validUntil, goneFrom := expires.Add(-time.Second), expires.Add(2*time.Second)
	for _, p := range ps {
		switch {
		case p.status != 0 && p.done.Before(validUntil):
			before++
			if (p.status < 200 || p.status >= 300) && p.status != http.StatusTooManyRequests {
				bad = append(bad, fmt.Sprintf("%d %.1f s before expiry", p.status, expires.Sub(p.done).Seconds()))
			}
		case p.status == 0 && p.sent.Before(validUntil):
			before++
			bad = append(bad, fmt.Sprintf("no answer to a probe sent %.1f s before expiry", expires.Sub(p.sent).Seconds()))
		case p.sent.After(goneFrom):
			after++
			if p.status != http.StatusUnauthorized {
				st := strconv.Itoa(p.status)
				if p.status == 0 {
					st = "no answer"
				}
				bad = append(bad, fmt.Sprintf("%s %.1f s after expiry", st, p.sent.Sub(expires).Seconds()))
			}
		}
	}
	if before == 0 {
		bad = append(bad, "no probe answered before expires_at − 1 s")
	}
	if after == 0 {
		bad = append(bad, "no probe sent after expires_at + 2 s")
	}
	return before, after, bad
}

// keyExpiry is k3's timing (ADR-0098 k3): the key's lifetime, how long the
// probes run on after its expiry, and the probe interval per replica.
var keyExpiry = struct{ lifetime, tail, every time.Duration }{15 * time.Second, 5 * time.Second, 100 * time.Millisecond}

// verifyKeys runs k1 – k4 on the running demo (ADR-0098 §2).
func verifyKeys(ctx context.Context, v *verifier, cp *demo.Client, gws []gateway) {
	kr := newKeyring(cp)
	id, err := kr.consumerID(ctx, keysConsumer)
	if err != nil {
		for _, c := range []checkDef{{"k1", "a demo key without expires_at expires in 30 min"}, {"k2", "expires_at beyond 30 min is refused"}, {"k3", "a 15 s key works until it expires, then 401 on every replica"}} {
			v.judge(c.id, c.what, false, "a run", "setup: %v", err)
		}
		verifyDemoState(ctx, v, cp)
		return
	}

	// k1: no expires_at → 30 minutes.
	a, k, err := issue(ctx, cp, keysConsumer, id, map[string]any{})
	switch {
	case err != nil:
		v.judge("k1", "a demo key without expires_at expires in 30 min", false, "201, expires_at − created_at = 30 min ± 1 s", "%v", err)
	case k == nil:
		v.judge("k1", "a demo key without expires_at expires in 30 min", false, "201, expires_at − created_at = 30 min ± 1 s", "status %d: %s", a.Status, demo.ErrorMessage(a.Body))
	case k.expiresAt == nil:
		v.judge("k1", "a demo key without expires_at expires in 30 min", false, "201, expires_at − created_at = 30 min ± 1 s", "201 %s with no expires_at", k.key)
	default:
		d := k.expiresAt.Sub(k.createdAt)
		v.measure("k1_lifetime_s", d.Seconds())
		off := d - 30*time.Minute
		v.judge("k1", "a demo key without expires_at expires in 30 min", off >= -time.Second && off <= time.Second, "201, expires_at − created_at = 30 min ± 1 s",
			"201 %s, expires_at − created_at = %s", k.key, d.Round(time.Millisecond))
	}

	// k2: 31 minutes → 422 validation_failed naming expires_at.
	a, k, err = issue(ctx, cp, keysConsumer, id, map[string]any{"expires_at": time.Now().Add(31 * time.Minute).UTC().Format(time.RFC3339)})
	switch {
	case err != nil:
		v.judge("k2", "expires_at beyond 30 min is refused", false, "422 validation_failed naming expires_at", "%v", err)
	case k != nil:
		v.judge("k2", "expires_at beyond 30 min is refused", false, "422 validation_failed naming expires_at", "201 %s", k.key)
	default:
		code, named := errorCode(a.Body), namesField(a.Body, "expires_at")
		v.judge("k2", "expires_at beyond 30 min is refused", a.Status == http.StatusUnprocessableEntity && code == "validation_failed" && named,
			"422 validation_failed naming expires_at", "%d %s, names expires_at: %t", a.Status, code, named)
	}

	verifyExpiry(ctx, v, cp, gws, id)
	verifyDemoState(ctx, v, cp)
}

// verifyExpiry is k3: a key with expires_at = now + 15 s is applied on every
// replica (each probed every 100 ms until it answers non-401, at most 10 s),
// then probed on each every 100 ms until expires_at + 5 s.
func verifyExpiry(ctx context.Context, v *verifier, cp *demo.Client, gws []gateway, consumerID int64) {
	const id, what, want = "k3", "a 15 s key works until it expires, then 401 on every replica",
		"every probe answered before expires_at − 1 s 2xx or 429 and every probe sent after expires_at + 2 s 401, on every replica"
	a, k, err := issue(ctx, cp, keysConsumer, consumerID, map[string]any{"expires_at": time.Now().Add(keyExpiry.lifetime).UTC().Format(time.RFC3339Nano)})
	switch {
	case err != nil:
		v.judge(id, what, false, want, "setup: %v", err)
		return
	case k == nil:
		v.judge(id, what, false, want, "setup: issueKey status %d: %s", a.Status, demo.ErrorMessage(a.Body))
		return
	case k.expiresAt == nil:
		v.judge(id, what, false, want, "setup: %s has no expires_at", k.key)
		return
	}
	expires := *k.expiresAt
	issuedAt := time.Now()
	if err := waitApplied(ctx, gws, k.key); err != nil {
		v.judge(id, what, false, want, "setup: %v", err)
		return
	}
	applied := time.Since(issuedAt)
	v.measure("k3_applied_s", applied.Seconds())
	end := expires.Add(keyExpiry.tail)
	probes := make([][]keyProbe, len(gws))
	var wg sync.WaitGroup
	for i, g := range gws {
		wg.Go(func() {
			client := &http.Client{Timeout: verifyTimeout}
			defer client.CloseIdleConnections()
			tick := time.NewTicker(keyExpiry.every)
			defer tick.Stop()
			for time.Now().Before(end) && ctx.Err() == nil {
				p := keyProbe{sent: time.Now()}
				p.status, _ = probeOnce(ctx, client, g, k.key, "catalog")
				p.done = time.Now()
				probes[i] = append(probes[i], p)
				select {
				case <-ctx.Done():
				case <-tick.C:
				}
			}
		})
	}
	wg.Wait()
	var parts, problems []string
	for i, g := range gws {
		before, after, bad := judgeExpiry(probes[i], expires)
		parts = append(parts, fmt.Sprintf("%s %d before (2xx/429), %d after (401)", g.name, before, after))
		for _, b := range bad {
			problems = append(problems, g.name+": "+b)
		}
	}
	measured := fmt.Sprintf("%s applied on every replica in %s; %s", k.key, secs(applied), strings.Join(parts, "; "))
	if len(problems) > 0 {
		measured += "; " + strings.Join(firstN(problems, 5), "; ")
		if len(problems) > 5 {
			measured += fmt.Sprintf("; and %d more", len(problems)-5)
		}
	}
	v.judge(id, what, len(problems) == 0, want, "%s", measured)
}

// verifyDemoState is k4: getDemo answers 200 with the three seeded tenants
// and their plan limits, both faults off.
func verifyDemoState(ctx context.Context, v *verifier, cp *demo.Client) {
	const id, what, want = "k4", "getDemo answers with the seed, both faults off",
		"200 with free-co, pro-co and enterprise-co on their plans' limits; redis-cut and upstream-slow off"
	a, err := cp.Call(ctx, http.MethodGet, "/demo", nil)
	if err != nil {
		v.judge(id, what, false, want, "%v", err)
		return
	}
	if a.Status != http.StatusOK {
		v.judge(id, what, false, want, "status %d: %s", a.Status, demo.ErrorMessage(a.Body))
		return
	}
	measured, problems := judgeDemoState(a.Body)
	v.judge(id, what, len(problems) == 0, want, "%s", strings.Join(append([]string{measured}, problems...), "; "))
}

// judgeDemoState checks a getDemo body against the seed: what it shows,
// and the problems.
func judgeDemoState(body []byte) (string, []string) {
	var d struct {
		Tenants []struct {
			Name  string  `json:"name"`
			Plan  *string `json:"plan"`
			Limit *struct {
				Rate   int64  `json:"rate"`
				Burst  int64  `json:"burst"`
				Period string `json:"period"`
			} `json:"limit"`
		} `json:"tenants"`
		Faults []struct {
			Name   string `json:"name"`
			Active *bool  `json:"active"`
		} `json:"faults"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "getDemo", []string{"the answer is not a Demo: " + err.Error()}
	}
	var shown, problems []string
	seen := map[string]bool{}
	for _, t := range d.Tenants {
		s := t.Name
		want, ok := seededTenants[t.Name]
		if !ok {
			problems = append(problems, "tenant "+t.Name+" is not in the seed")
		}
		seen[t.Name] = true
		plan := "none"
		if t.Plan != nil {
			plan = *t.Plan
		}
		s += " " + plan
		if t.Limit == nil {
			s += " no limit"
		} else {
			s += fmt.Sprintf(" %d/%s burst %d", t.Limit.Rate, t.Limit.Period, t.Limit.Burst)
		}
		shown = append(shown, s)
		if !ok {
			continue
		}
		if t.Plan == nil || *t.Plan != want.plan {
			problems = append(problems, fmt.Sprintf("%s: plan %s, want %s", t.Name, plan, want.plan))
		}
		if t.Limit == nil {
			problems = append(problems, t.Name+": no limit")
			continue
		}
		p, err := time.ParseDuration(t.Limit.Period)
		if t.Limit.Rate != want.rate || t.Limit.Burst != want.burst || err != nil || p != want.period {
			problems = append(problems, fmt.Sprintf("%s: %d/%s burst %d, want %d/%s burst %d", t.Name, t.Limit.Rate, t.Limit.Period, t.Limit.Burst, want.rate, want.period, want.burst))
		}
	}
	for _, name := range sortedKeys(seededTenants) {
		if !seen[name] {
			problems = append(problems, "tenant "+name+" missing")
		}
	}
	faults := map[string]string{}
	for _, f := range d.Faults {
		st := "unknown"
		if f.Active != nil {
			st = map[bool]string{true: "on", false: "off"}[*f.Active]
		}
		faults[f.Name] = st
	}
	for _, f := range []string{"redis-cut", "upstream-slow"} {
		st, ok := faults[f]
		if !ok {
			st = "missing"
		}
		shown = append(shown, f+" "+st)
		if st != "off" {
			problems = append(problems, fmt.Sprintf("fault %s %s, want off", f, st))
		}
	}
	return "200: " + strings.Join(shown, ", "), problems
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func firstN(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}
