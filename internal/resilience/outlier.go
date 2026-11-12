// Package resilience holds the gateway's upstream state machines (ADR-0030):
// outlier ejection, the retry budget, the circuit breaker and the caps'
// gates. They read time from an injected Clock and know nothing of
// HTTP.
package resilience

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
)

// Clock tells the state machines the time; ratelimit.SystemClock and
// ratelimit.FakeClock implement it.
type Clock interface {
	Now() time.Time
}

// Outlier is one endpoint's outlier ejection state (GW-09, ADR-0030). It is
// safe for concurrent use.
type Outlier struct {
	mu       sync.Mutex
	failures int       // failed attempts in a row since the last success or ejection
	times    int       // k: ejections so far, decayed by the time spent back in rotation
	back     time.Time // when the last ejection ended
	until    atomic.Int64
}

// OutlierState is an Outlier's state, for tests and logs.
type OutlierState struct {
	Failures int       // failed attempts in a row
	Times    int       // k, as of the last ejection
	Until    time.Time // the last ejection's end; zero if never ejected
}

// Record notes the outcome of one attempt sent to the endpoint. It returns
// true when failed makes s.ConsecutiveFailures failures in a row: the count
// starts again from 0, and the caller ejects the endpoint with Eject if the
// upstream's max_ejected_percent allows. Outcomes that arrive while the
// endpoint is ejected, from attempts sent before, are ignored, so it comes
// back with a count of 0.
func (o *Outlier) Record(now time.Time, failed bool, s *config.OutlierDetection) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Ejected(now) { // under mu, so no outcome lands between Eject and the ejection's end
		return false
	}
	if !failed {
		o.failures = 0
		return false
	}
	o.failures++
	if o.failures < s.ConsecutiveFailures {
		return false
	}
	o.failures = 0
	return true
}

// Eject ejects the endpoint at now for base_ejection × k, at most
// max_ejection, and returns that duration; 0 if it is already ejected. k is
// the number of times it has been ejected, less one for each full
// base_ejection it has spent back in rotation since its last ejection ended:
// k = max(0, k − floor(time back ÷ base_ejection)) + 1.
func (o *Outlier) Eject(now time.Time, s *config.OutlierDetection) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Ejected(now) {
		return 0
	}
	if o.times > 0 {
		o.times = max(0, o.times-int(now.Sub(o.back)/s.BaseEjection))
	}
	o.times++
	d := min(s.BaseEjection*time.Duration(o.times), s.MaxEjection)
	o.back = now.Add(d)
	o.failures = 0
	o.until.Store(o.back.UnixNano())
	return d
}

// Ejected reports whether the endpoint is ejected at now. It takes no lock,
// so an upstream can count its ejected endpoints while one of them records.
func (o *Outlier) Ejected(now time.Time) bool {
	return now.UnixNano() < o.until.Load()
}

// State returns the Outlier's state.
func (o *Outlier) State() OutlierState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return OutlierState{Failures: o.failures, Times: o.times, Until: o.back}
}
