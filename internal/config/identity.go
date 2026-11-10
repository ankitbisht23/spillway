package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// KeyPrefixLen is the length of an API key's prefix, its first bytes, which
// find the key without its hash (SEC-03).
const KeyPrefixLen = 12

// Identity is a compiled identity section: consumers and static API keys
// (GW-17, ADR-0017). It never changes once compiled.
type Identity struct {
	Version int64
	keys    map[string]apiKey // by prefix
}

// Consumer is a client of the API.
type Consumer struct {
	Name   string
	Tenant string // empty if none
}

type apiKey struct {
	hash     [sha256.Size]byte
	consumer *Consumer
}

// Lookup returns the consumer that owns key. The key is found by its prefix,
// then its SHA-256 is compared with the stored hash in constant time
// (SEC-03). A nil Identity, with no identity file, has no keys.
func (id *Identity) Lookup(key string) (*Consumer, bool) {
	if id == nil || len(key) < KeyPrefixLen {
		return nil, false
	}
	k, ok := id.keys[key[:KeyPrefixLen]]
	if !ok {
		return nil, false
	}
	sum := sha256.Sum256([]byte(key))
	if subtle.ConstantTimeCompare(sum[:], k.hash[:]) != 1 {
		return nil, false
	}
	return k.consumer, true
}

// CompileIdentity validates an identity section and builds its compiled
// form. Errors name API keys by index, never by any part of the key.
func CompileIdentity(c *configpb.Identity) (*Identity, error) {
	id := &Identity{Version: c.GetVersion(), keys: map[string]apiKey{}}
	consumers := map[string]*Consumer{}
	for _, cc := range c.GetConsumers() {
		if cc.GetName() == "" {
			return nil, errors.New("consumer: name is required")
		}
		if consumers[cc.GetName()] != nil {
			return nil, fmt.Errorf("consumer %q: defined twice", cc.GetName())
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
