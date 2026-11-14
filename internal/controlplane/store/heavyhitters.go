package store

import (
	"cmp"
	"context"
	"encoding/base64"
	"maps"
	"slices"
	"strings"
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
// (policy, key digest), duplicates summed with the later IDs kept, in
// (policy, key digest) order, so that heartbeats of several replicas lock
// shared windows in one order and cannot deadlock (ADR-0051). A key that is
// not one a gateway sends (a policy ID below 1, a digest that is not 22
// base64url characters, negative or no counts) is dropped, so one bad entry
// cannot fail the heartbeat.
func heavyHitters(keys []*configpb.TopKey) db.AddHeavyHittersParams {
	type id struct {
		policy int64
		digest string
	}
	sum := map[id]*configpb.TopKey{}
	for _, k := range keys {
		if k.GetPolicyId() < 1 || !digest(k.GetKeyDigest()) || k.GetDenied() < 0 || k.GetShadowDenied() < 0 || k.GetDenied()+k.GetShadowDenied() < 1 ||
			k.GetConsumerId() < 0 || k.GetApiKeyId() < 0 {
			continue
		}
		x, ok := sum[id{k.GetPolicyId(), k.GetKeyDigest()}]
		if !ok {
			sum[id{k.GetPolicyId(), k.GetKeyDigest()}] = &configpb.TopKey{
				PolicyId: k.GetPolicyId(), KeyDigest: k.GetKeyDigest(), ConsumerId: k.GetConsumerId(), ApiKeyId: k.GetApiKeyId(),
				Denied: k.GetDenied(), ShadowDenied: k.GetShadowDenied(),
			}
			continue
		}
		x.Denied += k.GetDenied()
		x.ShadowDenied += k.GetShadowDenied()
		if k.GetConsumerId() != 0 {
			x.ConsumerId = k.GetConsumerId()
		}
		if k.GetApiKeyId() != 0 {
			x.ApiKeyId = k.GetApiKeyId()
		}
	}
	var p db.AddHeavyHittersParams
	for _, i := range slices.SortedFunc(maps.Keys(sum), func(a, b id) int {
		return cmp.Or(cmp.Compare(a.policy, b.policy), strings.Compare(a.digest, b.digest))
	}) {
		x := sum[i]
		p.PolicyIds = append(p.PolicyIds, x.PolicyId)
		p.KeyDigests = append(p.KeyDigests, x.KeyDigest)
		p.ConsumerIds = append(p.ConsumerIds, x.ConsumerId)
		p.ApiKeyIds = append(p.ApiKeyIds, x.ApiKeyId)
		p.Denied = append(p.Denied, x.Denied)
		p.ShadowDenied = append(p.ShadowDenied, x.ShadowDenied)
	}
	return p
}

// digest reports whether d is a key digest: 16 bytes in base64url without
// padding, 22 characters.
func digest(d string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(d)
	return err == nil && len(b) == 16
}
