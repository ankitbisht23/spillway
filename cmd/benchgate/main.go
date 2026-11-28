// Command benchgate is the NFR-05 regression gate (docs/design.md, Benchmark
// method; ADR-0003, ADR-0073 §D8). It judges one bench.sh result against a
// checkout of the bench-results branch: the nightly baseline is the newest
// valid result from main with the same arch, recorded CPU model, target rate
// and setup that the gate passed (or an M1-era result from before the gate),
// and the release baseline is the newest such result for the last release
// tag's commit. A run is flagged when its median throughput drops, or its
// median p99 rises, by more than 10% and more than the baseline's run-to-run
// spread, against either baseline; the job then re-runs the set once and
// passes -rerun-of, and a re-run flagged again fails both. A run set with
// nothing comparable (verdict first) is also shown, for information only,
// against the newest eligible baseline on another CPU model (P34, ADR-0083):
// that never flags, fails or makes a baseline. A run set on the committed
// exclusion list (-exclusions, ADR-0107) is never any of these baselines;
// the summary names the listed run sets it skipped.
//
// It prints the gate's verdict as JSON on stdout and a Markdown summary on
// stderr (also appended to -summary), writes the result with its gate field
// to -out, and exits 0 (pass, first, ungated), 1 (flagged, fail, invalid) or
// 2 (bad input).
//
//	benchgate -results <bench-results checkout> -candidate result.json [-out result.json]
//	    [-release-tag v1.0.0 -release-commit <sha>] [-rerun-of first.json] [-ungated] [-summary file]
//	    [-exclusions test/load/bench-baseline-exclusions.json]
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	results       string // a checkout of bench-results
	candidate     string
	out           string
	summary       string
	releaseTag    string
	releaseCommit string
	rerunOf       string
	exclusions    string // the committed exclusion list (ADR-0107)
	ungated       bool
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fl := flag.NewFlagSet("benchgate", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.StringVar(&o.results, "results", "", "a checkout of the bench-results branch (holds results/<arch>/)")
	fl.StringVar(&o.candidate, "candidate", "", "the bench.sh result to judge")
	fl.StringVar(&o.out, "out", "", "write the candidate with its gate field here (may be -candidate)")
	fl.StringVar(&o.summary, "summary", "", "also append the Markdown summary to this file (GITHUB_STEP_SUMMARY)")
	fl.StringVar(&o.releaseTag, "release-tag", "", "the last release tag, if any")
	fl.StringVar(&o.releaseCommit, "release-commit", "", "the commit of -release-tag")
	fl.StringVar(&o.rerunOf, "rerun-of", "", "the flagged result this run set re-runs; set to fail if the re-run confirms it")
	fl.StringVar(&o.exclusions, "exclusions", "", "the committed list of stored run sets that are never baselines (ADR-0107)")
	fl.BoolVar(&o.ungated, "ungated", false, "record the result as ungated: no comparison, never a baseline")
	if err := fl.Parse(args); err != nil {
		return o, err
	}
	switch {
	case fl.NArg() > 0:
		return o, fmt.Errorf("unexpected arguments %q", fl.Args())
	case o.results == "" && !o.ungated:
		return o, errors.New("-results is required")
	case o.candidate == "":
		return o, errors.New("-candidate is required")
	case (o.releaseTag == "") != (o.releaseCommit == ""):
		return o, errors.New("-release-tag and -release-commit go together")
	case o.ungated && o.rerunOf != "":
		return o, errors.New("-rerun-of needs the gate, not -ungated")
	case o.ungated && o.exclusions != "":
		return o, errors.New("-exclusions needs the gate, not -ungated")
	}
	return o, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "benchgate:", err)
		return 2
	}
	code, err := gateFile(o, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "benchgate:", err)
		return 2
	}
	return code
}

// gateFile judges o.candidate, writes what o asks for, and returns the exit
// code for the verdict.
func gateFile(o options, stdout, stderr io.Writer) (int, error) {
	raw, err := os.ReadFile(filepath.Clean(o.candidate))
	if err != nil {
		return 0, err
	}
	c, err := parseResult(raw, "")
	if err != nil {
		return 0, err
	}

	var first *result
	var firstRaw []byte
	if o.rerunOf != "" {
		if firstRaw, err = os.ReadFile(filepath.Clean(o.rerunOf)); err != nil {
			return 0, err
		}
		if first, err = parseResult(firstRaw, ""); err != nil {
			return 0, err
		}
		if first.Gate == nil || first.Gate.Verdict != verdictFlagged {
			return 0, fmt.Errorf("-rerun-of %s: its verdict is not %s", o.rerunOf, verdictFlagged)
		}
		if first.Arch != c.Arch {
			return 0, fmt.Errorf("-rerun-of %s: arch %s, the candidate's is %s", o.rerunOf, first.Arch, c.Arch)
		}
	}

	var g gate
	var hist []*result
	if o.ungated {
		g = gate{Verdict: verdictUngated, Comparisons: []comparison{}}
	} else {
		root, err := os.OpenRoot(o.results)
		if err != nil {
			return 0, err
		}
		defer func() { _ = root.Close() }()
		if hist, err = history(root.FS(), c.Arch); err != nil {
			return 0, err
		}
		if o.exclusions != "" {
			data, err := os.ReadFile(filepath.Clean(o.exclusions))
			if err != nil {
				return 0, err
			}
			list, err := parseExclusions(data)
			if err != nil {
				return 0, fmt.Errorf("%s: %w", o.exclusions, err)
			}
			if err := applyExclusions(hist, list, c.Arch); err != nil {
				return 0, fmt.Errorf("%s: %w", o.exclusions, err)
			}
		}
		rerunOf := ""
		if first != nil {
			rerunOf = first.name
		}
		g = judge(hist, c, o.releaseTag, o.releaseCommit, rerunOf)
	}

	if o.out != "" {
		if err := writeWithGate(o.out, raw, g); err != nil {
			return 0, err
		}
	}
	// A confirmed regression fails the flagged run set too, so neither
	// becomes a baseline (ADR-0003).
	if g.Verdict == verdictFail {
		fg := *first.Gate
		fg.Verdict = verdictFail
		if err := writeWithGate(o.rerunOf, firstRaw, fg); err != nil {
			return 0, err
		}
	}

	verdict, err := json.Marshal(g)
	if err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", verdict)
	md := summary(c, g, hist, o.releaseCommit)
	_, _ = fmt.Fprint(stderr, md)
	if o.summary != "" {
		f, err := os.OpenFile(filepath.Clean(o.summary), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		_, err = io.WriteString(f, md)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return 0, err
		}
	}

	switch g.Verdict {
	case verdictFlagged, verdictFail, verdictInvalid:
		return 1, nil
	}
	return 0, nil
}

// writeWithGate writes the result raw to path with its gate field set to g,
// keeping every other field. Top-level fields come out in name order.
func writeWithGate(path string, raw []byte, g gate) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	gj, err := json.Marshal(g)
	if err != nil {
		return err
	}
	fields["gate"] = gj
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Clean(path), append(out, '\n'), 0o600)
}
