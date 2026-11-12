package resilience

import (
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

var t0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

var _ Clock = ratelimit.SystemClock

func settings(n int) *config.OutlierDetection {
	return &config.OutlierDetection{ConsecutiveFailures: n, BaseEjection: 30 * time.Second, MaxEjection: 100 * time.Second, MaxEjectedPercent: 50}
}

// fail records n failures and returns how many of them reached the threshold.
func fail(o *Outlier, now time.Time, n int, s *config.OutlierDetection) int {
	trips := 0
	for range n {
		if o.Record(now, true, s) {
			trips++
		}
	}
	return trips
}

// TestOutlierThreshold: the N-th failure in a row trips, a success resets
// the count, and the count restarts after a trip whether or not the caller
// ejects (GW-09).
func TestOutlierThreshold(t *testing.T) {
	s := settings(3)
	var o Outlier
	if fail(&o, t0, 2, s) != 0 {
		t.Fatal("tripped below the threshold")
	}
	o.Record(t0, false, s)
	if fail(&o, t0, 2, s) != 0 {
		t.Fatal("a success did not reset the count")
	}
	if !o.Record(t0, true, s) {
		t.Fatal("the third failure in a row did not trip")
	}
	if got := o.State().Failures; got != 0 {
		t.Errorf("count after a trip = %d, want 0", got)
	}
	if o.Ejected(t0) {
		t.Error("ejected without Eject")
	}
	// Not ejected (the cap held it back): it takes another N in a row.
	if fail(&o, t0, 2, s) != 0 || !o.Record(t0, true, s) {
		t.Error("after a held-back trip, the next trip did not take N failures")
	}
	// A setting changed in place applies to the running count.
	fail(&o, t0, 2, s)
	if !o.Record(t0, true, settings(3)) || fail(&o, t0, 1, settings(1)) != 1 {
		t.Error("the threshold is not read on each record")
	}
}

// TestOutlierEjection: an ejection lasts base × k, capped at max; outcomes
// while ejected are ignored; the endpoint returns with a count of 0 (GW-09).
func TestOutlierEjection(t *testing.T) {
	s := settings(1)
	clock := ratelimit.NewFakeClock(t0)
	var o Outlier
	want := []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second, 100 * time.Second, 100 * time.Second}
	for i, w := range want {
		now := clock.Now()
		if !o.Record(now, true, s) {
			t.Fatalf("ejection %d: no trip", i+1)
		}
		if d := o.Eject(now, s); d != w {
			t.Fatalf("ejection %d lasts %v, want %v", i+1, d, w)
		}
		if d := o.Eject(now, s); d != 0 {
			t.Errorf("ejection %d: a second Eject while ejected = %v, want 0", i+1, d)
		}
		clock.Advance(w - time.Nanosecond)
		if !o.Ejected(clock.Now()) {
			t.Fatalf("ejection %d ended early", i+1)
		}
		if o.Record(clock.Now(), true, s) {
			t.Errorf("ejection %d: an outcome while ejected tripped", i+1)
		}
		clock.Advance(time.Nanosecond)
		if o.Ejected(clock.Now()) {
			t.Fatalf("ejection %d did not end after %v", i+1, w)
		}
		if st := o.State(); st.Failures != 0 || st.Times != i+1 {
			t.Errorf("ejection %d: back with %+v, want 0 failures and k = %d", i+1, st, i+1)
		}
	}
}

// TestOutlierDecay: k drops by one for each full base_ejection back in
// rotation, worked out at the next ejection, and never below 0.
func TestOutlierDecay(t *testing.T) {
	s := settings(1)
	var o Outlier
	steps := []struct {
		back time.Duration // since the previous ejection ended
		want time.Duration
	}{
		{0, 30 * time.Second},                // k = 1
		{0, 60 * time.Second},                // k = 1 − 0 + 1 = 2
		{59 * time.Second, 60 * time.Second}, // one full base back: k = 2 − 1 + 1 = 2
		{60 * time.Second, 30 * time.Second}, // two bases back: k = 2 − 2 + 1 = 1
		{10 * time.Minute, 30 * time.Second}, // k = max(0, 1 − 20) + 1 = 1
	}
	for i, st := range steps {
		now := o.State().Until.Add(st.back)
		if i == 0 {
			now = t0
		}
		o.Record(now, true, s)
		if d := o.Eject(now, s); d != st.want {
			t.Errorf("step %d, %v back: ejected for %v, want %v", i+1, st.back, d, st.want)
		}
	}
}

// TestOutlierSettingsInPlace: changed durations apply at the next ejection,
// and k survives the change (GW-29).
func TestOutlierSettingsInPlace(t *testing.T) {
	var o Outlier
	s := settings(1)
	o.Record(t0, true, s)
	o.Eject(t0, s)
	now := o.State().Until
	s2 := &config.OutlierDetection{ConsecutiveFailures: 1, BaseEjection: 10 * time.Second, MaxEjection: 15 * time.Second, MaxEjectedPercent: 50}
	o.Record(now, true, s2)
	if d := o.Eject(now, s2); d != 15*time.Second {
		t.Errorf("second ejection under new settings = %v, want min(10s × 2, 15s)", d)
	}
}
