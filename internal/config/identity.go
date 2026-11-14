package config

import (
	"cmp"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// KeyPrefixLen is the length of an API key's prefix, its first bytes, which
// find the key without its hash (SEC-03).
const KeyPrefixLen = 12

// KeyHash is what logs may show of an API key: the first 12 hex digits of
// its SHA-256 (SEC-03).
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// Identity is a compiled identity section: consumers and their API keys,
// static (GW-17, ADR-0017) or managed (CP-15, ADR-0039). It never changes
// once compiled.
type Identity struct {
	Version  int64
	keys     map[string]apiKey    // by prefix
	subjects map[string]*Consumer // by JWT subject; managed mode only (GW-19, ADR-0039)
	managed  bool                 // keys are managed (spw1): prefix and hash of the secret
	now      func() time.Time     // the clock expiry is checked on
	// Managed mode only (RL-08, RL-09, ADR-0041): the access lists, and the
	// overrides by consumer and policy ID, each list in the order that
	// picks the one that applies.
	allow, deny access
	overrides   map[[2]int64][]override
}

// access is one action's access-list entries.
type access struct {
	cidrs     []netip.Prefix
	keys      map[int64]bool
	consumers map[int64]bool
}

// override is an override or burst grant of one policy's limits for one
// consumer (RL-08, ADR-0041).
type override struct {
	id           int64
	limits       []*configpb.Limit
	starts, ends time.Time // zero: from the start, and no end
}

// active reports whether o applies at t: started (or no start) and not
// ended (or no end).
func (o *override) active(t time.Time) bool {
	return !t.Before(o.starts) && (o.ends.IsZero() || t.Before(o.ends))
}

// Consumer is a client of the API. Name and Tenant fill the key template
// placeholders {consumer} and {tenant}: names in static mode, decimal IDs
// in managed mode, which are stable across renames (ADR-0039).
type Consumer struct {
	Name   string
	Tenant string // empty if none
	// TenantName is the tenant's name, the tenant metric label (OB-01);
	// managed mode only.
	TenantName string
	// Plan is the ID of the consumer's plan, its own or else its tenant's
	// (GW-19, ADR-0041); managed mode only, 0 in static mode.
	Plan int64
	// ID is the consumer's ID; managed mode only, 0 in static mode.
	ID int64
	// Suspended: the consumer or its tenant is suspended, and its requests
	// are answered 403 (ADR-0041); managed mode only.
	Suspended bool
}

type apiKey struct {
	id       int64 // managed keys only
	hash     [sha256.Size]byte
	consumer *Consumer
	expires  time.Time // zero: never; managed keys only
}

// Lookup returns the consumer that owns key. The key is found by its prefix,
// then the SHA-256 of the key (static) or of its secret part (managed) is
// compared with the stored hash in constant time (SEC-03); a managed key
// must follow the key grammar and not have expired on the gateway's clock
// (D2). A nil Identity, with no identity file, has no keys.
func (id *Identity) Lookup(key string) (*Consumer, bool) {
	c, _, ok := id.Resolve(key)
	return c, ok
}

// Resolve is Lookup that also returns the key's ID, 0 for a static key.
func (id *Identity) Resolve(key string) (*Consumer, int64, bool) {
	if id == nil {
		return nil, 0, false
	}
	var prefix, hashed string
	if id.managed {
		var ok bool
		if prefix, hashed, ok = apikey.Parse(key); !ok {
			return nil, 0, false
		}
	} else if len(key) < KeyPrefixLen {
		return nil, 0, false
	} else {
		prefix, hashed = key[:KeyPrefixLen], key
	}
	k, ok := id.keys[prefix]
	if !ok {
		return nil, 0, false
	}
	sum := sha256.Sum256([]byte(hashed))
	if subtle.ConstantTimeCompare(sum[:], k.hash[:]) != 1 {
		return nil, 0, false
	}
	if !k.expires.IsZero() && !id.now().Before(k.expires) {
		return nil, 0, false
	}
	return k.consumer, k.id, true
}

// CIDR reports whether the CIDR access lists deny or allow a request from
// client IP ip (RL-09, ADR-0041); the caller lets deny win.
func (id *Identity) CIDR(ip netip.Addr) (deny, allow bool) {
	if id == nil || !ip.IsValid() {
		return false, false
	}
	return id.deny.cidr(ip), id.allow.cidr(ip)
}

func (a *access) cidr(ip netip.Addr) bool {
	for _, p := range a.cidrs {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Access reports whether a request authenticated as consumer c, with the
// key whose ID is keyID (0 for none), is denied — by the key or consumer
// deny list, or a suspended consumer or tenant — or allowed by the key or
// consumer allow list (RL-09, ADR-0041); the caller lets deny win. An
// anonymous request (c nil) is neither.
func (id *Identity) Access(c *Consumer, keyID int64) (deny, allow bool) {
	if id == nil || c == nil {
		return false, false
	}
	deny = c.Suspended || id.deny.consumers[c.ID] || keyID != 0 && id.deny.keys[keyID]
	allow = id.allow.consumers[c.ID] || keyID != 0 && id.allow.keys[keyID]
	return deny, allow
}

// Override returns the limits of the override of policy for consumer that
// applies at t, the gateway's clock, if any (RL-08, ADR-0041): among the
// active ones, a burst grant (one with an end) beats a standing override;
// among grants, the latest start wins (none counting as earliest), then
// the highest ID.
func (id *Identity) Override(consumer, policy int64, t time.Time) ([]*configpb.Limit, bool) {
	if id == nil {
		return nil, false
	}
	for i, os := 0, id.overrides[[2]int64{consumer, policy}]; i < len(os); i++ {
		if os[i].active(t) {
			return os[i].limits, true
		}
	}
	return nil, false
}

// Subject returns the consumer whose jwt_subject is sub, the subject of a
// verified token (GW-19, ADR-0039). Static identity has none.
func (id *Identity) Subject(sub string) (*Consumer, bool) {
	if id == nil {
		return nil, false
	}
	c, ok := id.subjects[sub]
	return c, ok
}

// KeyFormatManaged is the key format of managed identity, which the control
// plane sends (ADR-0038, ADR-0039).
const KeyFormatManaged = "spw1"

// CompileIdentity validates an identity section and builds its compiled
// form. Errors name API keys by index, never by any part of the key. Static
// identity (key_format unset) has consumers and static keys only; managed
// identity comes from the control plane.
func CompileIdentity(c *configpb.Identity) (*Identity, error) {
	switch c.GetKeyFormat() {
	case "":
		if err := staticOnly(c); err != nil {
			return nil, err
		}
	case KeyFormatManaged:
		return compileManaged(c)
	default:
		return nil, fmt.Errorf("key_format %q: want none (static keys) or %q (managed keys)", c.GetKeyFormat(), KeyFormatManaged)
	}
	id := &Identity{Version: c.GetVersion(), keys: map[string]apiKey{}, now: time.Now}
	consumers := map[string]*Consumer{}
	for _, cc := range c.GetConsumers() {
		if cc.GetName() == "" {
			return nil, errors.New("consumer: name is required")
		}
		if consumers[cc.GetName()] != nil {
			return nil, fmt.Errorf("consumer %q: defined twice", cc.GetName())
		}
		// Tenant names go into Redis hash tags, {t:<tenant>} (ADR-0021).
		if t := cc.GetTenant(); t != "" && (len(t) > 64 || strings.ContainsFunc(t, notNameChar)) {
			return nil, fmt.Errorf("consumer %q: tenant must be 1-64 characters from A-Z, a-z, 0-9, '.', '_' and '-'", cc.GetName())
		}
		consumers[cc.GetName()] = &Consumer{Name: cc.GetName(), Tenant: cc.GetTenant()}
	}
	prefixes := map[string]int{}
	for i, k := range c.GetApiKeys() {
		if len(k.GetPrefix()) != KeyPrefixLen {
			return nil, fmt.Errorf("api_keys[%d].prefix: want the key's first %d characters", i, KeyPrefixLen)
		}
		if j, ok := prefixes[k.GetPrefix()]; ok {
			return nil, fmt.Errorf("api_keys[%d].prefix: the same as api_keys[%d]'s", i, j)
		}
		prefixes[k.GetPrefix()] = i
		hash, err := hex.DecodeString(k.GetHash())
		if err != nil || len(hash) != sha256.Size || k.GetHash() != strings.ToLower(k.GetHash()) {
			return nil, fmt.Errorf("api_keys[%d].hash: want the key's SHA-256 as 64 lower-case hex digits", i)
		}
		owner := consumers[k.GetConsumer()]
		if owner == nil {
			return nil, fmt.Errorf("api_keys[%d]: consumer %q is not defined", i, k.GetConsumer())
		}
		id.keys[k.GetPrefix()] = apiKey{hash: [sha256.Size]byte(hash), consumer: owner}
	}
	return id, nil
}

// compileManaged compiles the identity the control plane sends (CP-20,
// ADR-0037, ADR-0039, ADR-0041): consumers by ID and by JWT subject, with
// their tenants, plans and statuses, their keys by prefix with the hash of
// their secret and their expiry, the access lists and the overrides. The
// control plane sends only keys that are neither revoked nor expired, and
// overrides that have not ended, when it builds the snapshot; expiry and
// ends after that are checked at each request. Errors name keys by ID,
// never by any part of the key.
func compileManaged(c *configpb.Identity) (*Identity, error) {
	id := &Identity{Version: c.GetVersion(), keys: map[string]apiKey{}, subjects: map[string]*Consumer{}, managed: true, now: time.Now}
	tenants := map[int64]*configpb.Tenant{}
	for _, t := range c.GetTenants() {
		if t.GetId() <= 0 {
			return nil, fmt.Errorf("tenant %q: id is required", t.GetName())
		}
		tenants[t.GetId()] = t
	}
	consumers := map[int64]*Consumer{}
	var err error
	for _, cc := range c.GetConsumers() {
		if cc.GetId() <= 0 || consumers[cc.GetId()] != nil {
			return nil, fmt.Errorf("consumer %q: want a unique id", cc.GetName())
		}
		tenant, ok := tenants[cc.GetTenantId()]
		if !ok {
			return nil, fmt.Errorf("consumer %d: tenant %d is not defined", cc.GetId(), cc.GetTenantId())
		}
		consumer := &Consumer{
			Name: strconv.FormatInt(cc.GetId(), 10), Tenant: strconv.FormatInt(cc.GetTenantId(), 10), TenantName: tenant.GetName(),
			Plan: cmp.Or(cc.GetPlanId(), tenant.GetPlanId()), ID: cc.GetId(),
		}
		if consumer.Suspended, err = suspended(cc.GetStatus(), tenant.GetStatus()); err != nil {
			return nil, fmt.Errorf("consumer %d: %w", cc.GetId(), err)
		}
		consumers[cc.GetId()] = consumer
		// Errors name consumers by ID, never by their subject.
		if sub := cc.GetJwtSubject(); sub != "" {
			if id.subjects[sub] != nil {
				return nil, fmt.Errorf("consumer %d: its jwt_subject is another consumer's", cc.GetId())
			}
			id.subjects[sub] = consumer
		}
	}
	for _, k := range c.GetApiKeys() {
		if len(k.GetPrefix()) != apikey.PrefixLen || !apikey.IsBase62(k.GetPrefix()) {
			return nil, fmt.Errorf("api key %d: want an %d-character base62 prefix", k.GetId(), apikey.PrefixLen)
		}
		if _, dup := id.keys[k.GetPrefix()]; dup {
			return nil, fmt.Errorf("api key %d: its prefix is another key's", k.GetId())
		}
		hash, herr := hex.DecodeString(k.GetHash())
		if herr != nil || len(hash) != sha256.Size || k.GetHash() != strings.ToLower(k.GetHash()) {
			return nil, fmt.Errorf("api key %d: want the secret's SHA-256 as 64 lower-case hex digits", k.GetId())
		}
		owner := consumers[k.GetConsumerId()]
		if owner == nil {
			return nil, fmt.Errorf("api key %d: consumer %d is not defined", k.GetId(), k.GetConsumerId())
		}
		var expires time.Time
		if k.GetExpiresAt() != nil {
			expires = k.GetExpiresAt().AsTime()
		}
		id.keys[k.GetPrefix()] = apiKey{id: k.GetId(), hash: [sha256.Size]byte(hash), consumer: owner, expires: expires}
	}
	for _, e := range c.GetAccessLists() {
		if err := id.addAccess(e); err != nil {
			return nil, fmt.Errorf("access list entry %d: %w", e.GetId(), err)
		}
	}
	if err := id.addOverrides(c.GetOverrides(), consumers); err != nil {
		return nil, err
	}
	return id, nil
}

// suspended reports whether a consumer with status consumer, of a tenant
// with status tenant, is suspended (ADR-0041); unset means active.
func suspended(consumer, tenant string) (bool, error) {
	for _, st := range []string{consumer, tenant} {
		if st != "" && st != "active" && st != "suspended" {
			return false, fmt.Errorf("status %q: want active or suspended", st)
		}
	}
	return consumer == "suspended" || tenant == "suspended", nil
}

// addAccess adds one access-list entry (RL-09). Entries may name keys and
// consumers this identity does not have, such as a revoked key's; they
// match nothing.
func (id *Identity) addAccess(e *configpb.AccessListEntry) error {
	var a *access
	switch e.GetAction() {
	case "allow":
		a = &id.allow
	case "deny":
		a = &id.deny
	default:
		return fmt.Errorf("action %q: want allow or deny", e.GetAction())
	}
	switch e.GetSubjectType() {
	case "cidr":
		p, err := netip.ParsePrefix(e.GetCidr())
		if err != nil || p != p.Masked() {
			return fmt.Errorf("cidr %q: want a CIDR with no bits set past its prefix", e.GetCidr())
		}
		a.cidrs = append(a.cidrs, p)
	case "key":
		if e.GetApiKeyId() <= 0 {
			return errors.New("api_key_id is required")
		}
		a.keys = setOf(a.keys, e.GetApiKeyId())
	case "consumer":
		if e.GetConsumerId() <= 0 {
			return errors.New("consumer_id is required")
		}
		a.consumers = setOf(a.consumers, e.GetConsumerId())
	default:
		return fmt.Errorf("subject_type %q: want key, consumer or cidr", e.GetSubjectType())
	}
	return nil
}

func setOf(m map[int64]bool, id int64) map[int64]bool {
	if m == nil {
		m = map[int64]bool{}
	}
	m[id] = true
	return m
}

// addOverrides keeps the overrides by consumer and policy, each list in
// the order Override tries them: burst grants first, latest start first
// (none counting as earliest), then highest ID; standing overrides last
// (ADR-0041). Their limits are checked against the policy at each request
// (config.Policy.OverrideLimits), since identity does not know the config.
func (id *Identity) addOverrides(os []*configpb.Override, consumers map[int64]*Consumer) error {
	for _, o := range os {
		if consumers[o.GetConsumerId()] == nil {
			return fmt.Errorf("override %d: consumer %d is not defined", o.GetId(), o.GetConsumerId())
		}
		if o.GetPolicyId() <= 0 {
			return fmt.Errorf("override %d: policy_id is required", o.GetId())
		}
		ov := override{id: o.GetId(), limits: o.GetLimits()}
		if o.GetStartsAt() != nil {
			ov.starts = o.GetStartsAt().AsTime()
		}
		if o.GetEndsAt() != nil {
			ov.ends = o.GetEndsAt().AsTime()
		}
		if id.overrides == nil {
			id.overrides = map[[2]int64][]override{}
		}
		k := [2]int64{o.GetConsumerId(), o.GetPolicyId()}
		id.overrides[k] = append(id.overrides[k], ov)
	}
	for _, list := range id.overrides {
		slices.SortFunc(list, func(a, b override) int {
			if ag, bg := !a.ends.IsZero(), !b.ends.IsZero(); ag != bg {
				if ag {
					return -1 // a grant (an end) first
				}
				return 1
			}
			if c := b.starts.Compare(a.starts); c != 0 {
				return c // the latest start first
			}
			return cmp.Compare(b.id, a.id)
		})
	}
	return nil
}

// staticOnly rejects the managed-mode fields of an identity file: IDs,
// tenants, statuses, JWT subjects, expiry, access lists and overrides come
// from the control plane only (ADR-0038).
func staticOnly(c *configpb.Identity) error {
	if len(c.GetTenants()) > 0 || len(c.GetAccessLists()) > 0 || len(c.GetOverrides()) > 0 {
		return errors.New("tenants, access_lists and overrides come from the control plane (managed mode) only")
	}
	for _, cc := range c.GetConsumers() {
		if cc.GetId() != 0 || cc.GetTenantId() != 0 || cc.GetPlanId() != 0 || cc.GetStatus() != "" || cc.GetJwtSubject() != "" {
			return fmt.Errorf("consumer %q: id, tenant_id, plan_id, status and jwt_subject come from the control plane (managed mode) only", cc.GetName())
		}
	}
	for i, k := range c.GetApiKeys() {
		if k.GetId() != 0 || k.GetConsumerId() != 0 || k.GetExpiresAt() != nil {
			return fmt.Errorf("api_keys[%d]: id, consumer_id and expires_at come from the control plane (managed mode) only", i)
		}
	}
	return nil
}
