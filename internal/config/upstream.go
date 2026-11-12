package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Load balancing algorithms (GW-07, ADR-0030).
const (
	LBRoundRobin     = "round_robin"
	LBP2C            = "p2c" // least request with the power of two choices; the default
	LBConsistentHash = "consistent_hash"
)

// compileLB returns an upstream's algorithm, p2c when unset, and for
// consistent_hash its compiled hash_on key template.
func compileLB(lb, hashOn string) (string, *KeyTemplate, error) {
	switch lb {
	case "":
		lb = LBP2C
	case LBRoundRobin, LBP2C, LBConsistentHash:
	default:
		return "", nil, fmt.Errorf("lb %q: want round_robin, p2c or consistent_hash", lb)
	}
	switch {
	case lb == LBConsistentHash && hashOn == "":
		return "", nil, errors.New("hash_on is required with lb consistent_hash")
	case lb != LBConsistentHash && hashOn != "":
		return "", nil, errors.New("hash_on is only for lb consistent_hash")
	case hashOn == "":
		return lb, nil, nil
	}
	t, err := parseKeyTemplate(hashOn)
	if err != nil {
		return "", nil, fmt.Errorf("hash_on: %w", err)
	}
	return lb, &t, nil
}

// HealthCheck is an upstream's compiled active health check (GW-08,
// ADR-0030).
type HealthCheck struct {
	Path      string        // with its query, if any
	Interval  time.Duration // between the starts of two probes
	Timeout   time.Duration // one probe; at most Interval
	Unhealthy int           // consecutive failures that make an endpoint unhealthy
	Healthy   int           // consecutive successes that make it healthy again
}

// OutlierDetection is an upstream's compiled outlier ejection (GW-09,
// ADR-0030).
type OutlierDetection struct {
	ConsecutiveFailures int
	BaseEjection        time.Duration
	MaxEjection         time.Duration
	MaxEjectedPercent   int
}

// compileHealthCheck returns nil for an unset block: no active checks.
func compileHealthCheck(h *configpb.HealthCheck) (*HealthCheck, error) {
	if h == nil {
		return nil, nil
	}
	out := &HealthCheck{Path: h.GetPath(), Interval: 5 * time.Second, Unhealthy: 3, Healthy: 2}
	if !validProbePath(out.Path) {
		return nil, fmt.Errorf("path %q: want a path such as /healthz", out.Path)
	}
	if v := h.GetInterval(); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 100*time.Millisecond || d > time.Hour {
			return nil, fmt.Errorf("interval: %q is not a duration from 100ms to 1h", v)
		}
		out.Interval = d
	}
	out.Timeout = min(time.Second, out.Interval)
	if v := h.GetTimeout(); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > out.Interval {
			return nil, errors.New("timeout: want more than 0 and at most interval")
		}
		out.Timeout = d
	}
	for _, f := range []struct {
		name string
		v    int32
		to   *int
	}{{"unhealthy", h.GetUnhealthy(), &out.Unhealthy}, {"healthy", h.GetHealthy(), &out.Healthy}} {
		if f.v < 0 || f.v > 100 {
			return nil, fmt.Errorf("%s: want 1 to 100", f.name)
		}
		if f.v > 0 {
			*f.to = int(f.v)
		}
	}
	return out, nil
}

// validProbePath reports whether p is an origin-form request target: a path
// starting with "/", with an optional query, and no space, "#" or control
// character.
func validProbePath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.ContainsFunc(p, func(c rune) bool { return c <= ' ' || c == 0x7f || c == '#' }) {
		return false
	}
	_, err := url.ParseRequestURI(p)
	return err == nil
}

// compileOutlier fills in the defaults: every upstream has outlier ejection
// (ADR-0030).
func compileOutlier(o *configpb.OutlierDetection) (OutlierDetection, error) {
	out := OutlierDetection{ConsecutiveFailures: 5, BaseEjection: 30 * time.Second, MaxEjectedPercent: 50}
	if n := o.GetConsecutiveFailures(); n < 0 || n > 1000 {
		return out, errors.New("consecutive_failures: want 1 to 1000")
	} else if n > 0 {
		out.ConsecutiveFailures = int(n)
	}
	if v := o.GetBaseEjection(); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			return out, fmt.Errorf("base_ejection: %q is not a duration from 1s to 1h", v)
		}
		out.BaseEjection = d
	}
	out.MaxEjection = max(300*time.Second, out.BaseEjection)
	if v := o.GetMaxEjection(); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < out.BaseEjection || d > 24*time.Hour {
			return out, errors.New("max_ejection: want at least base_ejection and at most 24h")
		}
		out.MaxEjection = d
	}
	if n := o.GetMaxEjectedPercent(); n < 0 || n > 100 {
		return out, errors.New("max_ejected_percent: want 1 to 100")
	} else if n > 0 {
		out.MaxEjectedPercent = int(n)
	}
	return out, nil
}

// RetryBudget is an upstream's compiled retry budget (GW-13, ADR-0030): a
// retry is granted while the retries granted over the last 10 s, it
// included, are at most Ratio × the first attempts over that time plus
// MinPerSec × 10 s.
type RetryBudget struct {
	Ratio     float64
	MinPerSec float64
}

// compileRetryBudget fills in the defaults: every upstream has a retry
// budget (ADR-0030).
func compileRetryBudget(b *configpb.RetryBudget) (RetryBudget, error) {
	out := RetryBudget{Ratio: 0.2, MinPerSec: 3}
	if v := b.GetRatio(); v != 0 {
		if !(v > 0 && v <= 1) { // NaN too
			return out, errors.New("ratio: want more than 0 and at most 1")
		}
		out.Ratio = v
	}
	if v := b.GetMinPerSec(); v != 0 {
		if !(v > 0 && v <= 10000) {
			return out, errors.New("min_per_sec: want more than 0 and at most 10000")
		}
		out.MinPerSec = v
	}
	return out, nil
}
