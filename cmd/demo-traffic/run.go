package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/demo"
)

// "demo-traffic run --scenario=<base|d1…d7> [--check] [--out file]
// [--block]" (SC-02, ADR-0096 §1, §2): the base load per tenant and, for
// d1…d7, the base load plus the scenario's bursts, faults or actions, with
// a line a second (or as the scenario says) and a summary per tenant at the
// end. --check turns a scenario into its verify-m8 check (ADR-0098): PASS
// and FAIL lines on stderr and a JSON report to --out. It switches off any
// fault it switched on, on every exit path.

// healthAddr is the run's health listener, loopback inside its container
// (docs/ports.md); "demo-traffic healthcheck" asks it.
const healthAddr = "127.0.0.1:8090"

// runner is one "demo-traffic run".
type runner struct {
	sc     *scenario
	e      *engine
	cp     *demo.Client
	ctx    context.Context // ends on SIGINT and SIGTERM
	stdout io.Writer
	stderr io.Writer
	check  bool
	block  bool
	runID  string
	scrape *http.Client // admin-port scrapes

	outMu    sync.Mutex
	stopOnce sync.Once
	stopped  chan struct{}

	mu       sync.Mutex
	checks   []checkResult
	measured map[string]any
	faults   map[string]bool // switched on by this run and not yet off
}

// checkResult is one verify-m8 check (ADR-0098 §1).
type checkResult struct {
	ID       string `json:"id"`
	What     string `json:"what"`
	Pass     bool   `json:"pass"`
	Measured string `json:"measured"`
	Want     string `json:"want"`
}

// say prints a viewer's line on stdout, with the time since time 0.
func (r *runner) say(format string, args ...any) {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	at := ""
	if !r.e.t0.IsZero() {
		at = fmt.Sprintf("%4.0fs  ", max(0, r.e.since().Seconds()))
	}
	_, _ = fmt.Fprintf(r.stdout, at+format+"\n", args...)
}

// judge records a check and prints its PASS or FAIL line on stderr.
func (r *runner) judge(id, what string, pass bool, want, measured string, args ...any) {
	c := checkResult{ID: id, What: what, Pass: pass, Want: want, Measured: fmt.Sprintf(measured, args...)}
	r.mu.Lock()
	r.checks = append(r.checks, c)
	r.mu.Unlock()
	r.outMu.Lock()
	defer r.outMu.Unlock()
	if pass {
		_, _ = fmt.Fprintf(r.stderr, "PASS %s %s: %s\n", c.ID, c.What, c.Measured)
	} else {
		_, _ = fmt.Fprintf(r.stderr, "FAIL %s %s: %s (want %s)\n", c.ID, c.What, c.Measured, c.Want)
	}
}

// measure records a number for the JSON report.
func (r *runner) measure(name string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.measured[name] = v
}

// setFault switches a demo fault through setDemoFault (SC-03, ADR-0095 §5)
// and remembers what it switched on, so that the run switches it off on
// every exit path.
func (r *runner) setFault(ctx context.Context, name string, on bool) error {
	state := "off"
	if on {
		state = "on"
	}
	var out struct {
		Active *bool `json:"active"`
	}
	if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodPut, "/demo/faults/"+name, map[string]any{"active": on, "note": "demo-traffic run --scenario=" + r.sc.name + " (" + state + ")"}, &out); err != nil {
		return err
	}
	if out.Active == nil || *out.Active != on {
		return fmt.Errorf("setDemoFault %s %s: the control plane answered another state", name, state)
	}
	r.mu.Lock()
	r.faults[name] = on
	r.mu.Unlock()
	return nil
}

// clearFaults switches off every fault the run switched on, with its own
// time limit (the run's context may have ended).
func (r *runner) clearFaults() {
	r.mu.Lock()
	var on []string
	for n, v := range r.faults {
		if v {
			on = append(on, n)
		}
	}
	r.mu.Unlock()
	slices.Sort(on)
	for _, n := range on {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := r.setFault(ctx, n, false)
		cancel()
		if err != nil {
			_, _ = fmt.Fprintf(r.stderr, "demo-traffic run: could not switch %s off: %v (make demo-fault-clear does)\n", n, err)
			continue
		}
		r.say("%s: off", n)
	}
}

// stopAll ends every stream's schedule and waits for the last answers.
func (r *runner) stopAll() {
	r.stopOnce.Do(func() {
		r.e.mu.Lock()
		ss := slices.Clone(r.e.streams)
		r.e.mu.Unlock()
		for _, s := range ss {
			if s.stop != nil {
				s.stop()
			}
		}
		for _, s := range ss {
			s.wait()
		}
		close(r.stopped)
	})
}

// streamsOf returns the streams of consumer on route (every replica's).
func (r *runner) streamsOf(consumer, route string) []*stream {
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	var out []*stream
	for _, s := range r.e.streams {
		if s.spec.consumer == consumer && (route == "" || s.spec.route == route) {
			out = append(out, s)
		}
	}
	return out
}

// answersOf returns the answers of consumer on route ("" for every route),
// every replica's.
func (r *runner) answersOf(consumer, route string) []answer {
	var out []answer
	for _, s := range r.streamsOf(consumer, route) {
		as, _, _ := s.snapshot()
		out = append(out, as...)
	}
	return out
}

// live takes and merges the live tallies of consumer's streams on route.
func (r *runner) live(consumer, route string) *tally {
	t := newTally()
	for _, s := range r.streamsOf(consumer, route) {
		t.merge(s.takeLive())
	}
	return t
}

func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// lookupScenario returns the named scenario with fresh state, nil if none
// (a variable so that tests can shorten a scenario).
var lookupScenario = func(name string) *scenario { return scenarios()[name] }

// runRun is "demo-traffic run".
func runRun(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("scenario", "base", "base, or a scenario d1 … d7 (base load plus the scenario)")
	check := fs.Bool("check", false, "run the scenario as its verify-m8 check: PASS/FAIL lines on stderr (ADR-0098)")
	out := fs.String("out", "", "with -check: write the JSON report to this file")
	block := fs.Bool("block", false, "d7: block the leaked key once heavy hitters show it, and time the block per replica")
	duration := fs.Duration("duration", 0, "base: stop after this long (default: until SIGINT or SIGTERM)")
	health := fs.String("health", healthAddr, "the health listener's address; empty for none")
	timeout := fs.Duration("timeout", 10*time.Second, "per-call timeout of the REST API and Keycloak")
	if code, done := parse(fs, args); done {
		return code
	}
	sc := lookupScenario(*name)
	switch {
	case sc == nil:
		_, _ = fmt.Fprintf(stderr, "demo-traffic run: unknown scenario %q (want base or d1 … d7)\n", *name)
		return 2
	case *check && sc.name == "base":
		_, _ = fmt.Fprintln(stderr, "demo-traffic run: -check needs a scenario d1 … d7")
		return 2
	case *out != "" && !*check:
		_, _ = fmt.Fprintln(stderr, "demo-traffic run: -out needs -check")
		return 2
	case *block && sc.name != "d7":
		_, _ = fmt.Fprintln(stderr, "demo-traffic run: -block is d7's")
		return 2
	case *duration != 0 && sc.name != "base":
		_, _ = fmt.Fprintln(stderr, "demo-traffic run: -duration is base's; every scenario has a fixed length")
		return 2
	}
	cp, err := cpClient(getenv, *timeout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic run:", err)
		return 2
	}
	gws, err := gatewaysFrom(setting(getenv, "SPILLWAY_DEMO_GATEWAYS", "http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000"),
		setting(getenv, "SPILLWAY_DEMO_ADMIN_URLS", "http://gateway-1:8001,http://gateway-2:8001,http://gateway-3:8001"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic run:", err)
		return 2
	}
	r := &runner{
		sc: sc, cp: cp, ctx: ctx, stdout: stdout, stderr: stderr, check: *check, block: *block, runID: newRunID(),
		scrape: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2}}, stopped: make(chan struct{}),
		measured: map[string]any{}, faults: map[string]bool{},
		e: &engine{gws: gws, keys: newKeyring(cp), reqCtx: ctx, keep: sc.name != "base"},
	}
	var ready atomic.Bool
	if *health != "" {
		stop, _, err := serveHealth(*health, &ready)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-traffic run: health listener:", err)
			return 1
		}
		defer stop()
	}
	code := r.run(*duration, &ready)
	if *check && *out != "" {
		if err := r.writeReport(*out); err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-traffic run:", err)
			code = 1
		}
	}
	return code
}

// cpClient is the REST API client with the service token (SEC-09: the
// secret is never printed).
func cpClient(getenv func(string) string, timeout time.Duration) (*demo.Client, error) {
	sec, err := secretSetting(getenv)
	if err != nil {
		return nil, err
	}
	return demo.NewClient(setting(getenv, "SPILLWAY_DEMO_CP_URL", "http://controlplane:8080")+"/api/v1",
		setting(getenv, "SPILLWAY_DEMO_TOKEN_URL", "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/token"),
		setting(getenv, "SPILLWAY_DEMO_CLIENT_ID", "spillway-ci"), sec, timeout), nil
}

// run runs the scenario and returns the exit code: 0, or 1 on a failure
// or a FAIL.
func (r *runner) run(duration time.Duration, ready *atomic.Bool) int {
	defer r.clearFaults()
	sc := r.sc
	length := sc.length(r.check)
	if sc.name == "base" {
		length = duration
		if length == 0 {
			length = 100 * 365 * 24 * time.Hour // until SIGINT or SIGTERM
		}
	}
	r.e.end = length
	specs := append(baseLoad(), sc.streams(r.check)...)
	if err := r.setup(specs); err != nil {
		r.setupFailed(err)
		return 1
	}
	r.e.t0 = time.Now().Add(200 * time.Millisecond)
	for _, s := range r.e.add(specs...) {
		s.start(r.ctx)
	}
	ready.Store(true)
	r.header(specs, length)

	tickDone := make(chan struct{})
	go r.ticker(tickDone)
	err := sc.run(r)
	r.stopAll()
	<-tickDone
	r.clearFaults()
	if r.ctx.Err() != nil {
		r.say("stopped")
	}
	if sc.end != nil {
		sc.end(r)
	}
	r.summary()
	if err != nil {
		if r.check {
			r.setupFailed(err)
		} else {
			_, _ = fmt.Fprintln(r.stderr, "demo-traffic run:", err)
		}
		return 1
	}
	if r.check {
		if sc.checks != nil {
			sc.checks(r)
		}
		return r.verdict()
	}
	if r.ctx.Err() != nil && sc.name != "base" {
		return 1
	}
	return 0
}

// setup checks demo mode and issues a key for every consumer of specs,
// and the scenario's own, then waits until every replica accepts them.
func (r *runner) setup(specs []streamSpec) error {
	ctx := r.ctx
	// demo mode first (ADR-0095 §1): with it off, nothing is sent.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		a, err := r.cp.Call(ctx, http.MethodGet, "/demo", nil)
		if err == nil && a.Status == http.StatusOK {
			break
		}
		if err == nil && a.Status == http.StatusNotFound {
			return errors.New("demo mode is off (GET /api/v1/demo answered 404)")
		}
		if time.Now().After(deadline) || ctx.Err() != nil || (err != nil && !demo.NotReady(err)) {
			if err == nil {
				err = fmt.Errorf("GET /api/v1/demo: status %d: %s", a.Status, demo.ErrorMessage(a.Body))
			}
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	var consumers []string
	for _, sp := range specs {
		if !slices.Contains(consumers, sp.consumer) {
			consumers = append(consumers, sp.consumer)
		}
	}
	var keys []*apiKey
	for _, c := range consumers {
		k, err := r.e.keys.newKey(ctx, c)
		if err != nil {
			return fmt.Errorf("a key for %s: %w", c, err)
		}
		keys = append(keys, k)
	}
	for _, k := range keys {
		if err := waitApplied(ctx, r.e.gws, k); err != nil {
			return err
		}
		r.e.keys.use(k)
	}
	var names []string
	for _, k := range keys {
		names = append(names, fmt.Sprintf("%s %s", k.consumer, k))
	}
	r.measure("keys", names)
	_, _ = fmt.Fprintf(r.stdout, "keys: %s\n", strings.Join(names, ", "))
	return nil
}

// setupFailed prints every check of the scenario not judged yet as a
// setup FAIL (ADR-0098 §1), or the error.
func (r *runner) setupFailed(err error) {
	if !r.check {
		_, _ = fmt.Fprintln(r.stderr, "demo-traffic run: FAIL:", err)
		return
	}
	r.mu.Lock()
	judged := map[string]bool{}
	for _, c := range r.checks {
		judged[c.ID] = true
	}
	r.mu.Unlock()
	for _, d := range r.sc.ids {
		if !judged[d.id] {
			r.judge(d.id, d.what, false, "a run", "setup: %v", err)
		}
	}
}

// verdict prints the last line and returns the exit code.
func (r *runner) verdict() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var failed []string
	for _, c := range r.checks {
		if !c.Pass {
			failed = append(failed, c.ID)
		}
	}
	if len(failed) == 0 {
		_, _ = fmt.Fprintf(r.stderr, "%s: PASS (%d checks)\n", r.sc.name, len(r.checks))
		return 0
	}
	_, _ = fmt.Fprintf(r.stderr, "%s: FAIL (%d of %d checks: %s)\n", r.sc.name, len(failed), len(r.checks), strings.Join(failed, ", "))
	return 1
}

// header prints what the run sends.
func (r *runner) header(specs []streamSpec, length time.Duration) {
	var names []string
	for _, g := range r.e.gws {
		names = append(names, g.name)
	}
	total := 0.0
	for _, sp := range baseLoad() {
		total += sp.perSecond()
	}
	l := "until stopped"
	if r.sc.name != "base" || length < 50*365*24*time.Hour {
		l = fmt.Sprintf("%.0f s", length.Seconds())
	}
	mode := ""
	if r.check {
		mode = " (verify-m8 check, run " + r.runID + ")"
	}
	_, _ = fmt.Fprintf(r.stdout, "demo-traffic %s: %s%s; %s; base load %.0f rps round robin over %s\n", r.sc.name, r.sc.title, mode, l, total, strings.Join(names, ", "))
	for _, sp := range specs[len(baseLoad()):] {
		w := ""
		if sp.from > 0 || sp.to > 0 {
			to := sp.to
			if to == 0 {
				to = length
			}
			w = fmt.Sprintf(" from %.0f s to %.0f s", sp.from.Seconds(), to.Seconds())
		}
		if sp.toggle {
			w += fmt.Sprintf(" in odd %.0f s slots", sp.slot.Seconds())
		}
		if sp.replica > 0 && sp.replica <= len(r.e.gws) {
			w += " to " + r.e.gws[sp.replica-1].name + " only"
		} else {
			w += " round robin"
		}
		_, _ = fmt.Fprintf(r.stdout, "  + %s %s %g rps%s\n", sp.consumer, sp.route, sp.perSecond(), w)
	}
}

// ticker calls the scenario's tick every second until the streams stop.
func (r *runner) ticker(done chan struct{}) {
	defer close(done)
	every := r.sc.every
	if every == 0 {
		every = time.Second
	}
	for n := 1; ; n++ {
		if !r.e.sleepUntil(r.ctx, time.Duration(n)*every) {
			return
		}
		select {
		case <-r.stopped:
			return
		default:
		}
		if r.sc.tick != nil {
			r.sc.tick(r, n)
		}
	}
}

// summary prints the run's numbers per tenant and per stream.
func (r *runner) summary() {
	type row struct {
		tenant, label string
		t             *tally
		sent, sched   int64
	}
	byLabel := map[string]*row{}
	var order []string
	r.e.mu.Lock()
	streams := slices.Clone(r.e.streams)
	r.e.mu.Unlock()
	for _, s := range streams {
		_, t, res := s.snapshot()
		label := s.spec.consumer + " " + s.spec.route
		rw := byLabel[label]
		if rw == nil {
			rw = &row{tenant: tenantOf(s.spec.consumer), label: label, t: newTally()}
			byLabel[label] = rw
			order = append(order, label)
		}
		rw.t.merge(t)
		rw.sent += res.Requests.Sent
		rw.sched += res.Requests.Scheduled
	}
	sort.SliceStable(order, func(i, j int) bool { return byLabel[order[i]].tenant < byLabel[order[j]].tenant })
	tenants := map[string]*tally{}
	var tnames []string
	for _, l := range order {
		rw := byLabel[l]
		if tenants[rw.tenant] == nil {
			tenants[rw.tenant] = newTally()
			tnames = append(tnames, rw.tenant)
		}
		tenants[rw.tenant].merge(rw.t)
	}
	r.outMu.Lock()
	defer r.outMu.Unlock()
	w := r.stdout
	_, _ = fmt.Fprintf(w, "summary (%s, %.0f s):\n", r.sc.name, r.e.since().Seconds())
	_, _ = fmt.Fprintf(w, "  %-30s %8s %8s %8s %8s %8s %8s %9s %9s\n", "tenant / consumer route", "answers", "allowed", "denied", "403/401", "503", "other", "p50", "p99")
	for _, tn := range tnames {
		t := tenants[tn]
		_, _ = fmt.Fprintf(w, "  %-30s %8d %8d %8d %8d %8d %8d %9s %9s\n", tn, t.n, t.ok, t.limited, t.forbidden, t.unavailable, t.other+t.failed, ms(t.p50()), ms(t.p99()))
		for _, l := range order {
			if rw := byLabel[l]; rw.tenant == tn {
				_, _ = fmt.Fprintf(w, "    %-28s %8d %8d %8d %8d %8d %8d %9s %9s\n", l, rw.t.n, rw.t.ok, rw.t.limited, rw.t.forbidden, rw.t.unavailable, rw.t.other+rw.t.failed, ms(rw.t.p50()), ms(rw.t.p99()))
			}
		}
	}
}

// ms formats a duration in milliseconds.
func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

// report is the JSON --out writes.
type report struct {
	Scenario  string         `json:"scenario"`
	RunID     string         `json:"run_id"`
	StartedAt time.Time      `json:"started_at"`
	LengthS   float64        `json:"length_s"`
	Pass      bool           `json:"pass"`
	Checks    []checkResult  `json:"checks"`
	Streams   []streamReport `json:"streams"`
	Measured  map[string]any `json:"measured"`
}

type streamReport struct {
	Name        string  `json:"name"`
	Consumer    string  `json:"consumer"`
	Route       string  `json:"route"`
	Replica     string  `json:"replica,omitempty"`
	RPS         float64 `json:"rps"`
	Scheduled   int64   `json:"scheduled"`
	Sent        int64   `json:"sent"`
	Dropped     int64   `json:"dropped"`
	Answers     int64   `json:"answers"`
	Allowed     int64   `json:"allowed"`
	Denied      int64   `json:"denied"`
	Forbidden   int64   `json:"forbidden"`
	Unavailable int64   `json:"unavailable"`
	Other       int64   `json:"other"`
	NoAnswer    int64   `json:"no_answer"`
	P50MS       float64 `json:"p50_ms"`
	P99MS       float64 `json:"p99_ms"`
}

// writeReport writes the JSON report to path (0644: no secret in it).
func (r *runner) writeReport(path string) error {
	r.mu.Lock()
	rep := report{Scenario: r.sc.name, RunID: r.runID, StartedAt: r.e.t0.UTC(), LengthS: r.e.since().Seconds(), Pass: len(r.checks) > 0,
		Checks: slices.Clone(r.checks), Measured: r.measured}
	for _, c := range rep.Checks {
		rep.Pass = rep.Pass && c.Pass
	}
	r.mu.Unlock()
	r.e.mu.Lock()
	streams := slices.Clone(r.e.streams)
	r.e.mu.Unlock()
	for _, s := range streams {
		_, t, res := s.snapshot()
		sr := streamReport{Name: s.name, Consumer: s.spec.consumer, Route: s.spec.route, RPS: s.spec.perSecond(),
			Scheduled: res.Requests.Scheduled, Sent: res.Requests.Sent, Dropped: res.Requests.Dropped,
			Answers: t.n, Allowed: t.ok, Denied: t.limited, Forbidden: t.forbidden, Unavailable: t.unavailable, Other: t.other, NoAnswer: t.failed,
			P50MS: float64(t.p50().Microseconds()) / 1000, P99MS: float64(t.p99().Microseconds()) / 1000}
		if s.spec.replica > 0 {
			sr.Replica = r.e.gws[s.spec.replica-1].name
		}
		rep.Streams = append(rep.Streams, sr)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: results, no secret
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644) //nolint:gosec // G306: results, no secret
}

// serveHealth serves GET /healthz on addr: 200 once the streams run, else
// 503. It returns a function that stops it and the address it listens on.
func serveHealth(addr string, ready *atomic.Bool) (func(), string, error) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, "", err
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }, ln.Addr().String(), nil
}

// runHealthcheck is "demo-traffic healthcheck": exit 0 when the run's
// health listener answers 200.
func runHealthcheck(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", healthAddr, "the run's health listener")
	if code, done := parse(fs, args); done {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*addr+"/healthz", nil) //nolint:gosec // G704: loopback only
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic healthcheck:", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintln(stderr, "demo-traffic healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
