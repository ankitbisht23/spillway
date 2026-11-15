package live

import "github.com/prometheus/client_golang/prometheus"

// metrics are the stream's metrics on the admin port (ADR-0059). No
// subject, route or tenant is a label; query is a catalogue name.
type metrics struct {
	connections prometheus.Gauge
	keys        prometheus.Gauge
	evaluations *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	coalesced   prometheus.Counter
	closed      *prometheus.CounterVec
	rejected    *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		connections: prometheus.NewGauge(prometheus.GaugeOpts{Name: "cp_stream_connections", Help: "Open live-data streams (GET /api/v1/stream)."}),
		keys:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "cp_stream_keys", Help: "Named-query keys being evaluated: distinct queries and parameters with a subscriber."}),
		evaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cp_stream_evaluations_total",
			Help: "Evaluations of named queries, by query and outcome: ok, error, or skipped (an evaluation still running or no slot in time).",
		}, []string{"query", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cp_stream_evaluation_duration_seconds",
			Help:    "Time to evaluate a named query, by source: prometheus or postgres.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4},
		}, []string{"source"}),
		coalesced: prometheus.NewCounter(prometheus.CounterOpts{Name: "cp_stream_coalesced_total", Help: "Results that replaced a result a slow stream had not yet taken."}),
		closed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cp_stream_closed_total",
			Help: "Closed streams by reason: client, slow (an event not taken within the write timeout), token_expired, shutdown.",
		}, []string{"reason"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cp_stream_rejected_total",
			Help: "Stream requests refused before streaming, by reason: invalid (400) or limit (503).",
		}, []string{"reason"}),
	}
	for _, name := range Names() {
		for _, outcome := range []string{"ok", "error", "skipped"} {
			m.evaluations.WithLabelValues(name, outcome)
		}
	}
	for _, s := range []string{"prometheus", "postgres"} {
		m.duration.WithLabelValues(s)
	}
	for _, r := range []string{"client", "slow", "token_expired", "shutdown"} {
		m.closed.WithLabelValues(r)
	}
	for _, r := range []string{"invalid", "limit"} {
		m.rejected.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.connections, m.keys, m.evaluations, m.duration, m.coalesced, m.closed, m.rejected)
	}
	return m
}
