package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// "chaos latency" is the sender of the Redis-latency chaos test (ADR-0074
// §D9), run by test/chaos/redis-latency.sh in the compose network against
// gateway-4, whose Redis traffic goes through toxiproxy (one proxy per node,
// SPILLWAY_REDIS_ADDR_MAP) and which runs test/chaos/latency.yaml: three
// routes, each with one Redis policy, fail_open, fail_closed and degraded
// (50/s, burst 50). It drives each route at -rate for -duration (open
// model); from -toxic-from to -toxic-to it puts a -latency toxic on every
// proxy through toxiproxy's API. Over the toxic window minus its first
// second: fail_open answers >= 99.9% 2xx; fail_closed answers 503 "rate
// limit unavailable" (reason fail_closed) to every request; degraded admits
// within ±10% of rate × window + burst; and each route's median latency is
// at most its pre-toxic median + 10 ms, since calls end at the budget, not
// 50 ms later. From 5 s after the toxic's removal every answer is a Redis
// decision again: it carries Spillway-Decision-Time, or it is a deny-cache
// 429, which carries neither that nor Spillway-Redis-Error (a cached deny
// replays a Redis deny without a call, RL-19); none carries
// Spillway-Redis-Error. It prints its report as JSON on stdout and one
// PASS/FAIL line per check on stderr, removes its toxics on every exit, and
// exits 0 on PASS, 1 on FAIL and 2 on bad flags.
//
//	chaos latency [-gateway http://gateway-4:8000] [-toxiproxy http://toxiproxy:8474]

// The routes of test/chaos/latency.yaml, in route order: their failure
// modes and paths.
var (
	latencyRoutes = []string{"fail_open", "fail_closed", "degraded"}
	latencyPaths  = []string{"/anything/latency-open", "/anything/latency-closed", "/anything/latency-degraded"}
)

type latencyOptions struct {
	gateway, metrics, toxiproxy string
	proxies                     []string
	configVersion               int
	rate                        int // per route
	duration                    time.Duration
	toxicFrom, toxicTo          time.Duration
	latency                     time.Duration
	settleIn                    time.Duration // the window's first part left out
	recoverIn                   time.Duration // after the toxic's removal
	slack                       time.Duration // on each route's median
	degradedRate, degradedBurst int
	timeout                     time.Duration
}

func parseLatencyFlags(args []string, stderr io.Writer) (latencyOptions, error) {
	o := latencyOptions{}
	var proxies string
	fs := flag.NewFlagSet("chaos latency", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.gateway, "gateway", "http://gateway-4:8000", "the gateway's traffic listener")
	fs.StringVar(&o.metrics, "metrics", "http://gateway-4:8001/metrics", "the gateway's admin metrics")
	fs.StringVar(&o.toxiproxy, "toxiproxy", "http://toxiproxy:8474", "toxiproxy's API")
	fs.StringVar(&proxies, "proxies", "redis-0,redis-1,redis-2,redis-3,redis-4,redis-5", "the proxies to put the toxic on")
	fs.IntVar(&o.configVersion, "config-version", 73, "version of test/chaos/latency.yaml, which the gateway must run first")
	fs.IntVar(&o.rate, "rate", 100, "requests per second per route")
	fs.DurationVar(&o.duration, "duration", 60*time.Second, "length of the run")
	fs.DurationVar(&o.toxicFrom, "toxic-from", 20*time.Second, "when the toxic goes on, from the start")
	fs.DurationVar(&o.toxicTo, "toxic-to", 40*time.Second, "when it comes off")
	fs.DurationVar(&o.latency, "latency", 50*time.Millisecond, "the toxic's added latency")
	fs.DurationVar(&o.settleIn, "settle", time.Second, "the toxic window's first part, left out of its rules")
	fs.DurationVar(&o.recoverIn, "recover", 5*time.Second, "Redis decides again within this of the toxic's removal")
	fs.DurationVar(&o.slack, "slack", 10*time.Millisecond, "most rise of each route's median latency")
	fs.IntVar(&o.degradedRate, "degraded-rate", 50, "the degraded policy's rate per second")
	fs.IntVar(&o.degradedBurst, "degraded-burst", 50, "and its burst")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	for p := range strings.SplitSeq(proxies, ",") {
		if p = strings.TrimSpace(p); p != "" {
			o.proxies = append(o.proxies, p)
		}
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case len(o.proxies) == 0:
		err = errors.New("-proxies: none")
	case o.rate < 1 || o.degradedRate < 1 || o.degradedBurst < 1:
		err = errors.New("-rate, -degraded-rate and -degraded-burst: must be positive")
	case o.toxicFrom <= 0 || o.toxicTo <= o.toxicFrom+o.settleIn || o.toxicTo+o.recoverIn >= o.duration:
		err = errors.New("want 0 < -toxic-from, -toxic-from + -settle < -toxic-to and -toxic-to + -recover < -duration")
	case o.latency <= 0 || o.timeout <= 0 || o.settleIn < 0 || o.recoverIn < 0 || o.slack < 0:
		err = errors.New("-latency and -timeout must be positive; -settle, -recover and -slack not negative")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos latency:", err)
	}
	return o, err
}

// latencySample is one request's outcome.
type latencySample struct {
	route      int
	sent       bool
	due, done  time.Time
	status     int // 0: no answer
	decided    bool
	redisError string
	failClosed bool // 503 "rate limit unavailable"
}

// latencyRoute is one route's numbers in the report.
type latencyRoute struct {
	Name string `json:"name"`
	// The toxic window minus its first second.
	Window struct {
		Requests   int     `json:"requests"`
		OK         int     `json:"2xx"`
		FailClosed int     `json:"fail_closed_503"`
		Other      int     `json:"other"`
		MedianMS   float64 `json:"median_ms"`
	} `json:"window"`
	PreMedianMS float64 `json:"pre_toxic_median_ms"`
	// From -recover after the toxic's removal to the end.
	Recovered struct {
		Requests      int `json:"requests"`
		Decided       int `json:"decided"`
		CachedDenies  int `json:"cached_denies"`
		RedisErrors   int `json:"redis_errors"`
		NotDecidedNow int `json:"not_decided"`
	} `json:"recovered"`
}

type latencyReport struct {
	Requests struct {
		Scheduled int `json:"scheduled"`
		Sent      int `json:"sent"`
	} `json:"requests"`
	ToxicOn  time.Time      `json:"toxic_on"`
	ToxicOff time.Time      `json:"toxic_off"`
	WindowS  float64        `json:"window_s"`
	Routes   []latencyRoute `json:"routes"`
	Expected struct {
		DegradedAdmits float64 `json:"degraded_admits"`
	} `json:"expected"`
	Pass bool `json:"pass"`
}

func runLatency(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseLatencyFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	return runLatencyWith(ctx, o, stdout, stderr)
}

func runLatencyWith(ctx context.Context, o latencyOptions, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "chaos latency:", err)
		return 1
	}
	client := &http.Client{
		Transport: &http.Transport{MaxConnsPerHost: maxConnsPerGateway, MaxIdleConnsPerHost: maxConnsPerGateway},
		Timeout:   o.timeout,
	}
	tox := &toxiproxy{base: o.toxiproxy, client: &http.Client{Timeout: 5 * time.Second}}
	// No toxic left from an earlier run, and none left by this one.
	for _, p := range o.proxies {
		_ = tox.remove(ctx, p)
	}
	defer func() {
		for _, p := range o.proxies {
			_ = tox.remove(context.WithoutCancel(ctx), p)
		}
	}()
	if err := waitVersion(ctx, client, o.metrics, o.configVersion, 30*time.Second); err != nil {
		return fail(err)
	}
	if err := waitDecided(ctx, client, o.gateway+latencyPaths[0], 30*time.Second); err != nil {
		return fail(err)
	}

	arrivals := schedule(len(latencyRoutes), o.rate, o.duration)
	samples := make([]latencySample, len(arrivals))
	start := time.Now().Add(100 * time.Millisecond)
	var on, off time.Time
	var toxErr error
	var mu sync.Mutex
	toxics := sync.WaitGroup{}
	toxics.Go(func() {
		sleepUntil(ctx, start.Add(o.toxicFrom))
		err := tox.addAll(ctx, o.proxies, o.latency)
		mu.Lock()
		on, toxErr = time.Now(), err
		mu.Unlock()
		sleepUntil(ctx, start.Add(o.toxicTo))
		for _, p := range o.proxies {
			if err := tox.remove(context.WithoutCancel(ctx), p); err != nil {
				mu.Lock()
				toxErr = errors.Join(toxErr, err)
				mu.Unlock()
			}
		}
		mu.Lock()
		off = time.Now()
		mu.Unlock()
	})
	inFlight := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i, a := range arrivals {
		due := start.Add(a.at)
		sleepUntil(ctx, due)
		if ctx.Err() != nil {
			break
		}
		select {
		case inFlight <- struct{}{}:
		default:
			continue
		}
		s := &samples[i]
		s.route, s.sent, s.due = a.key, true, due
		wg.Go(func() {
			defer func() { <-inFlight }()
			latencySend(ctx, client, o.gateway+latencyPaths[s.route], s)
		})
	}
	wg.Wait()
	toxics.Wait()
	if ctx.Err() != nil {
		return 1
	}
	if toxErr != nil {
		return fail(fmt.Errorf("toxiproxy: %w", toxErr))
	}
	for i, a := range arrivals { // dropped requests still name their route
		samples[i].route = a.key
	}
	rep := judgeLatency(o, samples, on, off)
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

// latencySend makes one request and notes its outcome in s.
func latencySend(ctx context.Context, client *http.Client, url string, s *latencySample) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: the compose network's services are the point
	if err != nil {
		return
	}
	resp, err := client.Do(req) //nolint:gosec // G704: the compose network's gateway is the point
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	s.status, s.done = resp.StatusCode, time.Now()
	s.redisError = resp.Header.Get(redisErrorHeader)
	s.decided = resp.Header.Get(decisionTimeHeader) != ""
	s.failClosed = resp.StatusCode == http.StatusServiceUnavailable && bytes.Contains(body, []byte("rate limit unavailable"))
}

// judgeLatency applies the rules to the samples, given when the toxic went
// on and came off.
func judgeLatency(o latencyOptions, samples []latencySample, on, off time.Time) latencyReport {
	var rep latencyReport
	rep.ToxicOn, rep.ToxicOff = on, off
	from, to := on.Add(o.settleIn), off
	recovered := off.Add(o.recoverIn)
	rep.WindowS = to.Sub(from).Seconds()
	rep.Expected.DegradedAdmits = float64(o.degradedRate)*rep.WindowS + float64(o.degradedBurst)
	var pre, in [3][]float64
	rep.Routes = make([]latencyRoute, len(latencyRoutes))
	for i := range rep.Routes {
		rep.Routes[i].Name = latencyRoutes[i]
	}
	// The pre-toxic window starts after a warm-up of a quarter of the time
	// before the toxic, at most 5 s.
	preFrom := samples[0].due
	for _, s := range samples {
		if s.sent && (preFrom.IsZero() || s.due.Before(preFrom)) {
			preFrom = s.due
		}
	}
	preFrom = preFrom.Add(min(5*time.Second, on.Sub(preFrom)/4))
	for _, s := range samples {
		rep.Requests.Scheduled++
		if !s.sent {
			continue
		}
		rep.Requests.Sent++
		r := &rep.Routes[s.route]
		ms := float64(s.done.Sub(s.due).Microseconds()) / 1000
		switch {
		case !s.due.Before(preFrom) && s.due.Before(on) && s.status != 0:
			pre[s.route] = append(pre[s.route], ms)
		case !s.due.Before(from) && s.due.Before(to):
			r.Window.Requests++
			switch {
			case s.status/100 == 2:
				r.Window.OK++
			case s.failClosed:
				r.Window.FailClosed++
			default:
				r.Window.Other++
			}
			if s.status != 0 {
				in[s.route] = append(in[s.route], ms)
			}
		case !s.due.Before(recovered):
			r.Recovered.Requests++
			cached := s.status == http.StatusTooManyRequests && !s.decided && s.redisError == ""
			switch {
			case s.redisError != "":
				r.Recovered.RedisErrors++
			case s.decided:
				r.Recovered.Decided++
			case cached:
				r.Recovered.CachedDenies++
			default:
				r.Recovered.NotDecidedNow++
			}
		}
	}
	for i := range rep.Routes {
		rep.Routes[i].PreMedianMS = median(pre[i])
		rep.Routes[i].Window.MedianMS = median(in[i])
	}
	rep.Pass = !slices.Contains(rep.verdicts(o), false)
	return rep
}

// median of v; 0 for none.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// verdicts are the checks in lines' order.
func (r latencyReport) verdicts(o latencyOptions) []bool {
	open, closed, degraded := r.Routes[0].Window, r.Routes[1].Window, r.Routes[2].Window
	out := []bool{
		r.Requests.Sent*100 >= r.Requests.Scheduled*99 && !r.ToxicOn.IsZero() && !r.ToxicOff.IsZero(),
		open.Requests > 0 && open.OK*1000 >= open.Requests*999,
		closed.Requests > 0 && closed.FailClosed == closed.Requests,
		degraded.Requests > 0 && float64(degraded.OK) >= 0.9*r.Expected.DegradedAdmits && float64(degraded.OK) <= 1.1*r.Expected.DegradedAdmits,
	}
	slack := float64(o.slack.Microseconds()) / 1000
	for _, rt := range r.Routes {
		out = append(out, rt.Window.Requests > 0 && rt.PreMedianMS > 0 && rt.Window.MedianMS <= rt.PreMedianMS+slack)
	}
	for _, rt := range r.Routes {
		rec := rt.Recovered
		out = append(out, rec.Requests > 0 && rec.RedisErrors == 0 && rec.NotDecidedNow == 0)
	}
	return out
}

// lines are the checks, one PASS/FAIL line each, in the verify scripts'
// format.
func (r latencyReport) lines(o latencyOptions) []string {
	v := r.verdicts(o)
	line := func(i int, check, measured string) string {
		verdict := "FAIL"
		if v[i] {
			verdict = "PASS"
		}
		return fmt.Sprintf("%-4s  %-44s %s", verdict, check, measured)
	}
	open, closed, degraded := r.Routes[0].Window, r.Routes[1].Window, r.Routes[2].Window
	out := []string{
		line(0, "sender sent >= 99%, toxic on and off", fmt.Sprintf("%d/%d sent; toxic on at %s, off at %s; judged window %.1f s",
			r.Requests.Sent, r.Requests.Scheduled, r.ToxicOn.UTC().Format(time.RFC3339Nano), r.ToxicOff.UTC().Format(time.RFC3339Nano), r.WindowS)),
		line(1, "fail_open: >= 99.9% 2xx under +latency", fmt.Sprintf("%d/%d 2xx, %d other", open.OK, open.Requests, open.Requests-open.OK)),
		line(2, "fail_closed: every answer 503 fail_closed", fmt.Sprintf("%d/%d 503 fail_closed, %d 2xx, %d other", closed.FailClosed, closed.Requests, closed.OK, closed.Other)),
		line(3, "degraded: admits within ±10% of expected", fmt.Sprintf("%d admitted of %d; expected %.0f (%d/s × %.1f s + %d), allowed %.0f – %.0f",
			degraded.OK, degraded.Requests, r.Expected.DegradedAdmits, o.degradedRate, r.WindowS, o.degradedBurst, 0.9*r.Expected.DegradedAdmits, 1.1*r.Expected.DegradedAdmits)),
	}
	for i, rt := range r.Routes {
		out = append(out, line(4+i, fmt.Sprintf("%s: median <= pre-toxic + %s", rt.Name, o.slack),
			fmt.Sprintf("median %.2f ms under the toxic, %.2f ms before (+%.2f ms)", rt.Window.MedianMS, rt.PreMedianMS, rt.Window.MedianMS-rt.PreMedianMS)))
	}
	for i, rt := range r.Routes {
		rec := rt.Recovered
		out = append(out, line(4+len(r.Routes)+i, fmt.Sprintf("%s: Redis decides %s after removal", rt.Name, o.recoverIn),
			fmt.Sprintf("%d answers: %d with Spillway-Decision-Time, %d deny-cache 429s, %d with Spillway-Redis-Error, %d neither", rec.Requests, rec.Decided, rec.CachedDenies, rec.RedisErrors, rec.NotDecidedNow)))
	}
	return out
}

// waitVersion waits up to timeout for metrics to report gw_config_version
// version.
func waitVersion(ctx context.Context, client *http.Client, metrics string, version int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	want := "gw_config_version " + strconv.Itoa(version)
	var last error
	for {
		body, err := getBody(ctx, client, metrics)
		if err == nil {
			if slices.Contains(strings.Split(string(body), "\n"), want) {
				return nil
			}
			last = fmt.Errorf("%s: no %q", metrics, want)
		} else {
			last = err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("waiting for gw_config_version %d: %w", version, last)
		}
		sleepUntil(ctx, time.Now().Add(100*time.Millisecond))
	}
}

// waitDecided waits up to timeout for url to be answered with
// Spillway-Decision-Time: the gateway decides in Redis.
func waitDecided(ctx context.Context, client *http.Client, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var s latencySample
		latencySend(ctx, client, url, &s)
		if s.decided {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%s: no Redis decision within %s (status %d, Spillway-Redis-Error %q)", url, timeout, s.status, s.redisError)
		}
		sleepUntil(ctx, time.Now().Add(200*time.Millisecond))
	}
}

func getBody(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req) //nolint:gosec // G704: the compose network's services are the point
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// toxiproxy is a client of toxiproxy's API, for the latency toxic.
type toxiproxy struct {
	base   string
	client *http.Client
}

// toxicName is the toxic this test adds and removes.
const toxicName = "chaos-latency"

// addAll puts a latency toxic of d on each proxy's responses.
func (t *toxiproxy) addAll(ctx context.Context, proxies []string, d time.Duration) error {
	body, _ := json.Marshal(map[string]any{
		"name": toxicName, "type": "latency", "stream": "downstream", "toxicity": 1.0,
		"attributes": map[string]any{"latency": d.Milliseconds(), "jitter": 0},
	})
	var errs []error
	for _, p := range proxies {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/proxies/"+p+"/toxics", bytes.NewReader(body)) //nolint:gosec // G704: the compose network's services are the point
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := t.client.Do(req) //nolint:gosec // G704: toxiproxy's API in the compose network
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			errs = append(errs, fmt.Errorf("adding the toxic to %s: status %d", p, resp.StatusCode))
		}
	}
	return errors.Join(errs...)
}

// remove takes the toxic off proxy p; a toxic that is not there is no error.
func (t *toxiproxy) remove(ctx context.Context, p string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.base+"/proxies/"+p+"/toxics/"+toxicName, nil) //nolint:gosec // G704: toxiproxy's API in the compose network
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req) //nolint:gosec // G704: as addAll
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("removing the toxic from %s: status %d", p, resp.StatusCode)
	}
	return nil
}
