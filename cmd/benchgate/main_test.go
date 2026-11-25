package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are trimmed bench.sh results: testdata/result.json is a
// gated-era result from main (x64, Xeon 8370C, 4,600 rps, median 4,600
// rps with spread 0.2%, p99 2.5 ms with spread 24%); testdata/legacy.json
// is the M1 baseline as stored (no gate field, no trace_sample_ratio).

const (
	xeon = "Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz"
	epyc = "AMD EPYC 7763 64-Core Processor"
)

// fixture returns testdata/<file> with edit applied to its decoded fields.
func fixture(t *testing.T, file string, edit func(m map[string]any)) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.Clean(file)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// at sets the timestamp, ref and gate verdict ("" for none) of a result.
func at(ts, ref, verdict string) func(m map[string]any) {
	return func(m map[string]any) {
		m["timestamp"] = ts
		m["ref"] = ref
		if verdict != "" {
			m["gate"] = map[string]any{"verdict": verdict}
		}
	}
}

func set(m map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		m = m[k].(map[string]any)
	}
	m[keys[len(keys)-1]] = v
}

// edits combines edits.
func edits(fs ...func(m map[string]any)) func(m map[string]any) {
	return func(m map[string]any) {
		for _, f := range fs {
			f(m)
		}
	}
}

func with(path string, v any) func(m map[string]any) {
	return func(m map[string]any) { set(m, path, v) }
}

// store writes a result into a bench-results checkout at its stored name.
func store(t *testing.T, dir string, data []byte) string {
	t.Helper()
	r, err := parseResult(data, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filepath.FromSlash(r.name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // G703: a file in the test's own temp directory
		t.Fatal(err)
	}
	return r.name
}

// candidate writes data as a local result file and returns its path.
func candidate(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // G703: a file in the test's own temp directory
		t.Fatal(err)
	}
	return path
}

// gateRun runs benchgate and returns its exit code, verdict and summary.
func gateRun(t *testing.T, args ...string) (int, gate, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	var g gate
	if code != 2 {
		if err := json.Unmarshal(stdout.Bytes(), &g); err != nil {
			t.Fatalf("verdict %q: %v (stderr %s)", stdout.String(), err, stderr.String())
		}
	}
	return code, g, stderr.String()
}

func readGate(t *testing.T, path string) gate {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ Gate gate }
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r.Gate
}

func flagged(g gate) []string {
	var out []string
	for _, c := range g.Comparisons {
		if c.Flagged {
			out = append(out, c.Against+" "+c.Metric)
		}
	}
	return out
}

const (
	t1 = "2026-11-20T03:17:00Z"
	t2 = "2026-11-21T03:17:00Z"
	t3 = "2026-11-22T03:17:00Z"
	t4 = "2026-11-23T03:17:00Z"
)

func TestPass(t *testing.T) {
	dir := t.TempDir()
	base := store(t, dir, fixture(t, "result.json", at(t1, mainRef, "pass")))
	c := candidate(t, fixture(t, "result.json", edits(at(t2, mainRef, ""), with("latency_us.p99.median", 2600.0))))
	code, g, md := gateRun(t, "-results", dir, "-candidate", c)
	if code != 0 || g.Verdict != verdictPass {
		t.Fatalf("code %d, verdict %q, want 0 pass\n%s", code, g.Verdict, md)
	}
	if g.Baseline == nil || *g.Baseline != base || g.ReleaseBaseline != nil || g.ReleaseTag != nil || g.RerunOf != nil {
		t.Errorf("gate = %+v", g)
	}
	if len(g.Comparisons) != 2 || g.Comparisons[1] != (comparison{
		Against: "nightly", Metric: "p99_us_median", Base: 2500, Value: 2600, Change: 0.04, Spread: 0.24,
	}) {
		t.Errorf("comparisons = %+v", g.Comparisons)
	}
	for _, want := range []string{
		"### Bench gate, x64: PASS",
		"Compared on CPU model: **" + xeon + "**",
		"Nightly baseline: `" + base + "`",
		"Release baseline: not available (no release tag yet)",
		"| nightly | p99_us_median | 2.500 ms | 2.600 ms | +4.0% | 24.0% | no |",
		"becomes the nightly baseline for **" + xeon + "**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("summary lacks %q:\n%s", want, md)
		}
	}
}

func TestThresholdAndSpread(t *testing.T) {
	for _, tt := range []struct {
		name    string
		base    func(m map[string]any)
		cand    func(m map[string]any)
		flagged []string
	}{
		{"p99 +20% within the 24% spread", nil, with("latency_us.p99.median", 3000.0), nil},
		{"p99 +30% over 10% and the spread", nil, with("latency_us.p99.median", 3250.0), []string{"nightly p99_us_median"}},
		{"p99 +5% over a 1% spread but not 10%", with("latency_us.p99.spread", 0.01), with("latency_us.p99.median", 2625.0), nil},
		{"p99 exactly +10% is not more than 10%", with("latency_us.p99.spread", 0.01), with("latency_us.p99.median", 2750.0), nil},
		{"p99 falling is never flagged", nil, with("latency_us.p99.median", 1000.0), nil},
		{"throughput -12% within a 15% spread", with("achieved_rps.spread", 0.15), with("achieved_rps.median", 4048.0), nil},
		{"throughput -12% over the 0.2% spread", nil, with("achieved_rps.median", 4048.0), []string{"nightly achieved_rps_median"}},
		{"throughput exactly -10%", nil, with("achieved_rps.median", 4140.0), nil},
		{"throughput rising is never flagged", nil, with("achieved_rps.median", 6000.0), nil},
		{"both", nil, edits(with("achieved_rps.median", 4000.0), with("latency_us.p99.median", 4000.0)),
			[]string{"nightly achieved_rps_median", "nightly p99_us_median"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store(t, dir, fixture(t, "result.json", edits(at(t1, mainRef, "pass"), func(m map[string]any) {
				if tt.base != nil {
					tt.base(m)
				}
			})))
			// The candidate is on main but invalid-free; a drop at a fixed
			// target can still be valid (99% of target is the floor).
			c := candidate(t, fixture(t, "result.json", edits(at(t2, mainRef, ""), tt.cand)))
			code, g, md := gateRun(t, "-results", dir, "-candidate", c)
			got := flagged(g)
			if strings.Join(got, ",") != strings.Join(tt.flagged, ",") {
				t.Fatalf("flagged %v, want %v\n%s", got, tt.flagged, md)
			}
			wantCode, wantVerdict := 0, verdictPass
			if tt.flagged != nil {
				wantCode, wantVerdict = 1, verdictFlagged
			}
			if code != wantCode || g.Verdict != wantVerdict {
				t.Errorf("code %d, verdict %q, want %d %s", code, g.Verdict, wantCode, wantVerdict)
			}
		})
	}
}

func TestConfirmingRerun(t *testing.T) {
	regressed := with("latency_us.p99.median", 4000.0)
	for _, tt := range []struct {
		name          string
		rerun         func(m map[string]any)
		code          int
		verdict       string
		firstVerdict  string
		rerunEligible bool
	}{
		{"confirmed", regressed, 1, verdictFail, verdictFail, false},
		{"cleared", with("latency_us.p99.median", 2550.0), 0, verdictPass, verdictFlagged, true},
		{"re-run invalid", edits(regressed, with("valid", false), with("invalid_reasons", []any{"run 2: achieved 4100 rps, below 99% of 4600"})),
			1, verdictInvalid, verdictFlagged, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			base := store(t, dir, fixture(t, "result.json", at(t1, mainRef, "pass")))
			first := candidate(t, fixture(t, "result.json", edits(at(t2, mainRef, ""), regressed)))
			code, g, _ := gateRun(t, "-results", dir, "-candidate", first, "-out", first)
			if code != 1 || g.Verdict != verdictFlagged || readGate(t, first).Verdict != verdictFlagged {
				t.Fatalf("first run set: code %d, verdict %q, want 1 flagged", code, g.Verdict)
			}

			rerun := candidate(t, fixture(t, "result.json", edits(at(t3, mainRef, ""), tt.rerun)))
			code, g, md := gateRun(t, "-results", dir, "-candidate", rerun, "-out", rerun, "-rerun-of", first)
			if code != tt.code || g.Verdict != tt.verdict {
				t.Fatalf("re-run: code %d, verdict %q, want %d %s\n%s", code, g.Verdict, tt.code, tt.verdict, md)
			}
			if g.RerunOf == nil || *g.RerunOf != "results/x64/20261121T031700Z.json" {
				t.Errorf("rerun_of = %v", g.RerunOf)
			}
			if tt.verdict != verdictInvalid && (g.Baseline == nil || *g.Baseline != base) {
				t.Errorf("re-run baseline = %v, want the first run set's %s", g.Baseline, base)
			}
			if got := readGate(t, rerun); got.Verdict != tt.verdict {
				t.Errorf("stored re-run verdict %q", got.Verdict)
			}
			fg := readGate(t, first)
			if fg.Verdict != tt.firstVerdict || fg.Baseline == nil || *fg.Baseline != base {
				t.Errorf("first run set now %+v, want verdict %s", fg, tt.firstVerdict)
			}

			// What the job stores: the flagged or failed first set never
			// becomes a baseline; a clearing re-run on main does.
			for _, f := range []string{first, rerun} {
				data, _ := os.ReadFile(filepath.Clean(f))
				store(t, dir, data)
			}
			next := candidate(t, fixture(t, "result.json", at(t4, mainRef, "")))
			_, g, _ = gateRun(t, "-results", dir, "-candidate", next)
			want := base
			if tt.rerunEligible {
				want = "results/x64/20261122T031700Z.json"
			}
			if g.Baseline == nil || *g.Baseline != want {
				t.Errorf("next night's baseline = %v, want %s", g.Baseline, want)
			}
		})
	}
}

func TestRerunOfNeedsAFlaggedResult(t *testing.T) {
	dir := t.TempDir()
	store(t, dir, fixture(t, "result.json", at(t1, mainRef, "pass")))
	for _, first := range [][]byte{
		fixture(t, "result.json", at(t2, mainRef, "")),
		fixture(t, "result.json", at(t2, mainRef, verdictPass)),
		fixture(t, "result.json", edits(at(t2, mainRef, verdictFlagged), with("arch", "arm64"))),
	} {
		rerun := candidate(t, fixture(t, "result.json", at(t3, mainRef, "")))
		if code, _, md := gateRun(t, "-results", dir, "-candidate", rerun, "-rerun-of", candidate(t, first)); code != 2 {
			t.Errorf("code %d, want 2\n%s", code, md)
		}
	}
}

func TestFirst(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cand  func(m map[string]any)
		other bool // the summary names the baseline's other CPU model
	}{
		{"another CPU model", with("hardware.cpu_model", epyc), true},
		{"no CPU model recorded", with("hardware.cpu_model", ""), false},
		{"another target rate", with("target_rps", 5000.0), false},
		{"another setup", with("setup.duration", "1m"), false},
		{"another trace ratio", with("setup.trace_sample_ratio", 0.1), false},
		{"another arch", with("arch", "arm64"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store(t, dir, fixture(t, "result.json", at(t1, mainRef, "pass")))
			c := candidate(t, fixture(t, "result.json", edits(at(t2, mainRef, ""), tt.cand, with("latency_us.p99.median", 9000.0))))
			code, g, md := gateRun(t, "-results", dir, "-candidate", c)
			if code != 0 || g.Verdict != verdictFirst || g.Baseline != nil || len(g.Comparisons) != 0 {
				t.Fatalf("code %d, gate %+v, want 0 first\n%s", code, g, md)
			}
			for _, want := range []string{"Compared on CPU model: none. **first**", "Nightly baseline: none", "becomes the nightly baseline"} {
				if !strings.Contains(md, want) {
					t.Errorf("summary lacks %q:\n%s", want, md)
				}
			}
			if got := strings.Contains(md, "other CPU models only: **"+xeon+"**"); got != tt.other {
				t.Errorf("summary names %s as another model: %v, want %v\n%s", xeon, got, tt.other, md)
			}
		})
	}
	t.Run("empty bench-results", func(t *testing.T) {
		c := candidate(t, fixture(t, "result.json", at(t2, "refs/heads/m7-bench-gate", "")))
		code, g, md := gateRun(t, "-results", t.TempDir(), "-candidate", c)
		if code != 0 || g.Verdict != verdictFirst {
			t.Fatalf("code %d, verdict %q", code, g.Verdict)
		}
		if !strings.Contains(md, "Results from `refs/heads/m7-bench-gate` never become baselines") {
			t.Errorf("summary:\n%s", md)
		}
	})
}

func TestInvalid(t *testing.T) {
	dir := t.TempDir()
	store(t, dir, fixture(t, "result.json", at(t1, mainRef, "pass")))
	reason := "run 3: achieved 4100 rps, below 99% of 4600"
	c := candidate(t, fixture(t, "result.json", edits(at(t2, mainRef, ""), with("valid", false), with("invalid_reasons", []any{reason}))))
	code, g, md := gateRun(t, "-results", dir, "-candidate", c, "-out", c)
	if code != 1 || g.Verdict != verdictInvalid || g.Baseline != nil || len(g.Comparisons) != 0 {
		t.Fatalf("code %d, gate %+v, want 1 invalid", code, g)
	}
	if !strings.Contains(md, reason) || !strings.Contains(md, "INVALID") {
		t.Errorf("summary:\n%s", md)
	}
	// Stored, it never becomes a baseline.
	data, _ := os.ReadFile(filepath.Clean(c))
	store(t, dir, data)
	_, g, _ = gateRun(t, "-results", dir, "-candidate", candidate(t, fixture(t, "result.json", at(t3, mainRef, ""))))
	if g.Baseline == nil || *g.Baseline != "results/x64/20261120T031700Z.json" {
		t.Errorf("baseline after an invalid run = %v", g.Baseline)
	}
}

func TestReleaseBaseline(t *testing.T) {
	const relCommit = "2222222222222222222222222222222222222222"
	release := edits(at(t1, mainRef, "pass"), with("commit", relCommit))
	t.Run("found", func(t *testing.T) {
		dir := t.TempDir()
		rel := store(t, dir, fixture(t, "result.json", edits(release, with("latency_us.p99.spread", 0.01))))
		// Two nights of sub-threshold creep: 2.5 → 2.7 → 2.9 ms.
		night := store(t, dir, fixture(t, "result.json", edits(at(t2, mainRef, "pass"), with("latency_us.p99.median", 2700.0), with("latency_us.p99.spread", 0.01))))
		c := candidate(t, fixture(t, "result.json", edits(at(t3, mainRef, ""), with("latency_us.p99.median", 2900.0))))
		code, g, md := gateRun(t, "-results", dir, "-candidate", c, "-release-tag", "v1.0.0", "-release-commit", relCommit)
		if code != 1 || g.Verdict != verdictFlagged {
			t.Fatalf("code %d, verdict %q, want 1 flagged\n%s", code, g.Verdict, md)
		}
		if *g.Baseline != night || *g.ReleaseBaseline != rel || *g.ReleaseTag != "v1.0.0" {
			t.Errorf("gate = %+v", g)
		}
		if got := flagged(g); strings.Join(got, ",") != "release p99_us_median" {
			t.Errorf("flagged %v, want only the release p99", got)
		}
		if !strings.Contains(md, "Release baseline (v1.0.0): `"+rel+"`") || !strings.Contains(md, "| release | p99_us_median | 2.500 ms | 2.900 ms | +16.0% | 1.0% | **yes** |") {
			t.Errorf("summary:\n%s", md)
		}
	})
	t.Run("missing for the release commit", func(t *testing.T) {
		dir := t.TempDir()
		// The release's result has another CPU model, so it does not count.
		store(t, dir, fixture(t, "result.json", edits(release, with("hardware.cpu_model", epyc))))
		night := store(t, dir, fixture(t, "result.json", at(t2, mainRef, "pass")))
		c := candidate(t, fixture(t, "result.json", at(t3, mainRef, "")))
		code, g, md := gateRun(t, "-results", dir, "-candidate", c, "-release-tag", "v1.0.0", "-release-commit", relCommit)
		if code != 0 || g.Verdict != verdictPass || *g.Baseline != night || g.ReleaseBaseline != nil || *g.ReleaseTag != "v1.0.0" || len(g.Comparisons) != 2 {
			t.Fatalf("code %d, gate %+v", code, g)
		}
		if !strings.Contains(md, "Release baseline: not available (no comparable result for v1.0.0, commit 2222222)") {
			t.Errorf("summary:\n%s", md)
		}
	})
	t.Run("before any release", func(t *testing.T) {
		dir := t.TempDir()
		store(t, dir, fixture(t, "result.json", release))
		c := candidate(t, fixture(t, "result.json", at(t3, mainRef, "")))
		_, g, _ := gateRun(t, "-results", dir, "-candidate", c)
		if g.Verdict != verdictPass || g.ReleaseTag != nil || g.ReleaseBaseline != nil || len(g.Comparisons) != 2 {
			t.Errorf("gate = %+v", g)
		}
	})
}

func TestEligibleBaselines(t *testing.T) {
	dir := t.TempDir()
	want := store(t, dir, fixture(t, "result.json", at(t1, mainRef, verdictFirst)))
	// Newer, but none of these may be a baseline.
	for i, edit := range []func(m map[string]any){
		at("2026-11-21T01:00:00Z", mainRef, verdictFlagged),
		at("2026-11-21T02:00:00Z", mainRef, verdictFail),
		at("2026-11-21T03:00:00Z", mainRef, verdictUngated),
		at("2026-11-21T04:00:00Z", mainRef, verdictInvalid),
		edits(at("2026-11-21T05:00:00Z", mainRef, verdictPass), with("valid", false)),
		at("2026-11-21T06:00:00Z", "refs/heads/m7-bench-gate", verdictPass),
		at("2026-11-21T07:00:00Z", "refs/heads/m7-bench-gate", ""),
	} {
		store(t, dir, fixture(t, "result.json", edits(edit, with("latency_us.p99.median", float64(100+i)))))
	}
	// Stored after the candidate: not its baseline either.
	store(t, dir, fixture(t, "result.json", at(t4, mainRef, verdictPass)))
	c := candidate(t, fixture(t, "result.json", at(t2, mainRef, "")))
	_, g, _ := gateRun(t, "-results", dir, "-candidate", c)
	if g.Baseline == nil || *g.Baseline != want {
		t.Errorf("baseline = %v, want %s", g.Baseline, want)
	}
}

func TestLegacyBaseline(t *testing.T) {
	dir := t.TempDir()
	legacy := store(t, dir, fixture(t, "legacy.json", nil))
	// A result with the M1 setup compares with it although it has no gate.
	c := candidate(t, fixture(t, "legacy.json", edits(at(t2, mainRef, ""), with("latency_us.p99.median", 2800.0))))
	code, g, md := gateRun(t, "-results", dir, "-candidate", c)
	if code != 0 || g.Verdict != verdictPass || g.Baseline == nil || *g.Baseline != legacy {
		t.Fatalf("code %d, gate %+v\n%s", code, g, md)
	}
	// A result traced at 1% (m4-tracing on) never compares with it
	// (ADR-0030), even on its CPU model and target.
	c = candidate(t, fixture(t, "result.json", edits(at(t2, mainRef, ""), with("hardware.cpu_model", epyc))))
	if _, g, _ = gateRun(t, "-results", dir, "-candidate", c); g.Verdict != verdictFirst {
		t.Errorf("traced result against the M1 baseline: verdict %q, want first", g.Verdict)
	}
}

func TestUngatedAndOutput(t *testing.T) {
	c := candidate(t, fixture(t, "result.json", at(t2, mainRef, "")))
	sum := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(sum, []byte("earlier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, g, md := gateRun(t, "-ungated", "-candidate", c, "-out", c, "-summary", sum)
	if code != 0 || g.Verdict != verdictUngated || g.Comparisons == nil {
		t.Fatalf("code %d, gate %+v", code, g)
	}
	if !strings.Contains(md, "UNGATED") || !strings.Contains(md, "never becomes a baseline") {
		t.Errorf("summary:\n%s", md)
	}
	got, _ := os.ReadFile(filepath.Clean(sum))
	if string(got) != "earlier\n"+md {
		t.Errorf("summary file = %q", got)
	}
	// The written result keeps every field and gains the gate.
	var before, after map[string]any
	_ = json.Unmarshal(fixture(t, "result.json", at(t2, mainRef, "")), &before)
	data, _ := os.ReadFile(filepath.Clean(c))
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	gj := after["gate"].(map[string]any)
	delete(after, "gate")
	if len(after) != len(before) || after["setup"] == nil || after["hardware"] == nil {
		t.Errorf("fields changed: %v", after)
	}
	if gj["verdict"] != verdictUngated || gj["baseline"] != nil || gj["rerun_of"] != nil {
		t.Errorf("stored gate = %v", gj)
	}
}

func TestBadInput(t *testing.T) {
	dir := t.TempDir()
	store(t, dir, fixture(t, "result.json", at(t1, mainRef, "pass")))
	good := candidate(t, fixture(t, "result.json", at(t2, mainRef, "")))
	for _, args := range [][]string{
		{"-candidate", good},
		{"-results", dir},
		{"-results", dir, "-candidate", good, "-release-tag", "v1.0.0"},
		{"-results", dir, "-candidate", good, "-release-commit", "abc"},
		{"-ungated", "-candidate", good, "-rerun-of", good},
		{"-results", dir, "-candidate", good, "extra"},
		{"-results", dir, "-candidate", filepath.Join(dir, "missing.json")},
		{"-results", dir, "-candidate", candidate(t, []byte("not json"))},
		{"-results", dir, "-candidate", candidate(t, []byte(`{"valid": true}`))},
		{"-results", filepath.Join(dir, "missing"), "-candidate", good},
	} {
		if code, _, _ := gateRun(t, args...); code != 2 {
			t.Errorf("%q: code %d, want 2", args, code)
		}
	}
	// A corrupt stored result is bad input, not a silent skip.
	if err := os.WriteFile(filepath.Join(dir, "results", "x64", "20261119T000000Z.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := gateRun(t, "-results", dir, "-candidate", good); code != 2 {
		t.Errorf("corrupt history: code %d, want 2", code)
	}
	if code := run([]string{"-h"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("-h: code %d", code)
	}
}

// P34 (ADR-0081, ADR-0083): a run set with verdict first is also compared,
// for information only, with the newest eligible baseline of the same arch,
// target rate and setup on another CPU model.
func TestCrossModelInfo(t *testing.T) {
	const genoa = "AMD EPYC 9V45 96-Core Processor"
	t.Run("shown, never flagged, never failing", func(t *testing.T) {
		dir := t.TempDir()
		store(t, dir, fixture(t, "result.json", at(t1, mainRef, "pass"))) // Xeon, older
		epycBase := store(t, dir, fixture(t, "result.json", edits(at(t2, mainRef, "pass"), with("hardware.cpu_model", epyc))))
		// Not eligible, or stored after the candidate: never the info baseline.
		store(t, dir, fixture(t, "result.json", edits(at("2026-11-21T09:00:00Z", mainRef, verdictFlagged), with("hardware.cpu_model", xeon))))
		store(t, dir, fixture(t, "result.json", edits(at("2026-11-21T10:00:00Z", "refs/heads/m7-x", verdictPass), with("hardware.cpu_model", xeon))))
		store(t, dir, fixture(t, "result.json", edits(at(t4, mainRef, verdictPass), with("hardware.cpu_model", xeon))))
		// 50% less throughput and 3.6× the p99 of the EPYC baseline: far over
		// any threshold, yet only information.
		cand := fixture(t, "result.json", edits(at(t3, mainRef, ""), with("hardware.cpu_model", genoa),
			with("achieved_rps.median", 2300.0), with("latency_us.p99.median", 9000.0)))
		c := candidate(t, cand)
		code, g, md := gateRun(t, "-results", dir, "-candidate", c, "-out", c)
		if code != 0 || g.Verdict != verdictFirst || g.Baseline != nil || len(g.Comparisons) != 0 || len(flagged(g)) != 0 {
			t.Fatalf("code %d, gate %+v, want 0 first with no comparisons\n%s", code, g, md)
		}
		x := g.CrossModel
		if x == nil || x.Baseline != epycBase || x.CPUModel != epyc || x.Info != crossModelInfo {
			t.Fatalf("cross_model = %+v, want the EPYC baseline %s", x, epycBase)
		}
		want := []infoComparison{
			{Metric: "achieved_rps_median", Base: 4600, Value: 2300, Change: -0.5},
			{Metric: "p99_us_median", Base: 2500, Value: 9000, Change: 2.6},
		}
		if len(x.Comparisons) != 2 || x.Comparisons[0] != want[0] || x.Comparisons[1] != want[1] {
			t.Errorf("comparisons = %+v, want %+v", x.Comparisons, want)
		}
		for _, s := range []string{
			"Compared on CPU model: none. **first**",
			"Information only (P34): against the newest eligible baseline on another CPU model, `" + epycBase + "`",
			"on **" + epyc + "**. Never flagged, never fails the job, never makes or blocks a baseline.",
			"| " + epyc + " | achieved_rps_median | 4600 rps | 2300 rps | -50.0% |",
			"| " + epyc + " | p99_us_median | 2.500 ms | 9.000 ms | +260.0% |",
			"Verdict **first**: the job passes.",
			"This run set becomes the nightly baseline for **" + genoa + "**",
		} {
			if !strings.Contains(md, s) {
				t.Errorf("summary lacks %q:\n%s", s, md)
			}
		}
		if got := readGate(t, c).CrossModel; got == nil || got.Baseline != epycBase {
			t.Errorf("stored cross_model = %+v", got)
		}
		// It made no difference to baselines: stored, the run set is its
		// model's first baseline, and the EPYC chain still uses its own.
		data, _ := os.ReadFile(filepath.Clean(c))
		genoaBase := store(t, dir, data)
		_, g, _ = gateRun(t, "-results", dir, "-candidate", candidate(t, fixture(t, "result.json", edits(at("2026-11-22T12:00:00Z", mainRef, ""), with("hardware.cpu_model", genoa)))))
		if g.Verdict != verdictPass || g.Baseline == nil || *g.Baseline != genoaBase || g.CrossModel != nil {
			t.Errorf("next run on the new model: gate %+v, want pass against %s and no cross_model", g, genoaBase)
		}
		_, g, _ = gateRun(t, "-results", dir, "-candidate", candidate(t, fixture(t, "result.json", edits(at("2026-11-22T13:00:00Z", mainRef, ""), with("hardware.cpu_model", epyc)))))
		if g.Baseline == nil || *g.Baseline != epycBase {
			t.Errorf("next EPYC run: baseline %v, want %s", g.Baseline, epycBase)
		}
	})
	for _, tt := range []struct {
		name string
		base func(m map[string]any) // the only stored result, Xeon unless edited
		cand func(m map[string]any)
	}{
		{"same model: a normal comparison", nil, nil},
		{"another target rate", nil, edits(with("hardware.cpu_model", epyc), with("target_rps", 5000.0))},
		{"another setup", nil, edits(with("hardware.cpu_model", epyc), with("setup.duration", "1m"))},
		{"no CPU model recorded on the candidate", nil, with("hardware.cpu_model", "")},
		{"no CPU model recorded on the baseline", with("hardware.cpu_model", ""), with("hardware.cpu_model", epyc)},
		{"the other model's result is not eligible", with("valid", false), with("hardware.cpu_model", epyc)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store(t, dir, fixture(t, "result.json", edits(at(t1, mainRef, "pass"), func(m map[string]any) {
				if tt.base != nil {
					tt.base(m)
				}
			})))
			c := candidate(t, fixture(t, "result.json", edits(at(t2, mainRef, ""), func(m map[string]any) {
				if tt.cand != nil {
					tt.cand(m)
				}
			})))
			code, g, md := gateRun(t, "-results", dir, "-candidate", c, "-out", c)
			if code != 0 || g.CrossModel != nil || strings.Contains(md, "Information only") {
				t.Errorf("code %d, cross_model %+v, want 0 and none\n%s", code, g.CrossModel, md)
			}
			raw, _ := os.ReadFile(filepath.Clean(c))
			if strings.Contains(string(raw), "cross_model") {
				t.Errorf("stored result has a cross_model field: %s", raw)
			}
		})
	}
}
