package main

import (
	"fmt"
	"strings"
)

// summary is the gate's Markdown report for one run set, for the job
// summary. It always says which CPU model the run was compared on, or that
// nothing was comparable (verdict first).
func summary(c *result, g gate, hist []*result, releaseCommit string) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	p("### Bench gate, %s: %s", c.Arch, strings.ToUpper(g.Verdict))
	p("")
	run := fmt.Sprintf("- Run set: `%s`, commit %s on `%s`, target %s rps, ", c.name, short(c.Commit), c.Ref, num(c.TargetRPS))
	if c.Valid {
		run += "valid"
	} else {
		run += "**invalid**"
	}
	if g.RerunOf != nil {
		run += fmt.Sprintf("; the confirming re-run of `%s`", *g.RerunOf)
	}
	p("%s", run)
	p("- CPU model of this run: %s", model(c.Hardware.CPUModel))
	p("- Median %s rps (spread %s), p99 %s ms (spread %s)",
		num(c.AchievedRPS.Median), pct(c.AchievedRPS.Spread), ms(c.Latency.P99.Median), pct(c.Latency.P99.Spread))

	switch g.Verdict {
	case verdictUngated:
		p("- Not gated: this dispatch ran with `gate` false, so it is stored without a comparison and never becomes a baseline.")
	case verdictInvalid:
		p("- Not compared: an invalid run set is stored as failed and never becomes a baseline (ADR-0003).")
		for _, r := range c.InvalidReasons {
			p("  - %s", r)
		}
	case verdictFirst:
		p("- Compared on CPU model: none. **first**: no valid, gated result from `%s` has CPU model %s with this arch, target rate and setup, so nothing was compared.", mainRef, model(c.Hardware.CPUModel))
		if other := otherModels(hist, c); other != "" {
			p("- Baselines exist for this arch, target rate and setup on other CPU models only: %s. Results on different CPU models are never compared (NFR-05).", other)
		}
	default:
		p("- Compared on CPU model: %s (same arch, target rate and setup)", model(c.Hardware.CPUModel))
	}

	if g.Verdict != verdictUngated && g.Verdict != verdictInvalid {
		p("- Nightly baseline: %s", ref(g.Baseline, hist))
		only := ""
		if g.Baseline != nil {
			only = ", so only the nightly baseline applies"
		}
		switch {
		case g.ReleaseTag == nil:
			p("- Release baseline: not available (no release tag yet)%s", only)
		case g.ReleaseBaseline == nil:
			p("- Release baseline: not available (no comparable result for %s, commit %s)%s", *g.ReleaseTag, short(releaseCommit), only)
		default:
			p("- Release baseline (%s): %s", *g.ReleaseTag, ref(g.ReleaseBaseline, hist))
		}
	}

	if len(g.Comparisons) > 0 {
		p("")
		p("| Against | Metric | Baseline | This run | Change | Baseline spread | Flagged |")
		p("| --- | --- | --- | --- | --- | --- | --- |")
		for _, x := range g.Comparisons {
			base, value := num(x.Base)+" rps", num(x.Value)+" rps"
			if x.Metric == "p99_us_median" {
				base, value = ms(x.Base)+" ms", ms(x.Value)+" ms"
			}
			flagged := "no"
			if x.Flagged {
				flagged = "**yes**"
			}
			p("| %s | %s | %s | %s | %+.1f%% | %s | %s |", x.Against, x.Metric, base, value, x.Change*100, pct(x.Spread), flagged)
		}
		p("")
		p("A metric is flagged when its median is worse by more than 10%% and by more than the baseline's run-to-run spread.")
	}

	p("")
	switch g.Verdict {
	case verdictPass:
		p("Verdict **pass**.")
	case verdictFirst:
		p("Verdict **first**: the job passes.")
	case verdictFlagged:
		p("Verdict **flagged**: the job re-runs the set once; only a re-run flagged again fails it. A flagged run set never becomes a baseline.")
	case verdictFail:
		p("Verdict **fail**: the re-run confirmed the regression. Both run sets are stored as failed and the job fails.")
	case verdictInvalid:
		p("Verdict **invalid**: the job fails.")
	case verdictUngated:
		p("Verdict **ungated**.")
	}
	if g.Verdict != verdictUngated {
		if c.Ref == mainRef && c.Valid && (g.Verdict == verdictPass || g.Verdict == verdictFirst) {
			p("This run set becomes the nightly baseline for %s on this arch, target rate and setup.", model(c.Hardware.CPUModel))
		} else if c.Ref != mainRef {
			p("Results from `%s` never become baselines; only gated runs on `%s` do (ADR-0003).", c.Ref, mainRef)
		}
	}
	p("")
	return b.String()
}

// ref describes the stored baseline name, from hist.
func ref(name *string, hist []*result) string {
	if name == nil {
		return "none"
	}
	for _, r := range hist {
		if r.name == *name {
			return fmt.Sprintf("`%s`, commit %s, %s", r.name, short(r.Commit), r.Timestamp)
		}
	}
	return "`" + *name + "`"
}

// otherModels lists the CPU models of baselines that would compare with c
// but for their CPU model.
func otherModels(hist []*result, c *result) string {
	var models []string
	seen := map[string]bool{}
	for _, b := range hist {
		m := b.Hardware.CPUModel
		if seen[m] || m == c.Hardware.CPUModel || b.name >= c.name || !eligible(b) {
			continue
		}
		bb := *b
		bb.Hardware.CPUModel = c.Hardware.CPUModel
		if c.Hardware.CPUModel != "" && comparable(&bb, c) {
			seen[m] = true
			models = append(models, model(m))
		}
	}
	return strings.Join(models, ", ")
}

func model(m string) string {
	if m == "" {
		return "**(not recorded)**"
	}
	return "**" + m + "**"
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func num(v float64) string { return fmt.Sprintf("%.0f", v) }
func ms(us float64) string { return fmt.Sprintf("%.3f", us/1000) }
func pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }
