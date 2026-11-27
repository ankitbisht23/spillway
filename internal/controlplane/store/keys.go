package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
)

// Managed API keys (CP-15 - CP-17, ADR-0039). Each issue, rotation and
// revocation is one identity change (CP-20): its own identity version and
// audit record in one transaction, and a NOTIFY that reaches the Watch
// streams at commit. Rows here never carry the hash; the key itself exists
// only in what IssueKey and RotateKey return.

// Key is an API key as listed: never the key or its hash.
type Key = db.ListConsumerKeysRow

// KeyGen makes a new key; apikey.Generate outside tests.
type KeyGen func() (apikey.Key, error)

// prefixDraws is how many prefixes an issue tries before it gives up: the
// first and up to 3 more after collisions (ADR-0039).
const prefixDraws = 4

// ErrPrefixesTaken means every prefix drawn for a new key was taken.
var ErrPrefixesTaken = errors.New("no free key prefix after 4 draws")

// MaxGrace is the longest rotation grace period (D2).
const MaxGrace = 7 * 24 * time.Hour

// IssueKey issues a key for a consumer, expiring at expiresAt unless nil
// (CP-15). It returns the key's row and the key, which nothing stores.
func (s *Store) IssueKey(ctx context.Context, a Audit, gen KeyGen, consumerID int64, expiresAt *time.Time) (Key, apikey.Key, error) {
	var row Key
	var key apikey.Key
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		if _, err := q.GetConsumer(ctx, consumerID); err != nil {
			return notFound(err)
		}
		var err error
		if row, key, err = insertKey(ctx, q, gen, db.InsertAPIKeyParams{ConsumerID: consumerID, ExpiresAt: expiresAt, Version: v}); err != nil {
			return err
		}
		return a.write(ctx, q, change{action: "key.issue", entity: "key", id: row.ID, after: row, identity: v})
	})
	return row, key, err
}

// insertKey inserts a new key with p's fields, drawing another prefix when
// one is taken.
func insertKey(ctx context.Context, q *db.Queries, gen KeyGen, p db.InsertAPIKeyParams) (Key, apikey.Key, error) {
	for range prefixDraws {
		k, err := gen()
		if err != nil {
			return Key{}, apikey.Key{}, fmt.Errorf("generate a key: %w", err)
		}
		p.Prefix, p.Hash = k.Prefix, k.Hash[:]
		row, err := q.InsertAPIKey(ctx, p)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the prefix is taken
		}
		if err != nil {
			return Key{}, apikey.Key{}, writeError(err)
		}
		return Key(row), k, nil
	}
	return Key{}, apikey.Key{}, ErrPrefixesTaken
}

// RotateKey issues a new key for the consumer of key id, which keeps
// working for grace: its expiry becomes the earlier of its own and now plus
// grace (CP-16, D2). The new key gets a copy of the old key's own
// access-list entries, allow and deny (RL-09, ADR-0054); the old key keeps
// its own. One identity change, one audit record, whose diff lists the
// copies under "new_access_lists" when there are any. A revoked or expired
// key is a ConflictError. A non-nil latest is the new key's expiry and
// ends the old key's grace at the latest then (demo mode's 30-minute rule,
// ADR-0095 §3).
func (s *Store) RotateKey(ctx context.Context, a Audit, gen KeyGen, id int64, grace time.Duration, latest *time.Time, note *string) (old, rotated Key, key apikey.Key, err error) {
	if grace < 0 || grace > MaxGrace {
		return Key{}, Key{}, apikey.Key{}, &InvalidError{Path: "grace", Msg: "grace: want 0s to 168h"}
	}
	err = s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetAPIKey(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if cur.RevokedAt != nil || cur.Expired {
			return &ConflictError{Msg: "the key is revoked or expired; issue a new key instead"}
		}
		if rotated, key, err = insertKey(ctx, q, gen, db.InsertAPIKeyParams{ConsumerID: cur.ConsumerID, ExpiresAt: latest, RotatedFrom: &id, Version: v}); err != nil {
			return err
		}
		after, err := q.ExpireAPIKey(ctx, db.ExpireAPIKeyParams{ID: id, GraceSeconds: grace.Seconds(), Latest: latest, Version: v})
		if err != nil {
			return err
		}
		old = Key(after)
		diff, err := a.beforeAfter(change{before: listed(cur), after: old})
		if err != nil {
			return err
		}
		if diff["new_key"], err = a.Show(rotated); err != nil {
			return err
		}
		copies, err := q.CopyKeyAccessLists(ctx, db.CopyKeyAccessListsParams{NewKeyID: rotated.ID, Version: v, OldKeyID: id})
		if err != nil {
			return err
		}
		if len(copies) > 0 {
			shown := make([]any, len(copies))
			for i, e := range copies {
				if shown[i], err = a.Show(e); err != nil {
					return err
				}
			}
			diff["new_access_lists"] = shown
		}
		return a.write(ctx, q, change{action: "key.rotate", entity: "key", id: id, note: note, identity: v, diff: diff})
	})
	return old, rotated, key, err
}

// RevokeKey revokes key id at once (CP-17). Revoking a revoked key changes
// nothing: no identity version, no audit record.
func (s *Store) RevokeKey(ctx context.Context, a Audit, id int64, note *string) (Key, error) {
	var out Key
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetAPIKey(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if cur.RevokedAt != nil {
			out = listed(cur)
			return errUnchanged // rolls back the version bump (import.go)
		}
		row, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{ID: id, Version: v})
		if err != nil {
			return err
		}
		out = Key(row)
		return a.write(ctx, q, change{action: "key.revoke", entity: "key", id: id, before: listed(cur), after: out, note: note, identity: v})
	})
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	return out, err
}

// DeleteExpiredKeys deletes up to limit keys that expired more than age ago
// on Postgres's clock and that no key was rotated from, with the
// access-list entries that name them (demo mode's janitor, ADR-0095 §4).
// One identity change for the lot, with one audit record per key whose
// removed lists its access-list entries; with nothing to delete, no
// identity version and no record. It returns how many keys it deleted.
func (s *Store) DeleteExpiredKeys(ctx context.Context, a Audit, age time.Duration, limit int32) (int, error) {
	var n int
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		rows, err := q.ExpiredAPIKeys(ctx, db.ExpiredAPIKeysParams{AgeSeconds: age.Seconds(), Lim: limit})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return errUnchanged // rolls back the version bump (import.go)
		}
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		entries, err := q.KeysAccessLists(ctx, ids)
		if err != nil {
			return err
		}
		lists := map[int64][]int64{}
		for _, e := range entries {
			lists[*e.ApiKeyID] = append(lists[*e.ApiKeyID], e.ID)
		}
		if err := q.DeleteAPIKeys(ctx, ids); err != nil {
			return err
		}
		for _, r := range rows {
			c := change{action: "key.delete", entity: "key", id: r.ID, before: Key(r), identity: v}
			if l := lists[r.ID]; len(l) > 0 {
				c.removed = map[string][]int64{"access_list_ids": l}
			}
			if err := a.write(ctx, q, c); err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	return n, err
}

// listed is a key row as listed.
func listed(r db.GetAPIKeyRow) Key {
	return Key{
		ID: r.ID, ConsumerID: r.ConsumerID, Prefix: r.Prefix, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt,
		RotatedFrom: r.RotatedFrom, Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
