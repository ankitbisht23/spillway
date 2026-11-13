package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Identity entities (CP-01, CP-20): each change applies at once with its own
// identity version (ADR-0037). Their rules here are each entity's own; the
// plans and policies they name are checked against the latest published
// config by the store.

// The REST form of identity rows, for audit records (CP-13).
var (
	showTenant     = showAs(func(t db.Tenant) (apiv1.Tenant, error) { return tenantOut(t), nil })
	showConsumer   = showAs(func(c db.Consumer) (apiv1.Consumer, error) { return consumerOut(c), nil })
	showAccessList = showAs(func(a db.AccessList) (apiv1.AccessListEntry, error) { return accessListOut(a), nil })
	showOverride   = showAs(overrideOut)
)

const (
	statusActive    = "active"
	statusSuspended = "suspended"
)

func status(s string, entity string) (string, error) {
	switch s {
	case "":
		return statusActive, nil
	case statusActive, statusSuspended:
		return s, nil
	}
	return "", validation(entity, "status", fmt.Sprintf("status %q: want active or suspended", s))
}

func timestamps(created, updated time.Time) (*time.Time, *time.Time) {
	c, u := created.UTC(), updated.UTC()
	return &c, &u
}

// Tenants

func tenantIn(b *apiv1.Tenant) (db.CreateTenantParams, error) {
	if !config.ValidName(b.Name) {
		return db.CreateTenantParams{}, validation("tenant", "name", nameRule)
	}
	if b.PlanId <= 0 {
		return db.CreateTenantParams{}, validation("tenant", "plan_id", "plan_id is required: every tenant has a plan")
	}
	st, err := status(string(b.Status), "tenant")
	return db.CreateTenantParams{Name: b.Name, PlanID: b.PlanId, Status: st}, err
}

func tenantOut(t db.Tenant) apiv1.Tenant {
	out := apiv1.Tenant{Id: &t.ID, Name: t.Name, PlanId: t.PlanID, Status: apiv1.TenantStatus(t.Status), Version: &t.Version}
	out.CreatedAt, out.UpdatedAt = timestamps(t.CreatedAt, t.UpdatedAt)
	return out
}

func (s *Server) ListTenants(ctx context.Context, req apiv1.ListTenantsRequestObject) (apiv1.ListTenantsResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListTenants(ctx, p, req.Params.Name)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(t db.Tenant) int64 { return t.ID })
	items := make([]apiv1.Tenant, len(rows))
	for i, t := range rows {
		items[i] = tenantOut(t)
	}
	return apiv1.ListTenants200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetTenant(ctx context.Context, req apiv1.GetTenantRequestObject) (apiv1.GetTenantResponseObject, error) {
	t, err := s.store.GetTenant(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return apiv1.GetTenant200JSONResponse{Body: tenantOut(t), Headers: apiv1.GetTenant200ResponseHeaders{ETag: etag(t.Version)}}, nil
}

func (s *Server) CreateTenant(ctx context.Context, req apiv1.CreateTenantRequestObject) (apiv1.CreateTenantResponseObject, error) {
	in, err := tenantIn(req.Body)
	if err != nil {
		return nil, err
	}
	t, err := s.store.CreateTenant(ctx, audit(ctx, showTenant), in)
	if err != nil {
		return nil, err
	}
	return apiv1.CreateTenant201JSONResponse{Body: tenantOut(t), Headers: apiv1.CreateTenant201ResponseHeaders{ETag: etag(t.Version), Location: location("tenants", t.ID)}}, nil
}

func (s *Server) UpdateTenant(ctx context.Context, req apiv1.UpdateTenantRequestObject) (apiv1.UpdateTenantResponseObject, error) {
	in, err := tenantIn(req.Body)
	if err != nil {
		return nil, err
	}
	t, err := s.store.UpdateTenant(ctx, audit(ctx, showTenant), req.Id, ifMatch(req.Params.IfMatch), in)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateTenant200JSONResponse{Body: tenantOut(t), Headers: apiv1.UpdateTenant200ResponseHeaders{ETag: etag(t.Version)}}, nil
}

func (s *Server) DeleteTenant(ctx context.Context, req apiv1.DeleteTenantRequestObject) (apiv1.DeleteTenantResponseObject, error) {
	if err := s.store.DeleteTenant(ctx, audit(ctx, showTenant), req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeleteTenant204Response{}, nil
}

// Consumers

func consumerIn(b *apiv1.Consumer) (db.CreateConsumerParams, error) {
	if !config.ValidName(b.Name) {
		return db.CreateConsumerParams{}, validation("consumer", "name", nameRule)
	}
	if b.TenantId <= 0 {
		return db.CreateConsumerParams{}, validation("consumer", "tenant_id", "tenant_id is required")
	}
	if b.PlanId != nil && *b.PlanId <= 0 {
		return db.CreateConsumerParams{}, validation("consumer", "plan_id", "plan_id: want a plan's ID, or null for the tenant's plan")
	}
	if b.JwtSubject != nil && *b.JwtSubject == "" {
		return db.CreateConsumerParams{}, validation("consumer", "jwt_subject", "jwt_subject: want a JWT sub, or null")
	}
	st, err := status(string(b.Status), "consumer")
	return db.CreateConsumerParams{TenantID: b.TenantId, Name: b.Name, PlanID: b.PlanId, Status: st, JwtSubject: b.JwtSubject}, err
}

func consumerOut(c db.Consumer) apiv1.Consumer {
	out := apiv1.Consumer{
		Id: &c.ID, TenantId: c.TenantID, Name: c.Name, PlanId: c.PlanID, Status: apiv1.ConsumerStatus(c.Status),
		JwtSubject: c.JwtSubject, Version: &c.Version,
	}
	out.CreatedAt, out.UpdatedAt = timestamps(c.CreatedAt, c.UpdatedAt)
	return out
}

func (s *Server) ListConsumers(ctx context.Context, req apiv1.ListConsumersRequestObject) (apiv1.ListConsumersResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListConsumers(ctx, p, req.Params.TenantId, req.Params.Name)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(c db.Consumer) int64 { return c.ID })
	items := make([]apiv1.Consumer, len(rows))
	for i, c := range rows {
		items[i] = consumerOut(c)
	}
	return apiv1.ListConsumers200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetConsumer(ctx context.Context, req apiv1.GetConsumerRequestObject) (apiv1.GetConsumerResponseObject, error) {
	c, err := s.store.GetConsumer(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return apiv1.GetConsumer200JSONResponse{Body: consumerOut(c), Headers: apiv1.GetConsumer200ResponseHeaders{ETag: etag(c.Version)}}, nil
}

func (s *Server) CreateConsumer(ctx context.Context, req apiv1.CreateConsumerRequestObject) (apiv1.CreateConsumerResponseObject, error) {
	in, err := consumerIn(req.Body)
	if err != nil {
		return nil, err
	}
	c, err := s.store.CreateConsumer(ctx, audit(ctx, showConsumer), in)
	if err != nil {
		return nil, err
	}
	return apiv1.CreateConsumer201JSONResponse{Body: consumerOut(c), Headers: apiv1.CreateConsumer201ResponseHeaders{ETag: etag(c.Version), Location: location("consumers", c.ID)}}, nil
}

func (s *Server) UpdateConsumer(ctx context.Context, req apiv1.UpdateConsumerRequestObject) (apiv1.UpdateConsumerResponseObject, error) {
	in, err := consumerIn(req.Body)
	if err != nil {
		return nil, err
	}
	c, err := s.store.UpdateConsumer(ctx, audit(ctx, showConsumer), req.Id, ifMatch(req.Params.IfMatch), in)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateConsumer200JSONResponse{Body: consumerOut(c), Headers: apiv1.UpdateConsumer200ResponseHeaders{ETag: etag(c.Version)}}, nil
}

func (s *Server) DeleteConsumer(ctx context.Context, req apiv1.DeleteConsumerRequestObject) (apiv1.DeleteConsumerResponseObject, error) {
	if err := s.store.DeleteConsumer(ctx, audit(ctx, showConsumer), req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeleteConsumer204Response{}, nil
}

// ListConsumerKeys lists a consumer's keys: prefix only, never the key or
// its hash (ADR-0036). Issue, rotate and revoke come with keys-identity.
func (s *Server) ListConsumerKeys(ctx context.Context, req apiv1.ListConsumerKeysRequestObject) (apiv1.ListConsumerKeysResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListConsumerKeys(ctx, req.Id, p)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(k db.ListConsumerKeysRow) int64 { return k.ID })
	items := make([]apiv1.ApiKey, len(rows))
	for i, k := range rows {
		items[i] = apiv1.ApiKey{
			Id: k.ID, ConsumerId: k.ConsumerID, Prefix: k.Prefix, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt,
			RotatedFrom: k.RotatedFrom, Version: k.Version, CreatedAt: k.CreatedAt.UTC(), UpdatedAt: k.UpdatedAt.UTC(),
		}
	}
	return apiv1.ListConsumerKeys200JSONResponse{Items: items, NextCursor: next}, nil
}

// Access lists (RL-09)

func accessListIn(b *apiv1.AccessListEntry) (db.CreateAccessListParams, error) {
	in := db.CreateAccessListParams{Action: string(b.Action), SubjectType: string(b.SubjectType), ApiKeyID: b.ApiKeyId, ConsumerID: b.ConsumerId, Note: b.Note}
	if in.Action != "allow" && in.Action != "deny" {
		return in, validation("access_list", "action", fmt.Sprintf("action %q: want allow or deny", in.Action))
	}
	set := map[string]bool{"key": b.ApiKeyId != nil, "consumer": b.ConsumerId != nil, "cidr": b.Cidr != nil}
	field := map[string]string{"key": "api_key_id", "consumer": "consumer_id", "cidr": "cidr"}
	if _, ok := set[in.SubjectType]; !ok {
		return in, validation("access_list", "subject_type", fmt.Sprintf("subject_type %q: want key, consumer or cidr", in.SubjectType))
	}
	for t, isSet := range set {
		if isSet != (t == in.SubjectType) {
			return in, validation("access_list", field[in.SubjectType], fmt.Sprintf("subject_type %s: set %s and none of the other subjects", in.SubjectType, field[in.SubjectType]))
		}
	}
	if b.Cidr != nil {
		p, err := netip.ParsePrefix(*b.Cidr)
		if err != nil {
			return in, validation("access_list", "cidr", fmt.Sprintf("cidr %q: want a CIDR such as 203.0.113.0/24", *b.Cidr))
		}
		if p != p.Masked() {
			return in, validation("access_list", "cidr", fmt.Sprintf("cidr %q has bits set past its prefix; want %s", *b.Cidr, p.Masked()))
		}
		in.Cidr = &p
	}
	return in, nil
}

func accessListOut(a db.AccessList) apiv1.AccessListEntry {
	out := apiv1.AccessListEntry{
		Id: &a.ID, Action: apiv1.AccessListEntryAction(a.Action), SubjectType: apiv1.AccessListEntrySubjectType(a.SubjectType),
		ApiKeyId: a.ApiKeyID, ConsumerId: a.ConsumerID, Note: a.Note, Version: &a.Version,
	}
	if a.Cidr != nil {
		c := a.Cidr.String()
		out.Cidr = &c
	}
	out.CreatedAt, out.UpdatedAt = timestamps(a.CreatedAt, a.UpdatedAt)
	return out
}

func (s *Server) ListAccessLists(ctx context.Context, req apiv1.ListAccessListsRequestObject) (apiv1.ListAccessListsResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	action, subject := (*string)(req.Params.Action), (*string)(req.Params.SubjectType)
	if action != nil && *action != "allow" && *action != "deny" {
		return nil, invalid("action: want allow or deny")
	}
	if subject != nil && *subject != "key" && *subject != "consumer" && *subject != "cidr" {
		return nil, invalid("subject_type: want key, consumer or cidr")
	}
	rows, err := s.store.ListAccessLists(ctx, p, action, subject)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(a db.AccessList) int64 { return a.ID })
	items := make([]apiv1.AccessListEntry, len(rows))
	for i, a := range rows {
		items[i] = accessListOut(a)
	}
	return apiv1.ListAccessLists200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetAccessList(ctx context.Context, req apiv1.GetAccessListRequestObject) (apiv1.GetAccessListResponseObject, error) {
	a, err := s.store.GetAccessList(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return apiv1.GetAccessList200JSONResponse{Body: accessListOut(a), Headers: apiv1.GetAccessList200ResponseHeaders{ETag: etag(a.Version)}}, nil
}

func (s *Server) CreateAccessList(ctx context.Context, req apiv1.CreateAccessListRequestObject) (apiv1.CreateAccessListResponseObject, error) {
	in, err := accessListIn(req.Body)
	if err != nil {
		return nil, err
	}
	a, err := s.store.CreateAccessList(ctx, audit(ctx, showAccessList), in)
	if err != nil {
		return nil, err
	}
	return apiv1.CreateAccessList201JSONResponse{Body: accessListOut(a), Headers: apiv1.CreateAccessList201ResponseHeaders{ETag: etag(a.Version), Location: location("access-lists", a.ID)}}, nil
}

func (s *Server) UpdateAccessList(ctx context.Context, req apiv1.UpdateAccessListRequestObject) (apiv1.UpdateAccessListResponseObject, error) {
	in, err := accessListIn(req.Body)
	if err != nil {
		return nil, err
	}
	a, err := s.store.UpdateAccessList(ctx, audit(ctx, showAccessList), req.Id, ifMatch(req.Params.IfMatch), in)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateAccessList200JSONResponse{Body: accessListOut(a), Headers: apiv1.UpdateAccessList200ResponseHeaders{ETag: etag(a.Version)}}, nil
}

func (s *Server) DeleteAccessList(ctx context.Context, req apiv1.DeleteAccessListRequestObject) (apiv1.DeleteAccessListResponseObject, error) {
	if err := s.store.DeleteAccessList(ctx, audit(ctx, showAccessList), req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeleteAccessList204Response{}, nil
}

// Overrides and burst grants (RL-08). The rules that depend on the policy
// (its level, key template and algorithm, ADR-0041) come with the plans task.

func overrideIn(b *apiv1.Override) (db.CreateOverrideParams, error) {
	in := db.CreateOverrideParams{ConsumerID: b.ConsumerId, PolicyID: &b.PolicyId, StartsAt: b.StartsAt, EndsAt: b.EndsAt, Note: b.Note}
	if b.ConsumerId <= 0 {
		return in, validation("override", "consumer_id", "consumer_id is required")
	}
	if b.PolicyId <= 0 {
		return in, validation("override", "policy_id", "policy_id is required")
	}
	if len(b.Limits) == 0 {
		return in, validation("override", "limits", "limits: at least one is required")
	}
	limits := make([]*configpb.Limit, len(b.Limits))
	for i, l := range b.Limits {
		limits[i] = &configpb.Limit{}
		if err := toProto(l, limits[i]); err != nil {
			return in, invalid(err.Error())
		}
		if limits[i].GetRate() < 1 {
			return in, validation("override", fmt.Sprintf("limits[%d].rate", i), fmt.Sprintf("limits[%d].rate: must be at least 1", i))
		}
	}
	if b.StartsAt != nil && b.EndsAt != nil && !b.EndsAt.After(*b.StartsAt) {
		return in, validation("override", "ends_at", "ends_at must be after starts_at")
	}
	var err error
	in.Limits, err = limitsJSON(limits)
	return in, err
}

func overrideOut(o db.Override) (apiv1.Override, error) {
	out := apiv1.Override{Id: &o.ID, ConsumerId: o.ConsumerID, StartsAt: o.StartsAt, EndsAt: o.EndsAt, Note: o.Note, Version: &o.Version, Limits: []apiv1.Limit{}}
	if o.PolicyID != nil {
		out.PolicyId = *o.PolicyID
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(o.Limits, &raw); err != nil {
		return out, fmt.Errorf("stored limits of override %d: %w", o.ID, err)
	}
	for _, r := range raw {
		l := &configpb.Limit{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(r, l); err != nil {
			return out, fmt.Errorf("stored limits of override %d: %w", o.ID, err)
		}
		b, err := json.Marshal(jsonFields(l.ProtoReflect()))
		if err != nil {
			return out, err
		}
		var limit apiv1.Limit
		if err := json.Unmarshal(b, &limit); err != nil {
			return out, err
		}
		out.Limits = append(out.Limits, limit)
	}
	if o.StartsAt != nil {
		t := o.StartsAt.UTC()
		out.StartsAt = &t
	}
	if o.EndsAt != nil {
		t := o.EndsAt.UTC()
		out.EndsAt = &t
	}
	out.CreatedAt, out.UpdatedAt = timestamps(o.CreatedAt, o.UpdatedAt)
	return out, nil
}

func (s *Server) ListOverrides(ctx context.Context, req apiv1.ListOverridesRequestObject) (apiv1.ListOverridesResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListOverrides(ctx, p, req.Params.ConsumerId, req.Params.PolicyId)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(o db.Override) int64 { return o.ID })
	items := make([]apiv1.Override, len(rows))
	for i, o := range rows {
		if items[i], err = overrideOut(o); err != nil {
			return nil, err
		}
	}
	return apiv1.ListOverrides200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetOverride(ctx context.Context, req apiv1.GetOverrideRequestObject) (apiv1.GetOverrideResponseObject, error) {
	o, err := s.store.GetOverride(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out, err := overrideOut(o)
	if err != nil {
		return nil, err
	}
	return apiv1.GetOverride200JSONResponse{Body: out, Headers: apiv1.GetOverride200ResponseHeaders{ETag: etag(o.Version)}}, nil
}

func (s *Server) CreateOverride(ctx context.Context, req apiv1.CreateOverrideRequestObject) (apiv1.CreateOverrideResponseObject, error) {
	in, err := overrideIn(req.Body)
	if err != nil {
		return nil, err
	}
	o, err := s.store.CreateOverride(ctx, audit(ctx, showOverride), in)
	if err != nil {
		return nil, err
	}
	out, err := overrideOut(o)
	if err != nil {
		return nil, err
	}
	return apiv1.CreateOverride201JSONResponse{Body: out, Headers: apiv1.CreateOverride201ResponseHeaders{ETag: etag(o.Version), Location: location("overrides", o.ID)}}, nil
}

func (s *Server) UpdateOverride(ctx context.Context, req apiv1.UpdateOverrideRequestObject) (apiv1.UpdateOverrideResponseObject, error) {
	in, err := overrideIn(req.Body)
	if err != nil {
		return nil, err
	}
	o, err := s.store.UpdateOverride(ctx, audit(ctx, showOverride), req.Id, ifMatch(req.Params.IfMatch), in)
	if err != nil {
		return nil, err
	}
	out, err := overrideOut(o)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateOverride200JSONResponse{Body: out, Headers: apiv1.UpdateOverride200ResponseHeaders{ETag: etag(o.Version)}}, nil
}

func (s *Server) DeleteOverride(ctx context.Context, req apiv1.DeleteOverrideRequestObject) (apiv1.DeleteOverrideResponseObject, error) {
	if err := s.store.DeleteOverride(ctx, audit(ctx, showOverride), req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return apiv1.DeleteOverride204Response{}, nil
}
