package store

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Heavy-hitter windows (RL-21, CP-22, ADR-0041): heartbeats' top_keys,
// summed per minute, read as the top throttled keys of a time range.

// HeavyHitter is a (policy, key digest)'s throttled requests summed over a
// time range, with the latest consumer and API key IDs reported, 0 for none.
type HeavyHitter = db.TopHeavyHittersRow

// HeavyHitterFilter selects GET /heavy-hitters' windows: those starting in
// [Since, Until), Since by default 15 minutes ago and Until none; and with
// PolicyID, only that policy's.
type HeavyHitterFilter struct {
	Since, Until *time.Time
	PolicyID     *int64
}

// TopHeavyHitters returns at most limit (policy, key digest)s of the
// windows f selects, most throttled first.
func (s *Store) TopHeavyHitters(ctx context.Context, f HeavyHitterFilter, limit int32) ([]HeavyHitter, error) {
	q := db.New(s.pool)
	if f.PolicyID == nil {
		return q.TopHeavyHitters(ctx, db.TopHeavyHittersParams{Since: f.Since, Until: f.Until, Lim: limit})
	}
	rows, err := q.TopHeavyHittersByPolicy(ctx, db.TopHeavyHittersByPolicyParams{PolicyID: *f.PolicyID, Since: f.Since, Until: f.Until, Lim: limit})
	if err != nil {
		return nil, err
	}
	out := make([]HeavyHitter, len(rows))
	for i, r := range rows {
		out[i] = HeavyHitter(r)
	}
	return out, nil
}

// heavyHitters are the rows a heartbeat's top_keys add (ADR-0041): one per
// (policy, key digest), duplicates summed with the later IDs kept. A key
// that is not one a gateway sends (a policy ID below 1, a digest that is
// not 22 base64url characters, negative or no counts) is dropped, so one
// bad entry cannot fail the heartbeat.
func heavyHitters(keys []*configpb.TopKey) db.AddHeavyHittersParams {
	type id struct {
		policy int64
		digest string
	}
	var p db.AddHeavyHittersParams
	at := map[id]int{}
	for _, k := range keys {
		if k.GetPolicyId() < 1 || !digest(k.GetKeyDigest()) || k.GetDenied() < 0 || k.GetShadowDenied() < 0 || k.GetDenied()+k.GetShadowDenied() < 1 ||
			k.GetConsumerId() < 0 || k.GetApiKeyId() < 0 {
			continue
		}
		i, ok := at[id{k.GetPolicyId(), k.GetKeyDigest()}]
		if !ok {
			at[id{k.GetPolicyId(), k.GetKeyDigest()}] = len(p.PolicyIds)
			p.PolicyIds = append(p.PolicyIds, k.GetPolicyId())
			p.KeyDigests = append(p.KeyDigests, k.GetKeyDigest())
			p.ConsumerIds = append(p.ConsumerIds, k.GetConsumerId())
			p.ApiKeyIds = append(p.ApiKeyIds, k.GetApiKeyId())
			p.Denied = append(p.Denied, k.GetDenied())
			p.ShadowDenied = append(p.ShadowDenied, k.GetShadowDenied())
			continue
		}
		p.Denied[i] += k.GetDenied()
		p.ShadowDenied[i] += k.GetShadowDenied()
		if k.GetConsumerId() != 0 {
			p.ConsumerIds[i] = k.GetConsumerId()
		}
		if k.GetApiKeyId() != 0 {
			p.ApiKeyIds[i] = k.GetApiKeyId()
		}
	}
	return p
}

// digest reports whether d is a key digest: 16 bytes in base64url without
// padding, 22 characters.
func digest(d string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(d)
	return err == nil && len(b) == 16
}
