package loadgen

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestSlotIndex(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want int
	}{
		{0, 0}, {9999 * time.Millisecond, 0}, {10 * time.Second, 1}, {25 * time.Second, 2},
		{-time.Nanosecond, -1}, {-10 * time.Second, -1}, {-10*time.Second - 1, -2},
	} {
		if got := SlotIndex(tt.d, 10*time.Second); got != tt.want {
			t.Errorf("SlotIndex(%v) = %d, want %d", tt.d, got, tt.want)
		}
	}
	for i, want := range map[int]bool{-2: false, -1: true, 0: false, 1: true, 2: false, 59: true} {
		if SlotOn(i) != want {
			t.Errorf("SlotOn(%d) = %v", i, !want)
		}
	}
}

// TestToggleSchedule runs a toggled tenant at 10 rps, 2 s warm-up and 6 s
// measured in 2 s slots on a fake clock: it is scheduled only in odd slots
// (warm-up slot -1, measured slot 1), every slot is listed with its on flag,
// a slot's histogram holds only the requests due after its first second,
// and validity counts the scheduled requests, not the whole measured time.
func TestToggleSchedule(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	clk := &fakeClock{now: start}
	var mu sync.Mutex
	var due []time.Duration
	g := Generator{Clock: clk, Rate: 10, Warmup: 2 * time.Second, Duration: 6 * time.Second, MaxInFlight: 100,
		Slot: 2 * time.Second, Settle: time.Second, Toggle: true,
		Do: func(context.Context, time.Time) (int, string, error) {
			mu.Lock()
			due = append(due, clk.Now().Sub(start))
			mu.Unlock()
			return http.StatusOK, "HTTP/1.1", nil
		}}
	r := g.Run(t.Context())
	// Warm-up slot -1 is 0–2 s, measured slot 1 is 4–6 s from the start.
	for _, d := range clk.sleeps {
		off := d.Sub(start)
		if (off < 0 || off >= 2*time.Second) && (off < 4*time.Second || off >= 6*time.Second) {
			t.Errorf("request due at +%v, outside the on slots", off)
		}
	}
	if len(clk.sleeps) != 40 {
		t.Errorf("%d requests scheduled, want 40 (20 warm-up, 20 measured)", len(clk.sleeps))
	}
	if r.Requests != (RequestCounts{Scheduled: 20, Sent: 20}) || !r.Valid || r.AchievedRPS != 10 {
		t.Errorf("requests %+v, valid %v, achieved %v; want 20 scheduled and sent, valid, 10 rps while on", r.Requests, r.Valid, r.AchievedRPS)
	}
	want := []SlotResult{{Index: 0, On: false}, {Index: 1, On: true, Requests: 10}, {Index: 2, On: false}}
	if len(r.Slots) != 3 {
		t.Fatalf("slots = %+v", r.Slots)
	}
	for i, s := range r.Slots {
		if s.Index != want[i].Index || s.On != want[i].On || s.Requests != want[i].Requests {
			t.Errorf("slot %d = %+v, want %+v", i, s, want[i])
		}
	}
}

// TestSlotsWithoutToggle: an always-on tenant fills every slot, and a
// toggled tenant that drops requests is invalid.
func TestSlotsWithoutToggle(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_000_000, 0)}
	g := Generator{Clock: clk, Rate: 10, Warmup: time.Second, Duration: 6 * time.Second, MaxInFlight: 100,
		Slot: 2 * time.Second, Settle: time.Second,
		Do: func(context.Context, time.Time) (int, string, error) { return http.StatusOK, "HTTP/1.1", nil }}
	r := g.Run(t.Context())
	if len(r.Slots) != 3 || !r.Valid {
		t.Fatalf("slots %+v, valid %v", r.Slots, r.Valid)
	}
	for _, s := range r.Slots {
		if !s.On || s.Requests != 10 {
			t.Errorf("slot %+v, want on with 10 settled requests", s)
		}
	}

	block := make(chan struct{})
	defer close(block)
	g = Generator{Clock: &fakeClock{now: time.Unix(1_000_000, 0)}, Rate: 10, Duration: 4 * time.Second, MaxInFlight: 5,
		Slot: 2 * time.Second, Settle: time.Second, Toggle: true,
		Do: func(ctx context.Context, _ time.Time) (int, string, error) {
			select {
			case <-block:
			case <-time.After(100 * time.Millisecond):
			}
			return http.StatusOK, "HTTP/1.1", nil
		}}
	if r = g.Run(t.Context()); r.Valid || r.Requests.Dropped == 0 {
		t.Errorf("a tenant that dropped requests: %+v, want invalid", r.Requests)
	}
}
