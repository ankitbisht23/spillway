package api

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/quota"
)

// Quota overrides and quota usage (RL-10, ADR-0070, ADR-0071). A quota
// override is an override with no policy; it needs no config check, so it
// is valid whatever the published config holds.

var showQuotaOverride = showAs(quotaOverrideOut)

func quotaOverrideIn(b *apiv1.QuotaOverride) (db.CreateQuotaOverrideParams, error) {
	in := db.CreateQuotaOverrideParams{ConsumerID: b.ConsumerId, StartsAt: b.StartsAt, EndsAt: b.EndsAt, Note: b.Note}
	if b.ConsumerId <= 0 {
		return in, validation("quota_override", "consumer_id", "consumer_id is required")
	}
	q := &configpb.Quotas{}
	if b.Quotas.Daily != nil {
		q.Daily = *b.Quotas.Daily
	}
	if b.Quotas.Monthly != nil {
		q.Monthly = *b.Quotas.Monthly
	}
	if err := config.CheckQuotas(q); err != nil {
		return in, validation("quota_override", "quotas", err.Error())
	}
	if b.StartsAt != nil && b.EndsAt != nil && !b.EndsAt.After(*b.StartsAt) {
		return in, validation("quota_override", "ends_at", "ends_at must be after starts_at")
	}
	var err error
	in.Quotas, err = protojson.Marshal(q)
	return in, err
}

func quotaOverrideOut(o db.Override) (apiv1.QuotaOverride, error) {
	out := apiv1.QuotaOverride{Id: &o.ID, ConsumerId: o.ConsumerID, Note: o.Note, Version: &o.Version}
	q := &configpb.Quotas{}
	if err := protojson.Unmarshal(o.Quotas, q); err != nil {
		return out, fmt.Errorf("stored quotas of quota override %d: %w", o.ID, err)
	}
	daily, monthly := q.GetDaily(), q.GetMonthly()
	out.Quotas = apiv1.Quotas{Daily: &daily, Monthly: &monthly}
	if o.StartsAt != nil {
		t := o.StartsAt.UTC()
		out.StartsAt = &t
	}
	if o.EndsAt != nil {
		t := o.EndsAt.UTC()
		out.EndsAt = &t
	}
	out.CreatedAt, out.UpdatedAt = timestamps(o.CreatedAt, o.UpdatedAt)
	return out, nil
}

func (s *Server) ListQuotaOverrides(ctx context.Context, req apiv1.ListQuotaOverridesRequestObject) (apiv1.ListQuotaOverridesResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListQuotaOverrides(ctx, p, req.Params.ConsumerId)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(o db.Override) int64 { return o.ID })
	items := make([]apiv1.QuotaOverride, len(rows))
	for i, o := range rows {
		if items[i], err = quotaOverrideOut(o); err != nil {
			return nil, err
		}
	}
	return apiv1.ListQuotaOverrides200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetQuotaOverride(ctx context.Context, req apiv1.GetQuotaOverrideRequestObject) (apiv1.GetQuotaOverrideResponseObject, error) {
	o, err := s.store.GetQuotaOverride(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out, err := quotaOverrideOut(o)
	if err != nil {
		return nil, err
	}
	return apiv1.GetQuotaOverride200JSONResponse{Body: out, Headers: apiv1.GetQuotaOverride200ResponseHeaders{ETag: etag(o.Version)}}, nil
}

func (s *Server) CreateQuotaOverride(ctx context.Context, req apiv1.CreateQuotaOverrideRequestObject) (apiv1.CreateQuotaOverrideResponseObject, error) {
	in, err := quotaOverrideIn(req.Body)
	if err != nil {
		return nil, err
	}
	o, err := s.store.CreateQuotaOverride(ctx, audit(ctx, showQuotaOverride), in)
	if err != nil {
		return nil, err
	}
	out, err := quotaOverrideOut(o)
	if err != nil {
		return nil, err
	}
	return apiv1.CreateQuotaOverride201JSONResponse{Body: out, Headers: apiv1.CreateQuotaOverride201ResponseHeaders{ETag: etag(o.Version), Location: location("quota-overrides", o.ID)}}, nil
}

func (s *Server) UpdateQuotaOverride(ctx context.Context, req apiv1.UpdateQuotaOverrideRequestObject) (apiv1.UpdateQuotaOverrideResponseObject, error) {
	in, err := quotaOverrideIn(req.Body)
	if err != nil {
		return nil, err
	}
	o, err := s.store.UpdateQuotaOverride(ctx, audit(ctx, showQuotaOverride), req.Id, ifMatch(req.Params.IfMatch), in)
	if err != nil {
		return nil, err
	}
	out, err := quotaOverrideOut(o)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateQuotaOverride200JSONResponse{Body: out, Headers: apiv1.UpdateQuotaOverride200ResponseHeaders{ETag: etag(o.Version)}}, nil
}

func (s *Server) DeleteQuotaOverride(ctx context.Context, req apiv1.DeleteQuotaOverrideRequestObject) (apiv1.DeleteQuotaOverrideResponseObject, error) {
	if err := s.store.DeleteQuotaOverride(ctx, audit(ctx, showQuotaOverride), req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeleteQuotaOverride204Response{}, nil
}

// maxQuotaUsageIDs is the most consumers one quota-usage request names: a
// page of the consumer list (ADR-0060, ADR-0070).
const maxQuotaUsageIDs = 50

// GetQuotaUsage answers the flushed usage of the consumers' current UTC day
// and month, on the control plane's clock, with their effective quotas
// (ADR-0070).
func (s *Server) GetQuotaUsage(ctx context.Context, req apiv1.GetQuotaUsageRequestObject) (apiv1.GetQuotaUsageResponseObject, error) {
	ids := req.Params.ConsumerId
	if len(ids) < 1 || len(ids) > maxQuotaUsageIDs {
		return nil, invalid(fmt.Sprintf("consumer_id: give 1 to %d", maxQuotaUsageIDs))
	}
	_, plans, err := s.store.PlanQuotas(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	day, month := quota.Day(now), quota.Month(now)
	rows, err := s.store.QuotaUsages(ctx, ids, day.ID, month.ID, plans, now)
	if err != nil {
		return nil, err
	}
	items := make([]apiv1.QuotaUsage, len(rows))
	for i, r := range rows {
		items[i] = apiv1.QuotaUsage{ConsumerId: r.ConsumerID, Daily: usagePeriod(r.Daily, r.Quotas.Daily), Monthly: usagePeriod(r.Monthly, r.Quotas.Monthly)}
	}
	return apiv1.GetQuotaUsage200JSONResponse{Items: items}, nil
}

func usagePeriod(u db.QuotaUsage, q int64) apiv1.QuotaUsagePeriod {
	out := apiv1.QuotaUsagePeriod{Period: u.Period, Used: u.Used, Quota: q}
	if !u.FlushedAt.IsZero() {
		t := u.FlushedAt.UTC()
		out.FlushedAt = &t
	}
	return out
}
