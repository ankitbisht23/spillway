// Command accuracy is the accuracy harness v1 (NFR-06, ADR-0021). From the
// compose network it drives M keys of one fail-closed GCRA policy at twice
// their limit through gateway-1 … gateway-N, round robin, with three arrival
// patterns one after another (open model, like loadgen), and checks every
// admission at the Spillway-Decision-Time the gateway echoed against GCRA's
// bound: per key, admitted in any closed window w ≤ burst + w·rate/period.
// It prints its report as JSON on stdout and one summary line per pattern on
// stderr, and exits 0 on PASS, 1 on FAIL and 2 on bad flags.
//
//	accuracy -replicas 3
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ankitbisht23/spillway/internal/hardware"
)

const (
	keyHeader          = "X-Accuracy-Key"         // the policy's key_template is {header:X-Accuracy-Key}
	decisionTimeHeader = "Spillway-Decision-Time" // dev mode only (ADR-0021)
	maxInFlight        = 10000                    // a request due while this many are in flight is dropped
	conns              = 1000                     // most connections per gateway
)

// policy is the limit of test/accuracy/gateway.yaml's one policy.
var policy = limit{rate: 100, period: time.Second, burst: 100}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// gateway is one gateway's traffic URL and admin metrics URL.
type gateway struct{ url, metrics string }

type options struct {
	replicas      int
	keys          int
	duration      time.Duration
	configVersion int
	timeout       time.Duration
	// Set by tests; from replicas and every pattern otherwise.
	gateways []gateway
	patterns []string
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("accuracy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&o.replicas, "replicas", 1, "gateways to send to, gateway-1 … gateway-N, 1 to 5")
	fs.IntVar(&o.keys, "keys", 10, "keys per pattern, each driven at twice its limit")
	fs.DurationVar(&o.duration, "duration", time.Minute, "length of each pattern's run, whole seconds")
	fs.IntVar(&o.configVersion, "config-version", 31, "version of test/accuracy/gateway.yaml, which every gateway must run first")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return o, err // printed by fs, with the usage
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case o.replicas < 1 || o.replicas > 5:
		err = errors.New("-replicas: must be 1 to 5")
	case o.keys < 1:
		err = errors.New("-keys: must be positive")
	case o.duration < time.Second || o.duration%time.Second != 0:
		err = errors.New("-duration: must be whole seconds, at least 1s")
	case o.timeout <= 0:
		err = errors.New("-timeout: must be positive")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "accuracy:", err)
	}
	for i := 1; i <= o.replicas; i++ {
		o.gateways = append(o.gateways, gateway{fmt.Sprintf("http://gateway-%d:8000/bytes/64", i), fmt.Sprintf("http://gateway-%d:8001/metrics", i)})
	}
	o.patterns = patterns
	return o, err
}

// run is the command: it parses args, waits until every gateway runs the
// harness config, drives and checks each pattern, prints the report and
// returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	return runWith(ctx, o, stdout, stderr)
}

func runWith(ctx context.Context, o options, stdout, stderr io.Writer) int {
	h := &harness{
		options: o,
		client: &http.Client{
			Transport: &http.Transport{MaxConnsPerHost: conns, MaxIdleConnsPerHost: conns},
			Timeout:   o.timeout,
		},
	}
	runID := strconv.FormatInt(time.Now().UnixMilli(), 36)
	if err := h.waitReady(ctx, runID, 30*time.Second); err != nil {
		_, _ = fmt.Fprintln(stderr, "accuracy:", err)
		return 1
	}
	r := report{Replicas: len(o.gateways), Keys: o.keys, DurationS: o.duration.Seconds(), Pass: true}
	r.Policy.Algorithm, r.Policy.Rate, r.Policy.PeriodMS, r.Policy.Burst = "gcra", policy.rate, policy.period.Milliseconds(), policy.burst
	for _, p := range o.patterns {
		keys := make([]string, o.keys)
		for k := range keys {
			keys[k] = fmt.Sprintf("%s-%s-%d", p, runID, k) // fresh keys: no state carries over
		}
		before, err := h.redisErrors(ctx)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "accuracy:", err)
			return 1
		}
		arrivals := schedule(p, o.keys, o.duration)
		res := summarize(p, keys, arrivals, h.drive(ctx, arrivals, keys), o.duration)
		after, err := h.redisErrors(ctx)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "accuracy:", err)
			return 1
		}
		for kind, n := range after {
			res.RedisErrors[kind] = n - before[kind]
		}
		_, _ = fmt.Fprintln(stderr, res.summary())
		r.Patterns = append(r.Patterns, res)
		r.Pass = r.Pass && res.Pass
		if ctx.Err() != nil {
			break
		}
	}
	r.Hardware = hardware.Read()
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		_, _ = fmt.Fprintln(stderr, "accuracy:", err)
		return 1
	}
	if !r.Pass || ctx.Err() != nil {
		return 1
	}
	return 0
}

type harness struct {
	options
	client *http.Client
}

// outcome is what became of one scheduled request.
type outcome struct {
	sent       bool
	status     int // 0: no answer
	latency    time.Duration
	decisionUS int64 // Spillway-Decision-Time, if the response had one
	decided    bool
}

// drive sends arrivals, starting at the next whole second of the wall clock
// so that the boundary pattern's seconds are Redis TIME's seconds too, and
// returns each one's outcome. As loadgen does (ADR-0014), a request due
// while maxInFlight are in flight is dropped; one the sender reaches late is
// sent late, its latency still counted from its due time. The gateways get
// the sent ones round robin.
func (h *harness) drive(ctx context.Context, arrivals []arrival, keys []string) []outcome {
	start := time.Now().Truncate(time.Second).Add(time.Second)
	out := make([]outcome, len(arrivals))
	inFlight := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	n := 0
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
		o, g := &out[i], h.gateways[n%len(h.gateways)]
		o.sent, n = true, n+1
		wg.Go(func() {
			defer func() { <-inFlight }()
			h.send(ctx, g.url, keys[a.key], due, o)
		})
	}
	wg.Wait()
	return out
}

func sleepUntil(ctx context.Context, t time.Time) {
	d := time.Until(t)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// send makes one request for key and notes its outcome in o. Latency runs
// from the request's due time to the end of its response.
func (h *harness) send(ctx context.Context, url, key string, due time.Time, o *outcome) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set(keyHeader, key)
	resp, err := h.client.Do(req) //nolint:gosec // G704: the gateways of the compose network are the point
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return
	}
	o.status, o.latency = resp.StatusCode, time.Since(due)
	if v := resp.Header.Get(decisionTimeHeader); v != "" {
		o.decisionUS, err = strconv.ParseInt(v, 10, 64)
		o.decided = err == nil
	}
}

// probes is how many keys of its own waitReady sends through each gateway:
// enough that every primary owns some of them, all but certainly
// (3·(2/3)^30 ≈ 2·10⁻⁵ that one owns none).
const probes = 30

// waitReady waits up to timeout for every gateway to report the harness
// config's version, then for each to decide a request for every probe key
// in Redis (a response with Spillway-Decision-Time). Gateways warm their
// Redis connections in the background and readiness does not wait for it
// (ADR-0024), so this makes the measured requests meet warm connections
// on every primary, whatever the gateway's logs still hold.
func (h *harness) waitReady(ctx context.Context, runID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wait := func() bool {
		sleepUntil(ctx, time.Now().Add(100*time.Millisecond))
		return time.Now().Before(deadline) && ctx.Err() == nil
	}
	for _, g := range h.gateways {
		for {
			v, err := h.scrape(ctx, g.metrics, "gw_config_version")
			if err == nil && v[""] == int64(h.configVersion) {
				break
			}
			if !wait() {
				if err != nil {
					return fmt.Errorf("waiting for gw_config_version %d: %w", h.configVersion, err)
				}
				return fmt.Errorf("%s reports gw_config_version %d, want %d", g.metrics, v[""], h.configVersion)
			}
		}
	}
	for i, g := range h.gateways {
		for k := range probes {
			for {
				var o outcome
				h.send(ctx, g.url, fmt.Sprintf("probe-%s-%d-%d", runID, i, k), time.Now(), &o)
				if o.decided {
					break
				}
				if !wait() {
					return fmt.Errorf("%s does not decide requests in Redis: last answer %d without %s", g.url, o.status, decisionTimeHeader)
				}
			}
		}
	}
	return nil
}

// redisErrors is gw_redis_errors_total by kind, summed over the gateways.
func (h *harness) redisErrors(ctx context.Context) (map[string]int64, error) {
	sum := map[string]int64{}
	for _, g := range h.gateways {
		v, err := h.scrape(ctx, g.metrics, "gw_redis_errors_total")
		if err != nil {
			return nil, err
		}
		for labels, n := range v {
			sum[strings.TrimSuffix(strings.TrimPrefix(labels, `{kind="`), `"}`)] += n
		}
	}
	return sum, nil
}

// scrape returns the samples of the metric name at url, by their label set
// ("" for none).
func (h *harness) scrape(ctx context.Context, url, name string) (map[string]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req) //nolint:gosec // G704: as send
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	v := map[string]int64{}
	for line := range strings.Lines(string(body)) {
		series, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		labels, isName := strings.CutPrefix(series, name)
		if !ok || !isName || labels != "" && labels[0] != '{' {
			continue
		}
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", url, series, err)
		}
		v[labels] = int64(f)
	}
	return v, nil
}

// report is the JSON the harness prints.
type report struct {
	Replicas  int     `json:"replicas"`
	Keys      int     `json:"keys"`
	DurationS float64 `json:"duration_s"`
	Policy    struct {
		Algorithm string `json:"algorithm"`
		Rate      int64  `json:"rate"` // cluster-wide
		PeriodMS  int64  `json:"period_ms"`
		Burst     int64  `json:"burst"`
	} `json:"policy"`
	Patterns []patternResult `json:"patterns"`
	Pass     bool            `json:"pass"`
	Hardware hardware.Info   `json:"hardware"`
}

type patternResult struct {
	Pattern  string        `json:"pattern"`
	Keys     int           `json:"keys"`
	Requests requestCounts `json:"requests"`
	Status   statusCounts  `json:"status"`
	// 2xx without Spillway-Decision-Time: decided by a failure mode, or in
	// any case not by Redis, so the bound cannot be checked on them.
	FailureModeAdmissions int64            `json:"failure_mode_admissions"`
	MaxOverAdmission      overAdmitted     `json:"max_over_admission"`
	MeanAdmittedRPS       float64          `json:"mean_admitted_rps_per_key"` // 2xx per key per second
	LimitRPS              float64          `json:"limit_rps"`
	P99LatencyUS          int64            `json:"p99_decision_latency_us"` // of every answer, from its due time
	RedisErrors           map[string]int64 `json:"redis_errors"`            // gw_redis_errors_total's increase over every gateway
	Pass                  bool             `json:"pass"`
}

type requestCounts struct {
	Scheduled int64 `json:"scheduled"`
	Sent      int64 `json:"sent"`
	Dropped   int64 `json:"dropped"`
}

type statusCounts struct {
	OK          int64 `json:"2xx"`
	Limited     int64 `json:"429"`
	Unavailable int64 `json:"503"`
	Other       int64 `json:"other"`
	NoAnswer    int64 `json:"no_answer"`
}

type overAdmitted struct {
	Count    int64  `json:"count"`
	Key      string `json:"key,omitempty"`
	WindowMS int64  `json:"window_ms,omitempty"`
	AtUS     int64  `json:"at_us,omitempty"` // the window's start, a decision time
}

// summarize checks one pattern's outcomes. It passes when no key's
// admissions exceed the bound, at least 99% of the scheduled requests were
// sent, every 2xx carried a decision time (ADR-0021), and every sent request
// got a 2xx, 429 or 503: one with no answer or another status may have been
// admitted unseen (ADR-0028). 503s are reported, not judged.
func summarize(pattern string, keys []string, arrivals []arrival, out []outcome, d time.Duration) patternResult {
	r := patternResult{Pattern: pattern, Keys: len(keys), LimitRPS: float64(policy.rate) / policy.period.Seconds(), RedisErrors: map[string]int64{}}
	admitted := make([][]int64, len(keys))
	var latencies []int64
	for i, o := range out {
		r.Requests.Scheduled++
		if !o.sent {
			r.Requests.Dropped++
			continue
		}
		r.Requests.Sent++
		if o.status == 0 {
			r.Status.NoAnswer++
			continue
		}
		latencies = append(latencies, o.latency.Microseconds())
		switch {
		case o.status/100 == 2:
			r.Status.OK++
			if o.decided {
				admitted[arrivals[i].key] = append(admitted[arrivals[i].key], o.decisionUS)
			} else {
				r.FailureModeAdmissions++
			}
		case o.status == http.StatusTooManyRequests:
			r.Status.Limited++
		case o.status == http.StatusServiceUnavailable:
			r.Status.Unavailable++
		default:
			r.Status.Other++
		}
	}
	for k, a := range admitted {
		slices.Sort(a)
		if over, w, at := overAdmission(a, policy); over > r.MaxOverAdmission.Count {
			r.MaxOverAdmission = overAdmitted{over, keys[k], w.Milliseconds(), at}
		}
	}
	r.MeanAdmittedRPS = float64(r.Status.OK) / float64(len(keys)) / d.Seconds()
	if len(latencies) > 0 {
		slices.Sort(latencies)
		r.P99LatencyUS = latencies[(len(latencies)*99+99)/100-1]
	}
	r.Pass = r.MaxOverAdmission.Count == 0 && r.Requests.Sent*100 >= r.Requests.Scheduled*99 && r.FailureModeAdmissions == 0 &&
		r.Status.Other == 0 && r.Status.NoAnswer == 0
	return r
}

// summary is r as one line for stderr.
func (r patternResult) summary() string {
	verdict, over := "PASS", "0"
	if !r.Pass {
		verdict = "FAIL"
	}
	if m := r.MaxOverAdmission; m.Count > 0 {
		over = fmt.Sprintf("%d (key %s, %d ms window from %d µs)", m.Count, m.Key, m.WindowMS, m.AtUS)
	}
	var redis []string
	for _, kind := range slices.Sorted(maps.Keys(r.RedisErrors)) {
		redis = append(redis, fmt.Sprintf("%s %d", kind, r.RedisErrors[kind]))
	}
	return fmt.Sprintf("%-8s %s  %d/%d sent; 2xx %d (%d failure-mode), 429 %d, 503 %d, other %d, no answer %d; max over-admission %s; %.1f/s admitted per key, limit %.0f/s; p99 %.1f ms; redis errors: %s",
		r.Pattern, verdict, r.Requests.Sent, r.Requests.Scheduled, r.Status.OK, r.FailureModeAdmissions, r.Status.Limited, r.Status.Unavailable, r.Status.Other, r.Status.NoAnswer,
		over, r.MeanAdmittedRPS, r.LimitRPS, float64(r.P99LatencyUS)/1000, strings.Join(redis, ", "))
}
