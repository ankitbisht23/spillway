// Package live serves the admin UI's live data (UI-03, RL-21, ADR-0059):
// GET /api/v1/stream's server-sent events, carrying named queries that the
// control plane evaluates against Prometheus and Postgres. Each distinct
// query and parameters (a key) is evaluated once per interval however many
// streams watch it, and every stream gets each result.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Postgres is what the catalogue reads from the store. Data is the REST
// answers' shape (ADR-0059), so the UI has one type for REST and stream.
type Postgres interface {
	// RouteExists reports whether the latest published config has a route
	// named name.
	RouteExists(ctx context.Context, name string) (bool, error)
	// TenantExists reports whether a tenant named name exists.
	TenantExists(ctx context.Context, name string) (bool, error)
	// Replicas are every replica with its staleness (the spec's
	// StreamReplicas).
	Replicas(ctx context.Context) (any, error)
	// HeavyHitters are the top 50 throttled keys of the windows since
	// since, of one policy or all (the spec's HeavyHitterList).
	HeavyHitters(ctx context.Context, since time.Time, policyID *int64) (any, error)
	// Unavailable reports whether err means Postgres is unavailable.
	Unavailable(err error) bool
}

// Config is a hub's sources and limits. Zero durations and counts take
// ADR-0059's values; tests shorten them.
type Config struct {
	Prometheus *Prometheus // nil when SPILLWAY_CP_PROMETHEUS_URL is unset
	Postgres   Postgres
	Registerer prometheus.Registerer // the admin port's registry; nil registers nothing
	Log        *slog.Logger

	Interval     time.Duration // between evaluations of a key: 5 s
	Timeout      time.Duration // of one evaluation: 4 s
	Concurrency  int           // evaluations at once across keys: 8
	WriteTimeout time.Duration // for a client to take an event: 10 s
	Keepalive    time.Duration // between keepalive comments: 15 s
	PerSubject   int           // open streams per token subject: 10
	Total        int           // open streams in all: 200
}

func (c *Config) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&c.Interval, 5*time.Second)
	set(&c.Timeout, 4*time.Second)
	set(&c.WriteTimeout, 10*time.Second)
	set(&c.Keepalive, 15*time.Second)
	for p, v := range map[*int]int{&c.Concurrency: 8, &c.PerSubject: 10, &c.Total: 200} {
		if *p == 0 {
			*p = v
		}
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
}

// Hub evaluates the keys that open streams subscribe to.
type Hub struct {
	cfg     Config
	metrics *metrics
	sem     chan struct{}   // Concurrency slots
	ctx     context.Context // cancelled at Shutdown: evaluations stop
	cancel  context.CancelFunc

	mu        sync.Mutex
	entries   map[string]*entry
	open      int            // open streams
	bySubject map[string]int // open streams per subject
	shutdown  chan struct{}  // closed at Shutdown
	closed    bool
}

// NewHub returns a hub; its metrics register with cfg.Registerer.
func NewHub(cfg Config) *Hub {
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		cfg: cfg, metrics: newMetrics(cfg.Registerer), sem: make(chan struct{}, cfg.Concurrency), ctx: ctx, cancel: cancel,
		entries: map[string]*entry{}, bySubject: map[string]int{}, shutdown: make(chan struct{}),
	}
}

// Shutdown ends every open stream with an end event (reason shutdown) and
// refuses new ones; register it with http.Server.RegisterOnShutdown so a
// graceful shutdown is not held up by streams.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.shutdown)
		h.cancel()
	}
}

// event is one SSE event, framed once and shared by every stream.
type event struct{ frame []byte }

func newEvent(name string, data any) *event {
	b, err := json.Marshal(data)
	if err != nil { // every event is made of JSON-safe values
		panic("live: marshalling a " + name + " event: " + err.Error())
	}
	return &event{frame: append(append([]byte("event: "+name+"\ndata: "), b...), '\n', '\n')}
}

// The events' data (the spec's StreamReady, StreamResult, StreamError,
// StreamEnd).

type readyData struct {
	Subscriptions []string `json:"subscriptions"`
	IntervalS     int      `json:"interval_s"`
}

type resultData struct {
	Sub  string    `json:"sub"`
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}

type errorData struct {
	Sub     string    `json:"sub"`
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

type endData struct {
	Reason string `json:"reason"`
}

// sink is one subscription of one stream: where a key's results go.
type sink struct {
	s *Stream
	i int // the subscription's index in the stream
}

// entry is a key's state: its subscribers, its loop and its last result,
// kept for one interval after the last subscriber leaves.
type entry struct {
	q         *query
	params    Params
	key       string
	sinks     map[sink]struct{}
	last      *event
	lastStart time.Time     // when the last evaluation started; zero before the first
	busy      bool          // an evaluation is running
	failing   bool          // the last evaluation failed (logged once per failure run)
	stop      chan struct{} // closed to stop the loop; nil when none runs
	expire    *time.Timer   // drops the entry; set while it has no sinks
}

// join subscribes s to q's key under p: s gets the key's last result at
// once, if there is one, and every result after; the key's loop starts if
// it was not running.
func (h *Hub) join(q *query, p Params, s sink) {
	k := q.key(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[k]
	if e == nil {
		e = &entry{q: q, params: p, key: k, sinks: map[sink]struct{}{}}
		h.entries[k] = e
	}
	if e.expire != nil {
		e.expire.Stop()
		e.expire = nil
	}
	e.sinks[s] = struct{}{}
	if e.last != nil {
		s.s.deliver(s.i, e.last)
	}
	if e.stop == nil {
		e.stop = make(chan struct{})
		h.metrics.keys.Inc()
		go h.loop(e, e.stop)
	}
}

// leave unsubscribes s from q's key under p; with the last subscriber the
// loop stops at once, and the entry, with its last result and start time,
// is kept for one interval, so a subscriber that comes back within it gets
// that result and no evaluation runs early.
func (h *Hub) leave(q *query, p Params, s sink) {
	k := q.key(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[k]
	if e == nil {
		return
	}
	delete(e.sinks, s)
	if len(e.sinks) > 0 || e.stop == nil {
		return
	}
	close(e.stop)
	e.stop = nil
	h.metrics.keys.Dec()
	e.expire = time.AfterFunc(h.cfg.Interval, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.entries[k] == e && len(e.sinks) == 0 && e.stop == nil {
			delete(h.entries, k)
		}
	})
}

// loop evaluates e at once, or one interval after its last evaluation
// started, then once per interval, until stop closes. A tick that finds
// an evaluation still running, or that passed during one, is skipped.
func (h *Hub) loop(e *entry, stop <-chan struct{}) {
	h.mu.Lock()
	next := time.Now()
	if !e.lastStart.IsZero() && e.lastStart.Add(h.cfg.Interval).After(next) {
		next = e.lastStart.Add(h.cfg.Interval)
	}
	h.mu.Unlock()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		timer.Reset(time.Until(next))
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		h.mu.Lock()
		if e.busy { // a stopped loop's evaluation, still running
			h.mu.Unlock()
			h.metrics.evaluations.WithLabelValues(e.q.name, "skipped").Inc()
			next = next.Add(h.cfg.Interval)
			continue
		}
		start := time.Now()
		e.busy, e.lastStart = true, start
		h.mu.Unlock()

		ev, err := h.evaluate(e, start)

		h.mu.Lock()
		e.busy = false
		if ev != nil {
			e.last = ev
			for s := range e.sinks {
				s.s.deliver(s.i, ev)
			}
			// Logged when a key starts and stops failing, not every interval.
			if err != nil && !e.failing {
				h.cfg.Log.Warn("live query failing", "query", e.q.name, "err", err)
			} else if err == nil && e.failing {
				h.cfg.Log.Info("live query recovered", "query", e.q.name)
			}
			e.failing = err != nil
		}
		h.mu.Unlock()

		for next = start.Add(h.cfg.Interval); !next.After(time.Now()); next = next.Add(h.cfg.Interval) {
			h.metrics.evaluations.WithLabelValues(e.q.name, "skipped").Inc()
		}
	}
}

// evaluate runs e's query once, within the timeout and a concurrency
// slot, and returns its result event, or its error event and the error;
// no event when it could not get a slot in time (counted as skipped).
func (h *Hub) evaluate(e *entry, at time.Time) (*event, error) {
	ctx, cancel := context.WithTimeout(h.ctx, h.cfg.Timeout)
	defer cancel()
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		h.metrics.evaluations.WithLabelValues(e.q.name, "skipped").Inc()
		return nil, nil
	}
	if e.q.source == fromPrometheus && h.cfg.Prometheus == nil {
		h.metrics.evaluations.WithLabelValues(e.q.name, "error").Inc()
		return newEvent("error", errorData{Sub: e.q.name, At: at.UTC(), Code: "prometheus_not_configured",
			Message: "Prometheus is not configured: the control plane has no SPILLWAY_CP_PROMETHEUS_URL"}), errNotConfigured
	}
	t0 := time.Now()
	data, err := e.q.eval(ctx, h, e.params, at)
	h.metrics.duration.WithLabelValues(e.q.source.String()).Observe(time.Since(t0).Seconds())
	if err == nil {
		h.metrics.evaluations.WithLabelValues(e.q.name, "ok").Inc()
		return newEvent("result", resultData{Sub: e.q.name, At: at.UTC(), Data: data}), nil
	}
	h.metrics.evaluations.WithLabelValues(e.q.name, "error").Inc()
	code, msg := h.classify(e.q, err)
	return newEvent("error", errorData{Sub: e.q.name, At: at.UTC(), Code: code, Message: msg}), err
}

var errNotConfigured = errors.New("SPILLWAY_CP_PROMETHEUS_URL is not set")

// classify maps an evaluation error to the error event's code and a
// fixed message: no error text reaches the browser.
func (h *Hub) classify(q *query, err error) (string, string) {
	if q.source == fromPrometheus {
		if errors.Is(err, ErrPrometheusUnavailable) {
			return "prometheus_unavailable", "Prometheus is unavailable: retrying every interval"
		}
		return "query_failed", "Prometheus could not answer the query"
	}
	if errors.Is(err, context.DeadlineExceeded) || h.cfg.Postgres.Unavailable(err) {
		return "unavailable", "the database is unavailable: retrying every interval"
	}
	return "query_failed", "the query failed"
}

func (h *Hub) rangeQuery(ctx context.Context, r timeRange, end time.Time, promql string) (matrix, error) {
	return h.cfg.Prometheus.rangeQuery(ctx, r, end, promql)
}

func (h *Hub) instantQuery(ctx context.Context, at time.Time, promql string) (vector, error) {
	return h.cfg.Prometheus.instantQuery(ctx, at, promql)
}

// keys returns the keys with an entry, sorted, for tests.
func (h *Hub) keys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Sorted(maps.Keys(h.entries))
}
