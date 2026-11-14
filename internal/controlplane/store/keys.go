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
// grace (CP-16, D2). One identity change, one audit record. A revoked or
// expired key is a ConflictError.
func (s *Store) RotateKey(ctx context.Context, a Audit, gen KeyGen, id int64, grace time.Duration, note *string) (old, rotated Key, key apikey.Key, err error) {
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
		if rotated, key, err = insertKey(ctx, q, gen, db.InsertAPIKeyParams{ConsumerID: cur.ConsumerID, RotatedFrom: &id, Version: v}); err != nil {
			return err
		}
		after, err := q.ExpireAPIKey(ctx, db.ExpireAPIKeyParams{ID: id, GraceSeconds: grace.Seconds(), Version: v})
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

// listed is a key row as listed.
func listed(r db.GetAPIKeyRow) Key {
	return Key{
		ID: r.ID, ConsumerID: r.ConsumerID, Prefix: r.Prefix, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt,
		RotatedFrom: r.RotatedFrom, Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
