package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Validate, publish, versions and rollback (CP-02 - CP-04, ADR-0037).

// ConfigVersion is a published config version; Snapshot is empty in lists.
type ConfigVersion = db.ConfigVersion

// Published is a published config version's number and config.
type Published struct {
	Version int64
	Config  *configpb.Config
}

// IdentityRef is an identity row that refers to a config entity by ID: a
// tenant's or consumer's plan_id, an override's policy_id (ADR-0037).
type IdentityRef struct {
	Entity string // tenant, consumer or override
	ID     int64
	Name   string // the tenant's or consumer's; empty for an override
	Field  string // plan_id or policy_id
	Ref    int64  // the plan or policy ID
}

// Check validates c, the config a publish or rollback would publish (CP-03),
// against refs, the identity rows that refer to plans or policies of base,
// the latest published version, that c might lack. It returns the changes
// from base to c for the audit record, or an error that stops the change.
type Check func(ctx context.Context, c *configpb.Config, base *Published, refs []IdentityRef) (changes any, err error)

// WorkingSet is the config working set at one revision, as the Config a
// publish would write, with the latest published version and the identity
// rows a validation needs (ADR-0037).
type WorkingSet struct {
	Revision int64
	Config   *configpb.Config
	Base     *Published // nil before the first publish
	Refs     []IdentityRef
}

// ReadWorkingSet reads the working set in one read-only snapshot, so its
// rows, revision and base agree.
func (s *Store) ReadWorkingSet(ctx context.Context) (WorkingSet, error) {
	var ws WorkingSet
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if ws.Revision, err = q.WorkingSetRevision(ctx); err != nil {
			return err
		}
		if ws.Config, _, err = readWorkingSet(ctx, q); err != nil {
			return err
		}
		if _, ws.Base, err = latestVersion(ctx, q); err != nil {
			return err
		}
		ws.Refs, err = identityRefs(ctx, q, ws.Base, ws.Config)
		return err
	})
	return ws, err
}

// Publish writes the working set as a new config version with note, if the
// working set is at revision ifMatch (CP-02) and passes check (CP-03). Under
// the working set's row lock, versions are monotonic, gapless and commit in
// order; the audit record and the NOTIFY are in the same transaction
// (ADR-0037).
func (s *Store) Publish(ctx context.Context, a Audit, ifMatch int64, note string, check Check) (ConfigVersion, error) {
	var out ConfigVersion
	err := s.inTx(ctx, func(q *db.Queries) error {
		rev, err := q.LockWorkingSet(ctx)
		if err != nil {
			return err
		}
		if err := checkVersion(rev, ifMatch); err != nil {
			return err
		}
		latest, base, err := latestVersion(ctx, q)
		if err != nil {
			return err
		}
		if latest != nil && latest.WorkingSetRevision == rev {
			return &ConflictError{Msg: fmt.Sprintf("nothing to publish: the working set has not changed since version %d", latest.Version)}
		}
		c, _, err := readWorkingSet(ctx, q)
		if err != nil {
			return err
		}
		refs, err := identityRefs(ctx, q, base, c)
		if err != nil {
			return err
		}
		changes, err := check(ctx, c, base, refs)
		if err != nil {
			return err
		}
		out, err = insertVersion(ctx, q, a, c, base, newVersion{kind: "publish", revision: rev, note: note, changes: changes})
		return err
	})
	return out, err
}

// Rollback publishes a copy of version target as a new version with note
// and resets the config working set to it, rows under their original IDs,
// if the working set is at revision ifMatch (CP-04, D5). The target must
// pass check against today's identity, which a rollback never changes
// (CP-20); history is never rewritten.
func (s *Store) Rollback(ctx context.Context, a Audit, target, ifMatch int64, note string, check Check) (ConfigVersion, error) {
	var out ConfigVersion
	err := s.inTx(ctx, func(q *db.Queries) error {
		rev, err := q.LockWorkingSet(ctx)
		if err != nil {
			return err
		}
		if err := checkVersion(rev, ifMatch); err != nil {
			return err
		}
		tv, err := q.GetConfigVersion(ctx, target)
		if err != nil {
			return notFound(err)
		}
		c, err := parseSnapshot(tv)
		if err != nil {
			return err
		}
		_, base, err := latestVersion(ctx, q)
		if err != nil {
			return err
		}
		refs, err := identityRefs(ctx, q, base, c)
		if err != nil {
			return err
		}
		changes, err := check(ctx, c, base, refs)
		if err != nil {
			return err
		}
		if rev, err = q.BumpRevision(ctx, &a.Actor.Sub); err != nil {
			return err
		}
		if err := restoreWorkingSet(ctx, q, c, rev); err != nil {
			return err
		}
		out, err = insertVersion(ctx, q, a, c, base, newVersion{kind: "rollback", rollbackOf: &target, revision: rev, note: note, changes: changes})
		return err
	})
	return out, err
}

// newVersion is what insertVersion writes besides the config.
type newVersion struct {
	kind       string // publish or rollback
	rollbackOf *int64
	revision   int64 // the working-set revision it publishes
	note       string
	changes    any
}

// insertVersion writes c as the version after base, then its audit record
// and its NOTIFY.
func insertVersion(ctx context.Context, q *db.Queries, a Audit, c *configpb.Config, base *Published, n newVersion) (ConfigVersion, error) {
	c = proto.Clone(c).(*configpb.Config)
	c.Version = 1
	var from *int64
	if base != nil {
		c.Version = base.Version + 1
		from = &base.Version
	}
	snapshot, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(c)
	if err != nil {
		return ConfigVersion{}, err
	}
	v, err := q.InsertConfigVersion(ctx, db.InsertConfigVersionParams{
		Version: c.Version, PublishedBy: a.Actor.Sub, Note: n.note, Kind: n.kind, RollbackOf: n.rollbackOf,
		WorkingSetRevision: n.revision, SchemaMajor: config.SchemaMajor, SchemaMinor: config.SchemaMinor, Snapshot: snapshot,
	})
	if err != nil {
		return v, writeError(err)
	}
	diff := map[string]any{"from_version": from, "to_version": v.Version, "changes": n.changes}
	if n.rollbackOf != nil {
		diff["rollback_of"] = *n.rollbackOf
	}
	if err := a.write(ctx, q, change{action: "config." + n.kind, entity: "config", id: v.Version, note: &n.note, config: v.Version, diff: diff}); err != nil {
		return v, err
	}
	return v, q.NotifyConfig(ctx, fmt.Sprint(v.Version))
}

// latestVersion returns the latest published version, nil before the first.
func latestVersion(ctx context.Context, q *db.Queries) (*ConfigVersion, *Published, error) {
	v, err := q.LatestConfigVersion(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	c, err := parseSnapshot(v)
	if err != nil {
		return nil, nil, err
	}
	return &v, &Published{Version: v.Version, Config: c}, nil
}

// parseSnapshot parses a published version's config with this binary's
// proto. A snapshot of another schema major, or with fields this binary
// does not know, is refused, never half read (CP-18, ADR-0037).
func parseSnapshot(v ConfigVersion) (*configpb.Config, error) {
	if v.SchemaMajor != config.SchemaMajor {
		return nil, &InvalidError{Msg: fmt.Sprintf("config version %d has schema %d.%d, which this control plane (schema %d.%d) cannot read",
			v.Version, v.SchemaMajor, v.SchemaMinor, config.SchemaMajor, config.SchemaMinor)}
	}
	c := &configpb.Config{}
	if err := protojson.Unmarshal(v.Snapshot, c); err != nil {
		return nil, &InvalidError{Msg: fmt.Sprintf("config version %d cannot be read by this control plane: %v", v.Version, err)}
	}
	return c, nil
}

// workingRows are the working set's rows by ID, for a rollback to keep
// their created_at.
type workingRows struct {
	created   map[string]map[int64]time.Time // by table
	endpoints map[int64]time.Time
}

// readWorkingSet reads every config table into one Config: entities by
// name, with their IDs, endpoints by ID. Specs are parsed strictly: a
// binary that does not know a stored field fails rather than publish
// without it.
func readWorkingSet(ctx context.Context, q *db.Queries) (*configpb.Config, workingRows, error) {
	c := &configpb.Config{}
	rows := workingRows{created: map[string]map[int64]time.Time{}, endpoints: map[int64]time.Time{}}
	ups, err := q.AllUpstreams(ctx)
	if err != nil {
		return nil, rows, err
	}
	eps, err := q.AllEndpoints(ctx)
	if err != nil {
		return nil, rows, err
	}
	byUpstream := map[int64][]*configpb.Endpoint{}
	for _, e := range eps {
		byUpstream[e.UpstreamID] = append(byUpstream[e.UpstreamID], &configpb.Endpoint{Id: e.ID, Address: e.Address, Draining: e.Draining})
		rows.endpoints[e.ID] = e.CreatedAt
	}
	for _, r := range ups {
		u := &configpb.Upstream{}
		if err := entity(row(r), u, rows.created, "upstreams"); err != nil {
			return nil, rows, err
		}
		u.Endpoints = byUpstream[r.ID]
		c.Upstreams = append(c.Upstreams, u)
	}
	if c.Routes, err = entities[configpb.Route](q.AllRoutes(ctx))(rows.created, "routes"); err != nil {
		return nil, rows, err
	}
	if c.Policies, err = entities[configpb.Policy](q.AllPolicies(ctx))(rows.created, "policies"); err != nil {
		return nil, rows, err
	}
	if c.Plans, err = entities[configpb.Plan](q.AllPlans(ctx))(rows.created, "plans"); err != nil {
		return nil, rows, err
	}
	sortByName(c)
	return c, rows, nil
}

// entities parses the rows of one config table.
func entities[M any, PM interface {
	*M
	proto.Message
}, R configRows](rs []R, err error) func(map[string]map[int64]time.Time, string) ([]PM, error) {
	return func(created map[string]map[int64]time.Time, table string) ([]PM, error) {
		if err != nil {
			return nil, err
		}
		out := make([]PM, len(rs))
		for i, r := range rs {
			out[i] = PM(new(M))
			if err := entity(row(r), out[i], created, table); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
}

// entity parses a config row into m, with its ID and name, and notes its
// created_at.
func entity(r row, m proto.Message, created map[string]map[int64]time.Time, table string) error {
	if err := protojson.Unmarshal(r.Spec, m); err != nil {
		return fmt.Errorf("%s %d: stored spec: %w", table, r.ID, err)
	}
	m2 := m.ProtoReflect()
	fields := m2.Descriptor().Fields()
	m2.Set(fields.ByName("id"), protoreflect.ValueOfInt64(r.ID))
	m2.Set(fields.ByName("name"), protoreflect.ValueOfString(r.Name))
	if created[table] == nil {
		created[table] = map[int64]time.Time{}
	}
	created[table][r.ID] = r.CreatedAt
	return nil
}

// sortByName orders each entity list of c by name, the canonical order
// (ADR-0037).
func sortByName(c *configpb.Config) {
	slices.SortFunc(c.Upstreams, func(a, b *configpb.Upstream) int { return cmp.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Routes, func(a, b *configpb.Route) int { return cmp.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Policies, func(a, b *configpb.Policy) int { return cmp.Compare(a.GetName(), b.GetName()) })
	slices.SortFunc(c.Plans, func(a, b *configpb.Plan) int { return cmp.Compare(a.GetName(), b.GetName()) })
}

// identityRefs returns the identity rows that refer to a plan or policy of
// base that c lacks. Every identity reference names an entity of the latest
// published version: identity writes check it there (ADR-0037), and every
// publish and rollback checks that what identity refers to stays. So these
// rows are the only ones c can break, and they are found by index.
func identityRefs(ctx context.Context, q *db.Queries, base *Published, c *configpb.Config) ([]IdentityRef, error) {
	if base == nil {
		return nil, nil
	}
	var plans, policies []int64
	for _, p := range base.Config.GetPlans() {
		if !slices.ContainsFunc(c.GetPlans(), func(x *configpb.Plan) bool { return x.GetId() == p.GetId() }) {
			plans = append(plans, p.GetId())
		}
	}
	for _, p := range base.Config.GetPolicies() {
		if !slices.ContainsFunc(c.GetPolicies(), func(x *configpb.Policy) bool { return x.GetId() == p.GetId() }) {
			policies = append(policies, p.GetId())
		}
	}
	var refs []IdentityRef
	if len(plans) > 0 {
		tenants, err := q.TenantsOnPlans(ctx, plans)
		if err != nil {
			return nil, err
		}
		for _, t := range tenants {
			refs = append(refs, IdentityRef{Entity: "tenant", ID: t.ID, Name: t.Name, Field: "plan_id", Ref: t.PlanID})
		}
		consumers, err := q.ConsumersOnPlans(ctx, plans)
		if err != nil {
			return nil, err
		}
		for _, cn := range consumers {
			refs = append(refs, IdentityRef{Entity: "consumer", ID: cn.ID, Name: cn.Name, Field: "plan_id", Ref: *cn.PlanID})
		}
	}
	if len(policies) > 0 {
		overrides, err := q.OverridesOnPolicies(ctx, policies)
		if err != nil {
			return nil, err
		}
		for _, o := range overrides {
			refs = append(refs, IdentityRef{Entity: "override", ID: o.ID, Field: "policy_id", Ref: *o.PolicyID})
		}
	}
	return refs, nil
}

// restoreWorkingSet replaces the config working tables with c's entities
// under their original IDs, each with version rev; rows that were already
// in the working set keep their created_at (CP-04).
func restoreWorkingSet(ctx context.Context, q *db.Queries, c *configpb.Config, rev int64) error {
	_, old, err := readWorkingSet(ctx, q)
	if err != nil {
		return err
	}
	for _, del := range []func(context.Context) error{q.DeleteAllUpstreams, q.DeleteAllRoutes, q.DeleteAllPolicies, q.DeleteAllPlans} {
		if err := del(ctx); err != nil {
			return err
		}
	}
	kept := func(table string, id int64) *time.Time {
		if t, ok := old.created[table][id]; ok {
			return &t
		}
		return nil
	}
	for _, u := range c.GetUpstreams() {
		spec, err := SpecOf(u)
		if err != nil {
			return err
		}
		if err := q.RestoreUpstream(ctx, db.RestoreUpstreamParams{ID: u.GetId(), Name: u.GetName(), Spec: spec, Version: rev, CreatedAt: kept("upstreams", u.GetId())}); err != nil {
			return err
		}
		for _, e := range u.GetEndpoints() {
			var created *time.Time
			if t, ok := old.endpoints[e.GetId()]; ok {
				created = &t
			}
			if err := q.RestoreEndpoint(ctx, db.RestoreEndpointParams{ID: e.GetId(), UpstreamID: u.GetId(), Address: e.GetAddress(), Draining: e.GetDraining(), CreatedAt: created}); err != nil {
				return err
			}
		}
	}
	for _, r := range c.GetRoutes() {
		spec, err := SpecOf(r)
		if err != nil {
			return err
		}
		if err := q.RestoreRoute(ctx, db.RestoreRouteParams{ID: r.GetId(), Name: r.GetName(), Spec: spec, Version: rev, CreatedAt: kept("routes", r.GetId())}); err != nil {
			return err
		}
	}
	for _, p := range c.GetPolicies() {
		spec, err := SpecOf(p)
		if err != nil {
			return err
		}
		if err := q.RestorePolicy(ctx, db.RestorePolicyParams{ID: p.GetId(), Name: p.GetName(), Spec: spec, Version: rev, CreatedAt: kept("policies", p.GetId())}); err != nil {
			return err
		}
	}
	for _, p := range c.GetPlans() {
		spec, err := SpecOf(p)
		if err != nil {
			return err
		}
		if err := q.RestorePlan(ctx, db.RestorePlanParams{ID: p.GetId(), Name: p.GetName(), Spec: spec, Version: rev, CreatedAt: kept("plans", p.GetId())}); err != nil {
			return err
		}
	}
	return nil
}

// SpecOf is m's protojson form without id, name and endpoints, which have
// columns: what a config row's spec holds (ADR-0036).
func SpecOf(m proto.Message) ([]byte, error) {
	m = proto.Clone(m)
	r := m.ProtoReflect()
	for _, f := range []protoreflect.Name{"id", "name", "endpoints"} {
		if fd := r.Descriptor().Fields().ByName(f); fd != nil {
			r.Clear(fd)
		}
	}
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
}

// ListVersions returns a page of published versions, newest first, with a
// version below before (0: from the newest), without their snapshots; one
// more than limit when there are more.
func (s *Store) ListVersions(ctx context.Context, before int64, limit int32) ([]ConfigVersion, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := db.New(s.pool).ListConfigVersions(ctx, db.ListConfigVersionsParams{Before: before, Lim: limit + 1})
	out := make([]ConfigVersion, len(rows))
	for i, r := range rows {
		out[i] = ConfigVersion{
			Version: r.Version, PublishedAt: r.PublishedAt, PublishedBy: r.PublishedBy, Note: r.Note, Kind: r.Kind, RollbackOf: r.RollbackOf,
			WorkingSetRevision: r.WorkingSetRevision, SchemaMajor: r.SchemaMajor, SchemaMinor: r.SchemaMinor,
		}
	}
	return out, err
}

// GetVersion returns one published version with its snapshot.
func (s *Store) GetVersion(ctx context.Context, version int64) (ConfigVersion, error) {
	v, err := db.New(s.pool).GetConfigVersion(ctx, version)
	return v, notFound(err)
}

// GetPublished returns one published version's config.
func (s *Store) GetPublished(ctx context.Context, version int64) (*Published, error) {
	v, err := s.GetVersion(ctx, version)
	if err != nil {
		return nil, err
	}
	c, err := parseSnapshot(v)
	if err != nil {
		return nil, err
	}
	return &Published{Version: v.Version, Config: c}, nil
}
