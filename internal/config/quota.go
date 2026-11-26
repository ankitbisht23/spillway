package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Quotas are a consumer's daily and monthly quotas, in route cost units per
// UTC calendar day and month (RL-10, ADR-0070); 0 is no quota for that
// period.
type Quotas struct {
	Daily, Monthly int64
}

// Any reports whether q has a quota for either period.
func (q Quotas) Any() bool { return q.Daily > 0 || q.Monthly > 0 }

// QuotasOf converts a Quotas message; nil is no quotas.
func QuotasOf(q *configpb.Quotas) Quotas {
	return Quotas{Daily: q.GetDaily(), Monthly: q.GetMonthly()}
}

// maxQuota bounds each quota (ADR-0070): far below 2^53, which the limiter
// script's numbers keep exact.
const maxQuota = 1_000_000_000_000_000

// quotaLimits is how many limits a consumer's quotas add to its tenant-slot
// call: one per period (ADR-0070).
const quotaLimits = 2

// CheckQuotas checks a plan's or a quota override's quotas: each 0 (none)
// to 10^15 (ADR-0070).
func CheckQuotas(q *configpb.Quotas) error {
	for _, v := range []struct {
		name string
		n    int64
	}{{"daily", q.GetDaily()}, {"monthly", q.GetMonthly()}} {
		if v.n < 0 || v.n > maxQuota {
			return fmt.Errorf("quotas.%s: %d is not 0 (none) to %d", v.name, v.n, int64(maxQuota))
		}
	}
	return nil
}

// errQuotasNeedManaged rejects quotas in static mode: their counters need
// tenant and consumer IDs, and the flusher runs in the control plane
// (ADR-0070).
var errQuotasNeedManaged = errors.New("quotas need managed mode (RL-10)")

// Quotas returns the quotas that apply to consumer c at t, the gateway's
// clock (RL-10, ADR-0070): its quota override that applies at t, else the
// quotas of its plan (its own, else its tenant's). Anonymous requests,
// consumers without a tenant and static mode have none.
func (s *Snapshot) Quotas(c *Consumer, id *Identity, t time.Time) Quotas {
	if c == nil || c.ID == 0 || c.Tenant == "" {
		return Quotas{}
	}
	if q, ok := id.QuotaOverride(c.ID, t); ok {
		return q
	}
	return s.PlanQuotas[c.Plan]
}

// QuotaOverrideConsumers is the number of consumers with a quota override
// that has a quota, active now or later (RL-10). A gateway without Redis
// cannot count them (ADR-0091).
func (id *Identity) QuotaOverrideConsumers() int {
	if id == nil {
		return 0
	}
	n := 0
	for k, os := range id.overrides {
		if k[1] != 0 {
			continue
		}
		for _, o := range os {
			if o.quotas.Any() {
				n++
				break
			}
		}
	}
	return n
}

// QuotaOverride returns the quotas of consumer's quota override that
// applies at t, if any (RL-10, ADR-0070), chosen as Override chooses: a
// burst grant beats a standing override, then the latest start, then the
// highest ID.
func (id *Identity) QuotaOverride(consumer int64, t time.Time) (Quotas, bool) {
	if id == nil {
		return Quotas{}, false
	}
	for _, o := range id.overrides[[2]int64{consumer, 0}] {
		if o.active(t) {
			return o.quotas, true
		}
	}
	return Quotas{}, false
}
