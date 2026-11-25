package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// "chaos gwkill" is the sender of the gateway kill -9 chaos test (ADR-0074
// §D9), run by test/chaos/gateway-kill.sh in the compose network against
// gateway-1 … gateway-3 in static mode with test/chaos/gwkill.yaml. It sends
// -rate requests per second round robin over the gateways (open model), on
// one transport per gateway with a -timeout per request, for -duration; at
// -kill-after it writes "kill <gateway>" on stderr and the script kills
// that container with docker kill -s KILL. Like a load balancer with passive
// checks, the sender ejects a gateway at its first failed request (no
// answer, or not a 2xx) and sends it nothing after. It passes when no
// request to another gateway failed, a failure from the killed one was
// observed, and every failed request went to the killed gateway and was
// sent before the sender observed its first failure: only the requests that
// were in flight to it, or on their way, failed. It is judged by order, not
// by time, so a host stall cannot fail it (ADR-0074, Stall tolerance). It
// prints its report as JSON on stdout and one PASS/FAIL line per check on
// stderr, and exits 0 on PASS, 1 on FAIL and 2 on bad flags.
//
//	chaos gwkill [-gateways gateway-1:8000,gateway-2:8000,gateway-3:8000] [-kill gateway-2]

type gwkillOptions struct {
	gateways      []string // host:port of each traffic listener
	kill          string   // the gateway to kill, a host of -gateways
	path          string
	configVersion int
	rate          int // in all
	duration      time.Duration
	killAfter     time.Duration
	timeout       time.Duration
}

func parseGwkillFlags(args []string, stderr io.Writer) (gwkillOptions, error) {
	o := gwkillOptions{}
	var gateways string
	fs := flag.NewFlagSet("chaos gwkill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&gateways, "gateways", "gateway-1:8000,gateway-2:8000,gateway-3:8000", "the gateways, comma-separated host:port")
	fs.StringVar(&o.kill, "kill", "gateway-2", "the gateway to kill: one of the -gateways hosts")
	fs.StringVar(&o.path, "path", "/bytes/1024", "the path to request")
	fs.IntVar(&o.configVersion, "config-version", 74, "version of test/chaos/gwkill.yaml, which every gateway must run first (admin port 8001)")
	fs.IntVar(&o.rate, "rate", 300, "requests per second over all gateways")
	fs.DurationVar(&o.duration, "duration", 40*time.Second, "length of the run")
	fs.DurationVar(&o.killAfter, "kill-after", 20*time.Second, "when to ask for the kill, from the start")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	for g := range strings.SplitSeq(gateways, ",") {
		if g = strings.TrimSpace(g); g != "" {
			o.gateways = append(o.gateways, g)
		}
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case len(o.gateways) < 2:
		err = errors.New("-gateways: want at least two")
	case o.killed() < 0:
		err = fmt.Errorf("-kill: %q is not a host of -gateways", o.kill)
	case o.rate < 1:
		err = errors.New("-rate: must be positive")
	case o.killAfter <= 0 || o.killAfter >= o.duration:
		err = errors.New("-kill-after: must be positive and shorter than -duration")
	case o.timeout <= 0:
		err = errors.New("-timeout: must be positive")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos gwkill:", err)
	}
	return o, err
}

// killed is the index of the gateway to kill; -1 if none.
func (o gwkillOptions) killed() int {
	for i, g := range o.gateways {
		if host, _, _ := strings.Cut(g, ":"); host == o.kill {
			return i
		}
	}
	return -1
}

// gwkillGateway is one gateway's numbers in the report.
type gwkillGateway struct {
	Addr   string `json:"addr"`
	Sent   int    `json:"sent"`
	OK     int    `json:"ok"`
	Failed int    `json:"failed"`
	// Ejected is when the sender observed the gateway's first failure.
	Ejected *time.Time `json:"ejected,omitempty"`
	// LateFailures failed although sent after Ejected; none can, since an
	// ejected gateway is sent nothing, unless the bookkeeping is wrong.
	LateFailures int `json:"late_failures"`
	// FailureKinds counts failures by kind: timeout, transport or the
	// status class.
	FailureKinds map[string]int `json:"failure_kinds,omitempty"`
}

type gwkillReport struct {
	Requests struct {
		Scheduled int `json:"scheduled"`
		Sent      int `json:"sent"`
	} `json:"requests"`
	KillRequested time.Time       `json:"kill_requested"`
	Killed        string          `json:"killed"`
	Gateways      []gwkillGateway `json:"gateways"`
	Pass          bool            `json:"pass"`
}

// balancer is round robin over the gateways that are not ejected.
type balancer struct {
	mu      sync.Mutex
	next    int
	ejected []*time.Time
}

// pick returns the next gateway that is not ejected and when the choice was
// made; -1 when all are ejected.
func (b *balancer) pick() (int, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for range b.ejected {
		i := b.next
		b.next = (b.next + 1) % len(b.ejected)
		if b.ejected[i] == nil {
			return i, time.Now()
		}
	}
	return -1, time.Time{}
}

// fail ejects gateway i at its first failure.
func (b *balancer) fail(i int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ejected[i] == nil {
		t := time.Now()
		b.ejected[i] = &t
	}
}

// gwkillSample is one request: the gateway it went to, when it was sent, and
// how it failed ("" for a 2xx).
type gwkillSample struct {
	gateway int
	sentAt  time.Time
	failure string
}

func runGwkill(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseGwkillFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	return runGwkillWith(ctx, o, "8001", stdout, stderr)
}

// runGwkillWith runs the test; adminPort is the gateways' admin port, on
// their traffic host.
func runGwkillWith(ctx context.Context, o gwkillOptions, adminPort string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "chaos gwkill:", err)
		return 1
	}
	clients := make([]*http.Client, len(o.gateways))
	for i := range clients {
		clients[i] = &http.Client{
			Transport: &http.Transport{MaxConnsPerHost: maxConnsPerGateway, MaxIdleConnsPerHost: maxConnsPerGateway},
			Timeout:   o.timeout,
		}
	}
	admin := &http.Client{Timeout: 5 * time.Second}
	for i, g := range o.gateways {
		host, _, _ := strings.Cut(g, ":")
		if err := waitVersion(ctx, admin, "http://"+host+":"+adminPort+"/metrics", o.configVersion, 30*time.Second); err != nil {
			return fail(fmt.Errorf("%s: %w", g, err))
		}
		if f := gwkillSend(ctx, clients[i], "http://"+g+o.path); f != "" {
			return fail(fmt.Errorf("%s: a first request failed (%s)", g, f))
		}
	}

	b := &balancer{ejected: make([]*time.Time, len(o.gateways))}
	interval := time.Second / time.Duration(o.rate)
	n := int(o.duration / interval)
	samples := make([]gwkillSample, 0, n)
	var mu sync.Mutex
	start := time.Now().Add(100 * time.Millisecond)
	var killAt time.Time
	timer := time.AfterFunc(time.Until(start.Add(o.killAfter)), func() {
		mu.Lock()
		killAt = time.Now()
		mu.Unlock()
		_, _ = fmt.Fprintf(stderr, "kill %s\n", o.kill)
	})
	defer timer.Stop()
	inFlight := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	scheduled := 0
	for k := range n {
		sleepUntil(ctx, start.Add(time.Duration(k)*interval))
		if ctx.Err() != nil {
			break
		}
		scheduled++
		select {
		case inFlight <- struct{}{}:
		default:
			continue
		}
		i, at := b.pick()
		if i < 0 {
			<-inFlight
			continue
		}
		wg.Go(func() {
			defer func() { <-inFlight }()
			f := gwkillSend(ctx, clients[i], "http://"+o.gateways[i]+o.path)
			if f != "" {
				b.fail(i)
			}
			mu.Lock()
			samples = append(samples, gwkillSample{gateway: i, sentAt: at, failure: f})
			mu.Unlock()
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return 1
	}
	mu.Lock()
	rep := judgeGwkill(o, scheduled, samples, b.ejected, killAt)
	mu.Unlock()
	for _, l := range rep.lines(o) {
		_, _ = fmt.Fprintln(stderr, l)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	if !rep.Pass {
		return 1
	}
	return 0
}

// gwkillSend makes one request and returns how it failed: "" for a 2xx,
// "timeout", "transport" or the status class.
func gwkillSend(ctx context.Context, client *http.Client, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: the compose network's services are the point
	if err != nil {
		return "transport"
	}
	resp, err := client.Do(req) //nolint:gosec // G704: the compose network's gateways are the point
	if err != nil {
		if errorTimeout(err) {
			return "timeout"
		}
		return "transport"
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return "transport"
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Sprintf("%dxx", resp.StatusCode/100)
	}
	return ""
}

// errorTimeout reports whether err is a timeout.
func errorTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// judgeGwkill applies the rules to the samples.
func judgeGwkill(o gwkillOptions, scheduled int, samples []gwkillSample, ejected []*time.Time, killAt time.Time) gwkillReport {
	var rep gwkillReport
	rep.Requests.Scheduled, rep.Requests.Sent = scheduled, len(samples)
	rep.KillRequested, rep.Killed = killAt, o.kill
	rep.Gateways = make([]gwkillGateway, len(o.gateways))
	for i, g := range o.gateways {
		rep.Gateways[i] = gwkillGateway{Addr: g, Ejected: ejected[i]}
	}
	for _, s := range samples {
		g := &rep.Gateways[s.gateway]
		g.Sent++
		if s.failure == "" {
			g.OK++
			continue
		}
		g.Failed++
		if g.FailureKinds == nil {
			g.FailureKinds = map[string]int{}
		}
		g.FailureKinds[s.failure]++
		if e := ejected[s.gateway]; e != nil && !s.sentAt.Before(*e) {
			g.LateFailures++
		}
	}
	rep.Pass = rep.verdicts(o)[0] && rep.verdicts(o)[1] && rep.verdicts(o)[2]
	return rep
}

func (r gwkillReport) verdicts(o gwkillOptions) []bool {
	k := o.killed()
	others := true
	for i, g := range r.Gateways {
		if i != k && g.Failed > 0 {
			others = false
		}
	}
	kg := r.Gateways[k]
	return []bool{
		r.Requests.Sent*100 >= r.Requests.Scheduled*99 && !r.KillRequested.IsZero(),
		others,
		kg.Ejected != nil && kg.LateFailures == 0,
	}
}

// lines are the checks, one PASS/FAIL line each.
func (r gwkillReport) lines(o gwkillOptions) []string {
	v := r.verdicts(o)
	line := func(i int, check, measured string) string {
		verdict := "FAIL"
		if v[i] {
			verdict = "PASS"
		}
		return fmt.Sprintf("%-4s  %-44s %s", verdict, check, measured)
	}
	k := o.killed()
	var others []string
	for i, g := range r.Gateways {
		if i != k {
			others = append(others, fmt.Sprintf("%s %d/%d ok, %d failed", g.Addr, g.OK, g.Sent, g.Failed))
		}
	}
	kg := r.Gateways[k]
	killed := fmt.Sprintf("no failure observed from %s: was it killed?", kg.Addr)
	if kg.Ejected != nil {
		killed = fmt.Sprintf("%d failed (%v), all sent before its first observed failure %.1f ms after the kill request; %d sent after it failed; %d ok before",
			kg.Failed, kg.FailureKinds, float64(kg.Ejected.Sub(r.KillRequested).Microseconds())/1000, kg.LateFailures, kg.OK)
	}
	return []string{
		line(0, "sender sent >= 99% of its schedule, kill sent", fmt.Sprintf("%d/%d sent; kill of %s requested at %s", r.Requests.Sent, r.Requests.Scheduled, o.kill, r.KillRequested.UTC().Format(time.RFC3339Nano))),
		line(1, "zero failed requests to the other gateways", strings.Join(others, "; ")),
		line(2, "only "+o.kill+"'s in-flight requests failed", killed),
	}
}
