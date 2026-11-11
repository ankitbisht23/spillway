package redis

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// Circuit breaker parameters (RL-22, ADR-0021).
const (
	breakerFailures = 5           // consecutive failed calls that open a breaker
	breakerOpen     = time.Second // how long it stays open before a probe
)

// errBreakerOpen fails a call that a node's open breaker refused; it counts
// as conn (ADR-0021).
var errBreakerOpen = errors.New("circuit breaker open")

// breakers holds one circuit breaker per primary address (RL-22). Closed,
// every call is sent; after breakerFailures consecutive failures a breaker
// opens for breakerOpen and refuses calls; then the first call is sent as a
// probe while others are refused, and its result closes or reopens it.
// Refused calls are not failures and never extend it (ADR-0021).
type breakers struct {
	clock  ratelimit.Clock
	log    *slog.Logger
	opened func() // called, outside the lock, each time a breaker opens or a probe fails

	mu    sync.Mutex
	nodes map[string]*breaker
}

type breaker struct {
	failures int       // consecutive, while closed
	open     bool      // open or half-open
	until    time.Time // while open: when a probe may go
	probing  bool      // half-open: the probe is out
	held     error     // set for good: every call fails with it (RL-30)
}

func newBreakers(clock ratelimit.Clock, log *slog.Logger, opened func()) *breakers {
	return &breakers{clock: clock, log: log, opened: opened, nodes: make(map[string]*breaker)}
}

func (b *breakers) get(addr string) *breaker {
	br := b.nodes[addr]
	if br == nil {
		br = &breaker{}
		b.nodes[addr] = br
	}
	return br
}

// allow reports whether a call to addr may be sent. probe is true when the
// call is the half-open breaker's probe. Every allowed call must be followed
// by done.
func (b *breakers) allow(addr string) (probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(addr)
	switch {
	case br.held != nil:
		return false, br.held
	case !br.open:
		return false, nil
	case br.probing || b.clock.Now().Before(br.until):
		return false, errBreakerOpen
	}
	br.probing = true
	return true, nil
}

// done records the result of a call that allow let through.
func (b *breakers) done(addr string, probe, ok bool) {
	b.mu.Lock()
	br := b.get(addr)
	opened := false
	switch {
	case ok:
		if br.open {
			b.log.Info("redis limiter: circuit breaker closed", "node", addr)
		}
		br.failures, br.open, br.probing = 0, false, false
	case probe && br.probing:
		br.probing, br.until, opened = false, b.clock.Now().Add(breakerOpen), true
		b.log.Warn("redis limiter: circuit breaker probe failed; open again", "node", addr, "for", breakerOpen)
	case !br.open:
		if br.failures++; br.failures >= breakerFailures {
			br.open, br.until, opened = true, b.clock.Now().Add(breakerOpen), true
			b.log.Warn("redis limiter: circuit breaker open", "node", addr, "failures", br.failures, "for", breakerOpen)
		}
	}
	// A call sent before the breaker opened that fails after it does not
	// extend it.
	b.mu.Unlock()
	if opened {
		b.opened()
	}
}

// hold opens addr's breaker for good: every call fails with err. It
// reports whether the breaker was not held before.
func (b *breakers) hold(addr string, err error) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(addr)
	first := br.held == nil
	br.held = err
	return first
}
