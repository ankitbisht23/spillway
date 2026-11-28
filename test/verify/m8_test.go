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

// m8Steps are test/verify/m8.sh's steps of a full run with the checks
// each prints (ADR-0098 §2).
var m8Steps = []struct {
	name string
	ids  []string
}{
	{"build", nil},
	{"g0", []string{"G0.g1", "G0.g2", "G0.g3", "G0.g4", "G0.g5"}},
	{"g0-fault", []string{"G0.g6"}},
	{"up", nil},
	{"k", []string{"k1", "k2", "k3", "k4"}},
	{"d1", []string{"d1.1", "d1.2", "d1.3", "d1.4"}},
	{"d2", []string{"d2.1", "d2.2", "d2.3", "d2.4"}},
	{"d3", []string{"d3.1", "d3.2", "d3.3", "d3.4"}},
	{"d4", []string{"d4.1", "d4.2", "d4.3", "d4.4", "d4.5", "d4.6"}},
	{"d5", []string{"d5.0", "d5.1", "d5.2", "d5.3", "d5.4", "d5.5", "d5.6", "d5.7", "d5.8"}},
	{"d6", []string{"d6.1", "d6.2", "d6.3", "d6.4"}},
	{"d7", []string{"d7.1", "d7.2", "d7.3", "d7.4"}},
	{"s", []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9a", "s9b", "s9c", "s10", "s11", "s12", "s13"}},
	{"down", nil},
	{"g1", []string{"G1.g1", "G1.g2", "G1.g3", "G1.g4", "G1.g5"}},
	{"g1-fault", []string{"G1.g6"}},
	{"g1-stack", []string{"G1.g7"}},
}

// m8Total is the number of check lines of a passing run: every check
// once, and s10 twice.
const m8Total = 5 + 1 + 4 + 4 + 4 + 4 + 6 + 9 + 4 + 4 + 15 + 1 + 5 + 1 + 1

// m8Run is a fixture of one verify-m8 run's directory.
type m8Run struct {
	logs    map[string]string
	codes   map[string]string // exit code, or "skip"
	notes   map[string]string
	reports map[string]string // step -> its JSON report
	omit    map[string]bool   // steps not in steps.tsv (an interrupted run)
}

// m8Passing is a full run in which every check passes, with the noise
// each part's log really has around its check lines.
func m8Passing() *m8Run {
	r := &m8Run{logs: map[string]string{}, codes: map[string]string{}, notes: map[string]string{}, reports: map[string]string{}, omit: map[string]bool{}}
	for _, s := range m8Steps {
		r.codes[s.name] = "0"
		var b strings.Builder
		switch {
		case s.name == "s":
			b.WriteString("demo-share: starting the stand-in tunnel\nPASS this line is not share-check's and is not a check\n")
			for _, id := range s.ids {
				fmt.Fprintf(&b, "share-check: %s PASS: what %s saw\n", id, id)
			}
			b.WriteString("share-check: s10 PASS: no tunnel container left, deploy/demo/run/ empty\ndemo-share-test: PASS (16 PASS lines) in 140 s\n")
		case s.ids != nil:
			fmt.Fprintf(&b, "   0s  base load 258 rps\n")
			for _, id := range s.ids {
				fmt.Fprintf(&b, "PASS %s what %s checks: measured %s\n", id, id, id)
			}
			fmt.Fprintf(&b, "%s: PASS (%d checks)\n", s.name, len(s.ids))
			if !strings.Contains(s.name, "-") {
				r.reports[s.name] = fmt.Sprintf(`{"scenario": %q, "pass": true, "measured": {"n": 1}}`, s.name)
			}
		default:
			b.WriteString("Container spillway-controlplane-1 Started\n")
		}
		r.logs[s.name] = b.String()
	}
	return r
}

func (r *m8Run) write(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "m8-20261128T040000Z")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var tsv strings.Builder
	for _, s := range m8Steps {
		if r.omit[s.name] {
			continue
		}
		fmt.Fprintf(&tsv, "%s\t%s\t%d\t%s\n", s.name, r.codes[s.name], 60, r.notes[s.name])
		if r.codes[s.name] != "skip" {
			if err := os.WriteFile(filepath.Join(dir, s.name+".log"), []byte(r.logs[s.name]), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if rep, ok := r.reports[s.name]; ok && r.codes[s.name] != "skip" {
			if err := os.WriteFile(filepath.Join(dir, s.name+".json"), []byte(rep), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	state := "started=2026-11-28T04:00:00Z\ncommit=0123456789abcdef\nbranch=m8-verify\nfinished=2026-11-28T04:31:00Z\nduration=1860\n"
	for name, content := range map[string]string{"steps.tsv": tsv.String(), "state.env": state} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// m8Report is the JSON test/verify/m8.sh writes next to a run's directory.
type m8Report struct {
	Pass      bool   `json:"pass"`
	Verdict   string `json:"verdict"`
	Commit    string `json:"commit"`
	DurationS *int   `json:"duration_s"`
	Steps     []struct {
		Step string `json:"step"`
		Code string `json:"code"`
	} `json:"steps"`
	Checks []struct {
		Part, ID, Verdict, Text string
	} `json:"checks"`
	Reports map[string]map[string]any `json:"reports"`
}

// m8 runs "m8.sh report <dir>" and returns its output, exit code and JSON.
func m8(t *testing.T, dir string) (string, int, m8Report) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", "m8.sh", "report", dir) //nolint:gosec // G204: the test's own temp directory
	b, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("m8.sh: %v\n%s", err, b)
		}
		code = ee.ExitCode()
	}
	var rep m8Report
	j, err := os.ReadFile(dir + ".json") //nolint:gosec // G304: the test's own temp directory
	if err != nil {
		t.Fatalf("no JSON: %v\n%s", err, b)
	}
	if err := json.Unmarshal(j, &rep); err != nil {
		t.Fatalf("JSON: %v\n%s", err, j)
	}
	return string(b), code, rep
}

// checkLine returns the output line of check id (the first), "" for none.
func checkLine(out, id string) string {
	for l := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && (f[0] == "PASS" || f[0] == "FAIL") && f[1] == id {
			return l
		}
	}
	return ""
}

// TestM8ReportPassing: a run whose parts all pass prints every check line
// once, in order, share-check's lines in the common form, and the PASS
// verdict last; the JSON holds every check and each part's report.
func TestM8ReportPassing(t *testing.T) {
	out, code, rep := m8(t, m8Passing().write(t))
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if want := fmt.Sprintf("verify-m8: PASS (%d checks)", m8Total); lines[len(lines)-1] != want {
		t.Errorf("last line %q, want %q", lines[len(lines)-1], want)
	}
	if lines[len(lines)-2] != "verify-m8: 31 min 00 s on 0123456789abcdef (m8-verify)" {
		t.Errorf("duration line %q", lines[len(lines)-2])
	}
	var ids []string
	for _, l := range lines {
		if f := strings.Fields(l); len(f) > 1 && (f[0] == "PASS" || f[0] == "FAIL") {
			ids = append(ids, f[1])
		}
	}
	var want []string
	for _, s := range m8Steps {
		want = append(want, s.ids...)
		if s.name == "s" {
			want = append(want, "s10")
		}
	}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("check lines\n%v\nwant\n%v", ids, want)
	}
	if l := checkLine(out, "s9b"); l != "PASS s9b share: what s9b saw" {
		t.Errorf("s9b line %q", l)
	}
	if strings.Contains(out, "not share-check's") {
		t.Error("a line of make demo-share-test that is not share-check's was taken as a check")
	}
	if !rep.Pass || len(rep.Checks) != m8Total || rep.Commit != "0123456789abcdef" || rep.DurationS == nil || *rep.DurationS != 1860 || len(rep.Steps) != len(m8Steps) {
		t.Errorf("JSON: pass %v, %d checks, commit %s, %d steps", rep.Pass, len(rep.Checks), rep.Commit, len(rep.Steps))
	}
	if c := rep.Checks[0]; c.Part != "g0" || c.ID != "G0.g1" || c.Verdict != "PASS" || c.Text != "what G0.g1 checks: measured G0.g1" {
		t.Errorf("first check %+v", c)
	}
	if rep.Reports["d5"]["scenario"] != "d5" || len(rep.Reports) != 10 {
		t.Errorf("reports %v", rep.Reports)
	}
}

// TestM8ReportFailures: each way a part can fail makes the run FAIL with
// the failed checks named, and nothing is excused.
func TestM8ReportFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(*m8Run)
		failed string // the verdict's list
		lines  []string
	}{
		{"a FAIL line", func(r *m8Run) {
			r.logs["d2"] = strings.Replace(r.logs["d2"], "PASS d2.1 what d2.1 checks: measured d2.1", "FAIL d2.1 quiet tenants' p99 flat: tenant pro-co 1.21 (want < 1.10)", 1)
			r.codes["d2"] = "1"
		}, "d2.1", []string{"FAIL d2.1 quiet tenants' p99 flat: tenant pro-co 1.21 (want < 1.10)"}},
		{"a share-check FAIL", func(r *m8Run) {
			r.logs["s"] = strings.Replace(r.logs["s"], "share-check: s4 PASS: what s4 saw", "share-check: s4 FAIL: what s4 saw", 1)
			r.codes["s"] = "1"
		}, "s4", []string{"FAIL s4 share: what s4 saw"}},
		{"a check missing from a part's output", func(r *m8Run) {
			r.logs["d5"] = strings.Replace(r.logs["d5"], "PASS d5.3 what d5.3 checks: measured d5.3\n", "", 1)
		}, "d5.3", []string{"FAIL d5.3 setup: no result (step d5 exit 0;"}},
		{"a part that crashed", func(r *m8Run) {
			r.logs["d4"] = "panic: boom\n"
			r.codes["d4"] = "2"
		}, "d4.1, d4.2, d4.3, d4.4, d4.5, d4.6", []string{"FAIL d4.1 setup: no result (step d4 exit 2;"}},
		{"a non-zero exit without a FAIL line", func(r *m8Run) { r.codes["d1"] = "1" }, "d1", []string{"FAIL d1 exit: exit 1 without a FAIL line"}},
		{"the demo did not come up", func(r *m8Run) {
			r.codes["up"] = "1"
			for _, s := range []string{"k", "d1", "d2", "d3", "d4", "d5", "d6", "d7", "s"} {
				r.codes[s], r.notes[s] = "skip", "make demo or make demo-reset failed (x/up.log)"
			}
		}, "k1, k2, k3, k4, d1.1", []string{"FAIL k1 setup: make demo or make demo-reset failed (x/up.log)", "FAIL s13 setup: make demo or make demo-reset failed (x/up.log)", "note: setup step up exited 1"}},
		{"an interrupted run", func(r *m8Run) {
			for _, s := range []string{"down", "g1", "g1-fault", "g1-stack"} {
				r.omit[s] = true
			}
		}, "G1.g1, G1.g2, G1.g3, G1.g4, G1.g5, G1.g6, G1.g7", []string{"FAIL G1.g7 setup: not run"}},
		{"G1 fails, G0 passes", func(r *m8Run) {
			r.logs["g1"] = strings.Replace(r.logs["g1"], "PASS G1.g5", "FAIL G1.g5", 1)
			r.codes["g1"] = "1"
		}, "G1.g5", []string{"PASS G0.g5 what G0.g5", "FAIL G1.g5 what G1.g5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := m8Passing()
			tc.mod(r)
			out, code, rep := m8(t, r.write(t))
			if code != 1 || rep.Pass {
				t.Errorf("exit %d, JSON pass %v:\n%s", code, rep.Pass, out)
			}
			last := strings.TrimSpace(out)
			last = last[strings.LastIndex(last, "\n")+1:]
			if !strings.HasPrefix(last, "verify-m8: FAIL (") || !strings.Contains(last, " checks: "+tc.failed) || rep.Verdict != last {
				t.Errorf("verdict %q (JSON %q), want the failed list to start %q", last, rep.Verdict, tc.failed)
			}
			for _, l := range tc.lines {
				if !strings.Contains(out, "\n"+l) && !strings.HasPrefix(out, l) {
					t.Errorf("no line starting %q:\n%s", l, out)
				}
			}
		})
	}
}

// TestM8Usage: m8.sh takes no arguments but report <dir>.
func TestM8Usage(t *testing.T) {
	for _, args := range [][]string{{"x"}, {"report"}} {
		cmd := exec.CommandContext(t.Context(), "bash", append([]string{"m8.sh"}, args...)...) //nolint:gosec // G204: the test's own arguments
		if b, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(b), "usage") {
			t.Errorf("%v: %v %s", args, err, b)
		}
	}
	cmd := exec.CommandContext(t.Context(), "bash", "m8.sh", "report", t.TempDir()) //nolint:gosec // G204: the test's own temp directory
	if b, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(b), "no steps.tsv") {
		t.Errorf("report of an empty directory: %v %s", err, b)
	}
}
