package redis

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// breakerTest is one node's breaker on the fake clock.
type breakerTest struct {
	t      *testing.T
	clock  *ratelimit.FakeClock
	logs   *bytes.Buffer
	b      *breakers
	opened int
	closed []string
}

const testNode = "redis-1:6379"

func newBreakerTest(t *testing.T) *breakerTest {
	bt := &breakerTest{t: t, clock: ratelimit.NewFakeClock(time.Unix(1_700_000_000, 0)), logs: &bytes.Buffer{}}
	bt.b = newBreakers(bt.clock, slog.New(slog.NewTextHandler(bt.logs, nil)), func() { bt.opened++ }, func(addr string) { bt.closed = append(bt.closed, addr) })
	return bt
}

// call sends one request call that ends replied, broken or neither (a
// timeout), if the breaker lets it.
func (bt *breakerTest) call(replied, broken bool) error {
	err := bt.b.allow(testNode)
	if err == nil {
		bt.b.done(testNode, replied, broken)
	}
	return err
}

func (bt *breakerTest) mustAllow(step string) {
	bt.t.Helper()
	if err := bt.call(true, false); err != nil {
		bt.t.Fatalf("%s: %v; want a closed breaker", step, err)
	}
}

func (bt *breakerTest) mustRefuse(step string) {
	bt.t.Helper()
	if err := bt.b.allow(testNode); !errors.Is(err, errBreakerOpen) {
		bt.t.Fatalf("%s: allow = %v, want %v", step, err, errBreakerOpen)
	}
}

// round is one health check round on the fake clock: the PING is sent now
// and answered, or missed after pingTimeout; then the clock moves on to the
// next round.
func (bt *breakerTest) round(ok bool) {
	sent := bt.clock.Now()
	if !ok {
		bt.clock.Advance(pingTimeout)
	}
	bt.b.pinged(testNode, sent, ok)
	bt.clock.Advance(pingEvery - (bt.clock.Now().Sub(sent)))
}

// TestBreakerBrokenConnections: 3 consecutive request calls whose
// connection broke open a breaker; a reply resets the count, a timeout
// leaves it; open, it refuses every call until a PING is answered (RL-22,
// ADR-0025).
func TestBreakerBrokenConnections(t *testing.T) {
	bt := newBreakerTest(t)

	// Two broken, a script error reply: the count starts again. Two broken,
	// a timeout: the count holds, and the next broken one opens it.
	_, _ = bt.call(false, true), bt.call(false, true)
	_ = bt.call(true, false)
	_, _ = bt.call(false, true), bt.call(false, true)
	_ = bt.call(false, false)
	if bt.opened != 0 {
		t.Fatal("opened before 3 consecutive broken connections")
	}
	_ = bt.call(false, true)
	if bt.opened != 1 {
		t.Fatalf("opened %d times after 3 broken connections, want 1", bt.opened)
	}
	bt.mustRefuse("just opened")
	// Time alone never closes it: no request is sent as a probe.
	bt.clock.Advance(time.Hour)
	bt.mustRefuse("an hour later")
	// A late failure of a call sent before it opened changes nothing.
	bt.b.done(testNode, false, true)
	if bt.opened != 1 {
		t.Errorf("opened %d times, want 1", bt.opened)
	}
	// Another node is not affected.
	if err := bt.b.allow("redis-2:6379"); err != nil {
		t.Errorf("another node's breaker: %v", err)
	}

	// The first answered PING closes it, and the count starts again.
	bt.round(true)
	bt.mustAllow("after an answered PING")
	_, _ = bt.call(false, true), bt.call(false, true)
	bt.mustAllow("2 broken connections after closing")

	got := bt.logs.String()
	if strings.Count(got, `circuit breaker open" node=`+testNode+" failures=3") != 1 || strings.Count(got, `circuit breaker closed" node=`+testNode+" open_for=1h0m0s") != 1 {
		t.Errorf("transition logs:\n%s", got)
	}
}

// TestBreakerHotKey: a storm of request timeouts, as a burst on one hot key
// past the budget makes, never opens a breaker; nor do script errors from
// Redis (ADR-0025).
func TestBreakerHotKey(t *testing.T) {
	bt := newBreakerTest(t)
	for i := range 10_000 {
		if err := bt.call(false, false); err != nil {
			t.Fatalf("timeout %d: %v", i+1, err)
		}
		if i%100 == 0 {
			bt.round(true) // the node still answers its PINGs
		}
	}
	for range 100 {
		bt.mustAllow("a script error")
	}
	if bt.opened != 0 || bt.logs.Len() != 0 {
		t.Errorf("opened %d times:\n%s", bt.opened, bt.logs.String())
	}
}

// TestBreakerPings: a node that stops answering is found by the health
// checks alone, its breaker open 300 to 400 ms after it stopped; the first
// answered PING closes it (RL-22, ADR-0025).
func TestBreakerPings(t *testing.T) {
	bt := newBreakerTest(t)
	for range 5 {
		bt.round(true)
	}
	// Two misses, an answer: the count starts again.
	bt.round(false)
	bt.round(false)
	bt.round(true)
	if bt.opened != 0 {
		t.Fatal("opened before 3 consecutive missed PINGs")
	}

	// The node pauses at some point within a round.
	bt.clock.Advance(30 * time.Millisecond)
	paused := bt.clock.Now()
	bt.clock.Advance(pingEvery - 30*time.Millisecond)
	for bt.opened == 0 {
		if err := bt.b.allow(testNode); err != nil {
			t.Fatalf("open before its third missed PING: %v", err)
		}
		bt.round(false)
	}
	// The third miss is known pingTimeout after its PING was sent.
	detected := bt.clock.Now().Sub(paused) - (pingEvery - pingTimeout)
	if detected < 300*time.Millisecond || detected > 400*time.Millisecond {
		t.Errorf("open %v after the node paused, want 300 to 400 ms", detected)
	}
	t.Logf("measure: open %v after the node paused", detected)
	bt.mustRefuse("open")
	// More misses keep it open without opening it again.
	for range 20 {
		bt.round(false)
	}
	bt.mustRefuse("still down")
	if bt.opened != 1 {
		t.Errorf("opened %d times, want 1", bt.opened)
	}

	bt.round(true)
	bt.mustAllow("the first answered PING")
	if !slices.Equal(bt.closed, []string{testNode}) {
		t.Errorf("closed callbacks %v, want one for %s", bt.closed, testNode)
	}
	// Closed afresh: it takes three new misses to open again.
	bt.round(false)
	bt.round(false)
	bt.mustAllow("two misses after closing")

	got := bt.logs.String()
	if strings.Count(got, `circuit breaker open" node=`+testNode+" missed_pings=3 since_first_miss=250ms") != 1 || strings.Count(got, `circuit breaker closed" node=`+testNode+" open_for=2.05s") != 1 {
		t.Errorf("transition logs:\n%s", got)
	}
}

// TestBreakerHold: a node held open fails every call with the held error,
// whatever the clock or its PINGs say, and is not PINGed (RL-30).
func TestBreakerHold(t *testing.T) {
	bt := newBreakerTest(t)
	bt.b.add("redis-2:6379")
	if !bt.b.hold("old:6379", errOldRedis) || bt.b.hold("old:6379", errOldRedis) {
		t.Error("hold reported the first time wrongly")
	}
	for range 3 {
		bt.b.pinged("old:6379", bt.clock.Now(), true)
		if err := bt.b.allow("old:6379"); !errors.Is(err, errOldRedis) {
			t.Fatalf("allow = %v, want %v", err, errOldRedis)
		}
		bt.clock.Advance(time.Hour)
	}
	if addrs, open := bt.b.checked(); !slices.Equal(addrs, []string{"redis-2:6379"}) || open {
		t.Errorf("checked = %v, %v; want only redis-2:6379, none open", addrs, open)
	}
}
