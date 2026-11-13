// Package store is the control plane's Postgres store (CP-01, ADR-0036):
// the embedded goose migrations, the sqlc queries in db, and the
// transactions every change runs in. Config edits run under the working-set
// revision's row lock and identity changes under the identity version's
// (ADR-0037), each in one transaction.
package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
)

// Store is the control plane's state in Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database at dsn with password, which never appears in
// dsn (ADR-0036), and checks that its schema is at least the one this binary
// needs: `controlplane serve` never migrates (CP-19).
func Open(ctx context.Context, dsn, password string) (*Store, error) {
	cfg, err := poolConfig(dsn, password)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool}
	if err := s.checkSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// poolConfig parses dsn, which must not carry a password, and sets password.
func poolConfig(dsn, password string) (*pgxpool.Config, error) {
	if u, err := url.Parse(dsn); err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("database URL: want postgres://user@host:port/database")
	} else if _, set := u.User.Password(); set || u.Query().Has("password") {
		return nil, errors.New("database URL: carries a password; give it in its own setting instead")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("database URL: not valid") // pgx's error would quote the URL
	}
	cfg.ConnConfig.Password = password
	return cfg, nil
}

// Close closes the store's connections.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether Postgres answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// checkSchema fails when the database's schema is older than this binary's
// migrations; a newer one is fine, as a rolling deploy runs the previous
// binary on the expanded schema (CP-19, ADR-0036).
func (s *Store) checkSchema(ctx context.Context) error {
	var have int64
	err := s.pool.QueryRow(ctx, "SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied").Scan(&have)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "42P01" { // undefined_table
		have, err = 0, nil
	}
	if err != nil {
		return fmt.Errorf("database schema version: %w", err)
	}
	if want := SchemaVersion(); have < want {
		return fmt.Errorf("database schema %d is older than this binary needs (%d): run controlplane migrate", have, want)
	}
	return nil
}

// ErrNotFound means no such entity.
var ErrNotFound = errors.New("not found")

// PreconditionError is an If-Match that is not the entity's current version
// (CP-14).
type PreconditionError struct{ Current int64 }

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("If-Match does not match the current version %d", e.Current)
}

// ConflictError is a change that the current state forbids: a name taken, an
// entity still in use (409).
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// InvalidError is a change that breaks an entity rule or refers to what does
// not exist (422).
type InvalidError struct {
	Path string // the field at fault, if one is
	Msg  string
}

func (e *InvalidError) Error() string { return e.Msg }

// Unavailable reports whether err means Postgres could not be reached (503).
func Unavailable(err error) bool {
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return true
	}
	return pgconn.SafeToRetry(err)
}

// Page is one page of a keyset scan: rows with an ID above After, at most
// Limit of them. The store returns one row more when there are more.
type Page struct {
	After int64
	Limit int32
}

func (p Page) lim() int32 { return p.Limit + 1 }

// inTx runs fn in one transaction.
func (s *Store) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(db.New(tx))
	})
}

// inConfigTx runs a config edit (ADR-0037): it bumps the working-set
// revision first, which serializes config writers, and fn writes rows with
// that revision as their version. Every config edit goes through here, and
// so will its audit record (ADR-0040).
func (s *Store) inConfigTx(ctx context.Context, fn func(q *db.Queries, revision int64) error) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		rev, err := q.BumpRevision(ctx, nil)
		if err != nil {
			return err
		}
		return fn(q, rev)
	})
}

// inIdentityTx runs an identity change (CP-20, ADR-0037): it bumps the
// identity version first, whose row lock orders identity changes so that
// versions follow commit order, and fn writes rows with that version. Every
// identity change goes through here, and so will its audit record and
// NOTIFY (ADR-0038, ADR-0040).
func (s *Store) inIdentityTx(ctx context.Context, fn func(q *db.Queries, version int64) error) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		v, err := q.BumpIdentityVersion(ctx)
		if err != nil {
			return err
		}
		return fn(q, v)
	})
}

// checkVersion is the If-Match check (CP-14): ifMatch must be the row's
// current version.
func checkVersion(current, ifMatch int64) error {
	if current != ifMatch {
		return &PreconditionError{Current: current}
	}
	return nil
}

// notFound turns pgx's no-rows error into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// constraintMessages explain the violations of named constraints that a
// request can cause.
var constraintMessages = map[string]string{
	"upstreams_name_key":            "an upstream with this name exists",
	"routes_name_key":               "a route with this name exists",
	"policies_name_key":             "a policy with this name exists",
	"plans_name_key":                "a plan with this name exists",
	"tenants_name_key":              "a tenant with this name exists",
	"consumers_tenant_id_name_key":  "the tenant has a consumer with this name",
	"consumers_jwt_subject_key":     "another consumer has this jwt_subject",
	"consumers_tenant_id_fkey":      "tenant_id: no such tenant",
	"access_lists_key":              "an entry with this action exists for this key",
	"access_lists_consumer":         "an entry with this action exists for this consumer",
	"access_lists_cidr":             "an entry with this action exists for this CIDR",
	"access_lists_api_key_id_fkey":  "api_key_id: no such key",
	"access_lists_consumer_id_fkey": "consumer_id: no such consumer",
	"overrides_consumer_id_fkey":    "consumer_id: no such consumer",
	"overrides_standing":            "the consumer has a standing override (no ends_at) for this policy",
}

// writeError turns constraint violations from a create or update into
// ConflictError (unique) and InvalidError (foreign key, check).
func writeError(err error) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return notFound(err)
	}
	msg := constraintMessages[pgErr.ConstraintName]
	switch pgErr.Code {
	case "23505": // unique_violation
		return &ConflictError{Msg: cmp.Or(msg, "the entity conflicts with another")}
	case "23503", "23514", "22P02": // foreign_key_violation, check_violation, invalid_text_representation
		return &InvalidError{Msg: cmp.Or(msg, "the entity breaks a rule of the schema ("+pgErr.ConstraintName+")")}
	}
	return err
}

// deleteError turns a foreign-key violation on delete into ConflictError.
func deleteError(err error, msg string) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && (pgErr.Code == "23503" || pgErr.Code == "23001") { // foreign_key_violation, restrict_violation
		return &ConflictError{Msg: msg}
	}
	return err
}
