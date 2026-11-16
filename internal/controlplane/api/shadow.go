package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/live"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// The shadow report (UI-01, D8, ADR-0060): what a published policy did, or
// would have done, over a window ending now.

// shadowWindows are the report's windows (D8).
var shadowWindows = map[apiv1.GetShadowReportParamsWindow]time.Duration{
	apiv1.GetShadowReportParamsWindowN15m: 15 * time.Minute,
	apiv1.GetShadowReportParamsWindowN1h:  time.Hour,
	apiv1.GetShadowReportParamsWindowN24h: 24 * time.Hour,
	apiv1.GetShadowReportParamsWindowN7d:  7 * 24 * time.Hour,
}

const (
	// shadowKeys is how many keys the report lists.
	shadowKeys = 50
	// shadowQueryTimeout bounds the report's Prometheus queries, as the
	// stream bounds its evaluations (ADR-0059).
	shadowQueryTimeout = 4 * time.Second
	// shadowConcurrency is how many reports query Prometheus at once,
	// whoever asks: a flood of reports waits for a slot, within
	// shadowQueryTimeout, and leaves Prometheus to the live screens
	// (ADR-0059).
	shadowConcurrency = 4
)

// WithPrometheus makes s answer the shadow report's counts from prom and
// returns s. Without it, or with nil, the counts are null and the report
// says Prometheus is not configured.
func (s *Server) WithPrometheus(prom *live.Prometheus) *Server {
	s.prometheus = prom
	s.shadowSlots = make(chan struct{}, shadowConcurrency)
	return s
}

// GetShadowReport answers the report of a policy of the latest published
// config version: its counts by mode from Prometheus under its published
// name, and its top keys from the heavy-hitter windows under its ID.
func (s *Server) GetShadowReport(ctx context.Context, req apiv1.GetShadowReportRequestObject) (apiv1.GetShadowReportResponseObject, error) {
	window := apiv1.GetShadowReportParamsWindowN1h
	if req.Params.Window != nil {
		window = *req.Params.Window
	}
	d, ok := shadowWindows[window]
	if !ok {
		return nil, invalid("window: want 15m, 1h, 24h or 7d")
	}
	latest, err := s.store.LatestConfig(ctx)
	if err != nil {
		return nil, err
	}
	var pol *configpb.Policy
	for _, p := range latest.GetConfig().GetPolicies() {
		if p.GetId() == req.Id {
			pol = p
		}
	}
	if pol == nil {
		return nil, &apiError{http.StatusNotFound, apiv1.ErrorCodeNotFound, fmt.Sprintf("policy %d is not in the latest published config: publish it first", req.Id), nil}
	}
	until := time.Now().UTC()
	since := until.Add(-d)
	keys, err := s.store.ShadowKeys(ctx, req.Id, since, shadowKeys)
	if err != nil {
		return nil, err
	}
	mode := pol.GetMode()
	if mode == "" {
		mode = "enforce"
	}
	out := apiv1.ShadowReport{
		PolicyId: req.Id, PolicyName: pol.GetName(), Mode: mode, ConfigVersion: latest.GetConfig().GetVersion(),
		Window: apiv1.ShadowReportWindow(window), Since: since, Until: until,
		Keys: make([]apiv1.ShadowReportKey, len(keys.Keys)), KeysShadowDeniedTotal: keys.ShadowDenied,
	}
	for i, k := range keys.Keys {
		out.Keys[i] = apiv1.ShadowReportKey{
			KeyDigest: k.KeyDigest, ConsumerId: nonZero(k.ConsumerID), ApiKeyId: nonZero(k.ApiKeyID), ShadowDenied: k.ShadowDenied, Denied: k.Denied,
		}
	}
	if keys.Oldest != nil {
		t := later(since, keys.Oldest.UTC())
		out.KeysSince = &t
	}
	if e := s.shadowCounts(ctx, &out, pol.GetName(), d); e != nil {
		out.MetricsError = e
	}
	return apiv1.GetShadowReport200JSONResponse(out), nil
}

// shadowCounts fills r's Prometheus fields, or leaves them null and says
// why. A failure is logged, never answered as an error: the keys still
// answer (ADR-0060).
func (s *Server) shadowCounts(ctx context.Context, r *apiv1.ShadowReport, policy string, window time.Duration) *apiv1.ShadowReportMetricsError {
	if s.prometheus == nil {
		e := apiv1.PrometheusNotConfigured
		return &e
	}
	ctx, cancel := context.WithTimeout(ctx, shadowQueryTimeout)
	defer cancel()
	var d live.Decisions
	var oldest time.Time
	var ok bool
	var err error
	select {
	case s.shadowSlots <- struct{}{}:
		d, err = s.prometheus.Decisions(ctx, policy, window, r.Until)
		if err == nil {
			oldest, ok, err = s.prometheus.OldestSample(ctx, r.Until)
		}
		<-s.shadowSlots
	case <-ctx.Done():
		err = fmt.Errorf("waiting for one of %d report slots: %w", shadowConcurrency, ctx.Err())
	}
	if err != nil {
		e := apiv1.QueryFailed
		if errors.Is(err, live.ErrPrometheusUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			e = apiv1.PrometheusUnavailable
		}
		s.log.Warn("shadow report: Prometheus query failed", "request_id", requestIDOf(ctx), "err", err)
		return &e
	}
	r.Evaluated, r.WouldDeny, r.Share = &d.Shadow, &d.ShadowDenied, ratio(d.ShadowDenied, d.Shadow)
	r.EnforceEvaluated, r.Denied, r.EnforceShare = &d.Enforce, &d.Denied, ratio(d.Denied, d.Enforce)
	if ok {
		t := later(r.Since, oldest.UTC())
		r.MetricsSince = &t
	}
	return nil
}

// ratio is n / of, or nil when of is 0.
func ratio(n, of float64) *float64 {
	if of <= 0 {
		return nil
	}
	r := n / of
	return &r
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
