package store

import (
	"context"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
)

// Kind is a config entity type (CP-01).
type Kind int

// The config entity types.
const (
	Upstreams Kind = iota
	Routes
	Policies
	Plans
)

// ConfigRow is a config entity: its spec is the protojson form of its
// config.proto message without id, name and endpoints, which have columns
// (ADR-0036).
type ConfigRow struct {
	ID        int64
	Name      string
	Spec      []byte
	Version   int64 // the working-set revision of its last change
	CreatedAt time.Time
	UpdatedAt time.Time
	Endpoints []db.Endpoint // upstreams only, by ID
}

// ConfigInput is a config entity as written.
type ConfigInput struct {
	Name      string
	Spec      []byte
	Endpoints []EndpointInput // upstreams only
}

// EndpointInput is an upstream endpoint as written. A PUT keeps the ID of
// each address the upstream already had.
type EndpointInput struct {
	Address  string
	Draining bool
}

// row is the shape the four config tables share, so sqlc's row types convert
// to it.
type row = struct {
	ID        int64
	Name      string
	Spec      []byte
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type configRows interface {
	db.Upstream | db.Route | db.Policy | db.Plan
}

func toRow[T configRows](r T, err error) (ConfigRow, error) {
	x := row(r)
	return ConfigRow{ID: x.ID, Name: x.Name, Spec: x.Spec, Version: x.Version, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}, err
}

func toRows[T configRows](rs []T, err error) ([]ConfigRow, error) {
	out := make([]ConfigRow, len(rs))
	for i, r := range rs {
		out[i], _ = toRow(r, nil)
	}
	return out, err
}

// table is one config table's queries.
type table struct {
	list   func(ctx context.Context, q *db.Queries, after int64, name *string, lim int32) ([]ConfigRow, error)
	get    func(ctx context.Context, q *db.Queries, id int64) (ConfigRow, error)
	create func(ctx context.Context, q *db.Queries, name string, spec []byte, version int64) (ConfigRow, error)
	update func(ctx context.Context, q *db.Queries, id int64, name string, spec []byte, version int64) (ConfigRow, error)
	delete func(ctx context.Context, q *db.Queries, id int64) (int64, error)
}

var tables = map[Kind]table{
	Upstreams: {
		list: func(ctx context.Context, q *db.Queries, after int64, name *string, lim int32) ([]ConfigRow, error) {
			return toRows(q.ListUpstreams(ctx, db.ListUpstreamsParams{After: after, Name: name, Lim: lim}))
		},
		get: func(ctx context.Context, q *db.Queries, id int64) (ConfigRow, error) {
			return toRow(q.GetUpstream(ctx, id))
		},
		create: func(ctx context.Context, q *db.Queries, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.CreateUpstream(ctx, db.CreateUpstreamParams{Name: name, Spec: spec, Version: version}))
		},
		update: func(ctx context.Context, q *db.Queries, id int64, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.UpdateUpstream(ctx, db.UpdateUpstreamParams{ID: id, Name: name, Spec: spec, Version: version}))
		},
		delete: func(ctx context.Context, q *db.Queries, id int64) (int64, error) { return q.DeleteUpstream(ctx, id) },
	},
	Routes: {
		list: func(ctx context.Context, q *db.Queries, after int64, name *string, lim int32) ([]ConfigRow, error) {
			return toRows(q.ListRoutes(ctx, db.ListRoutesParams{After: after, Name: name, Lim: lim}))
		},
		get: func(ctx context.Context, q *db.Queries, id int64) (ConfigRow, error) {
			return toRow(q.GetRoute(ctx, id))
		},
		create: func(ctx context.Context, q *db.Queries, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.CreateRoute(ctx, db.CreateRouteParams{Name: name, Spec: spec, Version: version}))
		},
		update: func(ctx context.Context, q *db.Queries, id int64, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.UpdateRoute(ctx, db.UpdateRouteParams{ID: id, Name: name, Spec: spec, Version: version}))
		},
		delete: func(ctx context.Context, q *db.Queries, id int64) (int64, error) { return q.DeleteRoute(ctx, id) },
	},
	Policies: {
		list: func(ctx context.Context, q *db.Queries, after int64, name *string, lim int32) ([]ConfigRow, error) {
			return toRows(q.ListPolicies(ctx, db.ListPoliciesParams{After: after, Name: name, Lim: lim}))
		},
		get: func(ctx context.Context, q *db.Queries, id int64) (ConfigRow, error) {
			return toRow(q.GetPolicy(ctx, id))
		},
		create: func(ctx context.Context, q *db.Queries, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.CreatePolicy(ctx, db.CreatePolicyParams{Name: name, Spec: spec, Version: version}))
		},
		update: func(ctx context.Context, q *db.Queries, id int64, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.UpdatePolicy(ctx, db.UpdatePolicyParams{ID: id, Name: name, Spec: spec, Version: version}))
		},
		delete: func(ctx context.Context, q *db.Queries, id int64) (int64, error) { return q.DeletePolicy(ctx, id) },
	},
	Plans: {
		list: func(ctx context.Context, q *db.Queries, after int64, name *string, lim int32) ([]ConfigRow, error) {
			return toRows(q.ListPlans(ctx, db.ListPlansParams{After: after, Name: name, Lim: lim}))
		},
		get: func(ctx context.Context, q *db.Queries, id int64) (ConfigRow, error) {
			return toRow(q.GetPlan(ctx, id))
		},
		create: func(ctx context.Context, q *db.Queries, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.CreatePlan(ctx, db.CreatePlanParams{Name: name, Spec: spec, Version: version}))
		},
		update: func(ctx context.Context, q *db.Queries, id int64, name string, spec []byte, version int64) (ConfigRow, error) {
			return toRow(q.UpdatePlan(ctx, db.UpdatePlanParams{ID: id, Name: name, Spec: spec, Version: version}))
		},
		delete: func(ctx context.Context, q *db.Queries, id int64) (int64, error) { return q.DeletePlan(ctx, id) },
	},
}

// ListConfig returns a page of kind's entities by ID, those named name if
// it is not nil.
func (s *Store) ListConfig(ctx context.Context, kind Kind, p Page, name *string) ([]ConfigRow, error) {
	q := db.New(s.pool)
	rows, err := tables[kind].list(ctx, q, p.After, name, p.lim())
	if err != nil {
		return nil, err
	}
	return rows, s.withEndpoints(ctx, q, kind, rows)
}

// GetConfig returns one entity of kind.
func (s *Store) GetConfig(ctx context.Context, kind Kind, id int64) (ConfigRow, error) {
	q := db.New(s.pool)
	r, err := tables[kind].get(ctx, q, id)
	if err != nil {
		return r, notFound(err)
	}
	rows := []ConfigRow{r}
	err = s.withEndpoints(ctx, q, kind, rows)
	return rows[0], err
}

// withEndpoints fills in the endpoints of upstream rows.
func (s *Store) withEndpoints(ctx context.Context, q *db.Queries, kind Kind, rows []ConfigRow) error {
	if kind != Upstreams || len(rows) == 0 {
		return nil
	}
	ids := make([]int64, len(rows))
	byID := make(map[int64]*ConfigRow, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
		byID[rows[i].ID] = &rows[i]
	}
	eps, err := q.ListEndpoints(ctx, ids)
	if err != nil {
		return err
	}
	for _, e := range eps {
		r := byID[e.UpstreamID]
		r.Endpoints = append(r.Endpoints, e)
	}
	return nil
}

// CreateConfig adds an entity of kind to the working set.
func (s *Store) CreateConfig(ctx context.Context, kind Kind, in ConfigInput) (ConfigRow, error) {
	var out ConfigRow
	err := s.inConfigTx(ctx, func(q *db.Queries, rev int64) error {
		r, err := tables[kind].create(ctx, q, in.Name, in.Spec, rev)
		if err != nil {
			return writeError(err)
		}
		for _, e := range in.Endpoints {
			ep, err := q.CreateEndpoint(ctx, db.CreateEndpointParams{UpstreamID: r.ID, Address: e.Address, Draining: e.Draining})
			if err != nil {
				return writeError(err)
			}
			r.Endpoints = append(r.Endpoints, ep)
		}
		out = r
		return nil
	})
	return out, err
}

// UpdateConfig replaces an entity of kind in the working set if its version
// is ifMatch (CP-14). An upstream's endpoints are matched by address: those
// kept keep their IDs.
func (s *Store) UpdateConfig(ctx context.Context, kind Kind, id, ifMatch int64, in ConfigInput) (ConfigRow, error) {
	var out ConfigRow
	err := s.inConfigTx(ctx, func(q *db.Queries, rev int64) error {
		cur, err := s.currentConfig(ctx, q, kind, id, ifMatch)
		if err != nil {
			return err
		}
		r, err := tables[kind].update(ctx, q, id, in.Name, in.Spec, rev)
		if err != nil {
			return writeError(err)
		}
		if kind == Upstreams {
			if r.Endpoints, err = replaceEndpoints(ctx, q, id, cur.Endpoints, in.Endpoints); err != nil {
				return err
			}
		}
		out = r
		return nil
	})
	return out, err
}

// currentConfig returns an entity of kind, with its endpoints, if its version
// is ifMatch.
func (s *Store) currentConfig(ctx context.Context, q *db.Queries, kind Kind, id, ifMatch int64) (ConfigRow, error) {
	cur, err := tables[kind].get(ctx, q, id)
	if err != nil {
		return cur, notFound(err)
	}
	if err := checkVersion(cur.Version, ifMatch); err != nil {
		return cur, err
	}
	rows := []ConfigRow{cur}
	err = s.withEndpoints(ctx, q, kind, rows)
	return rows[0], err
}

func replaceEndpoints(ctx context.Context, q *db.Queries, upstreamID int64, cur []db.Endpoint, in []EndpointInput) ([]db.Endpoint, error) {
	byAddr := make(map[string]db.Endpoint, len(cur))
	for _, e := range cur {
		byAddr[e.Address] = e
	}
	var out []db.Endpoint
	for _, e := range in {
		old, ok := byAddr[e.Address]
		delete(byAddr, e.Address)
		switch {
		case !ok:
			ep, err := q.CreateEndpoint(ctx, db.CreateEndpointParams{UpstreamID: upstreamID, Address: e.Address, Draining: e.Draining})
			if err != nil {
				return nil, writeError(err)
			}
			out = append(out, ep)
		case old.Draining != e.Draining:
			if err := q.SetEndpointDraining(ctx, db.SetEndpointDrainingParams{ID: old.ID, Draining: e.Draining}); err != nil {
				return nil, err
			}
			old.Draining = e.Draining
			out = append(out, old)
		default:
			out = append(out, old)
		}
	}
	for _, e := range byAddr {
		if err := q.DeleteEndpoint(ctx, e.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteConfig removes an entity of kind from the working set if its version
// is ifMatch (CP-14).
func (s *Store) DeleteConfig(ctx context.Context, kind Kind, id, ifMatch int64) error {
	return s.inConfigTx(ctx, func(q *db.Queries, _ int64) error {
		cur, err := tables[kind].get(ctx, q, id)
		if err != nil {
			return notFound(err)
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		_, err = tables[kind].delete(ctx, q, id)
		return err
	})
}

// DrainEndpoint marks an endpoint of an upstream draining in the working set
// if the upstream's version is ifMatch; the upstream gets a new version
// (ADR-0036).
func (s *Store) DrainEndpoint(ctx context.Context, upstreamID, endpointID, ifMatch int64) (ConfigRow, error) {
	var out ConfigRow
	err := s.inConfigTx(ctx, func(q *db.Queries, rev int64) error {
		cur, err := s.currentConfig(ctx, q, Upstreams, upstreamID, ifMatch)
		if err != nil {
			return err
		}
		found := false
		for i, e := range cur.Endpoints {
			if e.ID == endpointID {
				found = true
				cur.Endpoints[i].Draining = true
			}
		}
		if !found {
			return ErrNotFound
		}
		if err := q.SetEndpointDraining(ctx, db.SetEndpointDrainingParams{ID: endpointID, Draining: true}); err != nil {
			return err
		}
		r, err := tables[Upstreams].update(ctx, q, upstreamID, cur.Name, cur.Spec, rev)
		if err != nil {
			return err
		}
		r.Endpoints = cur.Endpoints
		out = r
		return nil
	})
	return out, err
}
