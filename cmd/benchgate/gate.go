package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"regexp"
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

	name     string     // where it is or will be stored: results/<arch>/<timestamp>.json
	setup    any        // Setup, decoded for comparison
	excluded *exclusion // its entry on the exclusion list, if any: never a baseline
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
	// Excluded names the run sets on the exclusion list that would otherwise
	// have been considered as a baseline (ADR-0107), newest first.
	Excluded []string `json:"excluded,omitempty"`
}

// exclusion is one entry of the committed exclusion list
// (test/load/bench-baseline-exclusions.json, ADR-0107): a stored run set
// that can never be a nightly, release or P34 baseline.
type exclusion struct {
	Arch   string `json:"arch"`
	Path   string `json:"path"` // its stored name, results/<arch>/<timestamp>.json
	Reason string `json:"reason"`
	ADR    string `json:"adr"` // the ADR that records it, ADR-NNNN
}

var (
	exclusionPath = regexp.MustCompile(`^results/(x64|arm64)/[0-9]{8}T[0-9]{6}Z\.json$`)
	exclusionADR  = regexp.MustCompile(`^ADR-[0-9]{4}$`)
)

// parseExclusions decodes and checks the exclusion list. Anything it does
// not recognise is an error, never skipped: an unknown field, a path that is
// not a stored name of its arch, an empty reason, a bad ADR or a duplicate.
func parseExclusions(data []byte) ([]exclusion, error) {
	var list struct {
		Exclusions *[]exclusion `json:"exclusions"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the exclusion list")
	}
	if list.Exclusions == nil {
		return nil, errors.New(`no "exclusions" array`)
	}
	seen := map[string]bool{}
	for i, e := range *list.Exclusions {
		m := exclusionPath.FindStringSubmatch(e.Path)
		switch {
		case m == nil:
			return nil, fmt.Errorf("exclusion %d: path %q is not results/<x64|arm64>/<YYYYMMDDTHHMMSSZ>.json", i, e.Path)
		case m[1] != e.Arch:
			return nil, fmt.Errorf("exclusion %d: arch %q does not match path %s", i, e.Arch, e.Path)
		case strings.TrimSpace(e.Reason) == "":
			return nil, fmt.Errorf("exclusion %d (%s): no reason", i, e.Path)
		case !exclusionADR.MatchString(e.ADR):
			return nil, fmt.Errorf("exclusion %d (%s): adr %q is not ADR-NNNN", i, e.Path, e.ADR)
		case seen[e.Path]:
			return nil, fmt.Errorf("exclusion %d: %s is listed twice", i, e.Path)
		}
		seen[e.Path] = true
	}
	return *list.Exclusions, nil
}

// applyExclusions marks the results in hist (one arch's history) that the
// list excludes. An entry of arch that names no stored result is an error:
// a typo must not leave the outlier a baseline.
func applyExclusions(hist []*result, list []exclusion, arch string) error {
	byName := make(map[string]*result, len(hist))
	for _, r := range hist {
		byName[r.name] = r
	}
	for i := range list {
		e := &list[i]
		if e.Arch != arch {
			continue
		}
		r, ok := byName[e.Path]
		if !ok {
			return fmt.Errorf("exclusion %s (%s): no such stored run set", e.Path, e.ADR)
		}
		r.excluded = e
	}
	return nil
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
// whose gate passed it, or an M1-era result stored before the gate existed,
// and not on the exclusion list.
func eligible(r *result) bool {
	return r.excluded == nil && eligibleUnlisted(r)
}

// eligibleUnlisted is eligible without the exclusion list.
func eligibleUnlisted(r *result) bool {
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

// skipped names the excluded results in hist that would otherwise have
// been considered as c's nightly or release baseline, and with crossModel
// also as its P34 baseline: stored before c, eligible but for the list, and
// comparable with c.
func skipped(hist []*result, c *result, crossModel bool) []string {
	var out []string
	for _, b := range hist {
		if b.excluded == nil || b.name >= c.name || !eligibleUnlisted(b) {
			continue
		}
		if comparable(b, c) || (crossModel && comparableButModel(b, c)) {
			out = append(out, b.name)
		}
	}
	return out
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
		g.Excluded = skipped(hist, c, true)
		return g
	}
	g.Excluded = skipped(hist, c, false)
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
