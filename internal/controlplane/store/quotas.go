package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Quota overrides (RL-10, ADR-0070, ADR-0071): overrides rows with no
// policy, their own resource. Every change is an identity change.

// ListQuotaOverrides returns a page of quota overrides by ID, those of
// consumerID if it is not nil.
func (s *Store) ListQuotaOverrides(ctx context.Context, p Page, consumerID *int64) ([]db.Override, error) {
	return db.New(s.pool).ListQuotaOverrides(ctx, db.ListQuotaOverridesParams{After: p.After, ConsumerID: consumerID, Lim: p.lim()})
}

// GetQuotaOverride returns one quota override.
func (s *Store) GetQuotaOverride(ctx context.Context, id int64) (db.Override, error) {
	o, err := db.New(s.pool).GetQuotaOverride(ctx, id)
	return o, notFound(err)
}

// CreateQuotaOverride adds a quota override or burst grant (CP-20, RL-10).
func (s *Store) CreateQuotaOverride(ctx context.Context, a Audit, in db.CreateQuotaOverrideParams) (db.Override, error) {
	var out db.Override
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		in.Version = v
		o, err := q.CreateQuotaOverride(ctx, in)
		if err != nil {
			return writeError(err)
		}
		out = o
		return a.write(ctx, q, change{action: "quota_override.create", entity: "quota_override", id: o.ID, after: o, note: o.Note, identity: v})
	})
	return out, err
}

// UpdateQuotaOverride replaces a quota override if its version is ifMatch
// (CP-14, CP-20).
func (s *Store) UpdateQuotaOverride(ctx context.Context, a Audit, id, ifMatch int64, in db.CreateQuotaOverrideParams) (db.Override, error) {
	var out db.Override
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetQuotaOverride(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		o, err := q.UpdateQuotaOverride(ctx, db.UpdateQuotaOverrideParams{
			ID: id, ConsumerID: in.ConsumerID, Quotas: in.Quotas, StartsAt: in.StartsAt, EndsAt: in.EndsAt, Note: in.Note, Version: v,
		})
		if err != nil {
			return writeError(err)
		}
		out = o
		return a.write(ctx, q, change{action: "quota_override.update", entity: "quota_override", id: id, before: cur, after: o, note: o.Note, identity: v})
	})
	return out, err
}

// DeleteQuotaOverride removes a quota override if its version is ifMatch
// (CP-14, CP-20).
func (s *Store) DeleteQuotaOverride(ctx context.Context, a Audit, id, ifMatch int64) error {
	return s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetQuotaOverride(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if _, err = q.DeleteQuotaOverride(ctx, id); err != nil {
			return err
		}
		return a.write(ctx, q, change{action: "quota_override.delete", entity: "quota_override", id: id, before: cur, identity: v})
	})
}

// PlanQuotas returns the quotas of the plans of the latest published config
// version, by plan ID, and that version (0 and none before the first
// publish): the quotas gateways enforce (ADR-0070).
func (s *Store) PlanQuotas(ctx context.Context) (int64, map[int64]config.Quotas, error) {
	u, err := s.LatestConfig(ctx)
	if err != nil || u == nil {
		return 0, nil, err
	}
	out := map[int64]config.Quotas{}
	for _, p := range u.GetConfig().GetPlans() {
		if q := config.QuotasOf(p.GetQuotas()); q.Any() {
			out[p.GetId()] = q
		}
	}
	return u.GetConfig().GetVersion(), out, nil
}

// QuotaUsage is one consumer's flushed usage of its current UTC day and
// month and its effective quotas (ADR-0070).
type QuotaUsage struct {
	ConsumerID     int64
	Quotas         config.Quotas
	Daily, Monthly db.QuotaUsage // Used 0 and FlushedAt zero without a row
}

// QuotaUsages returns the usage of the consumers in ids that exist, in
// that order, for the UTC day and month day and month name, with their
// effective quotas at now: the quota override that applies, chosen as
// gateways choose (config.Identity.QuotaOverride), else the quotas of
// their plan in planQuotas.
func (s *Store) QuotaUsages(ctx context.Context, ids []int64, day, month string, planQuotas map[int64]config.Quotas, now time.Time) ([]QuotaUsage, error) {
	q := db.New(s.pool)
	plans, err := q.ConsumerPlans(ctx, ids)
	if err != nil {
		return nil, err
	}
	overrides, err := q.LiveQuotaOverrides(ctx, ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.QuotaUsage(ctx, db.QuotaUsageParams{ConsumerIds: ids, Periods: []string{day, month}})
	if err != nil {
		return nil, err
	}
	byID := map[int64]*QuotaUsage{}
	for _, p := range plans {
		u := &QuotaUsage{ConsumerID: p.ID, Quotas: planQuotas[p.PlanID]}
		u.Daily.Period, u.Monthly.Period = day, month
		byID[p.ID] = u
	}
	for _, r := range rows {
		if u := byID[r.ConsumerID]; u != nil {
			if r.Period == day {
				u.Daily = r
			} else {
				u.Monthly = r
			}
		}
	}
	if err := applyQuotaOverrides(byID, overrides, now); err != nil {
		return nil, err
	}
	out := make([]QuotaUsage, 0, len(byID))
	seen := map[int64]bool{}
	for _, id := range ids {
		if u := byID[id]; u != nil && !seen[id] {
			out, seen[id] = append(out, *u), true
		}
	}
	return out, nil
}

// applyQuotaOverrides sets each consumer's quotas from its quota override
// that applies at now: a burst grant beats a standing override, then the
// latest start (none counting as earliest), then the highest ID (ADR-0041).
func applyQuotaOverrides(byID map[int64]*QuotaUsage, os []db.Override, now time.Time) error {
	active := slices.DeleteFunc(slices.Clone(os), func(o db.Override) bool {
		return o.StartsAt != nil && now.Before(*o.StartsAt) || o.EndsAt != nil && !now.Before(*o.EndsAt)
	})
	slices.SortFunc(active, func(a, b db.Override) int {
		if ag, bg := a.EndsAt != nil, b.EndsAt != nil; ag != bg {
			if ag {
				return -1
			}
			return 1
		}
		var as, bs time.Time
		if a.StartsAt != nil {
			as = *a.StartsAt
		}
		if b.StartsAt != nil {
			bs = *b.StartsAt
		}
		if c := bs.Compare(as); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	done := map[int64]bool{}
	for _, o := range active {
		u := byID[o.ConsumerID]
		if u == nil || done[o.ConsumerID] {
			continue
		}
		q := &configpb.Quotas{}
		if err := protojson.Unmarshal(o.Quotas, q); err != nil {
			return errors.Join(errors.New("stored quotas of a quota override"), err)
		}
		u.Quotas, done[o.ConsumerID] = config.QuotasOf(q), true
	}
	return nil
}

// QuotaCount is one counter the flusher read: a consumer's count of one
// period (ADR-0070).
type QuotaCount struct {
	ConsumerID int64
	Period     string
	Used       int64
}

// QuotaTx is the flusher's transaction, which holds its advisory lock.
type QuotaTx struct{ q *db.Queries }

// QuotaFlush runs fn in a transaction that first takes the flusher's
// transaction advisory lock (ADR-0070); if another transaction holds it,
// fn does not run and ran is false. An error from fn rolls back.
func (s *Store) QuotaFlush(ctx context.Context, fn func(*QuotaTx) error) (ran bool, err error) {
	err = s.inTx(ctx, func(q *db.Queries) error {
		locked, err := q.TryQuotaFlushLock(ctx)
		if err != nil || !locked {
			return err
		}
		ran = true
		return fn(&QuotaTx{q: q})
	})
	return ran, err
}

// QuotaConsumer is a consumer that may have quota counters, and its tenant.
type QuotaConsumer = db.QuotaConsumersRow

// Consumers returns every consumer that may have quota counters: on a plan
// in planIDs, or with a quota override that has not ended; by tenant.
func (t *QuotaTx) Consumers(ctx context.Context, planIDs []int64) ([]db.QuotaConsumersRow, error) {
	if planIDs == nil {
		planIDs = []int64{} // an empty array, not NULL
	}
	return t.q.QuotaConsumers(ctx, planIDs)
}

// Write upserts counts as of at: used = GREATEST(used, count), so a
// repeated or late flush never double-counts or lowers it (RL-10).
func (t *QuotaTx) Write(ctx context.Context, counts []QuotaCount, at time.Time) error {
	if len(counts) == 0 {
		return nil
	}
	p := db.UpsertQuotaUsageParams{FlushedAt: at}
	for _, c := range counts {
		p.ConsumerIds = append(p.ConsumerIds, c.ConsumerID)
		p.Periods = append(p.Periods, c.Period)
		p.Used = append(p.Used, c.Used)
	}
	return t.q.UpsertQuotaUsage(ctx, p)
}

// Usage returns the flushed counts of consumers for periods.
func (t *QuotaTx) Usage(ctx context.Context, consumers []int64, periods []string) ([]db.QuotaUsage, error) {
	return t.q.QuotaUsage(ctx, db.QuotaUsageParams{ConsumerIds: consumers, Periods: periods})
}

// Prune deletes the daily rows of days before before (d<YYYYMMDD>).
func (t *QuotaTx) Prune(ctx context.Context, before string) (int64, error) {
	return t.q.PruneQuotaUsage(ctx, before)
}
