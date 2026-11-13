package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
)

// Actor is who makes a change (ADR-0040): a bearer token's subject, its
// display name and its Spillway roles.
type Actor struct {
	Sub   string
	Name  string
	Roles []string
}

// Audit is what a change's audit record needs besides the change itself
// (CP-13, ADR-0040). Every mutation of the store takes one and writes its
// record in its own transaction, so a change that rolls back leaves none.
type Audit struct {
	Actor     Actor
	RequestID string
	// Show returns a row's REST representation, which the record's diff
	// holds; the rows are the store's own types (ConfigRow, db.Tenant, …).
	Show func(row any) (any, error)
}

// change is one audit record's content.
type change struct {
	action   string // <entity>.<verb>
	entity   string
	id       int64
	before   any // the row before, nil on create
	after    any // the row after, nil on delete
	note     *string
	removed  map[string][]int64 // rows a delete cascaded to, by kind
	identity int64              // the identity version the change made, 0 for none
	config   int64              // the config version the change made, 0 for none
	// diff, when not nil, is the record's diff instead of before and after:
	// publish and rollback record their changes list (ADR-0040).
	diff any
}

// write inserts c's audit record. The diff is {"before": …, "after": …}
// in the REST representation, plus "removed" for a cascading delete, or
// c.diff; it never holds a key, its hash, a token or a password (ADR-0040).
func (a Audit) write(ctx context.Context, q *db.Queries, c change) error {
	if a.Actor.Sub == "" || a.RequestID == "" || a.Show == nil && (c.before != nil || c.after != nil) {
		return errors.New("an audited change needs an actor, a request ID and Show")
	}
	diff := c.diff
	if diff == nil {
		var err error
		if diff, err = a.beforeAfter(c); err != nil {
			return err
		}
	}
	b, err := json.Marshal(diff)
	if err != nil {
		return err
	}
	roles := a.Actor.Roles
	if roles == nil {
		roles = []string{}
	}
	id := strconv.FormatInt(c.id, 10)
	p := db.InsertAuditParams{
		ActorSub: a.Actor.Sub, ActorName: a.Actor.Name, ActorRoles: roles, Action: c.action, Entity: c.entity,
		EntityID: &id, Diff: b, Note: c.note, RequestID: a.RequestID,
	}
	if c.identity != 0 {
		p.IdentityVersion = &c.identity
	}
	if c.config != 0 {
		p.ConfigVersion = &c.config
	}
	return q.InsertAudit(ctx, p)
}

// beforeAfter is c's diff of an entity change.
func (a Audit) beforeAfter(c change) (map[string]any, error) {
	diff := map[string]any{"before": nil, "after": nil}
	var err error
	if c.before != nil {
		if diff["before"], err = a.Show(c.before); err != nil {
			return nil, err
		}
	}
	if c.after != nil {
		if diff["after"], err = a.Show(c.after); err != nil {
			return nil, err
		}
	}
	if len(c.removed) > 0 {
		diff["removed"] = c.removed
	}
	return diff, nil
}

// AuditFilter narrows an audit-log page (ADR-0036): EntityID only with
// Entity; Since inclusive, Until exclusive.
type AuditFilter struct {
	Entity   *string
	EntityID *string
	ActorSub *string
	Since    *time.Time
	Until    *time.Time
}

// ListAudit returns a page of audit records, newest first, with an ID below
// before (0: from the newest); one more than limit when there are more.
func (s *Store) ListAudit(ctx context.Context, before int64, limit int32, f AuditFilter) ([]db.AuditLog, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	q, lim := db.New(s.pool), limit+1
	switch {
	case f.Entity != nil && f.EntityID != nil:
		return q.ListAuditByEntityID(ctx, db.ListAuditByEntityIDParams{
			Entity: *f.Entity, EntityID: f.EntityID, Before: before, ActorSub: f.ActorSub, Since: f.Since, Until: f.Until, Lim: lim,
		})
	case f.Entity != nil:
		return q.ListAuditByEntity(ctx, db.ListAuditByEntityParams{
			Entity: *f.Entity, Before: before, ActorSub: f.ActorSub, Since: f.Since, Until: f.Until, Lim: lim,
		})
	case f.ActorSub != nil:
		return q.ListAuditByActor(ctx, db.ListAuditByActorParams{ActorSub: *f.ActorSub, Before: before, Since: f.Since, Until: f.Until, Lim: lim})
	}
	return q.ListAudit(ctx, db.ListAuditParams{Before: before, Since: f.Since, Until: f.Until, Lim: lim})
}
