// Command chaos is the sender of the killed-primary chaos test (NFR-13,
// ADR-0021), run by test/chaos/redis-primary.sh in the compose network
// against gateway-1 with test/chaos/primary.yaml. It reads CLUSTER SHARDS,
// picks keys of the config's fail_open policy so that each primary owns an
// equal share, and drives every key at a constant rate (open model, like
// loadgen). After -kill-after it writes "kill <node>" on stderr, naming the
// primary that owns slot 0; the script kills that node and writes the
// container's exit time (RFC 3339) on stdin, the kill time. A response is a
// Redis decision when it carries Spillway-Decision-Time, a failure-mode
// decision when it is a 2xx without it. It passes when every request for a
// key on a surviving primary is a Redis decision, the killed primary's keys
// get failure-mode decisions only between the kill and their first Redis
// decision after it, and that failover window is at most -max-window.
// Docker records the exit up to about a millisecond after the process died,
// so a failure-mode decision is early only when it came before the sender
// asked for the kill. It prints its report as JSON on stdout and one
// PASS/FAIL line per check on stderr, and exits 0 on PASS, 1 on FAIL and 2
// on bad flags.
//
//	chaos < kill-time
//
// Two subcommands serve make verify-m4 (ADR-0031): "chaos mock", the
// fault-injecting upstream (mock.go), and "chaos upstream", the sender of
// its scenarios (upstream.go).
package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/hardware"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

const (
	keyHeader          = "X-Chaos-Key"            // the policy's key_template is {header:X-Chaos-Key}
	decisionTimeHeader = "Spillway-Decision-Time" // dev mode only (ADR-0021)
	maxInFlight        = 1000                     // a request due while this many are in flight is dropped
	// The policy of test/chaos/primary.yaml, which names its Redis keys.
	policyName   = "chaos-primary"
	policyPeriod = time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var code int
	switch args := os.Args[1:]; {
	case len(args) > 0 && args[0] == "mock":
		code = runMock(ctx, args[1:], os.Stderr)
	case len(args) > 0 && args[0] == "upstream":
		code = runUpstream(ctx, args[1:], os.Stdout, os.Stderr)
	default:
		code = run(ctx, args, os.Stdin, os.Stdout, os.Stderr)
	}
	stop()
	os.Exit(code)
}

type options struct {
	url, metrics  string
	keys          int
	rate          int // per key
	duration      time.Duration
	killAfter     time.Duration
	maxWindow     time.Duration
	configVersion int
	timeout       time.Duration
	// shards reads the cluster's shards; from -redis, or set by tests.
	shards func(ctx context.Context) ([]shard, error)
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	var seeds string
	fs := flag.NewFlagSet("chaos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.url, "url", "http://gateway-1:8000/anything/chaos", "gateway URL of the chaos route")
	fs.StringVar(&o.metrics, "metrics", "http://gateway-1:8001/metrics", "the gateway's admin metrics URL")
	fs.StringVar(&seeds, "redis", "redis-0:6379,redis-1:6379,redis-2:6379,redis-3:6379,redis-4:6379,redis-5:6379", "Redis Cluster nodes to read CLUSTER SHARDS from, the first that answers")
	fs.IntVar(&o.keys, "keys", 30, "keys, an equal share on each primary")
	fs.IntVar(&o.rate, "rate", 20, "requests per second per key")
	fs.DurationVar(&o.duration, "duration", 40*time.Second, "length of the run")
	fs.DurationVar(&o.killAfter, "kill-after", 10*time.Second, "when to ask for the kill, from the start of the run")
	fs.DurationVar(&o.maxWindow, "max-window", 10*time.Second, "longest failover window that passes")
	fs.IntVar(&o.configVersion, "config-version", 42, "version of test/chaos/primary.yaml, which the gateway must run first")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return o, err // printed by fs, with the usage
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case o.keys < 1:
		err = errors.New("-keys: must be positive")
	case o.rate < 1:
		err = errors.New("-rate: must be positive")
	case o.killAfter <= 0 || o.killAfter >= o.duration:
		err = errors.New("-kill-after: must be positive and shorter than -duration")
	case o.maxWindow <= 0:
		err = errors.New("-max-window: must be positive")
	case o.timeout <= 0:
		err = errors.New("-timeout: must be positive")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos:", err)
	}
	o.shards = func(ctx context.Context) ([]shard, error) { return clusterShards(ctx, strings.Split(seeds, ",")) }
	return o, err
}

// run is the command: it parses args, prepares and drives the run, reads
// the kill time from stdin, checks the outcomes, prints the report and
// returns the exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	return runWith(ctx, o, stdin, stdout, stderr)
}

func runWith(ctx context.Context, o options, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "chaos:", err)
		return 1
	}
	c := &sender{options: o, client: &http.Client{
		Transport: &http.Transport{MaxConnsPerHost: maxInFlight, MaxIdleConnsPerHost: maxInFlight},
		Timeout:   o.timeout,
	}}
	if err := c.waitConfig(ctx, 30*time.Second); err != nil {
		return fail(err)
	}
	shards, err := o.shards(ctx)
	if err != nil {
		return fail(fmt.Errorf("reading CLUSTER SHARDS: %w", err))
	}
	killed := slices.IndexFunc(shards, func(s shard) bool { return s.owns(0) })
	if killed < 0 {
		return fail(errors.New("no primary owns slot 0"))
	}
	keys := pickKeys(strconv.FormatInt(time.Now().UnixMilli(), 36), shards, killed, o.keys)
	if err := c.warm(ctx, keys, 30*time.Second); err != nil {
		return fail(err)
	}
	arrivals := schedule(len(keys), o.rate, o.duration)
	var requested atomic.Int64 // Unix µs
	out := c.drive(ctx, arrivals, keys, func() {
		requested.Store(time.Now().UnixMicro())
		_, _ = fmt.Fprintf(stderr, "kill %s\n", shards[killed].primary)
	})
	if ctx.Err() != nil {
		return 1
	}
	kill, err := readKillTime(stdin)
	if err != nil {
		return fail(err)
	}
	r := analyze(keys, arrivals, out, time.UnixMicro(requested.Load()), kill, o.maxWindow)
	r.KilledNode, r.KillTime, r.Hardware = shards[killed].primary, kill.UTC().Format(time.RFC3339Nano), hardware.Read()
	for _, l := range r.lines(o.maxWindow) {
		_, _ = fmt.Fprintln(stderr, l)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fail(err)
	}
	if !r.Pass {
		return 1
	}
	return 0
}

// shard is one primary of the cluster and the slots it owns.
type shard struct {
	primary string // host name, e.g. redis-0
	ranges  []goredis.SlotRange
}

func (s shard) owns(slot int) bool {
	return slices.ContainsFunc(s.ranges, func(r goredis.SlotRange) bool { return int64(slot) >= r.Start && int64(slot) <= r.End })
}

// clusterShards reads CLUSTER SHARDS from the first seed that answers and
// returns every shard that has slots and an online primary.
func clusterShards(ctx context.Context, seeds []string) ([]shard, error) {
	var err error
	for _, addr := range seeds {
		rdb := goredis.NewClient(&goredis.Options{Addr: addr, MaxRetries: -1})
		var cs []goredis.ClusterShard
		cs, err = rdb.ClusterShards(ctx).Result()
		_ = rdb.Close()
		if err != nil {
			continue
		}
		var shards []shard
		for _, s := range cs {
			for _, n := range s.Nodes {
				if n.Role == "master" && n.Health == "online" && len(s.Slots) > 0 {
					shards = append(shards, shard{n.Hostname, s.Slots})
				}
			}
		}
		return shards, nil
	}
	return nil, err
}

// key is one key the sender drives: its X-Chaos-Key value and whether the
// primary to be killed owns its slot.
type key struct {
	name   string
	killed bool
}

// pickKeys returns n keys, n/len(shards) on each primary (the remainder
// dropped), in the order found. A key's slot is that of its Redis key, the
// {k:<digest>} key of a request without a tenant (ADR-0021).
func pickKeys(runID string, shards []shard, killed, n int) []key {
	quota := n / len(shards)
	count := make([]int, len(shards))
	var keys []key
	for i := 0; len(keys) < quota*len(shards); i++ {
		name := fmt.Sprintf("%s-%d", runID, i)
		slot := keySlot(rlredis.NoTenantKey(rlredis.Digest(name), policyName, policyPeriod))
		s := slices.IndexFunc(shards, func(s shard) bool { return s.owns(slot) })
		if s < 0 || count[s] == quota {
			continue
		}
		count[s]++
		keys = append(keys, key{name, s == killed})
	}
	return keys
}

// arrival is one scheduled request: when it is due, from the start of the
// run, and for which key.
type arrival struct {
	at  time.Duration
	key int
}

// schedule has every key send rate requests per second over d, evenly
// spaced, key k offset by k/keys of the interval; sorted by due time.
func schedule(keys, rate int, d time.Duration) []arrival {
	interval := time.Second / time.Duration(rate)
	var a []arrival
	for k := range keys {
		for t := interval * time.Duration(k) / time.Duration(keys); t < d; t += interval {
			a = append(a, arrival{t, k})
		}
	}
	slices.SortStableFunc(a, func(x, y arrival) int { return cmp.Compare(x.at, y.at) })
	return a
}

type sender struct {
	options
	client *http.Client
}

// outcome is what became of one scheduled request.
type outcome struct {
	sent       bool
	due, done  time.Time
	status     int // 0: no answer
	decided    bool
	decisionUS int64 // Spillway-Decision-Time, if decided
}

// drive sends arrivals from 100 ms from now, calls kill at killAfter from
// that start, and returns each arrival's outcome. As loadgen does
// (ADR-0014), a request due while maxInFlight are in flight is dropped.
func (c *sender) drive(ctx context.Context, arrivals []arrival, keys []key, kill func()) []outcome {
	start := time.Now().Add(100 * time.Millisecond)
	timer := time.AfterFunc(time.Until(start.Add(c.killAfter)), kill)
	defer timer.Stop()
	out := make([]outcome, len(arrivals))
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
		o := &out[i]
		o.sent, o.due = true, due
		wg.Go(func() {
			defer func() { <-inFlight }()
			c.send(ctx, keys[a.key].name, o)
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

// send makes one request for key and notes its outcome in o.
func (c *sender) send(ctx context.Context, key string, o *outcome) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return
	}
	req.Header.Set(keyHeader, key)
	resp, err := c.client.Do(req) //nolint:gosec // G704: the gateway of the compose network is the point
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return
	}
	o.status, o.done = resp.StatusCode, time.Now()
	if v := resp.Header.Get(decisionTimeHeader); v != "" {
		o.decisionUS, err = strconv.ParseInt(v, 10, 64)
		o.decided = err == nil
	}
}

// waitConfig waits up to timeout for the gateway to report the chaos
// config's version.
func (c *sender) waitConfig(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		v, err := c.scrapeVersion(ctx)
		if err == nil && v == c.configVersion {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if err != nil {
				return fmt.Errorf("waiting for gw_config_version %d: %w", c.configVersion, err)
			}
			return fmt.Errorf("%s reports gw_config_version %d, want %d", c.metrics, v, c.configVersion)
		}
		sleepUntil(ctx, time.Now().Add(100*time.Millisecond))
	}
}

// scrapeVersion scrapes gw_config_version.
func (c *sender) scrapeVersion(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.metrics, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req) //nolint:gosec // G704: as send
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(body)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "gw_config_version "); ok {
			f, err := strconv.ParseFloat(v, 64)
			return int(f), err
		}
	}
	return 0, fmt.Errorf("%s has no gw_config_version", c.metrics)
}

// warm waits up to timeout for a Redis decision on every key, so the run
// starts with warm connections to every primary (ADR-0024).
func (c *sender) warm(ctx context.Context, keys []key, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, k := range keys {
		for {
			var o outcome
			c.send(ctx, k.name, &o)
			if o.decided {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return fmt.Errorf("%s does not decide key %s in Redis: last answer %d without %s", c.url, k.name, o.status, decisionTimeHeader)
			}
			sleepUntil(ctx, time.Now().Add(100*time.Millisecond))
		}
	}
	return nil
}

// readKillTime reads the killed container's exit time, RFC 3339, from the
// first line of r.
func readKillTime(r io.Reader) (time.Time, error) {
	s := bufio.NewScanner(r)
	if !s.Scan() {
		return time.Time{}, fmt.Errorf("no kill time on stdin: %w", cmp.Or(s.Err(), io.EOF))
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s.Text()))
	if err != nil {
		return time.Time{}, fmt.Errorf("kill time: %w", err)
	}
	return t, nil
}

// report is the JSON the sender prints.
type report struct {
	KilledNode string        `json:"killed_node"`
	KillTime   string        `json:"kill_time"` // the killed container's exit
	Keys       keyCounts     `json:"keys"`
	Requests   requestCounts `json:"requests"`
	Surviving  classCounts   `json:"surviving_keys"`
	Killed     classCounts   `json:"killed_keys"`
	// The killed primary's keys' failure-mode decisions answered before
	// the kill was requested or sent after the key's window ended, and the
	// first few of
	// them, with times from the kill.
	OutsideWindow  int64    `json:"failure_mode_outside_window"`
	OutsideSamples []string `json:"failure_mode_outside_window_samples,omitempty"`
	// The first few failure-mode decisions for keys on the surviving
	// primaries, with times from the kill.
	SurvivingSamples []string `json:"surviving_failure_mode_samples,omitempty"`
	// When the first failure-mode decision was answered, from the kill.
	FirstFailureModeMS float64 `json:"first_failure_mode_ms"`
	// Per killed key, from the kill to its first Redis decision after it;
	// Recovered keys had one.
	Recovered int           `json:"keys_recovered"`
	WindowMS  windowRange   `json:"failover_window_ms"`
	Pass      bool          `json:"pass"`
	Hardware  hardware.Info `json:"hardware"`
}

type keyCounts struct {
	Killed    int `json:"killed_primary"`
	Surviving int `json:"surviving_primaries"`
}

type requestCounts struct {
	Scheduled int64 `json:"scheduled"`
	Sent      int64 `json:"sent"`
	Dropped   int64 `json:"dropped"`
}

// classCounts sorts answered and unanswered requests: Redis decisions carry
// Spillway-Decision-Time; failure-mode decisions are 2xx without it; other
// is everything else (no answer, or any other status).
type classCounts struct {
	Redis       int64 `json:"redis"`
	FailureMode int64 `json:"failure_mode"`
	Other       int64 `json:"other"`
}

type windowRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// analyze checks the outcomes of a run whose kill was requested at
// requested and recorded at kill.
func analyze(keys []key, arrivals []arrival, out []outcome, requested, kill time.Time, maxWindow time.Duration) report {
	var r report
	killUS := kill.UnixMicro()
	// end[k]: the decision time of key k's first Redis decision after the
	// kill, 0 if none.
	end := make([]int64, len(keys))
	for i, o := range out {
		if k := arrivals[i].key; o.decided && keys[k].killed && o.decisionUS > killUS && (end[k] == 0 || o.decisionUS < end[k]) {
			end[k] = o.decisionUS
		}
	}
	for i, o := range out {
		r.Requests.Scheduled++
		if !o.sent {
			r.Requests.Dropped++
			continue
		}
		r.Requests.Sent++
		k := arrivals[i].key
		c := &r.Surviving
		if keys[k].killed {
			c = &r.Killed
		}
		switch {
		case o.decided:
			c.Redis++
		case o.status/100 == 2:
			c.FailureMode++
			if !keys[k].killed && len(r.SurvivingSamples) < 5 {
				r.SurvivingSamples = append(r.SurvivingSamples, fmt.Sprintf("key %s sent at %+.1f ms, answered at %+.1f ms",
					keys[k].name, float64(o.due.UnixMicro()-killUS)/1000, float64(o.done.UnixMicro()-killUS)/1000))
			}
			if ms := float64(o.done.UnixMicro()-killUS) / 1000; keys[k].killed && (c.FailureMode == 1 || ms < r.FirstFailureModeMS) {
				r.FirstFailureModeMS = ms
			}
			if keys[k].killed && (o.done.Before(requested) || end[k] != 0 && o.due.UnixMicro() > end[k]) {
				r.OutsideWindow++
				if len(r.OutsideSamples) < 5 {
					r.OutsideSamples = append(r.OutsideSamples, fmt.Sprintf("key %s sent at %+.1f ms, answered at %+.1f ms, window end %+.1f ms",
						keys[k].name, float64(o.due.UnixMicro()-killUS)/1000, float64(o.done.UnixMicro()-killUS)/1000, float64(end[k]-killUS)/1000))
				}
			}
		default:
			c.Other++
		}
	}
	var windows []float64
	for k, e := range end {
		if keys[k].killed {
			r.Keys.Killed++
		} else {
			r.Keys.Surviving++
		}
		if e != 0 {
			windows = append(windows, float64(e-killUS)/1000)
		}
	}
	r.Recovered = len(windows)
	if len(windows) > 0 {
		r.WindowMS = windowRange{slices.Min(windows), slices.Max(windows)}
	}
	r.Pass = r.Requests.Sent*100 >= r.Requests.Scheduled*99 &&
		r.Surviving.FailureMode+r.Surviving.Other == 0 &&
		r.Killed.Other == 0 && r.OutsideWindow == 0 &&
		r.Keys.Killed > 0 && r.Recovered == r.Keys.Killed && r.WindowMS.Max <= float64(maxWindow.Milliseconds())
	return r
}

// lines are r's checks, one PASS/FAIL line each, in the verify scripts'
// format.
func (r report) lines(maxWindow time.Duration) []string {
	line := func(ok bool, check, measured string) string {
		verdict := "FAIL"
		if ok {
			verdict = "PASS"
		}
		return fmt.Sprintf("%-4s  %-44s %s", verdict, check, measured)
	}
	window := "none: no killed key got a Redis decision after the kill"
	if r.Recovered > 0 {
		window = fmt.Sprintf("%.2f s (keys %.2f – %.2f s, %d/%d recovered); kill of %s at %s", r.WindowMS.Max/1000, r.WindowMS.Min/1000, r.WindowMS.Max/1000, r.Recovered, r.Keys.Killed, r.KilledNode, r.KillTime)
	}
	return []string{
		line(r.Requests.Sent*100 >= r.Requests.Scheduled*99, "sender sent >= 99% of its schedule",
			fmt.Sprintf("%d/%d sent", r.Requests.Sent, r.Requests.Scheduled)),
		line(r.Surviving.FailureMode+r.Surviving.Other == 0, "surviving primaries' keys: Redis decisions",
			fmt.Sprintf("%d keys: %d Redis decisions, %d failure-mode, %d other", r.Keys.Surviving, r.Surviving.Redis, r.Surviving.FailureMode, r.Surviving.Other)),
		line(r.Killed.Other == 0 && r.OutsideWindow == 0, "killed primary: failure modes only in window",
			fmt.Sprintf("%d keys: %d Redis decisions, %d failure-mode (first %+.1f ms from the kill, %d outside the window), %d other",
				r.Keys.Killed, r.Killed.Redis, r.Killed.FailureMode, r.FirstFailureModeMS, r.OutsideWindow, r.Killed.Other)),
		line(r.Keys.Killed > 0 && r.Recovered == r.Keys.Killed && r.WindowMS.Max <= float64(maxWindow.Milliseconds()),
			fmt.Sprintf("failover window <= %s", maxWindow), window),
	}
}
