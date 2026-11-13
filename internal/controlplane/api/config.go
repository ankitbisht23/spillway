package api

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// configKind describes one config entity type (CP-01).
type configKind struct {
	kind       store.Kind
	entity     string // as in error details
	collection string // its path under BasePath
	newMsg     func() proto.Message
	check      func(proto.Message) error // the entity rules of compile (ADR-0037)
}

var (
	upstreams = configKind{store.Upstreams, "upstream", "upstreams", func() proto.Message { return &configpb.Upstream{} },
		func(m proto.Message) error { return config.CheckUpstream(m.(*configpb.Upstream)) }}
	routes = configKind{store.Routes, "route", "routes", func() proto.Message { return &configpb.Route{} },
		func(m proto.Message) error { return config.CheckRoute(m.(*configpb.Route)) }}
	policies = configKind{store.Policies, "policy", "policies", func() proto.Message { return &configpb.Policy{} },
		func(m proto.Message) error { return config.CheckPolicy(m.(*configpb.Policy)) }}
	plans = configKind{store.Plans, "plan", "plans", func() proto.Message { return &configpb.Plan{} },
		func(m proto.Message) error { return config.CheckPlan(m.(*configpb.Plan)) }}
)

const nameRule = "name must be 1-64 characters from A-Z, a-z, 0-9, '.', '_' and '-'"

// parseConfig checks a config entity's body against its entity rules and
// SEC-06, and returns it as stored (ADR-0036, ADR-0037).
func (s *Server) parseConfig(ctx context.Context, k configKind, body any) (store.ConfigInput, error) {
	m := k.newMsg()
	if err := toProto(body, m); err != nil {
		return store.ConfigInput{}, invalid(err.Error())
	}
	r := m.ProtoReflect()
	name := r.Get(r.Descriptor().Fields().ByName("name")).String()
	if !config.ValidName(name) {
		return store.ConfigInput{}, validation(k.entity, "name", nameRule)
	}
	if err := k.check(m); err != nil {
		return store.ConfigInput{}, validation(k.entity, "", err.Error())
	}
	in := store.ConfigInput{Name: name}
	if u, ok := m.(*configpb.Upstream); ok {
		for _, e := range u.GetEndpoints() {
			if err := s.allowedEndpoint(ctx, e.GetAddress()); err != nil {
				return in, validation(k.entity, "endpoints", err.Error())
			}
			in.Endpoints = append(in.Endpoints, store.EndpointInput{Address: e.GetAddress(), Draining: e.GetDraining()})
		}
	}
	var err error
	in.Spec, err = specOf(m)
	return in, err
}

// lookupTimeout bounds the resolution of an endpoint's host name (ADR-0037).
const lookupTimeout = 2 * time.Second

// allowedEndpoint checks an endpoint address against the allowed networks
// (SEC-06, ADR-0037): an IP outside them is an error, and so is a host name
// any of whose addresses is; a name that does not resolve passes, since the
// gateway checks again when it resolves and dials (ADR-0008).
func (s *Server) allowedEndpoint(ctx context.Context, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err // compile's check already caught it
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !s.allowed.Allows(ip) {
			return fmt.Errorf("endpoint %q is not in the allowed upstream networks (SEC-06)", address)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	ips, err := s.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if !s.allowed.Allows(ip) {
			return fmt.Errorf("endpoint %q resolves to %s, which is not in the allowed upstream networks (SEC-06)", address, ip)
		}
	}
	return nil
}

func listConfig[T any](ctx context.Context, s *Server, k configKind, limit *int32, cursor, name *string) ([]T, *string, error) {
	p, err := page(limit, cursor)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.store.ListConfig(ctx, k.kind, p, name)
	if err != nil {
		return nil, nil, err
	}
	rows, next := pageOf(rows, p, func(r store.ConfigRow) int64 { return r.ID })
	items := make([]T, len(rows))
	for i, r := range rows {
		if err := fromRow(r, k.newMsg(), &items[i]); err != nil {
			return nil, nil, err
		}
	}
	return items, next, nil
}

func getConfig[T any](ctx context.Context, s *Server, k configKind, id int64) (T, *string, error) {
	var out T
	r, err := s.store.GetConfig(ctx, k.kind, id)
	if err != nil {
		return out, nil, err
	}
	err = fromRow(r, k.newMsg(), &out)
	return out, etag(r.Version), err
}

// showConfig is the REST form of a config row, as T, for audit records.
func showConfig[T any](k configKind) func(any) (any, error) {
	return showAs(func(r store.ConfigRow) (T, error) {
		var out T
		err := fromRow(r, k.newMsg(), &out)
		return out, err
	})
}

func createConfig[T any](ctx context.Context, s *Server, k configKind, body any) (T, *string, *string, error) {
	var out T
	in, err := s.parseConfig(ctx, k, body)
	if err != nil {
		return out, nil, nil, err
	}
	r, err := s.store.CreateConfig(ctx, audit(ctx, showConfig[T](k)), k.kind, in)
	if err != nil {
		return out, nil, nil, err
	}
	err = fromRow(r, k.newMsg(), &out)
	return out, etag(r.Version), location(k.collection, r.ID), err
}

func updateConfig[T any](ctx context.Context, s *Server, k configKind, id int64, match string, body any) (T, *string, error) {
	var out T
	in, err := s.parseConfig(ctx, k, body)
	if err != nil {
		return out, nil, err
	}
	r, err := s.store.UpdateConfig(ctx, audit(ctx, showConfig[T](k)), k.kind, id, ifMatch(match), in)
	if err != nil {
		return out, nil, err
	}
	err = fromRow(r, k.newMsg(), &out)
	return out, etag(r.Version), err
}

// Upstreams

func (s *Server) ListUpstreams(ctx context.Context, req apiv1.ListUpstreamsRequestObject) (apiv1.ListUpstreamsResponseObject, error) {
	items, next, err := listConfig[apiv1.Upstream](ctx, s, upstreams, req.Params.Limit, req.Params.Cursor, req.Params.Name)
	if err != nil {
		return nil, err
	}
	return apiv1.ListUpstreams200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetUpstream(ctx context.Context, req apiv1.GetUpstreamRequestObject) (apiv1.GetUpstreamResponseObject, error) {
	out, tag, err := getConfig[apiv1.Upstream](ctx, s, upstreams, req.Id)
	if err != nil {
		return nil, err
	}
	return apiv1.GetUpstream200JSONResponse{Body: out, Headers: apiv1.GetUpstream200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) CreateUpstream(ctx context.Context, req apiv1.CreateUpstreamRequestObject) (apiv1.CreateUpstreamResponseObject, error) {
	out, tag, loc, err := createConfig[apiv1.Upstream](ctx, s, upstreams, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.CreateUpstream201JSONResponse{Body: out, Headers: apiv1.CreateUpstream201ResponseHeaders{ETag: tag, Location: loc}}, nil
}

func (s *Server) UpdateUpstream(ctx context.Context, req apiv1.UpdateUpstreamRequestObject) (apiv1.UpdateUpstreamResponseObject, error) {
	out, tag, err := updateConfig[apiv1.Upstream](ctx, s, upstreams, req.Id, req.Params.IfMatch, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateUpstream200JSONResponse{Body: out, Headers: apiv1.UpdateUpstream200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) DeleteUpstream(ctx context.Context, req apiv1.DeleteUpstreamRequestObject) (apiv1.DeleteUpstreamResponseObject, error) {
	if err := s.store.DeleteConfig(ctx, audit(ctx, showConfig[apiv1.Upstream](upstreams)), store.Upstreams, req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeleteUpstream204Response{}, nil
}

func (s *Server) DrainEndpoint(ctx context.Context, req apiv1.DrainEndpointRequestObject) (apiv1.DrainEndpointResponseObject, error) {
	r, err := s.store.DrainEndpoint(ctx, audit(ctx, showConfig[apiv1.Upstream](upstreams)), req.Id, req.Eid, ifMatch(req.Params.IfMatch))
	if err != nil {
		return nil, err
	}
	var out apiv1.Upstream
	if err := fromRow(r, upstreams.newMsg(), &out); err != nil {
		return nil, err
	}
	return apiv1.DrainEndpoint200JSONResponse{Body: out, Headers: apiv1.DrainEndpoint200ResponseHeaders{ETag: etag(r.Version)}}, nil
}

// Routes

func (s *Server) ListRoutes(ctx context.Context, req apiv1.ListRoutesRequestObject) (apiv1.ListRoutesResponseObject, error) {
	items, next, err := listConfig[apiv1.Route](ctx, s, routes, req.Params.Limit, req.Params.Cursor, req.Params.Name)
	if err != nil {
		return nil, err
	}
	return apiv1.ListRoutes200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetRoute(ctx context.Context, req apiv1.GetRouteRequestObject) (apiv1.GetRouteResponseObject, error) {
	out, tag, err := getConfig[apiv1.Route](ctx, s, routes, req.Id)
	if err != nil {
		return nil, err
	}
	return apiv1.GetRoute200JSONResponse{Body: out, Headers: apiv1.GetRoute200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) CreateRoute(ctx context.Context, req apiv1.CreateRouteRequestObject) (apiv1.CreateRouteResponseObject, error) {
	out, tag, loc, err := createConfig[apiv1.Route](ctx, s, routes, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.CreateRoute201JSONResponse{Body: out, Headers: apiv1.CreateRoute201ResponseHeaders{ETag: tag, Location: loc}}, nil
}

func (s *Server) UpdateRoute(ctx context.Context, req apiv1.UpdateRouteRequestObject) (apiv1.UpdateRouteResponseObject, error) {
	out, tag, err := updateConfig[apiv1.Route](ctx, s, routes, req.Id, req.Params.IfMatch, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateRoute200JSONResponse{Body: out, Headers: apiv1.UpdateRoute200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) DeleteRoute(ctx context.Context, req apiv1.DeleteRouteRequestObject) (apiv1.DeleteRouteResponseObject, error) {
	if err := s.store.DeleteConfig(ctx, audit(ctx, showConfig[apiv1.Route](routes)), store.Routes, req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeleteRoute204Response{}, nil
}

// Policies

func (s *Server) ListPolicies(ctx context.Context, req apiv1.ListPoliciesRequestObject) (apiv1.ListPoliciesResponseObject, error) {
	items, next, err := listConfig[apiv1.Policy](ctx, s, policies, req.Params.Limit, req.Params.Cursor, req.Params.Name)
	if err != nil {
		return nil, err
	}
	return apiv1.ListPolicies200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetPolicy(ctx context.Context, req apiv1.GetPolicyRequestObject) (apiv1.GetPolicyResponseObject, error) {
	out, tag, err := getConfig[apiv1.Policy](ctx, s, policies, req.Id)
	if err != nil {
		return nil, err
	}
	return apiv1.GetPolicy200JSONResponse{Body: out, Headers: apiv1.GetPolicy200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) CreatePolicy(ctx context.Context, req apiv1.CreatePolicyRequestObject) (apiv1.CreatePolicyResponseObject, error) {
	out, tag, loc, err := createConfig[apiv1.Policy](ctx, s, policies, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.CreatePolicy201JSONResponse{Body: out, Headers: apiv1.CreatePolicy201ResponseHeaders{ETag: tag, Location: loc}}, nil
}

func (s *Server) UpdatePolicy(ctx context.Context, req apiv1.UpdatePolicyRequestObject) (apiv1.UpdatePolicyResponseObject, error) {
	out, tag, err := updateConfig[apiv1.Policy](ctx, s, policies, req.Id, req.Params.IfMatch, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdatePolicy200JSONResponse{Body: out, Headers: apiv1.UpdatePolicy200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) DeletePolicy(ctx context.Context, req apiv1.DeletePolicyRequestObject) (apiv1.DeletePolicyResponseObject, error) {
	if err := s.store.DeleteConfig(ctx, audit(ctx, showConfig[apiv1.Policy](policies)), store.Policies, req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeletePolicy204Response{}, nil
}

// Plans

func (s *Server) ListPlans(ctx context.Context, req apiv1.ListPlansRequestObject) (apiv1.ListPlansResponseObject, error) {
	items, next, err := listConfig[apiv1.Plan](ctx, s, plans, req.Params.Limit, req.Params.Cursor, req.Params.Name)
	if err != nil {
		return nil, err
	}
	return apiv1.ListPlans200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetPlan(ctx context.Context, req apiv1.GetPlanRequestObject) (apiv1.GetPlanResponseObject, error) {
	out, tag, err := getConfig[apiv1.Plan](ctx, s, plans, req.Id)
	if err != nil {
		return nil, err
	}
	return apiv1.GetPlan200JSONResponse{Body: out, Headers: apiv1.GetPlan200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) CreatePlan(ctx context.Context, req apiv1.CreatePlanRequestObject) (apiv1.CreatePlanResponseObject, error) {
	out, tag, loc, err := createConfig[apiv1.Plan](ctx, s, plans, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.CreatePlan201JSONResponse{Body: out, Headers: apiv1.CreatePlan201ResponseHeaders{ETag: tag, Location: loc}}, nil
}

func (s *Server) UpdatePlan(ctx context.Context, req apiv1.UpdatePlanRequestObject) (apiv1.UpdatePlanResponseObject, error) {
	out, tag, err := updateConfig[apiv1.Plan](ctx, s, plans, req.Id, req.Params.IfMatch, req.Body)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdatePlan200JSONResponse{Body: out, Headers: apiv1.UpdatePlan200ResponseHeaders{ETag: tag}}, nil
}

func (s *Server) DeletePlan(ctx context.Context, req apiv1.DeletePlanRequestObject) (apiv1.DeletePlanResponseObject, error) {
	if err := s.store.DeleteConfig(ctx, audit(ctx, showConfig[apiv1.Plan](plans)), store.Plans, req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeletePlan204Response{}, nil
}
