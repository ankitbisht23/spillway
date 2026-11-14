package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
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
	Version int64
	keys    map[string]apiKey // by prefix
	managed bool              // keys are managed (spw1): prefix and hash of the secret
	now     func() time.Time  // the clock expiry is checked on
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
}

type apiKey struct {
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
	if id == nil {
		return nil, false
	}
	prefix, hashed := key, key
	if id.managed {
		var ok bool
		if prefix, hashed, ok = apikey.Parse(key); !ok {
			return nil, false
		}
	} else if len(key) < KeyPrefixLen {
		return nil, false
	} else {
		prefix = key[:KeyPrefixLen]
	}
	k, ok := id.keys[prefix]
	if !ok {
		return nil, false
	}
	sum := sha256.Sum256([]byte(hashed))
	if subtle.ConstantTimeCompare(sum[:], k.hash[:]) != 1 {
		return nil, false
	}
	if !k.expires.IsZero() && !id.now().Before(k.expires) {
		return nil, false
	}
	return k.consumer, true
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
// ADR-0037, ADR-0039): consumers by ID, with their tenants, and their keys
// by prefix with the hash of their secret and their expiry. The control
// plane sends only keys that are neither revoked nor expired when it builds
// the snapshot; expiry after that is checked at each lookup. Errors name
// keys by ID, never by any part of the key.
func compileManaged(c *configpb.Identity) (*Identity, error) {
	id := &Identity{Version: c.GetVersion(), keys: map[string]apiKey{}, managed: true, now: time.Now}
	tenants := map[int64]string{}
	for _, t := range c.GetTenants() {
		if t.GetId() <= 0 {
			return nil, fmt.Errorf("tenant %q: id is required", t.GetName())
		}
		tenants[t.GetId()] = t.GetName()
	}
	consumers := map[int64]*Consumer{}
	for _, cc := range c.GetConsumers() {
		if cc.GetId() <= 0 || consumers[cc.GetId()] != nil {
			return nil, fmt.Errorf("consumer %q: want a unique id", cc.GetName())
		}
		name, ok := tenants[cc.GetTenantId()]
		if !ok {
			return nil, fmt.Errorf("consumer %d: tenant %d is not defined", cc.GetId(), cc.GetTenantId())
		}
		consumers[cc.GetId()] = &Consumer{
			Name: strconv.FormatInt(cc.GetId(), 10), Tenant: strconv.FormatInt(cc.GetTenantId(), 10), TenantName: name,
		}
	}
	for _, k := range c.GetApiKeys() {
		if len(k.GetPrefix()) != apikey.PrefixLen || !apikey.IsBase62(k.GetPrefix()) {
			return nil, fmt.Errorf("api key %d: want an %d-character base62 prefix", k.GetId(), apikey.PrefixLen)
		}
		if _, dup := id.keys[k.GetPrefix()]; dup {
			return nil, fmt.Errorf("api key %d: its prefix is another key's", k.GetId())
		}
		hash, err := hex.DecodeString(k.GetHash())
		if err != nil || len(hash) != sha256.Size || k.GetHash() != strings.ToLower(k.GetHash()) {
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
		id.keys[k.GetPrefix()] = apiKey{hash: [sha256.Size]byte(hash), consumer: owner, expires: expires}
	}
	return id, nil
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
