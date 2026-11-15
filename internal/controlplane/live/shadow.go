package live

import (
	"context"
	"fmt"
	"time"
)

// The shadow report's Prometheus side (UI-01, ADR-0060): a policy's
// decisions over a window, and how far back Prometheus's data goes.

// Decisions are a policy's rate-limit decisions over a window by mode, from
// gw_ratelimit_decisions_total: increase() estimates, not exact counts.
type Decisions struct {
	// Shadow and ShadowDenied: every decision in shadow mode, and its
	// would-denies.
	Shadow, ShadowDenied float64
	// Enforce and Denied: the same in enforce mode.
	Enforce, Denied float64
}

// Decisions evaluates, at at, the decisions of the policy named policy
// (the metric's label) over the window before at.
func (p *Prometheus) Decisions(ctx context.Context, policy string, window time.Duration, at time.Time) (Decisions, error) {
	q := fmt.Sprintf(`sum by (mode, decision) (increase(gw_ratelimit_decisions_total{policy=%s}[%ds]))`, promString(policy), int64(window/time.Second))
	v, err := p.instantQuery(ctx, at, q)
	if err != nil {
		return Decisions{}, err
	}
	var d Decisions
	for _, s := range v.Series {
		if s.Value == nil {
			continue
		}
		n := *s.Value
		switch s.Labels["mode"] {
		case "shadow":
			d.Shadow += n
			if s.Labels["decision"] == "deny" {
				d.ShadowDenied += n
			}
		case "enforce":
			d.Enforce += n
			if s.Labels["decision"] == "deny" {
				d.Denied += n
			}
		}
	}
	return d, nil
}

// OldestSample is the time of the oldest sample Prometheus keeps, from its
// own prometheus_tsdb_lowest_timestamp_seconds (compose scrapes it), or
// false when that series has no value at at.
func (p *Prometheus) OldestSample(ctx context.Context, at time.Time) (time.Time, bool, error) {
	v, err := p.instantQuery(ctx, at, `min(prometheus_tsdb_lowest_timestamp_seconds)`)
	if err != nil || len(v.Series) == 0 || v.Series[0].Value == nil {
		return time.Time{}, false, err
	}
	return time.UnixMilli(int64(*v.Series[0].Value * 1000)), true, nil
}
