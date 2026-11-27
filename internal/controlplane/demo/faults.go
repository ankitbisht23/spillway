package demo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ankitbisht23/spillway/internal/demo"
)

// The demo's two fault switches (SC-03, ADR-0095 §5), changed through
// toxiproxy's API only: no container has the Docker socket.
const (
	RedisCut     = "redis-cut"     // D5: the demo gateways' Redis traffic cut at its proxies
	UpstreamSlow = "upstream-slow" // D6: the orders upstream 4 s slower at its proxies
)

// FaultNames are the faults, in getDemo's order.
var FaultNames = []string{RedisCut, UpstreamSlow}

const (
	// toxiproxyTimeout bounds each toxiproxy call (ADR-0095 §5).
	toxiproxyTimeout = 2 * time.Second
	// stateMaxAge is how old getDemo's fault states may be (ADR-0095 §2).
	stateMaxAge = time.Second
)

// fault is where a fault's toxics go: each of them on each of the proxies.
type fault struct {
	proxies []string
	toxics  []demo.Toxic
}

// faults are ADR-0095 §5's table. A timeout toxic with timeout 0 holds every
// byte in its direction and keeps the connection open: a black hole, so
// each Redis call ends at its time budget (RL-22) and the per-node breakers
// open, while the cluster itself never sees a fault. 4,000 ms is over the
// orders route's 2.5 s per-try timeout, so every attempt through a slowed
// endpoint ends at its deadline.
var faults = map[string]fault{
	RedisCut: {
		proxies: []string{"demo-redis-0", "demo-redis-1", "demo-redis-2", "demo-redis-3", "demo-redis-4", "demo-redis-5"},
		toxics: []demo.Toxic{
			{Name: "demo-cut-up", Type: "timeout", Stream: "upstream", Toxicity: 1, Attributes: map[string]int64{"timeout": 0}},
			{Name: "demo-cut-down", Type: "timeout", Stream: "downstream", Toxicity: 1, Attributes: map[string]int64{"timeout": 0}},
		},
	},
	UpstreamSlow: {
		proxies: []string{"demo-upstream-1", "demo-upstream-2", "demo-upstream-3"},
		toxics: []demo.Toxic{
			{Name: "demo-slow", Type: "latency", Stream: "downstream", Toxicity: 1, Attributes: map[string]int64{"latency": 4000, "jitter": 0}},
		},
	},
}

// ErrNoFault is a fault name that is not one of FaultNames.
var ErrNoFault = errors.New("no such fault")

// Faults switches the demo's faults through toxiproxy's API and reads
// their states back from it, the one source of truth. A fault is active
// while any of its toxics is in place, so a change that failed half-way
// shows as on until it is switched off. One change runs at a time.
type Faults struct {
	tp  *demo.Toxiproxy
	now func() time.Time

	mu       sync.Mutex
	states   map[string]*bool // the last read, nil for unknown
	statesAt time.Time        // when it was read; zero: read again
}

// NewFaults returns the faults of the toxiproxy API at base, such as
// http://toxiproxy:8474.
func NewFaults(base string) *Faults {
	return &Faults{tp: demo.NewToxiproxy(base, toxiproxyTimeout), now: time.Now}
}

// FaultError is a change toxiproxy could not make or a state it could not
// give. Its message, which the API answers, names the proxy and the toxic,
// never data; Cause, for the log, is toxiproxy's error.
type FaultError struct {
	msg   string
	Cause error
}

func (e *FaultError) Error() string { return e.msg }

// read returns each fault's state from toxiproxy's proxies, nil for a fault
// with a proxy toxiproxy does not have; an error when toxiproxy could not
// be read.
func (f *Faults) read(ctx context.Context) (map[string]*bool, map[string][]string, error) {
	toxics, err := f.tp.ProxyToxics(ctx)
	if err != nil {
		return nil, nil, &FaultError{"toxiproxy could not be read", err}
	}
	out := map[string]*bool{}
	for name, flt := range faults {
		active, known := false, true
		for _, p := range flt.proxies {
			names, ok := toxics[p]
			if !ok {
				known = false
				break
			}
			for _, x := range flt.toxics {
				active = active || slices.Contains(names, x.Name)
			}
		}
		if known {
			out[name] = &active
		}
	}
	return out, toxics, nil
}

// States returns each fault's state, by name, at most a second old; a
// fault's state is nil when toxiproxy could not be read or lacks one of its
// proxies.
func (f *Faults) States(ctx context.Context) map[string]*bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statesAt.IsZero() || f.now().Sub(f.statesAt) >= stateMaxAge {
		states, _, err := f.read(ctx)
		if err != nil {
			states = map[string]*bool{}
		}
		f.states, f.statesAt = states, f.now()
	}
	return f.states
}

// Set switches the fault name on or off and returns whether it was active
// before. On, it adds each of the fault's toxics that a proxy lacks; off,
// it removes each that is there; a fault already in that state changes
// nothing. Then it reads the state back. On an error the toxics already
// changed stay as they are, so the same call again completes the change.
func (f *Faults) Set(ctx context.Context, name string, on bool) (before bool, err error) {
	flt, ok := faults[name]
	if !ok {
		return false, ErrNoFault
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statesAt = time.Time{} // whatever happens, read again next time
	states, toxics, err := f.read(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range flt.proxies {
		if _, ok := toxics[p]; !ok {
			return false, &FaultError{fmt.Sprintf("toxiproxy has no proxy %s (make demo creates it)", p), nil}
		}
	}
	before = *states[name]
	for _, p := range flt.proxies {
		for _, x := range flt.toxics {
			switch there := slices.Contains(toxics[p], x.Name); {
			case on && !there:
				err = f.tp.AddToxic(ctx, p, x)
			case !on && there:
				err = f.tp.DeleteToxic(ctx, p, x.Name)
			}
			if err != nil {
				return before, &FaultError{fmt.Sprintf("toxiproxy could not change %s on %s", x.Name, p), err}
			}
		}
	}
	states, toxics, err = f.read(ctx)
	if err != nil {
		return before, err
	}
	if st := states[name]; st == nil || *st != on {
		return before, &FaultError{"toxiproxy did not keep the change of " + name, nil}
	}
	if on {
		for _, p := range flt.proxies {
			for _, x := range flt.toxics {
				if !slices.Contains(toxics[p], x.Name) {
					return before, &FaultError{fmt.Sprintf("toxiproxy did not keep %s on %s", x.Name, p), nil}
				}
			}
		}
	}
	return before, nil
}
