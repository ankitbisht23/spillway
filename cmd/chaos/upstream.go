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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ankitbisht23/spillway/internal/hardware"
)

const (
	attemptsHeader = "Spillway-Attempts" // dev mode only (ADR-0030)
	// faultGap: no request is handed out within this before t_f (drive).
	faultGap = 1500 * time.Microsecond
)

// upstreamOptions are "chaos upstream"'s flags.
type upstreamOptions struct {
	fault         string // 5xx or refused
	url, metrics  string
	control       string // the mock's control base URL
	faultHost     string // the mock's traffic address, resolved here
	tempo         string
	upstream      string
	configVersion int
	runID         string
	rate          int
	duration      time.Duration
	faultAfter    time.Duration
	timeout       time.Duration
	traceWait     time.Duration
	setupWait     time.Duration
	// The rules' constants (ADR-0031); tests change them.
	rules rules
}

func parseUpstreamFlags(args []string, stderr io.Writer) (upstreamOptions, error) {
	o := upstreamOptions{rules: defaultRules}
	fs := flag.NewFlagSet("chaos upstream", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.fault, "fault", "", "the scenario: 5xx or refused (required)")
	fs.StringVar(&o.url, "url", "http://gateway-1:8000/bytes/1024", "gateway URL of the verify route")
	fs.StringVar(&o.metrics, "metrics", "http://gateway-1:8001/metrics", "the gateway's admin metrics URL")
	fs.StringVar(&o.control, "control", "http://upstream-fault:8081", "the fault mock's control URL")
	fs.StringVar(&o.faultHost, "fault-endpoint", "upstream-fault:8080", "the fault mock's traffic address, as the gateway's config names it")
	fs.StringVar(&o.tempo, "tempo", "http://tempo:3200", "Tempo's HTTP API")
	fs.StringVar(&o.upstream, "upstream", "", "the upstream under test (default verify-<fault>)")
	fs.IntVar(&o.configVersion, "config-version", 0, "version of the scenario's config, which the gateway must run first (default 51 for 5xx, 52 for refused)")
	fs.StringVar(&o.runID, "run-id", "", "run ID in each X-Request-Id (default: the start time)")
	fs.IntVar(&o.rate, "rate", 500, "requests per second")
	fs.DurationVar(&o.duration, "duration", 40*time.Second, "length of the run")
	fs.DurationVar(&o.faultAfter, "fault-after", 10*time.Second, "when to start the fault, from the start of the run")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request timeout")
	fs.DurationVar(&o.traceWait, "trace-wait", 60*time.Second, "how long after the last answer Tempo may take to return the traces")
	fs.DurationVar(&o.setupWait, "setup-wait", 10*time.Second, "how long the config and the endpoints' health may each take before the run")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case o.fault != mode5xx && o.fault != modeRefused:
		err = fmt.Errorf("-fault %q: want 5xx or refused", o.fault)
	case o.rate < 1:
		err = errors.New("-rate: must be positive")
	case o.faultAfter <= 0 || o.faultAfter >= o.duration:
		err = errors.New("-fault-after: must be positive and shorter than -duration")
	case o.timeout <= 0 || o.traceWait <= 0 || o.setupWait <= 0:
		err = errors.New("-timeout, -trace-wait and -setup-wait: must be positive")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos upstream:", err)
		return o, err
	}
	if o.upstream == "" {
		o.upstream = "verify-" + o.fault
	}
	if o.configVersion == 0 {
		o.configVersion = map[string]int{mode5xx: 51, modeRefused: 52}[o.fault]
	}
	if o.runID == "" {
		o.runID = strconv.FormatInt(time.Now().Unix(), 36)
	}
	return o, nil
}

// runUpstream is "chaos upstream", the sender of make verify-m4 (ADR-0031):
// it checks that the gateway runs the scenario's config with all three
// endpoints healthy, sends at a constant rate (open model, like loadgen),
// starts the fault on the mock after -fault-after, scrapes the gateway's
// metrics every second, sets the mock back to none, looks up the retried
// requests' traces in Tempo and checks rules (0), (a), (b) and (c). It
// prints its report as JSON on stdout and one PASS/FAIL line per rule on
// stderr, and exits 0 on PASS, 1 on FAIL and 2 on bad flags.
//
//	chaos upstream -fault 5xx|refused
func runUpstream(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseUpstreamFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	return runUpstreamWith(ctx, o, stdout, stderr)
}

func runUpstreamWith(ctx context.Context, o upstreamOptions, stdout, stderr io.Writer) int {
	s := newUpstreamSender(o)
	setupFail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, line(false, o.fault+": setup", err.Error()))
		return 1
	}
	faultAddr, err := resolve(ctx, o.faultHost)
	if err != nil {
		return setupFail(err)
	}
	if err := setFault(ctx, s.control, o.control, modeNone); err != nil {
		return setupFail(err)
	}
	if err := s.waitSetup(ctx, faultAddr); err != nil {
		return setupFail(err)
	}
	run := s.drive(ctx)
	if ctx.Err() != nil {
		return 1
	}
	// Healthy again before the traces are read, and before the next scenario.
	resetErr := setFault(context.WithoutCancel(ctx), s.control, o.control, modeNone)
	retried := retriedRequests(run.out, run.faultAt, o.rules.traces)
	traces := s.checkTraces(ctx, retried, run.lastAnswer.Add(o.traceWait))
	for i, q := range retried {
		traces[i].SentMS = float64(q.sentAt.Sub(run.faultAt).Microseconds()) / 1000
	}

	r := analyzeUpstream(o, faultAddr, run, traces)
	if run.faultErr != nil {
		r.FaultError = run.faultErr.Error()
	}
	if resetErr != nil {
		r.ResetError = resetErr.Error()
	}
	r.Hardware = hardware.Read()
	for _, l := range r.lines(o.fault, o.rules) {
		_, _ = fmt.Fprintln(stderr, l)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos upstream:", err)
		return 1
	}
	if !r.Pass {
		return 1
	}
	return 0
}

// resolve returns hostport's first address as ip:port, as the gateway's
// pool names the endpoint.
func resolve(ctx context.Context, hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", host, err)
	}
	if len(ips) != 1 {
		return "", fmt.Errorf("%s resolves to %d addresses, want 1", host, len(ips))
	}
	return net.JoinHostPort(ips[0], port), nil
}

type upstreamSender struct {
	upstreamOptions
	client  *http.Client // traffic
	scraper *http.Client // metrics, 2 s
	control *http.Client // the mock and Tempo
}

func newUpstreamSender(o upstreamOptions) *upstreamSender {
	return &upstreamSender{
		upstreamOptions: o,
		client: &http.Client{
			Transport: &http.Transport{MaxConnsPerHost: maxInFlight, MaxIdleConnsPerHost: maxInFlight},
			Timeout:   o.timeout,
		},
		scraper: &http.Client{Timeout: o.rules.scrapeTimeout},
		control: &http.Client{Timeout: 5 * time.Second},
	}
}

// waitSetup waits up to setupWait for the scenario's config version, then
// up to setupWait for all three of the upstream's endpoints, the mock
// included, to take traffic.
func (s *upstreamSender) waitSetup(ctx context.Context, faultAddr string) error {
	var last scrape
	wait := func(ok func(scrape) bool) bool {
		deadline := time.Now().Add(s.setupWait)
		for {
			last = s.scrape(ctx, s.upstream)
			if last.OK && ok(last) {
				return true
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return false
			}
			sleepUntil(ctx, time.Now().Add(100*time.Millisecond))
		}
	}
	if !wait(func(m scrape) bool { return m.configVersion == s.configVersion }) {
		if !last.OK {
			return fmt.Errorf("waiting for gw_config_version %d: %s", s.configVersion, last.Err)
		}
		return fmt.Errorf("%s reports gw_config_version %d, want %d", s.metrics, last.configVersion, s.configVersion)
	}
	healthy := func(m scrape) bool {
		n := 0
		for _, v := range m.Healthy {
			if v == 1 {
				n++
			}
		}
		_, fault := m.Healthy[faultAddr]
		return len(m.Healthy) == 3 && n == 3 && fault
	}
	if !wait(healthy) {
		return fmt.Errorf("upstream %s: gw_upstream_endpoint_healthy %v, want 1 for 3 endpoints including %s", s.upstream, last.Healthy, faultAddr)
	}
	return nil
}

// request is what became of one scheduled request.
type request struct {
	sent     bool
	due      time.Time
	sentAt   time.Time
	done     time.Time // answer read in full, or the error
	status   int       // 0: no answer
	attempts int       // Spillway-Attempts, 0 if absent
	traceID  string    // hex
	spanID   string    // hex, the parent the gateway's server span must name
	errKind  string    // timeout, dial or other, when there was no answer
}

func (q *request) answered() bool { return q.status != 0 }

// scenarioRun is one scenario's measurements.
type scenarioRun struct {
	start      time.Time
	faultAt    time.Time // t_f: just before the fault request was sent
	faultErr   error
	out        []request
	lastAnswer time.Time
	scrapes    []scrape
}

// drive sends the schedule, starts the fault, and scrapes the gateway every
// second from 1 s before the first request until the first scrape sent at
// least 2 s after the last answer.
func (s *upstreamSender) drive(ctx context.Context) scenarioRun {
	r := scenarioRun{start: time.Now().Add(1100 * time.Millisecond)}
	interval := time.Second / time.Duration(s.rate)
	n := int(s.duration / interval)
	r.out = make([]request, n)
	for i := range r.out {
		r.out[i].due = r.start.Add(time.Duration(i) * interval)
		r.out[i].traceID, r.out[i].spanID = randomHex(16), randomHex(8)
	}

	stopAfter := make(chan time.Time, 1)
	var scrapes sync.WaitGroup
	scrapes.Go(func() { r.scrapes = s.scrapeLoop(ctx, r.start.Add(-time.Second), stopAfter) })

	// The drive loop itself starts the fault, between two requests: at
	// -fault-after, or later, at least faultGap after it handed out the
	// last request before it. A request sent just before t_f would race
	// the fault request to the mock (it goes through the gateway, the fault
	// request does not) and could be one of the failures that eject the
	// endpoint while it is not in rule (c)'s set. When the sender runs
	// late, that is up to faultGap after -fault-after; no request waits.
	faultAt := r.start.Add(s.faultAfter)
	gap := min(faultGap, interval*3/4)
	var lastSent time.Time
	var fault sync.WaitGroup
	inFlight := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i := range r.out {
		q := &r.out[i]
		if r.faultAt.IsZero() && !q.due.Before(faultAt) {
			sleepUntil(ctx, later(faultAt, lastSent.Add(gap)))
			if ctx.Err() != nil {
				break
			}
			r.faultAt = time.Now()
			fault.Go(func() { r.faultErr = setFault(ctx, s.control, s.upstreamOptions.control, s.fault) })
		}
		sleepUntil(ctx, q.due)
		if ctx.Err() != nil {
			break
		}
		select {
		case inFlight <- struct{}{}:
		default:
			continue // dropped
		}
		q.sent, lastSent = true, time.Now()
		wg.Go(func() {
			defer func() { <-inFlight }()
			s.send(ctx, i, q)
		})
	}
	wg.Wait()
	fault.Wait()
	if r.faultAt.IsZero() {
		r.faultErr = errors.New("the run ended before the fault")
	}
	for _, q := range r.out {
		if q.sent && q.done.After(r.lastAnswer) {
			r.lastAnswer = q.done
		}
	}
	stopAfter <- r.lastAnswer.Add(2 * time.Second)
	scrapes.Wait()
	return r
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// send makes request i and notes its outcome in q.
func (s *upstreamSender) send(ctx context.Context, i int, q *request) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil) //nolint:gosec // G704: the gateway of the compose network is the point
	if err != nil {
		q.errKind, q.done = "other", time.Now()
		return
	}
	req.Header.Set("Traceparent", "00-"+q.traceID+"-"+q.spanID+"-01")
	req.Header.Set("X-Request-Id", fmt.Sprintf("m4-%s-%s-%d", s.fault, s.runID, i))
	q.sentAt = time.Now()
	resp, err := s.client.Do(req) //nolint:gosec // G704: the gateway of the compose network is the point
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	q.done = time.Now()
	if err != nil {
		q.errKind = errorKind(err)
		return
	}
	q.status = resp.StatusCode
	q.attempts, _ = strconv.Atoi(resp.Header.Get(attemptsHeader))
}

// errorKind sorts a request's error: timeout (the client's), dial, or other.
func errorKind(err error) string {
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if oe, ok := errors.AsType[*net.OpError](err); ok && oe.Op == "dial" {
		return "dial"
	}
	return "other"
}

// scrape is one read of the gateway's metrics, for the upstream under test.
type scrape struct {
	Sent, Recv time.Time
	OK         bool
	Err        string `json:",omitempty"`
	// R, Q and X (ADR-0031): gw_retries_total{outcome="sent"},
	// gw_upstream_duration_seconds_count{attempt="1"} and
	// gw_retries_total{outcome="budget_exhausted"}; 0 before their series
	// exist (an upstream's first retry decision creates them).
	Retries, FirstAttempts, BudgetExhausted float64
	Healthy                                 map[string]float64 // gw_upstream_endpoint_healthy by endpoint
	Circuit                                 float64            // gw_circuit_state; -1 without the series
	configVersion                           int
}

// scrapeLoop scrapes from first every second, sending each scrape at its
// due time or at once when overdue, until it has sent one at or after the
// time stopAfter delivers.
func (s *upstreamSender) scrapeLoop(ctx context.Context, first time.Time, stopAfter <-chan time.Time) []scrape {
	var out []scrape
	var stop time.Time
	for k := 0; ; k++ {
		sleepUntil(ctx, first.Add(time.Duration(k)*time.Second))
		if ctx.Err() != nil {
			return out
		}
		if stop.IsZero() {
			select {
			case stop = <-stopAfter:
			default:
			}
		}
		m := s.scrape(ctx, s.upstream)
		out = append(out, m)
		if !stop.IsZero() && !m.Sent.Before(stop) {
			return out
		}
	}
}

// scrape reads the gateway's metrics for upstream.
func (s *upstreamSender) scrape(ctx context.Context, upstream string) scrape {
	m := scrape{Sent: time.Now(), Circuit: -1, Healthy: map[string]float64{}}
	body, err := s.get(ctx, s.scraper, s.metrics)
	m.Recv = time.Now()
	if err != nil {
		m.Err = err.Error()
		return m
	}
	m.OK = true
	for l := range strings.Lines(string(body)) {
		name, labels, v, ok := parseSample(l)
		if !ok {
			continue
		}
		if name == "gw_config_version" {
			m.configVersion = int(v)
			continue
		}
		if labels["upstream"] != upstream {
			continue
		}
		switch {
		case name == "gw_retries_total" && labels["outcome"] == "sent":
			m.Retries = v
		case name == "gw_retries_total" && labels["outcome"] == "budget_exhausted":
			m.BudgetExhausted = v
		case name == "gw_upstream_duration_seconds_count" && labels["attempt"] == "1":
			m.FirstAttempts = v
		case name == "gw_upstream_endpoint_healthy":
			m.Healthy[labels["endpoint"]] = v
		case name == "gw_circuit_state":
			m.Circuit = v
		}
	}
	return m
}

func (s *upstreamSender) get(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: the compose network's services are the point
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req) //nolint:gosec // G704: the compose network's services are the point
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return body, nil
}

// parseSample parses one line of the Prometheus text format: a sample's
// name, labels and value; ok is false for comments, blank lines and lines
// it cannot read.
func parseSample(l string) (name string, labels map[string]string, v float64, ok bool) {
	l = strings.TrimSpace(l)
	if l == "" || l[0] == '#' {
		return "", nil, 0, false
	}
	i := strings.IndexAny(l, "{ ")
	if i <= 0 {
		return "", nil, 0, false
	}
	name, rest := l[:i], l[i:]
	labels = map[string]string{}
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			rest = strings.TrimLeft(rest, ", ")
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			eq := strings.Index(rest, `="`)
			if eq <= 0 {
				return "", nil, 0, false
			}
			key := rest[:eq]
			rest = rest[eq+2:]
			var val strings.Builder
			closed := false
			for j := 0; j < len(rest); j++ {
				c := rest[j]
				if c == '\\' && j+1 < len(rest) {
					j++
					switch rest[j] {
					case 'n':
						val.WriteByte('\n')
					default:
						val.WriteByte(rest[j])
					}
					continue
				}
				if c == '"' {
					rest, closed = rest[j+1:], true
					break
				}
				val.WriteByte(c)
			}
			if !closed {
				return "", nil, 0, false
			}
			labels[key] = val.String()
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, v, true
}
