// Package verify holds tests of test/verify/m7.sh's rows: how it turns the
// logs of the checks it runs and the bench-results files it reads into one
// verdict per NFR row (ADR-0074, ADR-0088). The checks themselves run on a
// laptop and on GitHub's runners.
package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// line is one check line as the verify scripts print it.
func line(verdict, check, measured string) string {
	return fmt.Sprintf("%-4s  %-44s %s", verdict, check, measured)
}

func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// accuracy is one pattern's summary line from cmd/accuracy.
func accuracy(pattern, verdict string, over, allowed int) string {
	o := "0"
	if over > 0 {
		o = fmt.Sprintf("%d (key k1, 10 ms window from 5000 µs)", over)
	}
	if allowed > 0 {
		o += fmt.Sprintf(" of %d allowed", allowed)
	}
	return fmt.Sprintf("%-8s %s  6000/6000 sent; 2xx 1000 (0 failure-mode), 429 4700, 503 300, other 0, no answer 0; max over-admission %s; 100.0/s admitted per key, limit 100/s; p99 4.0 ms; 1.000 Redis calls per admitted; redis errors: timeout 300",
		pattern, verdict, o)
}

// run is a fixture of one verify-m7 run's directory.
type run struct {
	logs  map[string]string // step -> log
	codes map[string]int
	bench map[string]map[string]any // results/<arch>/<name>.json -> content
	state map[string]string
}

// passing is a full run in which every counted row passes and NFR-02 is
// INVALID, as on main today.
func passing() *run {
	r := &run{logs: map[string]string{}, codes: map[string]int{}, bench: map[string]map[string]any{}, state: map[string]string{
		"parts": "all", "want_bench": "1", "want_kind": "1", "bench_run": "1", "bench_url": "https://github.com/o/r/actions/runs/1",
		"bench_why": "", "kind_why": "", "commit": "0123456789abcdef", "branch": "m7-verify", "repo_url": "https://github.com/o/r",
		"started": "2026-11-26T04:00:00Z", "finished": "2026-11-26T06:15:00Z", "duration": "8100", "laptop": "Apple M4 Pro, Docker Desktop 14 CPUs, 8.3 GB",
	}}
	r.logs["m1-reload"] = lines("== config reload under load (NFR-14)",
		line("PASS", "zero failed requests during reloads", "60000/60000 answered 2xx, 0 failed, 0 not sent; p99 1.62 ms at 1000 rps"),
		line("PASS", "config reloads applied under load", "29 of 29 swaps (at least 25)"))
	r.logs["m2"] = lines("== (a) property tests, each algorithm against its own bound",
		line("PASS", "property GCRA", "10000 random cases"),
		"== (c) NFR-03 added latency with local limits",
		line("PASS", "direct to upstream-1, run 1", "60000/60000 answered 2xx"),
		line("PASS", "NFR-03 added p99 <= 2 ms", "median p99 1.900 (of 1.900, 1.950, 1.800) - 1.600 (of 1.600, 1.700, 1.500) = 0.300 ms added; median p50 0.9 - 0.8 = 0.1 ms added"))
	r.logs["m3"] = lines("== (a) NFR-06 accuracy, GCRA strict mode, 3 replicas",
		line("PASS", "accuracy: uniform, zero over-admission", "6000/6000 sent; 2xx 1000 (0 failure-mode), 429 5000, 503 0, other 0, no answer 0; max over-admission 0; p99 1 ms"),
		line("PASS", "accuracy: bursty, zero over-admission", "6000/6000 sent; 2xx 1000 (0 failure-mode), 429 5000, 503 0, other 0, no answer 0; max over-admission 0; p99 1 ms"),
		line("PASS", "accuracy: boundary, zero over-admission", "6000/6000 sent; 2xx 900 (0 failure-mode), 429 1500, 503 3600, other 0, no answer 0; max over-admission 0; p99 5 ms"),
		"== (b) NFR-13 Redis full outage (make chaos-redis-outage)",
		line("PASS", "fail_open: 2xx through the outage", "ok"),
		"      detail",
		"== (b) NFR-13 Redis primary killed (make chaos-redis-primary)",
		line("PASS", "surviving primaries' keys: Redis decisions", "ok"),
		"== (c) NFR-04 added latency with a Redis check",
		line("PASS", "NFR-04 added p99 <= 5 ms", "median p99 2.000 (of 2.000, 2.100, 1.900) - 1.700 (of 1.700, 1.800, 1.600) = 0.300 ms added; median p50 1 - 1 = 0 ms added"))
	for _, n := range []int{1, 5} {
		r.logs[fmt.Sprintf("acc-strict-%d", n)] = lines(accuracy("uniform", "PASS", 0, 0), accuracy("bursty", "PASS", 0, 0), accuracy("boundary", "PASS", 0, 0))
	}
	for _, n := range []int{1, 3, 5} {
		r.logs[fmt.Sprintf("acc-batched-%d", n)] = lines(accuracy("uniform", "PASS", 0, 10*n), accuracy("bursty", "PASS", 0, 10*n), accuracy("boundary", "PASS", n+2, 10*n))
	}
	r.logs["m5"] = lines("== setup: test/verify/m5.yaml published",
		"== (a) NFR-08: a publish reaches all replicas in < 5 s",
		line("PASS", "(a) publish on all 5 replicas, p99 < 5 s", "60 publishes: p99 55 ms, max 60 ms"),
		"== (b) NFR-09: a revoked key is rejected on all replicas in < 5 s",
		line("PASS", "(b) revoked key rejected on all 5 replicas < 5 s", "20 keys: max 59 ms"),
		"== (c) NFR-12: gateways serve unchanged through a 10-minute control-plane outage (about 12.5 minutes)",
		line("PASS", "(c1) zero failed requests", "0 failed of 347900"),
		line("PASS", "(c2) config frozen at last good", "ok"),
		line("PASS", "(c5) recovery within 60 s", "4 s"),
		line("PASS", "(c6) staleness alert fires in the outage", "SpillwayConfigSyncStale firing for gateway-1 at +71 s of the 600 s outage (threshold 60 s)"))
	r.logs["keys"] = lines("== NFR-10 limiter keys",
		line("PASS", "NFR-10 gcra keys: <= 200 B per key at 1000000", "118.3 B per key at 1000000 keys (Σ Δused_memory 118300000 B ÷ Σ Δ DBSIZE 1000000); created 1000000, denied 0, errors 0, DBSIZE off by 0.00% (<= 1%)"),
		line("PASS", "NFR-10 sliding_window keys: <= 200 B per key at 1000000", "157.5 B per key at 1000000 keys (Σ Δused_memory 157500000 B ÷ Σ Δ DBSIZE 1000000); created 1000000, denied 0, errors 0, DBSIZE off by 0.00% (<= 1%)"))
	r.logs["soak"] = lines("== NFR-11 availability soak",
		line("PASS", "every tenant sent >= 99% of its schedule", "A 400/400 rps"),
		line("PASS", "NFR-11 gateway-caused 5xx <= 0.1%", "0 of 1260000 requests (0.0000%); deliberate 5xx: shed 0"))
	r.logs["noisy"] = lines("== NFR-15 noisy neighbour",
		line("PASS", "every tenant sent >= 99% of its schedule", "ok"),
		line("PASS", "NFR-15 quiet tenant B: p99 on/off < 1.10", "median 0.378 over 30 of 30 pairs (min 0.2, max 0.9); pooled p99 noisy off 6.2 ms, on 2.4 ms; 60000 2xx"),
		line("PASS", "NFR-15 quiet tenant C: p99 on/off < 1.10", "median 0.377 over 30 of 30 pairs (min 0.2, max 0.9); pooled p99 noisy off 6.2 ms, on 2.4 ms; 60000 2xx"),
		line("PASS", "NFR-15 quiet tenant D: p99 on/off < 1.10", "median 0.376 over 30 of 30 pairs (min 0.2, max 0.9); pooled p99 noisy off 6.2 ms, on 2.4 ms; 60000 2xx"))
	r.logs["latency"] = lines("== Redis latency +50 ms (chaos)",
		line("PASS", "degraded: admits within ±10% of expected", "950 admitted of 1900; expected 1000"))
	r.logs["gwkill"] = lines("== Gateway kill -9 under load (chaos)",
		line("PASS", "zero failed requests to the other gateways", "gateway-1 0 of 4992"),
		line("PASS", "only gateway-2's in-flight requests failed", "1 failed, sent before the first observed failure"))
	r.logs["m4"] = lines("== one of three endpoints answers 503",
		line("PASS", "5xx: >= 99% succeed within 10 s", "ok"))
	r.logs["kind-up"] = "[kind-up    0s] creating\n"
	r.logs["kind-smoke"] = lines(line("PASS", "three nodes Ready", "3 of 3 Ready"), line("PASS", "gateway Deployment ready", "3/3/3"))
	r.logs["kind-rollout"] = lines(
		fmt.Sprintf("%-4s  %-52s %s", "PASS", "zero failed requests (NFR-14)", "failed 0 of 72000 sent; status {\"2xx\":72000}; errors {}; p99 3000 us, max 9000 us"),
		fmt.Sprintf("%-4s  %-52s %s", "PASS", "rollout complete in 5 min, 3 new pods Ready", "restart at +60 s, complete at +95 s (35 s); ready/updated 3 3"))
	r.logs["kind-down"] = "deleted\n"
	r.logs["dev"] = "up\n"

	stats := func(v float64) map[string]any { return map[string]any{"median": v, "min": v, "max": v, "spread": 0} }
	result := func(arch string, rate float64, verdict string, baseline, rerunOf any) map[string]any {
		return map[string]any{
			"arch": arch, "run_url": "https://github.com/o/r/actions/runs/1", "valid": true, "invalid_reasons": []string{},
			"target_rps": rate, "rps_per_vcpu": rate, "achieved_rps": stats(rate), "setup": map[string]any{"gateway_vcpus": 1},
			"latency_us": map[string]any{"p99": stats(2500)}, "hardware": map[string]any{"cpu_model": "cpu-" + arch},
			"gate": map[string]any{"verdict": verdict, "baseline": baseline, "rerun_of": rerunOf, "comparisons": []map[string]any{
				{"against": "nightly", "metric": "achieved_rps_median", "base": rate, "value": rate, "change": 0.0, "flagged": false},
				{"against": "nightly", "metric": "p99_us_median", "base": 2400, "value": 2500, "change": 0.0417, "flagged": false},
			}},
		}
	}
	r.bench["results/x64/20261126T040100Z.json"] = result("x64", 4600, "pass", "results/x64/20261126T032000Z.json", nil)
	r.bench["results/arm64/20261126T040100Z.json"] = result("arm64", 8800, "flagged", "results/arm64/20261126T032000Z.json", nil)
	r.bench["results/arm64/20261126T042200Z.json"] = result("arm64", 8800, "pass", "results/arm64/20261126T032000Z.json", "results/arm64/20261126T040100Z.json")
	other := result("x64", 4600, "pass", "results/x64/20261125T032000Z.json", nil)
	other["run_url"] = "https://github.com/o/r/actions/runs/0" // another run's result is never read
	other["valid"] = false
	r.bench["results/x64/20261126T030000Z.json"] = other
	scaling := func(arch string, valid, pass bool, s float64) map[string]any {
		probe := func(gw float64, bound bool) map[string]any {
			return map[string]any{"gateway_bound": bound, "vcpu": map[string]any{"gateway": gw, "loadgen": 1.0, "upstream": 0.7}}
		}
		return map[string]any{
			"arch": arch, "run_url": "https://github.com/o/r/actions/runs/9", "commit": "d488f06aaaa", "valid": valid, "pass": pass,
			"scaling": s, "setup": map[string]any{"mode": "scaling"}, "hardware": map[string]any{"cpu_model": "cpu-" + arch},
			"invalid_reasons": []string{"n=2 probe 1: not gateway-bound"},
			"n1":              map[string]any{"gateway_vcpus": 1, "r": 10000, "probes": []any{probe(1.0, true), probe(1.0, true), probe(1.0, true)}},
			"n2":              map[string]any{"gateway_vcpus": 2, "r": 17900, "probes": []any{probe(1.85, valid), probe(1.84, valid), probe(1.83, valid)}},
		}
	}
	r.bench["results/x64/20261125T171042Z.json"] = scaling("x64", false, false, 2.0126)
	r.bench["results/arm64/20261125T171047Z.json"] = scaling("arm64", false, false, 1.5169)
	return r
}

// report writes r to a directory, runs m7.sh report on it and returns the
// JSON it wrote and the script's exit code.
func (r *run) report(t *testing.T) (map[string]any, int) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	dir := filepath.Join(t.TempDir(), "m7-20261126T040000Z")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var steps, state, files strings.Builder
	for step, log := range r.logs {
		write(step+".log", log)
		write(step+".code", fmt.Sprintf("%d\n", r.codes[step]))
		fmt.Fprintf(&steps, "%s\t%d\t60\t120\n", step, r.codes[step])
	}
	write("steps.tsv", steps.String())
	for k, v := range r.state {
		fmt.Fprintf(&state, "%s='%s'\n", k, strings.ReplaceAll(v, "'", `'\''`))
	}
	write("state.env", state.String())
	for name, content := range r.bench {
		b, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		write("bench-"+strings.ReplaceAll(name, "/", "_"), string(b))
		files.WriteString(name + "\n")
	}
	write("bench-files.txt", files.String())

	cmd := exec.CommandContext(t.Context(), "bash", "m7.sh", "report", dir) //nolint:gosec // G204: the test's own temp directory
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("m7.sh report: %v: %s", err, out)
		}
		code = ee.ExitCode()
	}
	t.Logf("m7.sh report:\n%s", out)
	b, err := os.ReadFile(dir + ".json") //nolint:gosec // G304: the test's own temp directory
	if err != nil {
		t.Fatalf("no JSON written: %v: %s", err, out)
	}
	var j map[string]any
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	return j, code
}

// rowsOf maps each row's id to the row.
func rowsOf(t *testing.T, j map[string]any) map[string]map[string]any {
	t.Helper()
	m := map[string]map[string]any{}
	for _, r := range j["rows"].([]any) {
		row := r.(map[string]any)
		m[row["id"].(string)] = row
	}
	return m
}

func wantVerdicts(t *testing.T, rows map[string]map[string]any, want map[string]string) {
	t.Helper()
	for id, v := range want {
		row, ok := rows[id]
		if !ok {
			t.Errorf("%s: no row", id)
			continue
		}
		if row["verdict"] != v {
			t.Errorf("%s: verdict %v, want %s (measured: %v)", id, row["verdict"], v, row["measured"])
		}
	}
}

func TestReportPassing(t *testing.T) {
	j, code := passing().report(t)
	rows := rowsOf(t, j)
	if code != 0 || j["exit"] != "PASS" {
		t.Errorf("exit %d, %v; want 0, PASS", code, j["exit"])
	}
	if len(rows) != 18 {
		t.Errorf("%d rows, want 15 NFR rows and 3 chaos rows", len(rows))
	}
	want := map[string]string{"NFR-02": "KNOWN LIMIT"}
	for i := 1; i <= 15; i++ {
		if i != 2 {
			want[fmt.Sprintf("NFR-%02d", i)] = "PASS"
		}
	}
	for _, c := range []string{"Chaos: Redis +50 ms", "Chaos: gateway kill -9", "Chaos: upstream 5xx"} {
		want[c] = "PASS"
	}
	wantVerdicts(t, rows, want)
	// NFR-02 is printed honestly and left out of the exit status only by
	// the user's decision (ADR-0084 §4), which the output says.
	if rows["NFR-02"]["counted"] != false {
		t.Error("NFR-02 counted; want not counted")
	}
	for _, s := range []string{"x64 INVALID", "would-be S 2.013", "arm64 INVALID", "would-be S 1.517", "gateway 1.85/1.84/1.83 vCPU"} {
		if !strings.Contains(rows["NFR-02"]["measured"].(string), s) {
			t.Errorf("NFR-02 measured %q lacks %q", rows["NFR-02"]["measured"], s)
		}
	}
	if !strings.Contains(j["excluded"].(string), "ADR-0084") {
		t.Errorf("excluded = %q, want the known-limit note", j["excluded"])
	}
	// The numbers come through.
	for id, s := range map[string]string{
		"NFR-01": "arm64 8800 rps per vCPU at 8800 offered (floor 8800), valid",
		"NFR-03": "added p99 0.300 ms",
		"NFR-05": "arm64 pass after the confirming re-run on CPU cpu-arm64 vs nightly 20261126T032000Z.json (nightly rps 0%, nightly p99 4.2%)",
		"NFR-06": "1 replicas 0/0/0 (PASS; boundary 503s 5%); 3 replicas 0/0/0 (PASS; boundary 503s 60%); 5 replicas 0/0/0",
		"NFR-07": "3 replicas 0/0/5 of 30 (PASS",
		"NFR-08": "60 publishes: p99 55 ms",
		"NFR-10": "GCRA 118.3 B per key at 1000000 keys; created 1000000",
		"NFR-12": "4 of 4 checks PASS",
		"NFR-14": "rolling restart: 2 of 2 checks PASS, failed 0 of 72000 sent",
		"NFR-15": "B median 0.378 over 30 of 30 pairs",
	} {
		if !strings.Contains(rows[id]["measured"].(string), s) {
			t.Errorf("%s measured %q lacks %q", id, rows[id]["measured"], s)
		}
	}
}

func TestReportFailures(t *testing.T) {
	for _, c := range []struct {
		name    string
		edit    func(*run)
		want    map[string]string
		measure map[string]string
		fails   int
	}{
		{
			// A FAIL in one of verify-m3's sections fails only its row.
			name: "killed primary fails NFR-13 only",
			edit: func(r *run) {
				r.logs["m3"] = strings.Replace(r.logs["m3"], line("PASS", "surviving primaries' keys: Redis decisions", "ok"),
					line("FAIL", "surviving primaries' keys: Redis decisions", "2 failure-mode answers"), 1)
				r.codes["m3"] = 1
			},
			want:  map[string]string{"NFR-13": "FAIL", "NFR-04": "PASS", "NFR-06": "PASS"},
			fails: 1,
		},
		{
			// A gateway run whose Redis errors are not one stall burst voids
			// NFR-04; its line says which run and why.
			name: "NFR-04 without three valid pairs",
			edit: func(r *run) {
				r.logs["m3"] = strings.Replace(r.logs["m3"],
					line("PASS", "NFR-04 added p99 <= 5 ms", "median p99 2.000 (of 2.000, 2.100, 1.900) - 1.700 (of 1.700, 1.800, 1.600) = 0.300 ms added; median p50 1 - 1 = 0 ms added"),
					line("FAIL", "gateway-1, Redis policy, run 2", "59994/60000 answered 2xx, 6 failed (not excused: failures span 63156.0 ms > 1000 ms)")+"\n"+
						line("FAIL", "NFR-04 added p99 <= 5 ms", "no three valid pairs of runs to compare"), 1)
				r.codes["m3"] = 1
			},
			want:    map[string]string{"NFR-04": "FAIL", "NFR-13": "PASS", "NFR-06": "PASS"},
			measure: map[string]string{"NFR-04": "no three valid pairs of runs to compare; FAIL gateway-1, Redis policy, run 2: 59994/60000 answered 2xx, 6 failed (not excused"},
			fails:   1,
		},
		{
			// NFR-05 counts first as a FAIL (ADR-0074) when no CPU model has
			// a main baseline (ADR-0089); NFR-01 still passes.
			name: "x64 gate first",
			edit: func(r *run) {
				g := r.bench["results/x64/20261126T040100Z.json"]["gate"].(map[string]any)
				g["verdict"], g["baseline"], g["comparisons"] = "first", nil, []any{}
			},
			want:  map[string]string{"NFR-05": "FAIL", "NFR-01": "PASS"},
			fails: 1,
		},
		{
			name: "arm64 run set invalid",
			edit: func(r *run) {
				f := r.bench["results/arm64/20261126T040100Z.json"]
				f["valid"], f["invalid_reasons"] = false, []string{"run 3: achieved 8651 rps, below 99% of 8800"}
			},
			want:  map[string]string{"NFR-01": "FAIL"},
			fails: 1,
		},
		{
			name: "no bench run",
			edit: func(r *run) {
				r.state["bench_url"], r.state["bench_run"], r.state["bench_why"] = "", "", "preflight: the tree is not clean"
			},
			want:  map[string]string{"NFR-01": "FAIL", "NFR-05": "FAIL", "NFR-02": "KNOWN LIMIT"},
			fails: 2,
		},
		{
			// A setup failure fails every row of its step.
			name: "verify-m5 setup failure",
			edit: func(r *run) {
				r.logs["m5"] = lines("== setup: test/verify/m5.yaml published",
					line("FAIL", "(a) not run", "setup: chaos controlplane setup failed"),
					line("FAIL", "(b) not run", "setup: chaos controlplane setup failed"))
				r.codes["m5"] = 1
			},
			want:  map[string]string{"NFR-08": "FAIL", "NFR-09": "FAIL", "NFR-12": "FAIL"},
			fails: 3,
		},
		{
			name: "batched over-admission above bound",
			edit: func(r *run) {
				r.logs["acc-batched-3"] = lines(accuracy("uniform", "PASS", 0, 30), accuracy("bursty", "PASS", 0, 30), accuracy("boundary", "FAIL", 31, 30))
				r.codes["acc-batched-3"] = 1
			},
			want:  map[string]string{"NFR-07": "FAIL"},
			fails: 1,
		},
		{
			// A step that crashed without a FAIL line fails its row.
			name: "soak crashed",
			edit: func(r *run) {
				r.logs["soak"], r.codes["soak"] = "== NFR-11 availability soak\nError: no such service\n", 1
			},
			want:  map[string]string{"NFR-11": "FAIL"},
			fails: 1,
		},
		{
			name: "kind not brought up",
			edit: func(r *run) {
				r.state["kind_why"] = "make kind-up exited 1"
				delete(r.logs, "kind-rollout")
				delete(r.logs, "kind-smoke")
			},
			want:  map[string]string{"NFR-14": "FAIL"},
			fails: 1,
		},
		{
			name:  "a step did not run",
			edit:  func(r *run) { delete(r.logs, "noisy") },
			want:  map[string]string{"NFR-15": "NOT RUN"},
			fails: 1,
		},
		{
			// A valid, passing scaling result counts as a PASS like any row.
			name: "NFR-02 valid and passing",
			edit: func(r *run) {
				for _, f := range []string{"results/x64/20261125T171042Z.json", "results/arm64/20261125T171047Z.json"} {
					r.bench[f]["valid"], r.bench[f]["pass"], r.bench[f]["scaling"] = true, true, 1.8
					for _, p := range r.bench[f]["n2"].(map[string]any)["probes"].([]any) {
						p.(map[string]any)["gateway_bound"] = true
					}
				}
			},
			want: map[string]string{"NFR-02": "PASS"},
		},
		{
			// A valid scaling result below 1.70 is a FAIL, still not counted.
			name: "NFR-02 valid and failing",
			edit: func(r *run) {
				f := "results/arm64/20261125T171047Z.json"
				r.bench[f]["valid"] = true
				for _, p := range r.bench[f]["n2"].(map[string]any)["probes"].([]any) {
					p.(map[string]any)["gateway_bound"] = true
				}
			},
			want: map[string]string{"NFR-02": "KNOWN LIMIT"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := passing()
			c.edit(r)
			j, code := r.report(t)
			rows := rowsOf(t, j)
			wantVerdicts(t, rows, c.want)
			for id, m := range c.measure {
				if !strings.Contains(rows[id]["measured"].(string), m) {
					t.Errorf("%s measured %q lacks %q", id, rows[id]["measured"], m)
				}
			}
			wantExit, wantCode := "PASS", 0
			if c.fails > 0 {
				wantExit, wantCode = fmt.Sprintf("FAIL (%d rows)", c.fails), 1
			}
			if j["exit"] != wantExit || code != wantCode {
				t.Errorf("exit %d, %v; want %d, %s", code, j["exit"], wantCode, wantExit)
			}
		})
	}
	t.Run("NFR-02 not counted unless PASS", func(t *testing.T) {
		r := passing()
		r.bench["results/arm64/20261125T171047Z.json"]["valid"] = true
		j, _ := r.report(t)
		if row := rowsOf(t, j)["NFR-02"]; row["counted"] != false || !strings.Contains(row["measured"].(string), "arm64 FAIL") {
			t.Errorf("NFR-02 = %v", row)
		}
	})
}

// TestReadme checks that the block between the markers is rewritten from a
// full run's JSON, the rest of the file is kept, a second rewrite gives the
// same file, and a partial run is refused.
func TestReadme(t *testing.T) {
	j, _ := passing().report(t)
	dir := t.TempDir()
	js := filepath.Join(dir, "run.json")
	b, _ := json.Marshal(j)
	if err := os.WriteFile(js, b, 0o600); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(dir, "README.md")
	before := "# Title\n\nintro\n\n<!-- verify-m7:begin -->\n_old_\n<!-- verify-m7:end -->\n\n## Licence\n"
	if err := os.WriteFile(readme, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	rewrite := func(json string) ([]byte, error) {
		cmd := exec.CommandContext(t.Context(), "bash", "m7.sh", "readme", json) //nolint:gosec // G204: the test's own temp file
		cmd.Env = append(os.Environ(), "VERIFY_M7_README="+readme)
		return cmd.CombinedOutput()
	}
	if out, err := rewrite(js); err != nil {
		t.Fatalf("readme: %v: %s", err, out)
	}
	got, _ := os.ReadFile(readme) //nolint:gosec // G304: the test's own temp file
	s := string(got)
	for _, want := range []string{
		"# Title\n\nintro\n\n<!-- verify-m7:begin -->\n",
		"<!-- verify-m7:end -->\n\n## Licence\n",
		"**M7 exit: PASS**. NFR-02 is not counted",
		"| Row | Target | Measured | Where | Date | Commit | Result |",
		"| NFR-02 |",
		"| **KNOWN LIMIT** (not counted) |",
		"| NFR-15 |",
		"[run 1](https://github.com/o/r/actions/runs/1)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("README lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "_old_") {
		t.Error("the old block is still there")
	}
	if out, err := rewrite(js); err != nil {
		t.Fatalf("second readme: %v: %s", err, out)
	}
	if again, _ := os.ReadFile(readme); string(again) != s { //nolint:gosec // G304: the test's own temp file
		t.Error("a second rewrite changed the file")
	}

	j["parts"] = "10 11"
	b, _ = json.Marshal(j)
	if err := os.WriteFile(js, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := rewrite(js); err == nil {
		t.Errorf("a partial run rewrote the README: %s", out)
	}
	if again, _ := os.ReadFile(readme); string(again) != s { //nolint:gosec // G304: the test's own temp file
		t.Error("a refused rewrite changed the file")
	}
}

// mainResult is a gated result stored on bench-results from main: a
// baseline for later runs on its CPU model (ADR-0081).
func mainResult(r *run, name, model, verdict string) map[string]any {
	arch := strings.Split(name, "/")[1]
	var b map[string]any
	for n, f := range r.bench {
		if strings.HasPrefix(n, "results/"+arch+"/20261126T0401") {
			raw, _ := json.Marshal(f)
			_ = json.Unmarshal(raw, &b)
		}
	}
	b["ref"], b["run_url"], b["hardware"] = "refs/heads/main", "https://github.com/o/r/actions/runs/2", map[string]any{"cpu_model": model}
	b["gate"] = map[string]any{"verdict": verdict, "baseline": nil, "rerun_of": nil, "comparisons": []any{}}
	r.bench[name] = b
	return b
}

// x64First turns the verify run's x64 result into a first verdict on its
// CPU model, cpu-x64.
func x64First(r *run) {
	g := r.bench["results/x64/20261126T040100Z.json"]["gate"].(map[string]any)
	g["verdict"], g["baseline"], g["comparisons"] = "first", nil, []any{}
}

// TestReportNFR05 checks ADR-0089: an x64 first because the runner's CPU
// model has no eligible main baseline yet, while another model has one, is
// NOT DEMONSTRATED and left out of the exit status; every other first, and
// any arm64 verdict but pass, fails NFR-05.
func TestReportNFR05(t *testing.T) {
	const base = "results/x64/20261126T020000Z.json"
	for _, c := range []struct {
		name    string
		edit    func(*run)
		verdict string
		counted bool
		measure string
	}{
		{
			name: "x64 first, another model has a main baseline",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64-other", "first")
				mainResult(r, "results/x64/20261125T020000Z.json", "cpu-x64-third", "pass")
			},
			verdict: "NOT DEMONSTRATED", counted: false,
			measure: "x64 first on CPU cpu-x64, no main baseline on this CPU model; NOT DEMONSTRATED: main baselines exist only on cpu-x64-other, cpu-x64-third (ADR-0089)",
		},
		{
			name: "x64 first and arm64 failed",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64-other", "pass")
				r.bench["results/arm64/20261126T042200Z.json"]["gate"].(map[string]any)["verdict"] = "fail"
			},
			verdict: "FAIL", counted: true,
			measure: "NOT DEMONSTRATED: main baselines exist only on cpu-x64-other (ADR-0089); arm64 fail after the confirming re-run",
		},
		{
			// The gate found nothing on a model that has a main baseline:
			// the first is not the CPU model's doing.
			name: "x64 first though its model has a main baseline",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64", "pass")
				mainResult(r, "results/x64/20261125T020000Z.json", "cpu-x64-other", "pass")
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "arm64 first",
			edit: func(r *run) {
				delete(r.bench, "results/arm64/20261126T042200Z.json")
				g := r.bench["results/arm64/20261126T040100Z.json"]["gate"].(map[string]any)
				g["verdict"], g["baseline"], g["comparisons"] = "first", nil, []any{}
				mainResult(r, "results/arm64/20261126T020000Z.json", "cpu-arm64-other", "pass")
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "x64 fail against a same-model baseline",
			edit: func(r *run) {
				r.bench["results/x64/20261126T040100Z.json"]["gate"].(map[string]any)["verdict"] = "fail"
				mainResult(r, base, "cpu-x64-other", "pass")
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "only a branch result on another model",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64-other", "pass")["ref"] = "refs/heads/m7-verify"
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "only an invalid main result on another model",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64-other", "pass")["valid"] = false
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "only a flagged main result on another model",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64-other", "flagged")
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "only a main result with another setup",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64-other", "pass")["setup"] = map[string]any{"gateway_vcpus": 2}
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "only a main result at another rate",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, base, "cpu-x64-other", "pass")["target_rps"] = 5000
			},
			verdict: "FAIL", counted: true,
		},
		{
			name: "only a main result stored after the run",
			edit: func(r *run) {
				x64First(r)
				mainResult(r, "results/x64/20261126T050000Z.json", "cpu-x64-other", "pass")
			},
			verdict: "FAIL", counted: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := passing()
			c.edit(r)
			j, code := r.report(t)
			row := rowsOf(t, j)["NFR-05"]
			if row["verdict"] != c.verdict || row["counted"] != c.counted {
				t.Errorf("NFR-05 %v (counted %v), want %s (counted %v); measured %v", row["verdict"], row["counted"], c.verdict, c.counted, row["measured"])
			}
			if c.measure != "" && !strings.Contains(row["measured"].(string), c.measure) {
				t.Errorf("NFR-05 measured %q lacks %q", row["measured"], c.measure)
			}
			wantExit, wantCode := "PASS", 0
			if c.counted {
				wantExit, wantCode = "FAIL (1 rows)", 1
			}
			if j["exit"] != wantExit || code != wantCode {
				t.Errorf("exit %d, %v; want %d, %s", code, j["exit"], wantCode, wantExit)
			}
			ex, _ := j["excluded"].(string)
			if got := strings.Contains(ex, "ADR-0089"); got == c.counted {
				t.Errorf("excluded = %q; the ADR-0089 note should be there only when NFR-05 is not counted", ex)
			}
			if !c.counted && !strings.Contains(ex, "ADR-0084") {
				t.Errorf("excluded = %q lost NFR-02's note", ex)
			}
		})
	}
}
