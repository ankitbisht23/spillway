package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// make verify-m6's names (ADR-0061, test/verify/m6.yaml).
const (
	m6Policy      = "m6-limit"                // the policy the exit spec creates; its RateLimit item
	m6ProbeHeader = "X-Spillway-Probe-Client" // its key template is {header:X-Spillway-Probe-Client}
	m6ProbePath   = "/status/200"             // route m6-probe
)

// m6Options are "chaos m6"'s flags.
type m6Options struct {
	step         string
	controlPlane string // REST base URL, /api/v1 included
	tokenURL     string
	gateways     []string
	runID        string
	restTimeout  time.Duration
	// probe
	heavyRate, lightRate int           // requests per second over all gateways
	timeout              time.Duration // the probe's client timeout
	maxInFlight          int           // a request due while this many are in flight is dropped
	windowStart          time.Duration // a stage's window starts this long after its last apply
	window               time.Duration // and lasts this long
	minHeavyShare        float64       // rule (c): the least share of heavy requests answered 429
	limit                time.Duration // rule (a): every sample below it (NFR-08)
	publishes            int           // rule (a): the UI publishes the spec makes
	maxDuration          time.Duration // the load stops by itself after this
}

func parseM6Flags(args []string, stderr io.Writer) (m6Options, error) {
	o := m6Options{}
	var gateways string
	fs := flag.NewFlagSet("chaos m6", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: chaos m6 setup|probe [flags]")
		fs.PrintDefaults()
	}
	fs.StringVar(&o.controlPlane, "controlplane", "http://controlplane:8080/api/v1", "the control plane's REST API")
	fs.StringVar(&o.tokenURL, "token-url", "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/token", "Keycloak's token endpoint, for the "+ciClient+" client's service token")
	fs.StringVar(&gateways, "gateways", "gateway-1,gateway-2,gateway-3,gateway-4,gateway-5", "the replicas, comma-separated names")
	fs.StringVar(&o.runID, "run-id", "", "run ID in change notes (default: the start time)")
	fs.DurationVar(&o.restTimeout, "rest-timeout", 10*time.Second, "per-call timeout of the REST API and Keycloak")
	fs.IntVar(&o.heavyRate, "heavy-rate", 200, "probe: the heavy client's requests per second over all gateways")
	fs.IntVar(&o.lightRate, "light-rate", 10, "probe: the light client's requests per second over all gateways")
	fs.DurationVar(&o.timeout, "timeout", 20*time.Second, "probe: per-request timeout")
	fs.IntVar(&o.maxInFlight, "max-in-flight", 2000, "probe: a request due while this many are in flight is dropped")
	fs.DurationVar(&o.windowStart, "window-start", time.Second, "probe: a stage's window starts this long after its last apply heartbeat")
	fs.DurationVar(&o.window, "window", 20*time.Second, "probe: a stage's window length")
	fs.Float64Var(&o.minHeavyShare, "min-heavy-429", 0.5, "probe: rule (c)'s least share of heavy requests answered 429")
	fs.DurationVar(&o.limit, "limit", 5*time.Second, "probe: rule (a), every click-to-applied sample below it (NFR-08)")
	fs.IntVar(&o.publishes, "publishes", 23, "probe: rule (a)'s number of UI publishes")
	fs.DurationVar(&o.maxDuration, "max-duration", 30*time.Minute, "probe: the load stops by itself after this")
	var step string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		step, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.step = step
	for g := range strings.SplitSeq(gateways, ",") {
		if g = strings.TrimSpace(g); g != "" {
			o.gateways = append(o.gateways, g)
		}
	}
	if o.runID == "" {
		o.runID = strconv.FormatInt(time.Now().Unix(), 36)
	}
	var err error
	switch {
	case step != "setup" && step != "probe":
		err = fmt.Errorf("step %q: want setup or probe", step)
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case len(o.gateways) == 0:
		err = errors.New("-gateways: none")
	case o.heavyRate < 1 || o.lightRate < 1 || o.maxInFlight < 1 || o.publishes < 1:
		err = errors.New("-heavy-rate, -light-rate, -max-in-flight and -publishes: must be positive")
	case o.restTimeout <= 0 || o.timeout <= 0 || o.windowStart < 0 || o.window <= 0 || o.limit <= 0 || o.maxDuration <= 0:
		err = errors.New("-rest-timeout, -timeout, -window, -limit and -max-duration must be positive, -window-start not negative")
	case o.minHeavyShare <= 0 || o.minHeavyShare > 1:
		err = errors.New("-min-heavy-429: must be in (0, 1]")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos m6:", err)
	}
	return o, err
}

// runM6 is "chaos m6", the sender of make verify-m6 (ADR-0061), in the
// compose network, with a service token for the spillway-ci client (as
// "chaos controlplane"; never printed). Steps:
//
//   - setup: imports the config on stdin (test/verify/m6.yaml) with
//     If-Match and publishes it as the base version B (a 409, the working
//     set already published, answers the latest version), printed as JSON.
//   - probe: drives route m6-probe on every gateway, round robin, with the
//     heavy client (header X-Spillway-Probe-Client: heavy) at -heavy-rate
//     and the light one at -light-rate, open model; writes "probe started"
//     on stderr once the load runs. It reads stdin until it ends: the
//     lines "exit <JSON>" (what the exit spec recorded), "guard <JSON>"
//     (the leak guard's report) and "spec <exit code>". Then it stops the
//     load, waits for the requests in flight, reads each published
//     version's published_at and apply heartbeats, and judges ADR-0061's
//     rules (a) … (g).
//
// Each step prints its report as JSON on stdout and its PASS/FAIL lines
// on stderr, and exits 0 on PASS, 1 on FAIL and 2 on bad flags.
//
//	chaos m6 <step> [flags]
func runM6(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseM6Flags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	sec, err := secretFrom(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos m6:", err)
		return 2
	}
	cp := newCPRun(cpOptions{controlPlane: o.controlPlane, gateways: o.gateways, runID: o.runID, restTimeout: o.restTimeout},
		newCPClient(o.controlPlane, o.tokenURL, sec, o.restTimeout))
	return m6Step(ctx, o, cp, func(g string) string { return g + ":8000" }, stdin, stdout, stderr)
}

// m6Step runs o.step, prints its report and lines and returns the exit
// code; traffic is a gateway's host:port.
func m6Step(ctx context.Context, o m6Options, cp *cpRun, traffic func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	var report any
	var lines []string
	pass := true
	switch o.step {
	case "setup":
		yaml, err := io.ReadAll(io.LimitReader(stdin, 10<<20))
		if err != nil || len(yaml) == 0 {
			_, _ = fmt.Fprintln(stderr, "chaos m6: setup: no config on stdin", err)
			return 2
		}
		p, err := cp.importPublish(ctx, yaml, "make verify-m6 "+o.runID+": test/verify/m6.yaml")
		report = map[string]int64{"config_version": p.Version}
		if err != nil {
			lines, pass = []string{line(false, "setup", err.Error())}, false
		}
	case "probe":
		p := newProbe(o, traffic)
		rep := p.run(ctx, stdin, stderr, cp)
		report, pass, lines = rep, rep.Pass, rep.Lines
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report)
	for _, l := range lines {
		_, _ = fmt.Fprintln(stderr, l)
	}
	if !pass {
		return 1
	}
	return 0
}

// probeRecord is one request of the probe. Times are on the probe's clock,
// which is the clock of the VM (or CI host) that Postgres reads too.
type probeRecord struct {
	due, recv time.Time
	gateway   int
	heavy     bool
	status    int    // 0: no answer
	fail      string // "dropped", "timeout", "transport" or ""
	limitOK   bool   // a 429 whose RateLimit has "m6-limit" with r=0 and whose Retry-After is >= 1
}

type probe struct {
	o       m6Options
	traffic func(g string) string // a gateway's host:port
	clients []*http.Client        // one per gateway
	sem     chan struct{}

	mu   sync.Mutex
	recs []probeRecord
}

func newProbe(o m6Options, traffic func(string) string) *probe {
	p := &probe{o: o, traffic: traffic, sem: make(chan struct{}, o.maxInFlight)}
	for range o.gateways {
		// At most 512 connections per gateway: the probe sends from one
		// address, which must stay below the gateways' per-address cap of
		// 1,024 (ADR-0071).
		p.clients = append(p.clients, &http.Client{Timeout: o.timeout,
			Transport: &http.Transport{MaxConnsPerHost: maxConnsPerGateway, MaxIdleConnsPerHost: maxConnsPerGateway, MaxIdleConns: 4096, IdleConnTimeout: time.Minute}})
	}
	return p
}

// m6Inputs is what the script hands the probe after the UI run.
type m6Inputs struct {
	Exit     *m6Exit  // nil: the spec wrote nothing
	Guard    *m6Guard // nil: the leak guard wrote nothing
	SpecCode int      // -1: not given
}

// m6Exit is what web/e2e/m6-exit.spec.ts records (test-results/m6-exit.json).
type m6Exit struct {
	Run         string      `json:"run"`
	BaseVersion int64       `json:"base_version"`
	Publishes   []m6Publish `json:"publishes"`
	Review      *m6Review   `json:"review"`
	Steps       []string    `json:"steps"`
}

// m6Publish is one timed UI action: route (one of the 20), shadow,
// enforce or rollback; t_click and t_response in Unix ms on the host.
type m6Publish struct {
	Kind      string `json:"kind"`
	Version   int64  `json:"version"`
	TClick    int64  `json:"t_click"`
	TResponse int64  `json:"t_response"`
}

// m6Review is step 3: the shadow report and the probe clients' digests.
type m6Review struct {
	UIShowed    bool     `json:"ui_showed_would_deny"`
	UIWouldDeny string   `json:"ui_would_deny"`
	WouldDeny   *float64 `json:"would_deny"`
	Evaluated   *float64 `json:"evaluated"`
	Share       *float64 `json:"share"`
	Keys        []struct {
		KeyDigest    string `json:"key_digest"`
		ShadowDenied int64  `json:"shadow_denied"`
		Denied       int64  `json:"denied"`
	} `json:"keys"`
	HeavyDigest string `json:"heavy_digest"`
	LightDigest string `json:"light_digest"`

	// UIHeavy is the heavy key's would-deny as the UI's key table showed
	// it: this run's, as its row is under this run's policy ID; nil when
	// the table had no such row.
	UIHeavy *int64 `json:"ui_heavy_shadow_denied"`
}

// m6Guard is the leak guard's report of the exit spec (web/e2e/fixtures.ts).
type m6Guard struct {
	Checked       map[string]int `json:"checked"`
	SecretsSeen   int            `json:"secrets_seen"`
	Findings      []string       `json:"findings"`
	ConsoleErrors []string       `json:"console_errors"`
}

// readInputs reads the script's lines until stdin ends.
func readInputs(stdin io.Reader) (m6Inputs, error) {
	in := m6Inputs{SpecCode: -1}
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		word, rest, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		var err error
		switch word {
		case "":
		case "exit":
			in.Exit = &m6Exit{}
			err = json.Unmarshal([]byte(rest), in.Exit)
		case "guard":
			in.Guard = &m6Guard{}
			err = json.Unmarshal([]byte(rest), in.Guard)
		case "spec":
			in.SpecCode, err = strconv.Atoi(rest)
		default:
			err = fmt.Errorf("unknown line %q", word)
		}
		if err != nil {
			return in, fmt.Errorf("stdin %s: %w", word, err)
		}
	}
	return in, sc.Err()
}

// m6Report is the probe's verdict.
type m6Report struct {
	RunID    string           `json:"run_id"`
	Requests int              `json:"requests"`
	LoadS    float64          `json:"load_s"`
	Versions map[string]int64 `json:"versions,omitempty"` // B, S, E, R
	Samples  []m6Sample       `json:"samples"`
	Stages   []m6Stage        `json:"stages"`
	Rules    []m6Rule         `json:"rules"`
	Error    string           `json:"error,omitempty"`
	Pass     bool             `json:"pass"`
	Lines    []string         `json:"-"`
}

// m6Sample is one publish of rule (a).
type m6Sample struct {
	Kind          string   `json:"kind"`
	Version       int64    `json:"version"`
	UIMS          float64  `json:"ui_ms"`          // t_response - t_click, host clock
	PropagationMS float64  `json:"propagation_ms"` // last apply - published_at, Postgres clock
	SampleMS      float64  `json:"sample_ms"`
	Missing       []string `json:"missing,omitempty"`
}

// m6Stage is one exit stage's window and what the probe saw in it.
type m6Stage struct {
	Name        string           `json:"name"`
	Version     int64            `json:"version"`
	PublishedAt time.Time        `json:"published_at"`
	LastApplied time.Time        `json:"last_applied"`
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Counts      map[string]int64 `json:"counts"`   // client/status
	Gateways    map[string]int   `json:"gateways"` // client -> gateways that answered it
	BadLimit    int64            `json:"bad_limit_headers"`
}

type m6Rule struct {
	Check    string `json:"check"`
	Pass     bool   `json:"pass"`
	Measured string `json:"measured"`
}

// run drives the load until stdin ends (or -max-duration), then judges.
func (p *probe) run(ctx context.Context, stdin io.Reader, stderr io.Writer, cp *cpRun) m6Report {
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	type read struct {
		in  m6Inputs
		err error
	}
	got := make(chan read, 1)
	go func() {
		in, err := readInputs(stdin)
		got <- read{in, err}
		halt()
	}()
	start := time.Now().Add(100 * time.Millisecond)
	end := start.Add(p.o.maxDuration)
	var wg sync.WaitGroup
	wg.Go(func() { p.drive(ctx, start, end, p.o.heavyRate, true, stop) })
	wg.Go(func() { p.drive(ctx, start, end, p.o.lightRate, false, stop) })
	_, _ = fmt.Fprintf(stderr, "probe started: %d rps heavy, %d rps light over %d gateways\n", p.o.heavyRate, p.o.lightRate, len(p.o.gateways))
	var r read
	select {
	case r = <-got:
	case <-ctx.Done():
		halt()
		r.err = ctx.Err()
	case <-time.After(time.Until(end)):
		halt()
		r.err = fmt.Errorf("stdin did not end within -max-duration %v", p.o.maxDuration)
	}
	stopped := time.Now()
	wg.Wait() // every request in flight has its answer, or its timeout
	rep := m6Report{RunID: p.o.runID, LoadS: seconds(stopped.Sub(start))}
	p.mu.Lock()
	recs := p.recs
	p.mu.Unlock()
	rep.Requests = len(recs)
	if r.err != nil {
		rep.Error = r.err.Error()
	}
	versions := map[int64]published{}
	if r.in.Exit != nil {
		for _, x := range r.in.Exit.Publishes {
			if _, ok := versions[x.Version]; ok || x.Version <= 0 {
				continue
			}
			var v published
			if _, err := cp.rest.must(ctx, http.StatusOK, http.MethodGet, fmt.Sprintf("/config/versions/%d", x.Version), nil, &v); err != nil {
				rep.Error = strings.TrimPrefix(rep.Error+"; ", "; ") + err.Error()
				continue
			}
			versions[x.Version] = v
		}
	}
	judgeM6(&rep, p.o, r.in, versions, recs, stopped)
	return rep
}

// drive sends rate requests per second from start until stop is closed
// or end is reached, round robin over the gateways: open model, overdue
// requests sent at once, a request due while -max-in-flight are in
// flight dropped (ADR-0014).
func (p *probe) drive(ctx context.Context, start, end time.Time, rate int, heavy bool, stop <-chan struct{}) {
	interval := time.Second / time.Duration(rate)
	client := "light"
	if heavy {
		client = "heavy"
	}
	var wg sync.WaitGroup
	for i := 0; ; i++ {
		due := start.Add(time.Duration(i) * interval)
		if !due.Before(end) {
			break
		}
		if d := time.Until(due); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-stop:
				t.Stop()
			case <-ctx.Done():
				t.Stop()
			}
		}
		select {
		case <-stop:
		case <-ctx.Done():
		default:
			g := i % len(p.o.gateways)
			select {
			case p.sem <- struct{}{}:
				wg.Go(func() {
					defer func() { <-p.sem }()
					p.send(ctx, g, client, due)
				})
			default:
				p.record(probeRecord{due: due, recv: time.Now(), gateway: g, heavy: heavy, fail: "dropped"})
			}
			continue
		}
		break
	}
	wg.Wait()
}

func (p *probe) send(ctx context.Context, g int, client string, due time.Time) {
	rec := probeRecord{due: due, gateway: g, heavy: client == "heavy"}
	defer func() { rec.recv = time.Now(); p.record(rec) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.traffic(p.o.gateways[g])+m6ProbePath, nil)
	if err != nil {
		rec.fail = "transport"
		return
	}
	req.Header.Set(m6ProbeHeader, client)
	resp, err := p.clients[g].Do(req)
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if err != nil {
		rec.fail = "transport"
		if errorKind(err) == "timeout" {
			rec.fail = "timeout"
		}
		return
	}
	rec.status = resp.StatusCode
	if resp.StatusCode == http.StatusTooManyRequests {
		ra, raErr := strconv.Atoi(resp.Header.Get("Retry-After"))
		rec.limitOK = rateLimitExhausted(resp.Header.Values("RateLimit"), m6Policy) && raErr == nil && ra >= 1
	}
}

func (p *probe) record(r probeRecord) {
	p.mu.Lock()
	p.recs = append(p.recs, r)
	p.mu.Unlock()
}

// rateLimitExhausted reports whether the RateLimit fields hold the item
// name with r=0, as in `"m6-limit";r=0;t=1`.
func rateLimitExhausted(fields []string, name string) bool {
	for _, f := range fields {
		for item := range strings.SplitSeq(f, ",") {
			parts := strings.Split(strings.TrimSpace(item), ";")
			if parts[0] != strconv.Quote(name) {
				continue
			}
			for _, param := range parts[1:] {
				if strings.TrimSpace(param) == "r=0" {
					return true
				}
			}
		}
	}
	return false
}

// judgeM6 fills rep's samples, stages, rules and lines from what the
// script handed in, the versions read from the control plane and the
// probe's records; stopped is when the load stopped.
func judgeM6(rep *m6Report, o m6Options, in m6Inputs, versions map[int64]published, recs []probeRecord, stopped time.Time) {
	add := func(ok bool, check, measured string) {
		rep.Rules = append(rep.Rules, m6Rule{check, ok, measured})
	}
	n := len(o.gateways)
	lastApplied := func(v published) (time.Time, []string) {
		at := map[string]time.Time{}
		for _, x := range v.Applied {
			if slices.Contains(o.gateways, x.Replica) {
				if t, ok := at[x.Replica]; !ok || x.At.Before(t) {
					at[x.Replica] = x.At // the first apply heartbeat
				}
			}
		}
		var last time.Time
		var missing []string
		for _, g := range o.gateways {
			t, ok := at[g]
			if !ok {
				missing = append(missing, g)
			}
			if t.After(last) {
				last = t
			}
		}
		return last, missing
	}

	// (a) NFR-08: every UI publish, click to the last apply heartbeat.
	var exit m6Exit
	if in.Exit != nil {
		exit = *in.Exit
	}
	var sorted []float64
	var maxUI, maxProp float64
	missingAny := false
	for _, x := range exit.Publishes {
		s := m6Sample{Kind: x.Kind, Version: x.Version, UIMS: float64(x.TResponse - x.TClick)}
		v, ok := versions[x.Version]
		if !ok {
			s.Missing = slices.Clone(o.gateways)
		} else {
			last, missing := lastApplied(v)
			s.Missing = missing
			if len(missing) == 0 {
				s.PropagationMS = ms(last.Sub(v.PublishedAt))
			}
		}
		s.SampleMS = s.UIMS + s.PropagationMS
		if len(s.Missing) > 0 {
			missingAny = true
			s.SampleMS = math.Inf(1)
		}
		rep.Samples = append(rep.Samples, s)
		sorted = append(sorted, s.SampleMS)
		maxUI, maxProp = max(maxUI, s.UIMS), max(maxProp, s.PropagationMS)
	}
	slices.Sort(sorted)
	limit := ms(o.limit)
	pA := len(sorted) == o.publishes && !missingAny && len(sorted) > 0 && sorted[len(sorted)-1] < limit
	measured := fmt.Sprintf("%d of %d UI publishes: p50 %s, p99 %s (nearest rank, the max), max %s; limit %.0f ms each; largest UI part %.0f ms (host clock), largest propagation part %.0f ms (Postgres clock)",
		len(sorted), o.publishes, msText(nearestRank(sorted, 0.5)), msText(nearestRank(sorted, 0.99)), msText(nearestRank(sorted, 1)), limit, maxUI, maxProp)
	if missingAny {
		var late []string
		for _, s := range rep.Samples {
			if len(s.Missing) > 0 {
				late = append(late, fmt.Sprintf("version %d not applied on %s", s.Version, strings.Join(s.Missing, ", ")))
			}
		}
		measured += "; " + strings.Join(late, "; ")
	}
	add(pA, fmt.Sprintf("(a) NFR-08: click to all %d applied < 5 s", n), measured)

	// The stages' windows.
	stageOf := map[string]string{"shadow": "S", "enforce": "E", "rollback": "R"}
	rep.Versions = map[string]int64{"B": exit.BaseVersion}
	stages := map[string]*m6Stage{}
	for _, x := range exit.Publishes {
		name, ok := stageOf[x.Kind]
		if !ok {
			continue
		}
		rep.Versions[name] = x.Version
		v, ok := versions[x.Version]
		if !ok {
			continue
		}
		last, missing := lastApplied(v)
		if len(missing) > 0 {
			continue
		}
		st := &m6Stage{Name: name, Version: x.Version, PublishedAt: v.PublishedAt, LastApplied: last,
			From: last.Add(o.windowStart), To: last.Add(o.windowStart + o.window),
			Counts: map[string]int64{}, Gateways: map[string]int{}}
		answered := map[string]map[int]bool{"heavy": {}, "light": {}}
		for _, r := range recs {
			if r.due.Before(st.From) || !r.due.Before(st.To) {
				continue
			}
			c := clientName(r.heavy)
			st.Counts[c+"/"+statusName(r)]++
			if r.status != 0 {
				answered[c][r.gateway] = true
			}
			if r.status == http.StatusTooManyRequests && !r.limitOK {
				st.BadLimit++
			}
		}
		for c, gs := range answered {
			st.Gateways[c] = len(gs)
		}
		stages[name] = st
		rep.Stages = append(rep.Stages, *st)
	}
	// A stage's window is judged only if the load ran through it.
	covered := func(name string) (*m6Stage, string) {
		st := stages[name]
		switch {
		case rep.Versions[name] == 0:
			return nil, "stage " + name + " not reached"
		case st == nil:
			return nil, fmt.Sprintf("version %d not applied on all %d", rep.Versions[name], n)
		case stopped.Before(st.To):
			return nil, fmt.Sprintf("the load stopped %.1f s before the window's end", seconds(st.To.Sub(stopped)))
		}
		return st, ""
	}
	only200 := func(st *m6Stage, check string) {
		ok := st.Counts["heavy/200"]+st.Counts["light/200"] == st.Counts["heavy/all"]+st.Counts["light/all"] &&
			st.Counts["heavy/all"] > 0 && st.Counts["light/all"] > 0 && st.Gateways["heavy"] == n && st.Gateways["light"] == n
		add(ok, check, st.describe(n))
	}
	for _, st := range stages {
		for _, c := range []string{"heavy", "light"} {
			var all int64
			for k, v := range st.Counts {
				if strings.HasPrefix(k, c+"/") {
					all += v
				}
			}
			st.Counts[c+"/all"] = all
		}
	}

	// (b) shadow.
	if st, why := covered("S"); st == nil {
		add(false, "(b) shadow: no 429s", why)
	} else {
		only200(st, "(b) shadow: no 429s")
	}

	// (b2) reviewed.
	if rv := exit.Review; rv == nil {
		add(false, "(b2) reviewed: would-denies reported", "the spec recorded no review")
	} else {
		var heavy, light *int64
		for _, k := range rv.Keys {
			sd := k.ShadowDenied
			switch k.KeyDigest {
			case rv.HeavyDigest:
				heavy = &sd
			case rv.LightDigest:
				light = &sd
			}
		}
		wd := -1.0
		if rv.WouldDeny != nil {
			wd = *rv.WouldDeny
		}
		// would_deny counts by policy name over 15 min, so it can include
		// an earlier run's; the key rows, by this run's policy ID, are the
		// per-run evidence, in the API and in the UI.
		ok := rv.UIShowed && wd >= 1 && rv.HeavyDigest != "" && heavy != nil && *heavy >= 1 && (light == nil || *light == 0) &&
			rv.UIHeavy != nil && *rv.UIHeavy >= 1
		add(ok, "(b2) reviewed: would-denies reported", fmt.Sprintf("would_deny %s of %s shadow decisions (share %s; by policy name over 15 min, may include earlier runs); "+
			"%d key rows; heavy key %s, light key %s; UI showed %q, heavy key %s",
			optNum(rv.WouldDeny), optNum(rv.Evaluated), optPercent(rv.Share), len(rv.Keys), keyCount(heavy), keyCount(light), rv.UIWouldDeny, keyCount(rv.UIHeavy)))
	}

	// (c) enforced.
	if st, why := covered("E"); st == nil {
		add(false, "(c) enforced: heavy 429s", why)
	} else {
		share := 0.0
		if h := st.Counts["heavy/all"]; h > 0 {
			share = float64(st.Counts["heavy/429"]) / float64(h)
		}
		ok := share >= o.minHeavyShare && st.BadLimit == 0 &&
			st.Counts["heavy/429"]+st.Counts["heavy/200"] == st.Counts["heavy/all"] &&
			st.Counts["light/200"] == st.Counts["light/all"] && st.Counts["light/all"] > 0 &&
			st.Gateways["heavy"] == n && st.Gateways["light"] == n
		add(ok, "(c) enforced: heavy 429s", fmt.Sprintf("heavy 429 share %.1f %% (at least %.0f %%); %s; 429s without %q r=0 or Retry-After >= 1: %d",
			share*100, o.minHeavyShare*100, st.describe(n), m6Policy, st.BadLimit))
	}

	// (d) rolled back.
	if st, why := covered("R"); st == nil {
		add(false, "(d) rolled back: no 429s", why)
	} else {
		only200(st, "(d) rolled back: no 429s")
	}

	// (e) the whole run.
	counts := map[string]int64{}
	var first429, last429Due time.Time
	var early, late, light429 int64
	var lower, upper time.Time
	if e := stages["E"]; e != nil {
		lower = e.PublishedAt
	}
	if r := stages["R"]; r != nil {
		upper = r.LastApplied.Add(time.Second)
	}
	for _, r := range recs {
		counts[clientName(r.heavy)+"/"+statusName(r)]++
		if r.status != http.StatusTooManyRequests {
			continue
		}
		if !r.heavy {
			light429++
			continue
		}
		if first429.IsZero() || r.recv.Before(first429) {
			first429 = r.recv
		}
		if r.due.After(last429Due) {
			last429Due = r.due
		}
		if lower.IsZero() || r.recv.Before(lower) {
			early++
		}
		if !upper.IsZero() && !r.due.Before(upper) {
			late++
		}
	}
	var bad int64
	for k, v := range counts {
		if !strings.HasSuffix(k, "/200") && !strings.HasSuffix(k, "/429") {
			bad += v
		}
	}
	ok := len(recs) > 0 && bad == 0 && light429 == 0 && early == 0 && late == 0
	measured = fmt.Sprintf("%d requests in %.0f s: %s; other answers or failures %d; light 429s %d; heavy 429s before E's published_at %d, due after R's last apply + 1 s %d",
		len(recs), rep.LoadS, describeCounts(counts), bad, light429, early, late)
	if !first429.IsZero() {
		measured += "; first 429 received " + relative(first429, lower, "E's published_at") + ", last 429 due " + relative(last429Due, upper, "R's last apply + 1 s")
	}
	add(ok, "(e) whole run: only 200 and 429, 429s in E", measured)

	// (f1), (f2) the leak guard; (g) the spec.
	if g := in.Guard; g == nil {
		add(false, "(f1) console errors", "the leak guard wrote no report")
		add(false, "(f2) no tokens in storage, cookies, URLs", "the leak guard wrote no report")
	} else {
		add(len(g.ConsoleErrors) == 0, "(f1) console errors", fmt.Sprintf("%d on the control plane's pages%s", len(g.ConsoleErrors), firstOf(g.ConsoleErrors)))
		var places []string
		for _, k := range slices.Sorted(maps.Keys(g.Checked)) {
			places = append(places, fmt.Sprintf("%s %d", strings.ReplaceAll(k, "_", " "), g.Checked[k]))
		}
		add(len(g.Findings) == 0 && g.SecretsSeen > 0, "(f2) no tokens in storage, cookies, URLs",
			fmt.Sprintf("%d findings; %d tokens and keys watched; checked %s%s", len(g.Findings), g.SecretsSeen, strings.Join(places, ", "), firstOf(g.Findings)))
	}
	steps := strings.Join(exit.Steps, " → ")
	if steps == "" {
		steps = "none"
	}
	add(in.SpecCode == 0, "(g) the exit flow, all in the UI", fmt.Sprintf("m6-exit.spec.ts exit code %d; steps done: %s", in.SpecCode, steps))

	rep.Pass = rep.Error == ""
	for _, r := range rep.Rules {
		rep.Pass = rep.Pass && r.Pass
		rep.Lines = append(rep.Lines, line(r.Pass, r.Check, r.Measured))
	}
	if rep.Error != "" {
		rep.Lines = append(rep.Lines, line(false, "probe", rep.Error))
	}
}

func clientName(heavy bool) string {
	if heavy {
		return "heavy"
	}
	return "light"
}

func statusName(r probeRecord) string {
	if r.status == 0 {
		return r.fail
	}
	return strconv.Itoa(r.status)
}

// describe is a stage's window and counts.
func (st *m6Stage) describe(n int) string {
	return fmt.Sprintf("version %d, window %s – %s: heavy %s, light %s; answered by %d/%d and %d/%d gateways",
		st.Version, st.From.UTC().Format("15:04:05.000"), st.To.UTC().Format("15:04:05.000"),
		describeClient(st.Counts, "heavy"), describeClient(st.Counts, "light"), st.Gateways["heavy"], n, st.Gateways["light"], n)
}

func describeClient(counts map[string]int64, c string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		if s, ok := strings.CutPrefix(k, c+"/"); ok && s != "all" {
			parts = append(parts, fmt.Sprintf("%s: %d", s, counts[k]))
		}
	}
	return fmt.Sprintf("%d (%s)", counts[c+"/all"], strings.Join(parts, ", "))
}

func describeCounts(counts map[string]int64) string {
	for _, c := range []string{"heavy", "light"} {
		var all int64
		for k, v := range counts {
			if strings.HasPrefix(k, c+"/") {
				all += v
			}
		}
		counts[c+"/all"] = all
	}
	return "heavy " + describeClient(counts, "heavy") + ", light " + describeClient(counts, "light")
}

func msText(v float64) string {
	if math.IsInf(v, 1) {
		return "missing"
	}
	return fmt.Sprintf("%.0f ms", v)
}

func optNum(v *float64) string {
	if v == nil {
		return "null"
	}
	return strconv.FormatFloat(*v, 'f', 0, 64)
}

func optPercent(v *float64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%.1f %%", *v*100)
}

func keyCount(v *int64) string {
	if v == nil {
		return "absent"
	}
	return fmt.Sprintf("%d would-denies", *v)
}

func relative(t, ref time.Time, name string) string {
	if ref.IsZero() {
		return "(no " + name + ")"
	}
	d := t.Sub(ref)
	if d >= 0 {
		return fmt.Sprintf("%.3f s after %s", seconds(d), name)
	}
	return fmt.Sprintf("%.3f s before %s", seconds(-d), name)
}

func firstOf(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return "; first: " + xs[0]
}
