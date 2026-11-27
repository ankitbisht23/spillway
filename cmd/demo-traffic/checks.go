package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Helpers of the scenarios' checks (ADR-0098 §2). Time 0 is the run's
// first scheduled request; "answers due in [a, b)" are the answers to
// requests whose scheduled time falls in that window.

// within returns the answers due in [from, to).
func within(as []answer, from, to time.Duration) []answer {
	var out []answer
	for _, a := range as {
		if a.due >= from && a.due < to {
			out = append(out, a)
		}
	}
	return out
}

func count(as []answer, f func(answer) bool) int {
	n := 0
	for _, a := range as {
		if f(a) {
			n++
		}
	}
	return n
}

// share is the share of as for which f holds; 0 for none.
func share(as []answer, f func(answer) bool) float64 {
	if len(as) == 0 {
		return 0
	}
	return float64(count(as, f)) / float64(len(as))
}

func is2xx(a answer) bool    { return a.ok() }
func is429(a answer) bool    { return a.status == 429 }
func is503(a answer) bool    { return a.status == 503 }
func decided(a answer) bool  { return a.decisionUS != 0 }
func answered(a answer) bool { return a.status != 0 }

// latencies returns the answered requests' latencies, sorted.
func latencies(as []answer) []time.Duration {
	out := make([]time.Duration, 0, len(as))
	for _, a := range as {
		if answered(a) {
			out = append(out, a.latency())
		}
	}
	slices.Sort(out)
	return out
}

// quantile is the q-quantile (0 to 1) of sorted ds by the nearest-rank
// method; 0 for none.
func quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	i := int(q*float64(len(ds))+0.999999) - 1
	return ds[min(max(i, 0), len(ds)-1)]
}

// median is the median of xs (the mean of the middle two for an even
// count); 0 for none.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

// statusMix is the answers by status, "200 × 1,234, 429 × 56", with
// unanswered ones by error kind.
func statusMix(as []answer) string {
	n := map[string]int{}
	for _, a := range as {
		k := strconv.Itoa(a.status)
		if a.status == 0 {
			k = "no answer (" + a.errKind + ")"
		}
		n[k]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s × %d", k, n[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// rlItem is one item of a RateLimit or RateLimit-Policy field: a quoted
// policy name with integer parameters (ADR-0018).
type rlItem struct {
	name   string
	params map[string]int64
}

// parseRL parses a RateLimit or RateLimit-Policy List, as the gateway
// writes it: `"name";k=v;k=v, "name";k=v`. ok is false for anything else.
func parseRL(v string) (items []rlItem, ok bool) {
	if strings.TrimSpace(v) == "" {
		return nil, false
	}
	for raw := range strings.SplitSeq(v, ",") {
		raw = strings.TrimSpace(raw)
		if len(raw) < 2 || raw[0] != '"' {
			return nil, false
		}
		end := strings.IndexByte(raw[1:], '"')
		if end < 1 {
			return nil, false
		}
		it := rlItem{name: raw[1 : end+1], params: map[string]int64{}}
		if rest := raw[end+2:]; rest != "" {
			if rest[0] != ';' {
				return nil, false
			}
			for p := range strings.SplitSeq(rest[1:], ";") {
				k, val, found := strings.Cut(p, "=")
				n, err := strconv.ParseInt(val, 10, 64)
				if !found || k == "" || err != nil {
					return nil, false
				}
				it.params[k] = n
			}
		}
		items = append(items, it)
	}
	return items, true
}

// item returns the item named name.
func item(items []rlItem, name string) (rlItem, bool) {
	for _, it := range items {
		if it.name == name {
			return it, true
		}
	}
	return rlItem{}, false
}

// gcraLimit is a GCRA limit: rate per period with burst.
type gcraLimit struct {
	rate   int64
	period time.Duration
	burst  int64
}

// bound is GCRA's most admissions in any closed window of length w
// (docs/design.md, Testing, Property): burst + w·rate/period, rounded down.
func (l gcraLimit) bound(w time.Duration) int64 {
	return l.burst + w.Microseconds()*l.rate/l.period.Microseconds()
}

// accuracyWindows are the window lengths ADR-0021's check uses.
var accuracyWindows = []time.Duration{10 * time.Millisecond, 100 * time.Millisecond, time.Second, 10 * time.Second, time.Minute}

// windowMax is the most admissions in one closed window [t, t + w] that
// starts at an admission, for each window length, with the largest excess
// over l's bound (ADR-0021's check, as cmd/accuracy makes it). admitted are
// Redis decision times in µs, sorted.
func windowMax(admitted []int64, l gcraLimit) (most map[time.Duration]int64, over int64, overW time.Duration) {
	most = map[time.Duration]int64{}
	for _, win := range accuracyWindows {
		bound, span := l.bound(win), win.Microseconds()
		j := 0
		for i, t := range admitted {
			for j < len(admitted) && admitted[j] <= t+span {
				j++
			}
			n := int64(j - i)
			most[win] = max(most[win], n)
			if n-bound > over {
				over, overW = n-bound, win
			}
		}
	}
	return most, over, overW
}

// pct formats a share as a percentage.
func pct(x float64) string { return fmt.Sprintf("%.2f%%", 100*x) }

// secs formats a duration in seconds.
func secs(d time.Duration) string { return fmt.Sprintf("%.2f s", d.Seconds()) }
