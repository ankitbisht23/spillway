// Package quota holds what the gateway and the control plane's flusher share
// about quota periods (RL-10, ADR-0070): UTC calendar days and months, and
// their IDs in Redis keys and the quota_usage table.
package quota

import "time"

// Period is one UTC calendar day or month.
type Period struct {
	ID         string    // d<YYYYMMDD> for a day, m<YYYYMM> for a month
	Start, End time.Time // UTC; the period is [Start, End)
}

// Day is the UTC day containing t.
func Day(t time.Time) Period {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return Period{ID: "d" + start.Format("20060102"), Start: start, End: start.AddDate(0, 0, 1)}
}

// Month is the UTC month containing t.
func Month(t time.Time) Period {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return Period{ID: "m" + start.Format("200601"), Start: start, End: start.AddDate(0, 1, 0)}
}

// Seconds is the period's length in whole seconds, the w of its
// RateLimit-Policy item (ADR-0070).
func (p Period) Seconds() int64 { return int64(p.End.Sub(p.Start) / time.Second) }

// Grace is how long after a period ends the flusher still reads its
// counters, so the last increments of a day or month reach Postgres
// (ADR-0070).
const Grace = 60 * time.Second

// Flushed returns the periods the flusher reads at t, Redis's time: the day
// and month containing t, and the previous day and month while t is within
// Grace of their end (ADR-0070).
func Flushed(t time.Time) []Period {
	out := []Period{Day(t), Month(t)}
	if prev := Day(t.Add(-Grace)); prev.ID != out[0].ID {
		out = append(out, prev)
	}
	if prev := Month(t.Add(-Grace)); prev.ID != out[1].ID {
		out = append(out, prev)
	}
	return out
}

// TTL is how long a counter of p lives after a write at t: the rest of the
// period plus one day (ADR-0070).
func (p Period) TTL(t time.Time) time.Duration {
	return p.End.Sub(t) + 24*time.Hour
}
