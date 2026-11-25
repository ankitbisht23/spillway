package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
)

// Verdicts (ADR-0073 §D8).
const (
	verdictPass    = "pass"    // compared and not flagged
	verdictFirst   = "first"   // nothing comparable to compare with
	verdictFlagged = "flagged" // flagged; the job re-runs the set once
	verdictFail    = "fail"    // flagged, and the re-run confirmed it
	verdictInvalid = "invalid" // the run set is invalid: never compared
	verdictUngated = "ungated" // a dispatch without the gate: never compared
)

// threshold is the relative change that flags a run when it also exceeds
// the baseline's run-to-run spread (NFR-05).
const threshold = 0.10

// mainRef is the only ref whose results become baselines (ADR-0003).
const mainRef = "refs/heads/main"

// stats is one number's median, min, max and spread ((max − min) ÷ median)
// over a run set's runs, as test/load/bench.sh writes it.
type stats struct {
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Spread float64 `json:"spread"`
}

// result is the part of a bench.sh result that the gate reads.
type result struct {
	Timestamp      string   `json:"timestamp"`
	Commit         string   `json:"commit"`
	Ref            string   `json:"ref"`
	Arch           string   `json:"arch"`
	Valid          bool     `json:"valid"`
	InvalidReasons []string `json:"invalid_reasons"`
	TargetRPS      float64  `json:"target_rps"`
	AchievedRPS    stats    `json:"achieved_rps"`
	Latency        struct {
		P99 stats `json:"p99"`
	} `json:"latency_us"`
	Setup    json.RawMessage `json:"setup"`
	Hardware struct {
		CPUModel string `json:"cpu_model"`
	} `json:"hardware"`
	Gate *gate `json:"gate"`

	name  string // where it is or will be stored: results/<arch>/<timestamp>.json
	setup any    // Setup, decoded for comparison
}

// gate is what the gate stores in each result it judges.
type gate struct {
	Verdict         string       `json:"verdict"`
	Baseline        *string      `json:"baseline"`         // the nightly baseline's file
	ReleaseBaseline *string      `json:"release_baseline"` // the release baseline's file
	ReleaseTag      *string      `json:"release_tag"`
	Comparisons     []comparison `json:"comparisons"`
	RerunOf         *string      `json:"rerun_of"` // the flagged run set this one confirms or clears
	// CrossModel is set only with verdict first, when another CPU model has
	// an eligible baseline (P34): information only.
	CrossModel *crossModel `json:"cross_model,omitempty"`
}

// crossModel is P34's information-only comparison (ADR-0081, ADR-0083): a
// run set with verdict first against the newest eligible baseline of the
// same arch, target rate and setup on another CPU model. It never flags,
// never fails the job and never makes or blocks a baseline.
type crossModel struct {
	Info        string           `json:"info"`
	Baseline    string           `json:"baseline"`  // its stored file
	CPUModel    string           `json:"cpu_model"` // its CPU model
	Comparisons []infoComparison `json:"comparisons"`
}

// infoComparison is one metric of a crossModel. Change is (value − base) ÷
// base, as in comparison.
type infoComparison struct {
	Metric string  `json:"metric"` // "achieved_rps_median" or "p99_us_median"
	Base   float64 `json:"base"`
	Value  float64 `json:"value"`
	Change float64 `json:"change"`
}

// crossModelInfo is crossModel's Info.
const crossModelInfo = "information only (P34): another CPU model, never flagged, never fails, never makes or blocks a baseline"

// comparison is one metric of the candidate against one baseline. Change is
// (value − base) ÷ base, so a throughput drop is negative.
type comparison struct {
	Against string  `json:"against"` // "nightly" or "release"
	Metric  string  `json:"metric"`  // "achieved_rps_median" or "p99_us_median"
	Base    float64 `json:"base"`
	Value   float64 `json:"value"`
	Change  float64 `json:"change"`
	Spread  float64 `json:"spread"` // the baseline's run-to-run spread of the metric
	Flagged bool    `json:"flagged"`
}

// parseResult decodes one result. name is where it is or will be stored.
func parseResult(data []byte, name string) (*result, error) {
	var r result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if r.Arch == "" || r.Timestamp == "" {
		return nil, fmt.Errorf("%s: not a bench result (no arch or timestamp)", name)
	}
	if err := json.Unmarshal(r.Setup, &r.setup); err != nil {
		return nil, fmt.Errorf("%s: setup: %w", name, err)
	}
	if name == "" {
		name = storedName(&r)
	}
	r.name = name
	return &r, nil
}

// storedName is the file bench.yml stores a result as on bench-results:
// results/<arch>/<UTC timestamp without - and :>.json (ADR-0003).
func storedName(r *result) string {
	return "results/" + r.Arch + "/" + strings.NewReplacer("-", "", ":", "").Replace(r.Timestamp) + ".json"
}

// history reads the stored results of one architecture from a checkout of
// bench-results, newest first.
func history(results fs.FS, arch string) ([]*result, error) {
	names, err := fs.Glob(results, "results/"+arch+"/*.json")
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	slices.Reverse(names)
	out := make([]*result, 0, len(names))
	for _, name := range names {
		data, err := fs.ReadFile(results, name)
		if err != nil {
			return nil, err
		}
		r, err := parseResult(data, name)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// comparable reports whether b and c measured the same thing: same arch,
// recorded CPU model, target rate and setup (ADR-0073 §D8, ADR-0016). The
// whole setup object counts, trace_sample_ratio included, so results from
// before m4-tracing never compare with later ones (ADR-0030). An unrecorded
// CPU model compares with nothing.
func comparable(b, c *result) bool {
	return b.Arch == c.Arch &&
		c.Hardware.CPUModel != "" && b.Hardware.CPUModel == c.Hardware.CPUModel &&
		b.TargetRPS == c.TargetRPS &&
		reflect.DeepEqual(b.setup, c.setup)
}

// comparableButModel reports whether b and c would be comparable if b had
// c's CPU model: b has another recorded model, and c has one.
func comparableButModel(b, c *result) bool {
	if b.Hardware.CPUModel == "" || c.Hardware.CPUModel == "" || b.Hardware.CPUModel == c.Hardware.CPUModel {
		return false
	}
	bb := *b
	bb.Hardware.CPUModel = c.Hardware.CPUModel
	return comparable(&bb, c)
}

// eligible reports whether r can be a baseline: a valid result from main
// whose gate passed it, or an M1-era result stored before the gate existed.
func eligible(r *result) bool {
	if r.Ref != mainRef || !r.Valid {
		return false
	}
	return r.Gate == nil || r.Gate.Verdict == verdictPass || r.Gate.Verdict == verdictFirst
}

// baselines picks c's nightly baseline (the newest eligible comparable
// result) and its release baseline (the newest such result for the release
// commit, when one is given) from hist, newest first. Only results stored
// before c count, so a stored result gated again is never its own baseline.
func baselines(hist []*result, c *result, releaseCommit string) (nightly, release *result) {
	for _, b := range hist {
		if b.name >= c.name || !eligible(b) || !comparable(b, c) {
			continue
		}
		if nightly == nil {
			nightly = b
		}
		if release == nil && releaseCommit != "" && b.Commit == releaseCommit {
			release = b
		}
	}
	return nightly, release
}

// crossModelCompare is P34's comparison of c against the newest eligible
// result stored before it with the same arch, target rate and setup on
// another CPU model; nil when there is none.
func crossModelCompare(hist []*result, c *result) *crossModel {
	for _, b := range hist {
		if b.name >= c.name || !eligible(b) || !comparableButModel(b, c) {
			continue
		}
		x := &crossModel{Info: crossModelInfo, Baseline: b.name, CPUModel: b.Hardware.CPUModel}
		for _, m := range []struct {
			metric      string
			base, value float64
		}{
			{"achieved_rps_median", b.AchievedRPS.Median, c.AchievedRPS.Median},
			{"p99_us_median", b.Latency.P99.Median, c.Latency.P99.Median},
		} {
			x.Comparisons = append(x.Comparisons, infoComparison{Metric: m.metric, Base: m.base, Value: m.value, Change: (m.value - m.base) / m.base})
		}
		return x
	}
	return nil
}

// compare compares c with baseline b. A metric is flagged when its change
// is worse than 10% and than b's run-to-run spread of that metric.
func compare(against string, b, c *result) []comparison {
	rps := comparison{
		Against: against, Metric: "achieved_rps_median",
		Base: b.AchievedRPS.Median, Value: c.AchievedRPS.Median, Spread: b.AchievedRPS.Spread,
	}
	rps.Change = (rps.Value - rps.Base) / rps.Base
	drop := -rps.Change
	rps.Flagged = drop > threshold && drop > rps.Spread

	p99 := comparison{
		Against: against, Metric: "p99_us_median",
		Base: b.Latency.P99.Median, Value: c.Latency.P99.Median, Spread: b.Latency.P99.Spread,
	}
	p99.Change = (p99.Value - p99.Base) / p99.Base
	p99.Flagged = p99.Change > threshold && p99.Change > p99.Spread
	return []comparison{rps, p99}
}

// judge gates candidate c against the results in hist. releaseTag and
// releaseCommit name the last release ("" before any). rerunOf is the name
// of the flagged run set that c re-runs ("" for a first run set): a re-run
// flagged again is a confirmed regression, verdict fail.
func judge(hist []*result, c *result, releaseTag, releaseCommit, rerunOf string) gate {
	g := gate{Comparisons: []comparison{}}
	if releaseTag != "" {
		g.ReleaseTag = &releaseTag
	}
	if rerunOf != "" {
		g.RerunOf = &rerunOf
	}
	if !c.Valid {
		g.Verdict = verdictInvalid
		return g
	}
	nightly, release := baselines(hist, c, releaseCommit)
	if nightly == nil {
		g.Verdict = verdictFirst
		g.CrossModel = crossModelCompare(hist, c)
		return g
	}
	g.Baseline = &nightly.name
	g.Comparisons = append(g.Comparisons, compare("nightly", nightly, c)...)
	if release != nil {
		g.ReleaseBaseline = &release.name
		g.Comparisons = append(g.Comparisons, compare("release", release, c)...)
	}
	g.Verdict = verdictPass
	if slices.ContainsFunc(g.Comparisons, func(x comparison) bool { return x.Flagged }) {
		g.Verdict = verdictFlagged
		if rerunOf != "" {
			g.Verdict = verdictFail
		}
	}
	return g
}
