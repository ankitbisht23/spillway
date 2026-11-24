package quota

import (
	"slices"
	"testing"
	"time"
)

// TestPeriods: UTC days and months, their IDs and bounds across midnight,
// month ends, a leap day and a non-UTC input (RL-10, ADR-0070).
func TestPeriods(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	for _, tt := range []struct {
		at                 time.Time
		day, month         string
		dayEnd, monthStart string
		daySecs, monthSecs int64
	}{
		{time.Date(2026, 11, 24, 23, 59, 59, 999_999_999, time.UTC), "d20261124", "m202611", "2026-11-25T00:00:00Z", "2026-11-01T00:00:00Z", 86400, 30 * 86400},
		{time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), "d20261125", "m202611", "2026-11-26T00:00:00Z", "2026-11-01T00:00:00Z", 86400, 30 * 86400},
		{time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), "d20261231", "m202612", "2027-01-01T00:00:00Z", "2026-12-01T00:00:00Z", 86400, 31 * 86400},
		{time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC), "d20280229", "m202802", "2028-03-01T00:00:00Z", "2028-02-01T00:00:00Z", 86400, 29 * 86400},
		{time.Date(2027, 2, 28, 12, 0, 0, 0, time.UTC), "d20270228", "m202702", "2027-03-01T00:00:00Z", "2027-02-01T00:00:00Z", 86400, 28 * 86400},
		// 01:00 in India on 1 December is still 30 November in UTC.
		{time.Date(2026, 12, 1, 1, 0, 0, 0, ist), "d20261130", "m202611", "2026-12-01T00:00:00Z", "2026-11-01T00:00:00Z", 86400, 30 * 86400},
	} {
		d, m := Day(tt.at), Month(tt.at)
		if d.ID != tt.day || m.ID != tt.month || d.End.Format(time.RFC3339) != tt.dayEnd || m.Start.Format(time.RFC3339) != tt.monthStart ||
			d.Seconds() != tt.daySecs || m.Seconds() != tt.monthSecs || !d.Start.Add(24*time.Hour).Equal(d.End) {
			t.Errorf("%v: day %+v (%d s), month %+v (%d s)", tt.at, d, d.Seconds(), m, m.Seconds())
		}
		if tt.at.Before(d.Start) || !tt.at.Before(d.End) || tt.at.Before(m.Start) || !tt.at.Before(m.End) {
			t.Errorf("%v is not inside its periods", tt.at)
		}
	}
}

// TestFlushed: the flusher reads the current day and month, plus the
// previous ones for Grace after they end (ADR-0070).
func TestFlushed(t *testing.T) {
	ids := func(ps []Period) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	for _, tt := range []struct {
		at   time.Time
		want []string
	}{
		{time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), []string{"d20261124", "m202611"}},
		{time.Date(2026, 11, 25, 0, 0, 59, 0, time.UTC), []string{"d20261125", "m202611", "d20261124"}},
		{time.Date(2026, 11, 25, 0, 1, 0, 0, time.UTC), []string{"d20261125", "m202611"}},
		{time.Date(2026, 12, 1, 0, 0, 30, 0, time.UTC), []string{"d20261201", "m202612", "d20261130", "m202611"}},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), []string{"d20270101", "m202701", "d20261231", "m202612"}},
	} {
		if got := ids(Flushed(tt.at)); !slices.Equal(got, tt.want) {
			t.Errorf("Flushed(%v) = %v, want %v", tt.at, got, tt.want)
		}
	}
}

// TestTTL: a counter lives for the rest of its period plus a day.
func TestTTL(t *testing.T) {
	at := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)
	if got := Day(at).TTL(at); got != 30*time.Hour {
		t.Errorf("day TTL %v, want 30h", got)
	}
	if got := Month(at).TTL(at); got != (6*24+6+24)*time.Hour {
		t.Errorf("month TTL %v, want 6d6h + 1d", got)
	}
}
