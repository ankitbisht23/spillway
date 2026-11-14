package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Import replaces the config working set (CP-05, ADR-0037, ADR-0044).

// ImportDiff returns the changes from before to after for the import's
// audit record, and whether there are any.
type ImportDiff func(before, after *configpb.Config) (changes any, changed bool)

// ImportConfig replaces the config working set with c, if it is at revision
// ifMatch, in one transaction under the working set's row lock. Each entity
// of c is matched to a row of its type by its ID when it has one (an ID that
// does not exist is an InvalidError), otherwise by name, otherwise created;
// rows matched by nothing are deleted. An upstream's endpoints are matched
// by address, as a PUT does; an endpoint ID, when given, must be the one its
// address has. Only rows that change get the new revision as their version.
// c's entities must already pass their entity rules, with names and IDs
// unique per type. An import that changes nothing writes nothing and
// returns the current revision; otherwise the audit record holds diff's
// changes and note. c's ratelimit section replaces the settings, and the
// record holds them before and after when they change (ADR-0045).
func (s *Store) ImportConfig(ctx context.Context, a Audit, ifMatch int64, c *configpb.Config, note *string, diff ImportDiff) (int64, error) {
	var rev int64
	err := s.inTx(ctx, func(q *db.Queries) error {
		var err error
		if rev, err = q.LockWorkingSet(ctx); err != nil {
			return err
		}
		if err := checkVersion(rev, ifMatch); err != nil {
			return err
		}
		before, _, err := readWorkingSet(ctx, q)
		if err != nil {
			return err
		}
		plans := make([]importPlan, len(importKinds))
		for i, k := range importKinds {
			if plans[i], err = planImport(k, listOf(c, k.kind), listOf(before, k.kind)); err != nil {
				return err
			}
		}
		from := rev
		if rev, err = q.BumpRevision(ctx, &a.Actor.Sub); err != nil {
			return err
		}
		for i, k := range importKinds {
			if err := plans[i].apply(ctx, q, k, rev); err != nil {
				return err
			}
		}
		if err := writeSettings(ctx, q, before.GetRatelimit(), settingsOf(c), rev); err != nil {
			return err
		}
		after, _, err := readWorkingSet(ctx, q)
		if err != nil {
			return err
		}
		changes, changed := diff(before, after)
		settingsChanged := !proto.Equal(before.GetRatelimit(), after.GetRatelimit())
		if !changed && !settingsChanged {
			rev = from
			return errUnchanged
		}
		d := map[string]any{"from_revision": from, "to_revision": rev, "changes": changes}
		if settingsChanged {
			d["settings"] = map[string]any{"before": settingsJSON(before.GetRatelimit()), "after": settingsJSON(after.GetRatelimit())}
		}
		return a.write(ctx, q, change{action: "config.import", entity: "config", id: rev, note: note, diff: d})
	})
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	return rev, err
}

// errUnchanged rolls back an import that changed nothing, so it leaves the
// revision, every version and the audit log as they were.
var errUnchanged = errors.New("the import changes nothing")

// importKind is a config entity type as an import sees it.
type importKind struct {
	kind  Kind
	field string // the Config field, as in a YAML file
}

var importKinds = []importKind{{Upstreams, "upstreams"}, {Routes, "routes"}, {Policies, "policies"}, {Plans, "plans"}}

func listOf(c *configpb.Config, k Kind) []proto.Message {
	switch k {
	case Upstreams:
		return messages(c.GetUpstreams())
	case Routes:
		return messages(c.GetRoutes())
	case Policies:
		return messages(c.GetPolicies())
	}
	return messages(c.GetPlans())
}

func messages[M proto.Message](l []M) []proto.Message {
	out := make([]proto.Message, len(l))
	for i, m := range l {
		out[i] = m
	}
	return out
}

// importPlan is what an import writes to one config table.
type importPlan struct {
	deletes []int64
	renames []importRow // matched rows whose name changes
	updates []importRow // matched rows that change
	creates []importRow
}

// importRow is one entity of the file, with the row it matched.
type importRow struct {
	m   proto.Message // the entity as imported, with the row's ID
	cur proto.Message // the row's entity; nil when created
	eps []db.Endpoint // the row's endpoints, for an upstream
}

// planImport matches the file's entities of one type to the working set's.
func planImport(k importKind, file, cur []proto.Message) (importPlan, error) {
	entity := tables[k.kind].entity
	byID := make(map[int64]proto.Message, len(cur))
	byName := make(map[string]int64, len(cur))
	for _, m := range cur {
		byID[fieldOf(m, "id").Int()] = m
		byName[fieldOf(m, "name").String()] = fieldOf(m, "id").Int()
	}
	matched := make([]proto.Message, len(file))
	claimed := map[int64]bool{}
	for i, m := range file {
		if id := fieldOf(m, "id").Int(); id != 0 {
			if matched[i] = byID[id]; matched[i] == nil {
				return importPlan{}, &InvalidError{Path: fmt.Sprintf("%s[%d].id", k.field, i), Msg: fmt.Sprintf(
					"%s %q: id %d does not exist; ids are assigned by the control plane, so leave it out to create the %s", entity, fieldOf(m, "name").String(), id, entity)}
			}
			claimed[id] = true
		}
	}
	for i, m := range file {
		if id, ok := byName[fieldOf(m, "name").String()]; ok && fieldOf(m, "id").Int() == 0 && !claimed[id] {
			matched[i] = byID[id]
			claimed[id] = true
		}
	}
	var p importPlan
	for _, m := range cur {
		if id := fieldOf(m, "id").Int(); !claimed[id] {
			p.deletes = append(p.deletes, id)
		}
	}
	for i, m := range file {
		r := importRow{m: proto.Clone(m), cur: matched[i]}
		var id int64
		if r.cur != nil {
			id = fieldOf(r.cur, "id").Int()
		}
		setField(r.m, "id", protoreflect.ValueOfInt64(id))
		if u, ok := r.m.(*configpb.Upstream); ok {
			var err error
			if r.eps, err = matchEndpoints(u, r.cur, fmt.Sprintf("%s[%d]", k.field, i)); err != nil {
				return p, err
			}
		}
		switch {
		case r.cur == nil:
			p.creates = append(p.creates, r)
		case !proto.Equal(r.cur, r.m):
			if fieldOf(r.cur, "name").String() != fieldOf(r.m, "name").String() {
				p.renames = append(p.renames, r)
			}
			p.updates = append(p.updates, r)
		}
	}
	return p, nil
}

// matchEndpoints gives u's endpoints the IDs their addresses have in cur,
// the upstream u matched (nil for a new one), and orders them as the
// working set does, by ID, new ones last; it returns cur's endpoints. An
// endpoint ID that is not its address's is an InvalidError.
func matchEndpoints(u *configpb.Upstream, cur proto.Message, path string) ([]db.Endpoint, error) {
	var eps []db.Endpoint
	ids := map[string]int64{}
	if cur != nil {
		for _, e := range cur.(*configpb.Upstream).GetEndpoints() {
			eps = append(eps, db.Endpoint{ID: e.GetId(), UpstreamID: fieldOf(cur, "id").Int(), Address: e.GetAddress(), Draining: e.GetDraining()})
			ids[e.GetAddress()] = e.GetId()
		}
	}
	for j, e := range u.GetEndpoints() {
		id := ids[e.GetAddress()]
		if e.GetId() != 0 && e.GetId() != id {
			return nil, &InvalidError{Path: fmt.Sprintf("%s.endpoints[%d].id", path, j), Msg: fmt.Sprintf(
				"upstream %q: endpoint id %d is not the id of %s in this upstream; endpoints are matched by address, so leave the id out", u.GetName(), e.GetId(), e.GetAddress())}
		}
		e.Id = id
	}
	slices.SortStableFunc(u.Endpoints, func(a, b *configpb.Endpoint) int {
		return cmp.Compare(endpointRank(a), endpointRank(b))
	})
	return eps, nil
}

// apply writes p to k's table at revision rev: deletes first, then renames
// through a temporary name, so that names can move between rows, then
// updates and creates.
func (p importPlan) apply(ctx context.Context, q *db.Queries, k importKind, rev int64) error {
	t := tables[k.kind]
	for _, id := range p.deletes {
		if _, err := t.delete(ctx, q, id); err != nil {
			return err
		}
	}
	for _, r := range p.renames {
		id := fieldOf(r.m, "id").Int()
		spec, err := SpecOf(r.cur)
		if err != nil {
			return err
		}
		if _, err := t.update(ctx, q, id, fmt.Sprintf(".import.%d", id), spec, rev); err != nil {
			return writeError(err)
		}
	}
	for _, r := range p.updates {
		in, err := importInput(r.m)
		if err != nil {
			return err
		}
		id := fieldOf(r.m, "id").Int()
		if _, err := t.update(ctx, q, id, in.Name, in.Spec, rev); err != nil {
			return writeError(err)
		}
		if k.kind == Upstreams {
			if _, err := replaceEndpoints(ctx, q, id, r.eps, in.Endpoints); err != nil {
				return err
			}
		}
	}
	for _, r := range p.creates {
		in, err := importInput(r.m)
		if err != nil {
			return err
		}
		row, err := t.create(ctx, q, in.Name, in.Spec, rev)
		if err != nil {
			return writeError(err)
		}
		for _, e := range in.Endpoints {
			if _, err := q.CreateEndpoint(ctx, db.CreateEndpointParams{UpstreamID: row.ID, Address: e.Address, Draining: e.Draining}); err != nil {
				return writeError(err)
			}
		}
	}
	return nil
}

// importInput is an imported entity as written.
func importInput(m proto.Message) (ConfigInput, error) {
	in := ConfigInput{Name: fieldOf(m, "name").String()}
	if u, ok := m.(*configpb.Upstream); ok {
		for _, e := range u.GetEndpoints() {
			in.Endpoints = append(in.Endpoints, EndpointInput{Address: e.GetAddress(), Draining: e.GetDraining()})
		}
	}
	var err error
	in.Spec, err = SpecOf(m)
	return in, err
}

func fieldOf(m proto.Message, field protoreflect.Name) protoreflect.Value {
	r := m.ProtoReflect()
	return r.Get(r.Descriptor().Fields().ByName(field))
}

func setField(m proto.Message, field protoreflect.Name, v protoreflect.Value) {
	r := m.ProtoReflect()
	r.Set(r.Descriptor().Fields().ByName(field), v)
}

// endpointRank orders endpoints as the working set does: by ID, new ones
// (ID 0) last.
func endpointRank(e *configpb.Endpoint) int64 {
	if e.GetId() == 0 {
		return math.MaxInt64
	}
	return e.GetId()
}
