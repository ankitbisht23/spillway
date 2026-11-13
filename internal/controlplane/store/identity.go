package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// configRefs are the config entities an identity change refers to by ID.
type configRefs struct {
	plans    []int64
	policies []int64
}

// checkPublished checks that the plans and policies an identity change
// refers to exist in the latest published config version (ADR-0037). It
// first takes a share lock on the working set, which waits for a publish or
// rollback in progress, so none can remove what the change refers to before
// it commits. The lock order, identity version then working set, is the only
// one that holds both.
func checkPublished(ctx context.Context, q *db.Queries, refs configRefs) error {
	if len(refs.plans) == 0 && len(refs.policies) == 0 {
		return nil
	}
	if err := q.ShareWorkingSet(ctx); err != nil {
		return err
	}
	latest, err := q.LatestConfigVersion(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return &InvalidError{Msg: "no config version is published yet; publish one with the plans and policies this refers to"}
	}
	if err != nil {
		return err
	}
	c := &configpb.Config{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(latest.Snapshot, c); err != nil {
		return fmt.Errorf("config version %d: %w", latest.Version, err)
	}
	for _, id := range refs.plans {
		if !slices.ContainsFunc(c.GetPlans(), func(p *configpb.Plan) bool { return p.GetId() == id }) {
			return &InvalidError{Path: "plan_id", Msg: fmt.Sprintf("plan_id: plan %d is not in the published config version %d", id, latest.Version)}
		}
	}
	for _, id := range refs.policies {
		if !slices.ContainsFunc(c.GetPolicies(), func(p *configpb.Policy) bool { return p.GetId() == id }) {
			return &InvalidError{Path: "policy_id", Msg: fmt.Sprintf("policy_id: policy %d is not in the published config version %d", id, latest.Version)}
		}
	}
	return nil
}

func optional(id *int64) []int64 {
	if id == nil {
		return nil
	}
	return []int64{*id}
}

// Tenants

// ListTenants returns a page of tenants by ID, those named name if it is
// not nil.
func (s *Store) ListTenants(ctx context.Context, p Page, name *string) ([]db.Tenant, error) {
	return db.New(s.pool).ListTenants(ctx, db.ListTenantsParams{After: p.After, Name: name, Lim: p.lim()})
}

// GetTenant returns one tenant.
func (s *Store) GetTenant(ctx context.Context, id int64) (db.Tenant, error) {
	t, err := db.New(s.pool).GetTenant(ctx, id)
	return t, notFound(err)
}

// CreateTenant adds a tenant (CP-20).
func (s *Store) CreateTenant(ctx context.Context, a Audit, in db.CreateTenantParams) (db.Tenant, error) {
	var out db.Tenant
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		if err := checkPublished(ctx, q, configRefs{plans: []int64{in.PlanID}}); err != nil {
			return err
		}
		in.Version = v
		t, err := q.CreateTenant(ctx, in)
		if err != nil {
			return writeError(err)
		}
		out = t
		return a.write(ctx, q, change{action: "tenant.create", entity: "tenant", id: t.ID, after: t, identity: v})
	})
	return out, err
}

// UpdateTenant replaces a tenant if its version is ifMatch (CP-14, CP-20).
func (s *Store) UpdateTenant(ctx context.Context, a Audit, id, ifMatch int64, in db.CreateTenantParams) (db.Tenant, error) {
	var out db.Tenant
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetTenant(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if err := checkPublished(ctx, q, configRefs{plans: []int64{in.PlanID}}); err != nil {
			return err
		}
		t, err := q.UpdateTenant(ctx, db.UpdateTenantParams{ID: id, Name: in.Name, PlanID: in.PlanID, Status: in.Status, Version: v})
		if err != nil {
			return writeError(err)
		}
		out = t
		return a.write(ctx, q, change{action: "tenant.update", entity: "tenant", id: id, before: cur, after: t, identity: v})
	})
	return out, err
}

// DeleteTenant removes a tenant that has no consumers, if its version is
// ifMatch (CP-14, CP-20).
func (s *Store) DeleteTenant(ctx context.Context, a Audit, id, ifMatch int64) error {
	return s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetTenant(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if _, err = q.DeleteTenant(ctx, id); err != nil {
			return deleteError(err, "the tenant has consumers; delete them first")
		}
		return a.write(ctx, q, change{action: "tenant.delete", entity: "tenant", id: id, before: cur, identity: v})
	})
}

// Consumers

// ListConsumers returns a page of consumers by ID, filtered by tenant and
// name when those are not nil.
func (s *Store) ListConsumers(ctx context.Context, p Page, tenantID *int64, name *string) ([]db.Consumer, error) {
	return db.New(s.pool).ListConsumers(ctx, db.ListConsumersParams{After: p.After, TenantID: tenantID, Name: name, Lim: p.lim()})
}

// GetConsumer returns one consumer.
func (s *Store) GetConsumer(ctx context.Context, id int64) (db.Consumer, error) {
	c, err := db.New(s.pool).GetConsumer(ctx, id)
	return c, notFound(err)
}

// CreateConsumer adds a consumer (CP-20).
func (s *Store) CreateConsumer(ctx context.Context, a Audit, in db.CreateConsumerParams) (db.Consumer, error) {
	var out db.Consumer
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		if err := checkPublished(ctx, q, configRefs{plans: optional(in.PlanID)}); err != nil {
			return err
		}
		in.Version = v
		c, err := q.CreateConsumer(ctx, in)
		if err != nil {
			return writeError(err)
		}
		out = c
		return a.write(ctx, q, change{action: "consumer.create", entity: "consumer", id: c.ID, after: c, identity: v})
	})
	return out, err
}

// UpdateConsumer replaces a consumer if its version is ifMatch (CP-14,
// CP-20).
func (s *Store) UpdateConsumer(ctx context.Context, a Audit, id, ifMatch int64, in db.CreateConsumerParams) (db.Consumer, error) {
	var out db.Consumer
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetConsumer(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if err := checkPublished(ctx, q, configRefs{plans: optional(in.PlanID)}); err != nil {
			return err
		}
		c, err := q.UpdateConsumer(ctx, db.UpdateConsumerParams{
			ID: id, TenantID: in.TenantID, Name: in.Name, PlanID: in.PlanID, Status: in.Status, JwtSubject: in.JwtSubject, Version: v,
		})
		if err != nil {
			return writeError(err)
		}
		out = c
		return a.write(ctx, q, change{action: "consumer.update", entity: "consumer", id: id, before: cur, after: c, identity: v})
	})
	return out, err
}

// DeleteConsumer removes a consumer, with its keys, overrides and
// access-list entries, if its version is ifMatch (CP-14, CP-20). Its audit
// record lists the IDs the delete cascaded to (ADR-0040).
func (s *Store) DeleteConsumer(ctx context.Context, a Audit, id, ifMatch int64) error {
	return s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetConsumer(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		deps, err := q.ConsumerDependents(ctx, id)
		if err != nil {
			return err
		}
		removed := map[string][]int64{}
		for _, d := range deps {
			removed[d.Kind+"_ids"] = append(removed[d.Kind+"_ids"], d.ID)
		}
		if _, err = q.DeleteConsumer(ctx, id); err != nil {
			return err
		}
		return a.write(ctx, q, change{action: "consumer.delete", entity: "consumer", id: id, before: cur, removed: removed, identity: v})
	})
}

// ListConsumerKeys returns a page of a consumer's API keys by ID, without
// their hashes (ADR-0036).
func (s *Store) ListConsumerKeys(ctx context.Context, consumerID int64, p Page) ([]db.ListConsumerKeysRow, error) {
	var out []db.ListConsumerKeysRow
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.GetConsumer(ctx, consumerID); err != nil {
			return notFound(err)
		}
		var err error
		out, err = q.ListConsumerKeys(ctx, db.ListConsumerKeysParams{ConsumerID: consumerID, After: p.After, Lim: p.lim()})
		return err
	})
	return out, err
}

// Access lists

// ListAccessLists returns a page of access-list entries by ID, filtered by
// action and subject type when those are not nil.
func (s *Store) ListAccessLists(ctx context.Context, p Page, action, subjectType *string) ([]db.AccessList, error) {
	return db.New(s.pool).ListAccessLists(ctx, db.ListAccessListsParams{After: p.After, Action: action, SubjectType: subjectType, Lim: p.lim()})
}

// GetAccessList returns one access-list entry.
func (s *Store) GetAccessList(ctx context.Context, id int64) (db.AccessList, error) {
	a, err := db.New(s.pool).GetAccessList(ctx, id)
	return a, notFound(err)
}

// CreateAccessList adds an access-list entry (CP-20, RL-09).
func (s *Store) CreateAccessList(ctx context.Context, a Audit, in db.CreateAccessListParams) (db.AccessList, error) {
	var out db.AccessList
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		in.Version = v
		e, err := q.CreateAccessList(ctx, in)
		if err != nil {
			return writeError(err)
		}
		out = e
		return a.write(ctx, q, change{action: "access_list.create", entity: "access_list", id: e.ID, after: e, note: e.Note, identity: v})
	})
	return out, err
}

// UpdateAccessList replaces an access-list entry if its version is ifMatch
// (CP-14, CP-20).
func (s *Store) UpdateAccessList(ctx context.Context, a Audit, id, ifMatch int64, in db.CreateAccessListParams) (db.AccessList, error) {
	var out db.AccessList
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetAccessList(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		e, err := q.UpdateAccessList(ctx, db.UpdateAccessListParams{
			ID: id, Action: in.Action, SubjectType: in.SubjectType, ApiKeyID: in.ApiKeyID, ConsumerID: in.ConsumerID, Cidr: in.Cidr, Note: in.Note, Version: v,
		})
		if err != nil {
			return writeError(err)
		}
		out = e
		return a.write(ctx, q, change{action: "access_list.update", entity: "access_list", id: id, before: cur, after: e, note: e.Note, identity: v})
	})
	return out, err
}

// DeleteAccessList removes an access-list entry if its version is ifMatch
// (CP-14, CP-20).
func (s *Store) DeleteAccessList(ctx context.Context, a Audit, id, ifMatch int64) error {
	return s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetAccessList(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if _, err = q.DeleteAccessList(ctx, id); err != nil {
			return err
		}
		return a.write(ctx, q, change{action: "access_list.delete", entity: "access_list", id: id, before: cur, identity: v})
	})
}

// Overrides

// ListOverrides returns a page of overrides by ID, filtered by consumer and
// policy when those are not nil.
func (s *Store) ListOverrides(ctx context.Context, p Page, consumerID, policyID *int64) ([]db.Override, error) {
	return db.New(s.pool).ListOverrides(ctx, db.ListOverridesParams{After: p.After, ConsumerID: consumerID, PolicyID: policyID, Lim: p.lim()})
}

// GetOverride returns one override.
func (s *Store) GetOverride(ctx context.Context, id int64) (db.Override, error) {
	o, err := db.New(s.pool).GetOverride(ctx, id)
	return o, notFound(err)
}

// CreateOverride adds an override or burst grant (CP-20, RL-08).
func (s *Store) CreateOverride(ctx context.Context, a Audit, in db.CreateOverrideParams) (db.Override, error) {
	var out db.Override
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		if err := checkPublished(ctx, q, configRefs{policies: optional(in.PolicyID)}); err != nil {
			return err
		}
		in.Version = v
		o, err := q.CreateOverride(ctx, in)
		if err != nil {
			return writeError(err)
		}
		out = o
		return a.write(ctx, q, change{action: "override.create", entity: "override", id: o.ID, after: o, note: o.Note, identity: v})
	})
	return out, err
}

// UpdateOverride replaces an override if its version is ifMatch (CP-14,
// CP-20).
func (s *Store) UpdateOverride(ctx context.Context, a Audit, id, ifMatch int64, in db.CreateOverrideParams) (db.Override, error) {
	var out db.Override
	err := s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetOverride(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if err := checkPublished(ctx, q, configRefs{policies: optional(in.PolicyID)}); err != nil {
			return err
		}
		o, err := q.UpdateOverride(ctx, db.UpdateOverrideParams{
			ID: id, ConsumerID: in.ConsumerID, PolicyID: in.PolicyID, Limits: in.Limits, StartsAt: in.StartsAt, EndsAt: in.EndsAt, Note: in.Note, Version: v,
		})
		if err != nil {
			return writeError(err)
		}
		out = o
		return a.write(ctx, q, change{action: "override.update", entity: "override", id: id, before: cur, after: o, note: o.Note, identity: v})
	})
	return out, err
}

// DeleteOverride removes an override if its version is ifMatch (CP-14,
// CP-20).
func (s *Store) DeleteOverride(ctx context.Context, a Audit, id, ifMatch int64) error {
	return s.inIdentityTx(ctx, func(q *db.Queries, v int64) error {
		cur, err := q.GetOverride(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if _, err = q.DeleteOverride(ctx, id); err != nil {
			return err
		}
		return a.write(ctx, q, change{action: "override.delete", entity: "override", id: id, before: cur, identity: v})
	})
}
