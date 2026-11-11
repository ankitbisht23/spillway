package main

import "time"

// limit is a GCRA limit: rate per period, with burst.
type limit struct {
	rate   int64
	period time.Duration
	burst  int64
}

// bound is GCRA's most admissions in any closed window of length w
// (docs/design.md, Testing, Property): burst + w·rate/period, rounded down.
func (l limit) bound(w time.Duration) int64 {
	return l.burst + w.Microseconds()*l.rate/l.period.Microseconds()
}

// windows are the window lengths the bound is checked over (ADR-0021).
var windows = []time.Duration{10 * time.Millisecond, 100 * time.Millisecond, time.Second, 10 * time.Second, time.Minute}

// overAdmission is the most admissions above l's bound in one key's
// admissions, admitted (decision times in µs, sorted). Each admission t
// starts a closed window [t, t + w] for each w in windows; it returns the
// largest count above the bound, 0 if none, with that window's length and
// start.
func overAdmission(admitted []int64, l limit) (over int64, w time.Duration, at int64) {
	for _, win := range windows {
		bound, span := l.bound(win), win.Microseconds()
		j := 0
		for i, t := range admitted {
			for j < len(admitted) && admitted[j] <= t+span {
				j++
			}
			if n := int64(j-i) - bound; n > over {
				over, w, at = n, win, t
			}
		}
	}
	return over, w, at
}
