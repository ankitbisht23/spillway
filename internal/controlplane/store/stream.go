package store

import (
	"context"
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

// LatestVersions returns the latest config version (0 before the first
// publish) and the identity version.
func (s *Store) LatestVersions(ctx context.Context) (configVersion, identityVersion int64, err error) {
	r, err := db.New(s.pool).LatestVersions(ctx)
	return r.ConfigVersion, r.IdentityVersion, err
}

// Identity returns the identity snapshot (ADR-0037): the identity version
// and every row that can reach a gateway — tenants, consumers, keys that
// are neither revoked nor expired, access lists, overrides that have not
// ended — read in one REPEATABLE READ transaction, so the rows and the
// version always match. Keys are their prefix and SHA-256, never more.
func (s *Store) Identity(ctx context.Context) (*configpb.Identity, error) {
	id := &configpb.Identity{KeyFormat: config.KeyFormatManaged}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if id.Version, err = q.IdentityVersion(ctx); err != nil {
			return err
		}
		tenants, err := q.AllTenants(ctx)
		if err != nil {
			return err
		}
		for _, t := range tenants {
			id.Tenants = append(id.Tenants, &configpb.Tenant{Id: t.ID, Name: t.Name, PlanId: t.PlanID, Status: t.Status})
		}
		consumers, err := q.AllConsumers(ctx)
		if err != nil {
			return err
		}
		for _, c := range consumers {
			id.Consumers = append(id.Consumers, &configpb.Consumer{
				Id: c.ID, TenantId: c.TenantID, Name: c.Name, PlanId: deref(c.PlanID), Status: c.Status, JwtSubject: deref(c.JwtSubject),
			})
		}
		keys, err := q.LiveAPIKeys(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			id.ApiKeys = append(id.ApiKeys, &configpb.ApiKey{
				Id: k.ID, ConsumerId: k.ConsumerID, Prefix: k.Prefix, Hash: hex.EncodeToString(k.Hash), ExpiresAt: timestamp(k.ExpiresAt),
			})
		}
		lists, err := q.AllAccessLists(ctx)
		if err != nil {
			return err
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
			return err
		}
		for _, o := range overrides {
			ov := &configpb.Override{Id: o.ID, ConsumerId: o.ConsumerID, PolicyId: deref(o.PolicyID), StartsAt: timestamp(o.StartsAt), EndsAt: timestamp(o.EndsAt)}
			if ov.Limits, err = parseLimits(o.Limits); err != nil {
				return fmt.Errorf("override %d: stored limits: %w", o.ID, err)
			}
			id.Overrides = append(id.Overrides, ov)
		}
		return nil
	})
	if err != nil {
		return nil, err
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
