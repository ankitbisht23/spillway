package api

import (
	"context"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// Heavy hitters, from heartbeats' top_keys (RL-21, ADR-0041).

// The time range of GET /heavy-hitters (ADR-0041): 15 minutes by default,
// at most 7 days, the windows' retention.
const (
	defaultHeavyHitterRange = 15 * time.Minute
	maxHeavyHitterRange     = 7 * 24 * time.Hour
)

// ListHeavyHitters answers the top throttled keys of the windows that start
// in [since, until), most throttled first, at most limit of them and no
// next page.
func (s *Server) ListHeavyHitters(ctx context.Context, req apiv1.ListHeavyHittersRequestObject) (apiv1.ListHeavyHittersResponseObject, error) {
	p, err := page(req.Params.Limit, nil)
	if err != nil {
		return nil, err
	}
	f := store.HeavyHitterFilter{Since: req.Params.Since, Until: req.Params.Until, PolicyID: req.Params.PolicyId}
	if f.Since == nil && f.Until != nil {
		since := f.Until.Add(-defaultHeavyHitterRange)
		f.Since = &since
	}
	if f.Since != nil {
		until := time.Now()
		if f.Until != nil {
			until = *f.Until
		}
		if f.Until != nil && !f.Until.After(*f.Since) {
			return nil, invalid("until must be after since")
		}
		if until.Sub(*f.Since) > maxHeavyHitterRange {
			return nil, invalid("the range from since to until (or now) is at most 7 days")
		}
	}
	rows, err := s.store.TopHeavyHitters(ctx, f, p.Limit)
	if err != nil {
		return nil, err
	}
	items := make([]apiv1.HeavyHitter, len(rows))
	for i, r := range rows {
		items[i] = apiv1.HeavyHitter{
			PolicyId: r.PolicyID, KeyDigest: r.KeyDigest, ConsumerId: nonZero(r.ConsumerID), ApiKeyId: nonZero(r.ApiKeyID),
			Denied: r.Denied, ShadowDenied: r.ShadowDenied,
		}
	}
	return apiv1.ListHeavyHitters200JSONResponse{Items: items}, nil
}

// nonZero is id, or nil for 0, which means none.
func nonZero(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}
