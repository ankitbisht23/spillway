package redis

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// Circuit breaker and health check parameters (RL-22, ADR-0025).
const (
	breakerFailures = 3                      // consecutive broken connections of request calls that open a breaker
	pingEvery       = 100 * time.Millisecond // a health check PINGs every node this often
	pingTimeout     = 50 * time.Millisecond  // each PING's own timeout, outside any request budget
	pingMisses      = 3                      // consecutive missed PINGs that open a breaker
)

// errBreakerOpen fails a call that a node's open breaker refused; it counts
// as conn (ADR-0021).
var errBreakerOpen = errors.New("circuit breaker open")

// breakers holds one circuit breaker per node address (RL-22, ADR-0025).
// Closed, every call is sent. A breaker opens after breakerFailures
// consecutive request calls to its node whose connection broke, or after
// pingMisses consecutive health checks it missed; request timeouts never
// count. Open, it refuses every call until a health check's PING is
// answered, which closes it: no request is ever sent as a probe.
type breakers struct {
	clock  ratelimit.Clock
	log    *slog.Logger
	opened func()            // called, outside the lock, each time a breaker opens
	closed func(addr string) // called, outside the lock, each time addr's breaker closes

	mu    sync.Mutex
	nodes map[string]*breaker
}

type breaker struct {
	failures int       // consecutive request calls whose connection broke
	misses   int       // consecutive missed PINGs
	missed   time.Time // when the first of those PINGs was sent
	open     bool
	since    time.Time // while open: when it opened
	held     error     // set for good: every call fails with it (RL-30)
}

func newBreakers(clock ratelimit.Clock, log *slog.Logger, opened func(), closed func(addr string)) *breakers {
	return &breakers{clock: clock, log: log, opened: opened, closed: closed, nodes: make(map[string]*breaker)}
}

func (b *breakers) get(addr string) *breaker {
	br := b.nodes[addr]
	if br == nil {
		br = &breaker{}
		b.nodes[addr] = br
	}
	return br
}

// add makes addr a node the health checks PING, if it is not one already.
func (b *breakers) add(addr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.get(addr)
}

// checked returns the nodes to PING, all but those held open for good, and
// whether any breaker is open.
func (b *breakers) checked() (addrs []string, open bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for addr, br := range b.nodes {
		if br.held == nil {
			addrs = append(addrs, addr)
			open = open || br.open
		}
	}
	return addrs, open
}

// allow reports whether a call to addr may be sent; the node becomes one
// the health checks PING. Every allowed call must be followed by done.
func (b *breakers) allow(addr string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(addr)
	switch {
	case br.held != nil:
		return br.held
	case br.open:
		return errBreakerOpen
	}
	return nil
}

// done records the result of a call that allow let through: broken says its
// connection to the node broke. A call that got any reply from Redis resets
// the count, and one that timed out leaves it as it is.
func (b *breakers) done(addr string, replied, broken bool) {
	b.mu.Lock()
	br := b.get(addr)
	opened := false
	switch {
	case replied:
		br.failures = 0
	case broken && !br.open:
		if br.failures++; br.failures >= breakerFailures {
			b.openLocked(addr, br, "failures", br.failures)
			opened = true
		}
	}
	// A call sent before the breaker opened that fails after it does not
	// count.
	b.mu.Unlock()
	if opened {
		b.opened()
	}
}

// pinged records a health check of addr, sent at sent: pingMisses
// consecutive misses open its breaker, and a PING answered while it is open
// closes it.
func (b *breakers) pinged(addr string, sent time.Time, ok bool) {
	b.mu.Lock()
	br := b.get(addr)
	opened, closed := false, false
	switch {
	case br.held != nil:
	case ok:
		if br.open {
			b.log.Info("redis limiter: circuit breaker closed", "node", addr, "open_for", b.clock.Now().Sub(br.since))
			br.open, br.failures, closed = false, 0, true
		}
		br.misses = 0
	case !br.open:
		if br.misses++; br.misses == 1 {
			br.missed = sent
		}
		if br.misses >= pingMisses {
			b.openLocked(addr, br, "missed_pings", br.misses)
			opened = true
		}
	}
	b.mu.Unlock()
	if opened {
		b.opened()
	}
	if closed {
		b.closed(addr)
	}
}

// openLocked opens br, the breaker of addr, and logs why.
func (b *breakers) openLocked(addr string, br *breaker, why string, n int) {
	br.open, br.since = true, b.clock.Now()
	args := []any{"node", addr, why, n}
	if why == "missed_pings" {
		args = append(args, "since_first_miss", br.since.Sub(br.missed))
	}
	b.log.Warn("redis limiter: circuit breaker open", args...)
}

// hold opens addr's breaker for good: every call fails with err, and the
// health checks leave it alone. It reports whether the breaker was not held
// before.
func (b *breakers) hold(addr string, err error) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(addr)
	first := br.held == nil
	br.held = err
	return first
}
