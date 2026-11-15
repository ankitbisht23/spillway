package api

import (
	"context"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/heavyhitters"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// Heavy hitters, from heartbeats' top_keys (RL-21, ADR-0041).

// The time range of GET /heavy-hitters (ADR-0041): 15 minutes by default,
// at most 7 days, the windows' retention.
const (
	defaultHeavyHitterRange = 15 * time.Minute
	maxHeavyHitterRange     = 7 * 24 * time.Hour
)

// ListHeavyHitters answers the top throttled keys of the windows that start
// in [since, until), most throttled first, at most limit of them and no
// next page.
func (s *Server) ListHeavyHitters(ctx context.Context, req apiv1.ListHeavyHittersRequestObject) (apiv1.ListHeavyHittersResponseObject, error) {
	p, err := page(req.Params.Limit, nil)
	if err != nil {
		return nil, err
	}
	f, err := heavyHitterRange(req.Params.Since, req.Params.Until)
	if err != nil {
		return nil, err
	}
	f.PolicyID = req.Params.PolicyId
	rows, err := s.store.TopHeavyHitters(ctx, f, p.Limit)
	if err != nil {
		return nil, err
	}
	return apiv1.ListHeavyHitters200JSONResponse(heavyHittersOf(rows)), nil
}

func heavyHittersOf(rows []store.HeavyHitter) apiv1.HeavyHitterList {
	items := make([]apiv1.HeavyHitter, len(rows))
	for i, r := range rows {
		items[i] = apiv1.HeavyHitter{
			PolicyId: r.PolicyID, KeyDigest: r.KeyDigest, ConsumerId: nonZero(r.ConsumerID), ApiKeyId: nonZero(r.ApiKeyID),
			Denied: r.Denied, ShadowDenied: r.ShadowDenied,
		}
	}
	return apiv1.HeavyHitterList{Items: items}
}

// heavyHitterRange checks a range of windows (ADR-0041): since defaults to
// 15 minutes before until when only until is given (else the store's
// default, 15 minutes ago), until must be after since, and since at most
// 7 days before until (or now).
func heavyHitterRange(since, until *time.Time) (store.HeavyHitterFilter, error) {
	f := store.HeavyHitterFilter{Since: since, Until: until}
	if f.Since == nil && f.Until != nil {
		since := f.Until.Add(-defaultHeavyHitterRange)
		f.Since = &since
	}
	if f.Since != nil {
		until := time.Now()
		if f.Until != nil {
			until = *f.Until
		}
		if f.Until != nil && !f.Until.After(*f.Since) {
			return f, invalid("until must be after since")
		}
		if until.Sub(*f.Since) > maxHeavyHitterRange {
			return f, invalid("the range from since to until (or now) is at most 7 days")
		}
	}
	return f, nil
}

// LookupHeavyHitters answers one subject's windows of a policy (ADR-0053):
// the policy's key template, as of the latest published config version,
// filled with the subject as a gateway fills it, and the keyed digest of
// that. The store audits the lookup. No answer, error or log line echoes
// a subject value.
func (s *Server) LookupHeavyHitters(ctx context.Context, req apiv1.LookupHeavyHittersRequestObject) (apiv1.LookupHeavyHittersResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, invalid("body: required")
	}
	if b.PolicyId < 1 {
		return nil, invalid("policy_id: want a policy ID")
	}
	f, err := heavyHitterRange(b.Since, b.Until)
	if err != nil {
		return nil, err
	}
	values, err := subjectValues(&b.Subject)
	if err != nil {
		return nil, err
	}
	latest, err := s.store.LatestConfig(ctx)
	if err != nil {
		return nil, err
	}
	var tmpl string
	for _, p := range latest.GetConfig().GetPolicies() {
		if p.GetId() == b.PolicyId {
			tmpl = p.GetKeyTemplate()
		}
	}
	if tmpl == "" {
		return nil, &apiError{http.StatusNotFound, apiv1.ErrorCodeNotFound, "policy_id: no such policy in the latest published config version", nil}
	}
	if strings.Contains(tmpl, "{api_key}") {
		return nil, invalid("the policy's key template has {api_key}, which only the gateway can fill (the control plane keeps the hash of a key's secret part only); its windows carry api_key_id")
	}
	t, err := config.ParseKeyTemplate(tmpl)
	if err != nil {
		return nil, err // a published template always parses
	}
	filled, ok := t.Fill(values)
	if !ok {
		return nil, invalid("subject: give a value for every placeholder of the policy's key template " + strconv.Quote(tmpl))
	}
	digest := heavyhitters.Pseudonym([]byte(s.hitterSecret.Reveal()), ratelimit.Key(filled))
	rows, err := s.store.LookupHeavyHitters(ctx, audit(ctx, nil), b.PolicyId, digest, f)
	if err != nil {
		return nil, err
	}
	items := make([]apiv1.HeavyHitterWindow, len(rows))
	for i, r := range rows {
		items[i] = apiv1.HeavyHitterWindow{
			WindowStart: r.WindowStart.UTC(), PolicyId: r.PolicyID, KeyDigest: r.KeyDigest, ConsumerId: nonZero(r.ConsumerID),
			ApiKeyId: nonZero(r.ApiKeyID), Denied: r.Denied, ShadowDenied: r.ShadowDenied,
		}
	}
	return apiv1.LookupHeavyHitters200JSONResponse{KeyDigest: digest, Items: items}, nil
}

// subjectValues are the key values of a lookup's subject, as a gateway
// fills them (ADR-0053): {ip} an IPv4 address, or an IPv6 one's /64;
// {consumer} and {tenant} the IDs in decimal (managed identity); {method}
// upper-cased; header names canonical. Errors name the field, never its
// value.
func subjectValues(sub *apiv1.HeavyHitterSubject) (*config.KeyValues, error) {
	v := &config.KeyValues{}
	if sub.Ip != nil {
		ip, err := netip.ParseAddr(*sub.Ip)
		if err != nil || ip.Zone() != "" {
			return nil, invalid("subject.ip: want an IP address without a zone")
		}
		v.IP = config.IPKey(ip.Unmap())
	}
	if sub.ConsumerId != nil {
		if *sub.ConsumerId < 1 {
			return nil, invalid("subject.consumer_id: want a consumer ID")
		}
		v.Consumer = strconv.FormatInt(*sub.ConsumerId, 10)
	}
	if sub.TenantId != nil {
		if *sub.TenantId < 1 {
			return nil, invalid("subject.tenant_id: want a tenant ID")
		}
		v.Tenant = strconv.FormatInt(*sub.TenantId, 10)
	}
	if sub.Route != nil {
		v.Route = *sub.Route
	}
	if sub.Method != nil {
		v.Method = strings.ToUpper(*sub.Method)
	}
	if sub.Headers != nil {
		v.Header = http.Header{}
		for name, value := range *sub.Headers {
			v.Header[http.CanonicalHeaderKey(name)] = []string{value}
		}
	}
	return v, nil
}

// nonZero is id, or nil for 0, which means none.
func nonZero(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}
