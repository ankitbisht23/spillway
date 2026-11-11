package redis

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// TestBreakerTransitions walks one node's breaker through every transition
// on the fake clock (RL-22, ADR-0021): closed until 5 consecutive failures,
// open for 1 s, half-open with one probe, closed again on a successful probe
// and open for another second on a failed one.
func TestBreakerTransitions(t *testing.T) {
	clock := ratelimit.NewFakeClock(time.Unix(1_700_000_000, 0))
	var logs bytes.Buffer
	opened := 0
	b := newBreakers(clock, slog.New(slog.NewTextHandler(&logs, nil)), func() { opened++ })
	const node = "redis-1:6379"
	call := func(ok bool) error {
		t.Helper()
		probe, err := b.allow(node)
		if err == nil {
			b.done(node, probe, ok)
		}
		return err
	}
	mustAllow := func(step string) {
		t.Helper()
		if probe, err := b.allow(node); err != nil || probe {
			t.Fatalf("%s: allow = %v, %v; want a closed breaker", step, probe, err)
		} else {
			b.done(node, false, true)
		}
	}
	mustRefuse := func(step string) {
		t.Helper()
		if _, err := b.allow(node); !errors.Is(err, errBreakerOpen) {
			t.Fatalf("%s: allow error = %v, want %v", step, err, errBreakerOpen)
		}
	}

	// Four failures, then a success: the count starts again.
	for range 4 {
		_ = call(false)
	}
	mustAllow("a success after 4 failures")
	for range 4 {
		_ = call(false)
	}
	if opened != 0 {
		t.Fatal("opened before 5 consecutive failures")
	}
	// The 5th consecutive failure opens it for 1 s; refused calls neither
	// count nor extend it.
	_ = call(false)
	if opened != 1 {
		t.Fatalf("opened %d times after 5 failures, want 1", opened)
	}
	mustRefuse("just opened")
	clock.Advance(999 * time.Millisecond)
	mustRefuse("after 999ms")
	// Another node is not affected.
	if _, err := b.allow("redis-2:6379"); err != nil {
		t.Errorf("another node's breaker: %v", err)
	}

	// Half-open: one probe at a time; a failed probe opens it for another
	// second.
	clock.Advance(time.Millisecond)
	probe, err := b.allow(node)
	if err != nil || !probe {
		t.Fatalf("after 1s: allow = %v, %v; want the probe", probe, err)
	}
	mustRefuse("while the probe is out")
	b.done(node, true, false)
	if opened != 2 {
		t.Errorf("opened %d times after a failed probe, want 2", opened)
	}
	mustRefuse("after a failed probe")
	clock.Advance(time.Second)

	// A late failure of a call sent before the breaker opened does not
	// extend it.
	b.done(node, false, false)
	probe, err = b.allow(node)
	if err != nil || !probe {
		t.Fatalf("after another 1s: allow = %v, %v; want the probe", probe, err)
	}
	// A successful probe closes it.
	b.done(node, true, true)
	mustAllow("after a successful probe")
	for range 4 {
		_ = call(false)
	}
	mustAllow("4 failures after closing")
	if opened != 2 {
		t.Errorf("opened %d times, want 2", opened)
	}

	got := logs.String()
	if strings.Count(got, `circuit breaker open"`) != 1 || strings.Count(got, "probe failed") != 1 || strings.Count(got, "circuit breaker closed") != 1 || strings.Count(got, "node="+node) != 3 {
		t.Errorf("transition logs:\n%s", got)
	}
}

// TestBreakerHold: a node held open fails every call with the held error,
// whatever the clock says (RL-30).
func TestBreakerHold(t *testing.T) {
	clock := ratelimit.NewFakeClock(time.Unix(1_700_000_000, 0))
	b := newBreakers(clock, slog.New(slog.DiscardHandler), func() {})
	if !b.hold("old:6379", errOldRedis) || b.hold("old:6379", errOldRedis) {
		t.Error("hold reported the first time wrongly")
	}
	for range 3 {
		if _, err := b.allow("old:6379"); !errors.Is(err, errOldRedis) {
			t.Fatalf("allow = %v, want %v", err, errOldRedis)
		}
		clock.Advance(time.Hour)
	}
}
