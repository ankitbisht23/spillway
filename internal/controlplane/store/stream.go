package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// What the config stream reads (CP-06, CP-20, CP-22, ADR-0037, ADR-0038):
// the latest config version, the identity snapshot, both versions, the
// LISTEN connection and the active replica's lock session.

// The LISTEN channels; each payload is the new version, in decimal.
const (
	ChannelConfig   = "spillway_config"
	ChannelIdentity = "spillway_identity"
)

// LatestConfig returns the latest published config version as the stream
// sends it, or nil before the first publish. A version of another schema
// major is an error (CP-18).
func (s *Store) LatestConfig(ctx context.Context) (*configpb.ConfigUpdate, error) {
	v, err := db.New(s.pool).LatestConfigVersion(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := parseSnapshot(v)
	if err != nil {
		return nil, err
	}
	return &configpb.ConfigUpdate{
		SchemaVersion: &configpb.SchemaVersion{Major: v.SchemaMajor, Minor: v.SchemaMinor},
		Config:        c,
		PublishedAt:   timestamppb.New(v.PublishedAt),
	}, nil
}

// Versions are the newest config and identity versions, when each was
// made, and the stream epoch (ADR-0055).
type Versions struct {
	Config, Identity int64 // Config is 0 before the first publish
	// The config's published_at (zero before the first publish) and the
	// identity's changed_at: within one epoch a version is made once, so
	// another time at the same version means another history.
	ConfigAt, IdentityAt time.Time
	Epoch                string // "" when there is none yet
}

// LatestVersions returns the newest versions.
func (s *Store) LatestVersions(ctx context.Context) (Versions, error) {
	r, err := db.New(s.pool).LatestVersions(ctx)
	if err != nil {
		return Versions{}, err
	}
	v := Versions{Config: r.ConfigVersion, Identity: r.IdentityVersion, ConfigAt: r.ConfigAt, IdentityAt: r.IdentityAt, Epoch: r.Epoch}
	if v.Config == 0 {
		v.ConfigAt = time.Time{}
	}
	return v, nil
}

// StreamConfig returns the latest published config version as the stream
// sends it, with the stream epoch read in the same REPEATABLE READ
// transaction, so the two always match (ADR-0055); its Config is nil
// before the first publish. A version of another schema major is an error
// (CP-18).
func (s *Store) StreamConfig(ctx context.Context) (*configpb.ConfigUpdate, error) {
	u := &configpb.ConfigUpdate{}
	err := pgx.BeginTxFunc(ctx, s.pool, readSnapshot, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if u.Epoch, err = q.StreamEpoch(ctx); err != nil {
			return err
		}
		v, err := q.LatestConfigVersion(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.Config, err = parseSnapshot(v); err != nil {
			return err
		}
		u.SchemaVersion = &configpb.SchemaVersion{Major: v.SchemaMajor, Minor: v.SchemaMinor}
		u.PublishedAt = timestamppb.New(v.PublishedAt)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// StreamIdentity returns the identity snapshot as the stream sends it,
// with the stream epoch and the time its version was made (changed_at),
// read in the same transaction (ADR-0055).
func (s *Store) StreamIdentity(ctx context.Context) (*configpb.IdentityUpdate, time.Time, error) {
	u := &configpb.IdentityUpdate{SchemaVersion: &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor}}
	var at time.Time
	err := pgx.BeginTxFunc(ctx, s.pool, readSnapshot, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if u.Epoch, err = q.StreamEpoch(ctx); err != nil {
			return err
		}
		if at, err = q.IdentityChangedAt(ctx); err != nil {
			return err
		}
		u.Identity, err = readIdentity(ctx, q)
		return err
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	return u, at, nil
}

// readSnapshot is a transaction whose reads all see one snapshot.
var readSnapshot = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// RotateEpoch sets a new random stream epoch and returns it (ADR-0055).
// With old set, it does so only while the epoch is still old, and
// otherwise returns the epoch that replaced it and rotated false.
func (s *Store) RotateEpoch(ctx context.Context, old string) (epoch string, rotated bool, err error) {
	next, err := NewEpoch()
	if err != nil {
		return "", false, err
	}
	q := db.New(s.pool)
	epoch, err = q.RotateStreamEpoch(ctx, db.RotateStreamEpochParams{Next: next, Old: old})
	if errors.Is(err, pgx.ErrNoRows) {
		epoch, err = q.StreamEpoch(ctx)
		return epoch, false, err
	}
	return epoch, err == nil, err
}

// NewEpoch returns a random stream epoch: 16 bytes from crypto/rand, in
// lowercase hex (ADR-0055).
func NewEpoch() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Identity returns the identity snapshot (ADR-0037): the identity version
// and every row that can reach a gateway — tenants, consumers, keys that
// are neither revoked nor expired, access lists, overrides that have not
// ended — read in one REPEATABLE READ transaction, so the rows and the
// version always match. Keys are their prefix and SHA-256, never more.
func (s *Store) Identity(ctx context.Context) (*configpb.Identity, error) {
	var id *configpb.Identity
	err := pgx.BeginTxFunc(ctx, s.pool, readSnapshot, func(tx pgx.Tx) error {
		var err error
		id, err = readIdentity(ctx, db.New(tx))
		return err
	})
	if err != nil {
		return nil, err
	}
	return id, nil
}

// readIdentity reads the identity snapshot with q, which must be a
// REPEATABLE READ transaction's.
func readIdentity(ctx context.Context, q *db.Queries) (*configpb.Identity, error) {
	id := &configpb.Identity{KeyFormat: config.KeyFormatManaged}
	var err error
	if id.Version, err = q.IdentityVersion(ctx); err != nil {
		return nil, err
	}
	tenants, err := q.AllTenants(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tenants {
		id.Tenants = append(id.Tenants, &configpb.Tenant{Id: t.ID, Name: t.Name, PlanId: t.PlanID, Status: t.Status})
	}
	consumers, err := q.AllConsumers(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range consumers {
		id.Consumers = append(id.Consumers, &configpb.Consumer{
			Id: c.ID, TenantId: c.TenantID, Name: c.Name, PlanId: deref(c.PlanID), Status: c.Status, JwtSubject: deref(c.JwtSubject),
		})
	}
	keys, err := q.LiveAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		id.ApiKeys = append(id.ApiKeys, &configpb.ApiKey{
			Id: k.ID, ConsumerId: k.ConsumerID, Prefix: k.Prefix, Hash: hex.EncodeToString(k.Hash), ExpiresAt: timestamp(k.ExpiresAt),
		})
	}
	lists, err := q.AllAccessLists(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range lists {
		e := &configpb.AccessListEntry{Id: a.ID, Action: a.Action, SubjectType: a.SubjectType, ApiKeyId: deref(a.ApiKeyID), ConsumerId: deref(a.ConsumerID)}
		if a.Cidr != nil {
			e.Cidr = a.Cidr.String()
		}
		id.AccessLists = append(id.AccessLists, e)
	}
	overrides, err := q.LiveOverrides(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range overrides {
		ov := &configpb.Override{Id: o.ID, ConsumerId: o.ConsumerID, PolicyId: deref(o.PolicyID), StartsAt: timestamp(o.StartsAt), EndsAt: timestamp(o.EndsAt)}
		if ov.Limits, err = parseLimits(o.Limits); err != nil {
			return nil, fmt.Errorf("override %d: stored limits: %w", o.ID, err)
		}
		id.Overrides = append(id.Overrides, ov)
	}
	return id, nil
}

// parseLimits parses an override's stored limits, a JSON list of Limit
// messages.
func parseLimits(b []byte) ([]*configpb.Limit, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := make([]*configpb.Limit, len(raw))
	for i, r := range raw {
		out[i] = &configpb.Limit{}
		if err := protojson.Unmarshal(r, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func timestamp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

// Listen runs a dedicated LISTEN connection on the config and identity
// channels and calls notify with each notification's channel until ctx
// ends or the connection fails, which it returns; listening is the first
// thing it does, so a caller that reads the versions after ready runs
// misses nothing. Payloads are not trusted: the caller re-reads the
// versions.
func (s *Store) Listen(ctx context.Context, ready func(), notify func(channel string)) error {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, ch := range []string{ChannelConfig, ChannelIdentity} {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return err
		}
	}
	ready()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		notify(n.Channel)
	}
}

// The active replica's session advisory lock (CP-22, D6, ADR-0038):
// 1397774156 is 0x5350574C, "SPWL"; the second key 1 is "active replica"
// (2 is M7's quota flusher).
const (
	lockSpace  = 1397774156
	lockActive = 1
)

// LockSession is a dedicated connection that holds, or tries to take, the
// active replica's lock. TCP keepalives on both ends end a vanished peer's
// session, and so free its lock, about 10 s after it disappears.
type LockSession struct{ conn *pgx.Conn }

// lockKeepAlive is the keepalive both ends of a lock session use.
var lockKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 5 * time.Second, Interval: time.Second, Count: 5}

// OpenLockSession opens a lock session.
func (s *Store) OpenLockSession(ctx context.Context) (*LockSession, error) {
	cfg := s.pool.Config().ConnConfig.Copy()
	d := &net.Dialer{KeepAliveConfig: lockKeepAlive}
	cfg.DialFunc = d.DialContext
	cfg.RuntimeParams["tcp_keepalives_idle"] = strconv.Itoa(int(lockKeepAlive.Idle / time.Second))
	cfg.RuntimeParams["tcp_keepalives_interval"] = strconv.Itoa(int(lockKeepAlive.Interval / time.Second))
	cfg.RuntimeParams["tcp_keepalives_count"] = strconv.Itoa(lockKeepAlive.Count)
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &LockSession{conn: conn}, nil
}

// TryLock takes the lock if it is free and reports whether the session
// holds it.
func (l *LockSession) TryLock(ctx context.Context) (bool, error) {
	var ok bool
	err := l.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", int32(lockSpace), int32(lockActive)).Scan(&ok)
	return ok, err
}

// Ping checks that the session, and so the lock it holds, is still there.
func (l *LockSession) Ping(ctx context.Context) error { return l.conn.Ping(ctx) }

// Close ends the session, which frees the lock.
func (l *LockSession) Close(ctx context.Context) error { return l.conn.Close(ctx) }
