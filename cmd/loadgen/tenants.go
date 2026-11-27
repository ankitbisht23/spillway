package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"

	"github.com/ankitbisht23/spillway/internal/loadgen"
)

// The per-tenant mode (NFR-15, ADR-0074 §D9): each -tenant sends its own
// requests to -url at its own rate, with its own X-API-Key read once from a
// file, on its own connections, so that one tenant's load cannot queue
// another's requests inside loadgen. The measured time is cut into -slot
// slots; a tenant with toggle sends only in odd slots (slot 0 off).

// apiKeyHeader carries a tenant's API key (GW-17).
const apiKeyHeader = "X-API-Key" //nolint:gosec // G101: the header's name, not a credential

// slotSettle is left out of each slot's histogram, from the slot's start,
// so a slot measures the steady state of its on or off phase.
const slotSettle = time.Second

// tenantSpec is one -tenant flag: name=<name>,rate=<rps>,key-file=<path>[,toggle].
type tenantSpec struct {
	name    string
	rate    int
	keyFile string
	toggle  bool
}

var tenantName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// parseTenant parses one -tenant value.
func parseTenant(v string) (tenantSpec, error) {
	var t tenantSpec
	for f := range strings.SplitSeq(v, ",") {
		k, val, hasValue := strings.Cut(f, "=")
		switch {
		case k == "toggle" && !hasValue:
			t.toggle = true
		case k == "name" && hasValue:
			t.name = val
		case k == "rate" && hasValue:
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return t, fmt.Errorf("-tenant %q: rate must be a positive integer", v)
			}
			t.rate = n
		case k == "key-file" && hasValue && val != "":
			t.keyFile = val
		default:
			return t, fmt.Errorf("-tenant %q: unknown or malformed field %q (want name=,rate=,key-file=[,toggle])", v, f)
		}
	}
	switch {
	case !tenantName.MatchString(t.name):
		return t, fmt.Errorf("-tenant %q: name must be 1 to 64 letters, digits, - or _", v)
	case t.rate == 0:
		return t, fmt.Errorf("-tenant %q: rate is required", v)
	case t.keyFile == "":
		return t, fmt.Errorf("-tenant %q: key-file is required", v)
	}
	return t, nil
}

// tenantFlags collects repeated -tenant flags.
type tenantFlags []tenantSpec

func (f *tenantFlags) String() string { return fmt.Sprintf("%d tenants", len(*f)) }

func (f *tenantFlags) Set(v string) error {
	t, err := parseTenant(v)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(*f, func(x tenantSpec) bool { return x.name == t.name }) {
		return fmt.Errorf("-tenant %q: name %q given twice", v, t.name)
	}
	*f = append(*f, t)
	return nil
}

// readKey reads an API key file: its content without surrounding white
// space. The key is never printed; errors name the file only.
func readKey(path string) (string, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("key file %s: %w", path, err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" || strings.ContainsAny(k, " \t\r\n") {
		return "", fmt.Errorf("key file %s: want one key on one line", path)
	}
	return k, nil
}

// tenantResult is one tenant's part of the result. AchievedRPS is sent per
// second of the time the tenant was scheduled to send (its on slots only
// for a toggle tenant), and the tenant is valid when that is at least 99%
// of its rate.
type tenantResult struct {
	TargetRPS   int                   `json:"target_rps"`
	Toggle      bool                  `json:"toggle"`
	AchievedRPS float64               `json:"achieved_rps"`
	Valid       bool                  `json:"valid"`
	Requests    loadgen.RequestCounts `json:"requests"`
	Status      map[string]int64      `json:"status"`
	Errors      map[string]int64      `json:"errors"`
	Failures    *loadgen.Failures     `json:"failures,omitempty"`
	LatencyUS   loadgen.Latencies     `json:"latency_us"`
	PooledP99   *loadgen.PooledP99    `json:"pooled_p99"`
	Slots       []loadgen.SlotResult  `json:"slots"`
}

// runTenants makes one per-tenant run: a generator per tenant, all on one
// schedule start, each sending through do(key). The top-level numbers of the
// result are the tenants' together: target is the sum of their rates,
// latencies come from all their answers, and the run is valid when every
// tenant is.
func runTenants(ctx context.Context, o options, clk loadgen.Clock, do func(apiKey string) func(context.Context, time.Time) (int, string, error)) (result, error) {
	keys := make([]string, len(o.tenants))
	for i, t := range o.tenants {
		k, err := readKey(t.keyFile)
		if err != nil {
			return result{}, err
		}
		keys[i] = k
	}
	// A shared start a little ahead, so every tenant's slots line up.
	start := clk.Now().Add(100 * time.Millisecond)
	results := make([]loadgen.Result, len(o.tenants))
	all := make([]*loadgen.Stats, len(o.tenants))
	var wg sync.WaitGroup
	for i, t := range o.tenants {
		g := loadgen.Generator{Clock: clk, Rate: t.rate, Warmup: o.warmup, Duration: o.duration, MaxInFlight: o.maxInFlight,
			Do: do(keys[i]), Start: start, Slot: o.slot, Settle: min(slotSettle, o.slot/2), Toggle: t.toggle}
		wg.Go(func() { results[i], all[i] = g.RunStats(ctx) })
	}
	wg.Wait()

	r := result{Result: loadgen.Result{
		TargetRPS: 0, Valid: true, WarmupS: o.warmup.Seconds(), DurationS: o.duration.Seconds(),
		Status: map[string]int64{}, Errors: map[string]int64{},
	}, Tenants: map[string]tenantResult{}}
	latency := hdrhistogram.New(1, loadgen.MaxLatency.Microseconds(), 3)
	for i, t := range o.tenants {
		tr := results[i]
		r.Tenants[t.name] = tenantResult{
			TargetRPS: tr.TargetRPS, Toggle: t.toggle, AchievedRPS: tr.AchievedRPS, Valid: tr.Valid,
			Requests: tr.Requests, Status: tr.Status, Errors: tr.Errors, Failures: tr.Failures,
			LatencyUS: tr.LatencyUS, PooledP99: tr.Pooled, Slots: tr.Slots,
		}
		r.TargetRPS += tr.TargetRPS
		r.Valid = r.Valid && tr.Valid
		r.Requests.Scheduled += tr.Requests.Scheduled
		r.Requests.Sent += tr.Requests.Sent
		r.Requests.Dropped += tr.Requests.Dropped
		for k, v := range tr.Status {
			r.Status[k] += v
		}
		for k, v := range tr.Errors {
			r.Errors[k] += v
		}
		if r.Protocol == "" {
			r.Protocol = tr.Protocol
		}
		r.Failures = mergeFailures(r.Failures, tr.Failures)
		latency.Merge(all[i].Latency)
	}
	r.AchievedRPS = float64(r.Requests.Sent) / o.duration.Seconds()
	r.LatencyUS = loadgen.Latencies{
		P50:  latency.ValueAtQuantile(50),
		P99:  latency.ValueAtQuantile(99),
		P999: latency.ValueAtQuantile(99.9),
		Max:  latency.Max(),
	}
	return r, nil
}

// mergeFailures combines two tenants' failures.
func mergeFailures(a, b *loadgen.Failures) *loadgen.Failures {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		c := *b
		return &c
	case b == nil:
		return a
	}
	c := *a
	c.Count += b.Count
	if b.First.Before(c.First) {
		c.First = b.First
	}
	if b.Last.After(c.Last) {
		c.Last = b.Last
	}
	c.SpanMS = float64(c.Last.Sub(c.First).Microseconds()) / 1000
	return &c
}

// invalidTenants names the tenants below 99% of their rate.
func invalidTenants(r result) error {
	var bad []string
	for name, t := range r.Tenants {
		if !t.Valid {
			bad = append(bad, fmt.Sprintf("tenant %s achieved %.1f rps, below 99%% of %d rps", name, t.AchievedRPS, t.TargetRPS))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	slices.Sort(bad)
	return errors.New(strings.Join(bad, "; "))
}
