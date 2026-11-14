package config

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// ADR-0017's example keys, for tests only; files hold only their prefixes
// and hashes.
const (
	aliceKey = "alice-example-key-1"
	bobKey   = "bob-example-key-1"
)

func hashHex(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// identityYAML has consumers alice and bob (tenant acme) and carol (no
// tenant), and keys for alice and bob.
func identityYAML(version int) string {
	return "version: " + strconv.Itoa(version) + "\n" +
		"consumers: [{name: alice, tenant: acme}, {name: bob, tenant: acme}, {name: carol}]\n" +
		"api_keys:\n" +
		"  - {prefix: " + aliceKey[:12] + ", hash: " + hashHex(aliceKey) + ", consumer: alice}\n" +
		"  - {prefix: " + bobKey[:12] + ", hash: " + hashHex(bobKey) + ", consumer: bob}\n"
}

func parseIdentity(data []byte) (*Identity, error) {
	c, err := LoadIdentity(data)
	if err != nil {
		return nil, err
	}
	return CompileIdentity(c)
}

func TestIdentityLookup(t *testing.T) {
	id, err := parseIdentity([]byte(identityYAML(3)))
	if err != nil {
		t.Fatal(err)
	}
	if id.Version != 3 {
		t.Errorf("version = %d, want 3", id.Version)
	}
	for _, tt := range []struct {
		key, consumer, tenant string // consumer "" means not found
	}{
		{aliceKey, "alice", "acme"},
		{bobKey, "bob", "acme"},
		{aliceKey + "x", "", ""},            // right prefix, wrong hash
		{aliceKey[:12], "", ""},             // the prefix alone
		{aliceKey[:11], "", ""},             // shorter than a prefix
		{"carol-exampl-key-1", "", ""},      // unknown prefix
		{strings.ToUpper(aliceKey), "", ""}, // keys are case-sensitive
		{"", "", ""},
	} {
		c, ok := id.Lookup(tt.key)
		if ok != (tt.consumer != "") || ok && (c.Name != tt.consumer || c.Tenant != tt.tenant) {
			t.Errorf("Lookup(%q) = %+v, %v; want %q in tenant %q", tt.key, c, ok, tt.consumer, tt.tenant)
		}
	}
	var none *Identity
	if c, ok := none.Lookup(aliceKey); ok {
		t.Errorf("nil identity: Lookup = %+v, want no key", c)
	}
}

func TestKeyHash(t *testing.T) {
	// The first 12 hex digits of the hash in ADR-0017's example identity.
	if got := KeyHash(aliceKey); got != "64571644321b" || got != hashHex(aliceKey)[:12] {
		t.Errorf("KeyHash(alice's key) = %q, want 64571644321b", got)
	}
}

func TestIdentityRejects(t *testing.T) {
	key := func(prefix, hash, consumer string) string {
		return "consumers: [{name: alice}]\napi_keys: [{prefix: '" + prefix + "', hash: '" + hash + "', consumer: '" + consumer + "'}]"
	}
	h := hashHex(aliceKey)
	tests := []struct {
		name, yaml, want string
	}{
		{"empty file", "", "identity: empty"},
		{"not YAML", "consumers: [", "identity: yaml"},
		{"unknown field", "tenantz: []", `unknown field "tenantz"`},
		// Managed identity's fields come from the control plane only
		// (ADR-0038).
		{"tenants", "tenants: [{id: 1, name: acme}]", "tenants, access_lists and overrides come from the control plane (managed mode) only"},
		{"access list", "access_lists: [{id: 1, action: deny}]", "tenants, access_lists and overrides come from the control plane"},
		{"override", "overrides: [{id: 1}]", "tenants, access_lists and overrides come from the control plane"},
		{"consumer id", "consumers: [{name: a, id: 3}]", `consumer "a": id, tenant_id, plan_id, status and jwt_subject come from the control plane (managed mode) only`},
		{"consumer jwt subject", "consumers: [{name: a, jwt_subject: s}]", `consumer "a": id, tenant_id`},
		{"key expiry", "consumers: [{name: a}]\napi_keys: [{prefix: abcdefghijkl, hash: " + h + ", consumer: a, expires_at: '2030-01-01T00:00:00Z'}]", "api_keys[0]: id, consumer_id and expires_at come from the control plane (managed mode) only"},
		{"key format", "key_format: spw2", `key_format "spw2": want none (static keys) or "spw1" (managed keys)`},
		{"key in plain text", "api_keys: [{key: " + aliceKey + "}]", `unknown field "key"`},
		{"consumer without a name", "consumers: [{tenant: acme}]", "consumer: name is required"},
		{"consumer twice", "consumers: [{name: a}, {name: a}]", `consumer "a": defined twice`},
		{"tenant with a brace", "consumers: [{name: a, tenant: 'ac}me'}]", `consumer "a": tenant must be 1-64 characters from A-Z, a-z, 0-9, '.', '_' and '-'`},
		{"tenant with a colon", "consumers: [{name: a, tenant: 'acme:1'}]", `consumer "a": tenant must be 1-64 characters`},
		{"tenant too long", "consumers: [{name: a, tenant: " + strings.Repeat("t", 65) + "}]", `consumer "a": tenant must be 1-64 characters`},
		{"short prefix", key(aliceKey[:11], h, "alice"), "api_keys[0].prefix: want the key's first 12 characters"},
		{"long prefix", key(aliceKey[:13], h, "alice"), "api_keys[0].prefix"},
		{"no prefix", key("", h, "alice"), "api_keys[0].prefix"},
		{"prefix twice", "consumers: [{name: a}]\napi_keys: [{prefix: abcdefghijkl, hash: " + h + ", consumer: a}, {prefix: abcdefghijkl, hash: " + hashHex("x") + ", consumer: a}]", "api_keys[1].prefix: the same as api_keys[0]'s"},
		{"upper-case hash", key(aliceKey[:12], strings.ToUpper(h), "alice"), "api_keys[0].hash: want the key's SHA-256 as 64 lower-case hex digits"},
		{"short hash", key(aliceKey[:12], h[:63], "alice"), "api_keys[0].hash"},
		{"long hash", key(aliceKey[:12], h+"00", "alice"), "api_keys[0].hash"},
		{"hash not hex", key(aliceKey[:12], "z"+h[1:], "alice"), "api_keys[0].hash"},
		{"no hash", key(aliceKey[:12], "", "alice"), "api_keys[0].hash"},
		{"undefined consumer", key(aliceKey[:12], h, "bob"), `api_keys[0]: consumer "bob" is not defined`},
		{"no consumer", key(aliceKey[:12], h, ""), `api_keys[0]: consumer "" is not defined`},
		// YAML reads these unquoted values as numbers; protojson's error
		// would quote them.
		{"unquoted digit prefix", "api_keys: [{prefix: 123456789012, hash: " + h + "}]", "api_keys[0].prefix: not a string; quote it"},
		{"unquoted zero-led prefix", "api_keys: [{prefix: 000123456789, hash: " + h + "}]", "api_keys[0].prefix: not a string; quote it"},
		{"unquoted digit hash", "api_keys: [{prefix: abcdefghijkl, hash: 123456789}]", "api_keys[0].hash: not a string; quote it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseIdentity([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if err != nil && (strings.Contains(err.Error(), aliceKey[:12]) || strings.Contains(err.Error(), "123456789")) {
				t.Errorf("error %q names part of a key", err)
			}
		})
	}
}

// managedKey makes a managed key at run time (a literal one would trip the
// gitleaks rule, ADR-0039) and its identity entry for consumer, expiring
// at expires unless zero.
func managedKey(t *testing.T, id, consumer int64, expires time.Time) (string, *configpb.ApiKey) {
	t.Helper()
	k, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e := &configpb.ApiKey{Id: id, ConsumerId: consumer, Prefix: k.Prefix, Hash: hex.EncodeToString(k.Hash[:])}
	if !expires.IsZero() {
		e.ExpiresAt = timestamppb.New(expires)
	}
	return k.Secret.Reveal(), e
}

// TestManagedIdentity checks managed keys (CP-15, GW-17, ADR-0039): a key
// is found by its prefix and its secret's hash compared; template values
// are decimal IDs and the tenant label its name; anything malformed,
// unknown or with a wrong secret resolves to nothing.
func TestManagedIdentity(t *testing.T) {
	alice, aliceEntry := managedKey(t, 4, 3, time.Time{})
	bob, bobEntry := managedKey(t, 5, 6, time.Time{})
	id, err := CompileIdentity(&configpb.Identity{
		Version: 7, KeyFormat: KeyFormatManaged,
		Tenants:   []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2, Status: "active"}, {Id: 2, Name: "globex", PlanId: 2, Status: "active"}},
		Consumers: []*configpb.Consumer{{Id: 3, TenantId: 1, Name: "alice", Status: "active"}, {Id: 6, TenantId: 2, Name: "bob", Status: "active"}},
		ApiKeys:   []*configpb.ApiKey{aliceEntry, bobEntry},
	})
	if err != nil || id.Version != 7 {
		t.Fatalf("CompileIdentity = %+v, %v", id, err)
	}
	if c, ok := id.Lookup(alice); !ok || *c != (Consumer{Name: "3", Tenant: "1", TenantName: "acme"}) {
		t.Errorf("alice's key: Lookup = %+v, %v; want consumer 3 of tenant 1 (acme)", c, ok)
	}
	if c, ok := id.Lookup(bob); !ok || c.Name != "6" || c.TenantName != "globex" {
		t.Errorf("bob's key: Lookup = %+v, %v", c, ok)
	}
	_, other := managedKey(t, 9, 3, time.Time{})
	for name, key := range map[string]string{
		"wrong secret":      alice[:len(alice)-1] + map[bool]string{true: "1", false: "0"}[alice[len(alice)-1] == '0'],
		"bob's secret":      alice[:13] + bob[13:],
		"unknown prefix":    apikey.Start + other.GetPrefix() + alice[12:],
		"a suffix":          alice + "0",
		"one character off": alice[1:],
		"the prefix alone":  alice[:12],
		"lower case":        strings.ToLower(alice),
		"static form":       alice[:KeyPrefixLen] + "x",
		"empty":             "",
	} {
		if key == alice || key == bob {
			continue // the lower-cased key may equal the key
		}
		if c, ok := id.Lookup(key); ok {
			t.Errorf("%s: Lookup = %+v; want no consumer", name, c)
		}
	}
}

// TestManagedKeyExpiry checks rotation's overlap on a fake clock (CP-16,
// D2): the old key works until its expires_at, on the gateway's clock, and
// not from then on; the new key, without one, works throughout.
func TestManagedKeyExpiry(t *testing.T) {
	rotated := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	old, oldEntry := managedKey(t, 1, 3, rotated.Add(24*time.Hour))
	newKey, newEntry := managedKey(t, 2, 3, time.Time{})
	id, err := CompileIdentity(&configpb.Identity{
		Version: 2, KeyFormat: KeyFormatManaged,
		Tenants:   []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2}},
		Consumers: []*configpb.Consumer{{Id: 3, TenantId: 1, Name: "alice"}},
		ApiKeys:   []*configpb.ApiKey{oldEntry, newEntry},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		at       time.Time
		old, new bool
	}{
		{rotated, true, true},
		{rotated.Add(23 * time.Hour), true, true},
		{rotated.Add(24*time.Hour - time.Nanosecond), true, true},
		{rotated.Add(24 * time.Hour), false, true},
		{rotated.Add(7 * 24 * time.Hour), false, true},
	} {
		id.now = func() time.Time { return tt.at }
		_, oldOK := id.Lookup(old)
		_, newOK := id.Lookup(newKey)
		if oldOK != tt.old || newOK != tt.new {
			t.Errorf("at %v: old key %v, new key %v; want %v, %v", tt.at.Sub(rotated), oldOK, newOK, tt.old, tt.new)
		}
	}
}

// TestManagedIdentityErrors: a managed identity that cannot be right is
// refused, and errors never name any part of a key.
func TestManagedIdentityErrors(t *testing.T) {
	_, k := managedKey(t, 4, 3, time.Time{})
	tenants := []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2}}
	consumers := []*configpb.Consumer{{Id: 3, TenantId: 1, Name: "alice"}}
	with := func(f func(k *configpb.ApiKey)) []*configpb.ApiKey {
		c := proto.Clone(k).(*configpb.ApiKey)
		f(c)
		return []*configpb.ApiKey{c}
	}
	for _, tt := range []struct {
		name string
		id   *configpb.Identity
		want string
	}{
		{"short prefix", &configpb.Identity{Tenants: tenants, Consumers: consumers, ApiKeys: with(func(k *configpb.ApiKey) { k.Prefix = "abc" })}, "api key 4: want an 8-character base62 prefix"},
		{"prefix not base62", &configpb.Identity{Tenants: tenants, Consumers: consumers, ApiKeys: with(func(k *configpb.ApiKey) { k.Prefix = "abcdefg-" })}, "base62 prefix"},
		{"two keys, one prefix", &configpb.Identity{Tenants: tenants, Consumers: consumers, ApiKeys: append(with(func(*configpb.ApiKey) {}), with(func(k *configpb.ApiKey) { k.Id = 5 })...)}, "api key 5: its prefix is another key's"},
		{"upper-case hash", &configpb.Identity{Tenants: tenants, Consumers: consumers, ApiKeys: with(func(k *configpb.ApiKey) { k.Hash = strings.ToUpper(k.Hash) })}, "64 lower-case hex digits"},
		{"unknown consumer", &configpb.Identity{Tenants: tenants, Consumers: consumers, ApiKeys: with(func(k *configpb.ApiKey) { k.ConsumerId = 8 })}, "api key 4: consumer 8 is not defined"},
		{"unknown tenant", &configpb.Identity{Consumers: consumers}, "consumer 3: tenant 1 is not defined"},
		{"consumer without id", &configpb.Identity{Tenants: tenants, Consumers: []*configpb.Consumer{{TenantId: 1, Name: "x"}}}, `consumer "x": want a unique id`},
		{"tenant without id", &configpb.Identity{Tenants: []*configpb.Tenant{{Name: "acme"}}}, `tenant "acme": id is required`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.id.KeyFormat = KeyFormatManaged
			_, err := CompileIdentity(tt.id)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), k.GetPrefix()) || strings.Contains(err.Error(), k.GetHash()[:12]) {
				t.Errorf("error %q names part of a key", err)
			}
		})
	}
}

func TestIdentityWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.yaml")
	write(t, path, "consumers: [")
	if _, err := NewIdentityWatcher(path, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("NewIdentityWatcher accepted an invalid file")
	}

	write(t, path, identityYAML(1))
	w, err := NewIdentityWatcher(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	good := w.Identity()
	if _, ok := good.Lookup(aliceKey); !ok || good.Version != 1 {
		t.Fatalf("version %d, alice's key found = %v", good.Version, ok)
	}

	// An invalid file keeps the last good identity.
	write(t, path, "consumers: [{name: a}, {name: a}]")
	if err := w.reload(); err == nil || !strings.HasPrefix(err.Error(), "identity: ") || w.Identity() != good {
		t.Errorf("invalid file: err = %v, identity replaced = %v", err, w.Identity() != good)
	}

	write(t, path, "version: 2\nconsumers: [{name: alice}]\n")
	if err := w.reload(); err != nil || w.Identity().Version != 2 {
		t.Fatalf("reload = %v, version %d; want 2", err, w.Identity().Version)
	}
	if _, ok := w.Identity().Lookup(aliceKey); ok {
		t.Error("a removed key still resolves")
	}
	if _, ok := good.Lookup(aliceKey); !ok {
		t.Error("the identity a request already took changed")
	}
}

// FuzzLoadIdentity checks that loading and compiling an identity file never
// panic on hostile input.
func FuzzLoadIdentity(f *testing.F) {
	f.Add([]byte(identityYAML(1)))
	f.Add([]byte("api_keys: [{prefix: abcdefghijkl, hash: '" + strings.Repeat("0", 64) + "', consumer: a}]"))
	f.Add([]byte("consumers: [{name: a}, {name: a}]"))
	f.Fuzz(func(t *testing.T, data []byte) {
		id, err := parseIdentity(data)
		if err == nil {
			id.Lookup(string(data))
		}
	})
}

// TestComposeFiles: the compose gateways' config and identity compile, and
// the identity holds ADR-0017's example keys.
func TestComposeFiles(t *testing.T) {
	data, err := os.ReadFile("../../deploy/compose/gateway/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parse(data); err != nil {
		t.Errorf("config.yaml: %v", err)
	}
	if data, err = os.ReadFile("../../deploy/compose/gateway/identity.yaml"); err != nil {
		t.Fatal(err)
	}
	id, err := parseIdentity(data)
	if err != nil {
		t.Fatal(err)
	}
	for key, consumer := range map[string]string{aliceKey: "alice", bobKey: "bob"} {
		if c, ok := id.Lookup(key); !ok || c.Name != consumer || c.Tenant != "acme" {
			t.Errorf("Lookup(%s's key) = %+v, %v", consumer, c, ok)
		}
	}
}

// TestVerifyConfigs: the configs the exit checks, the accuracy harness and
// the chaos tests swap into the gateways (ADR-0015, ADR-0019, ADR-0021)
// compile, so a check never waits on a config the gateway rejects.
func TestVerifyConfigs(t *testing.T) {
	for _, d := range []string{"test/verify", "test/accuracy", "test/chaos"} {
		dir := os.DirFS("../../" + d)
		files, err := fs.Glob(dir, "*.yaml")
		if err != nil || len(files) == 0 {
			t.Fatalf("no configs in %s: %v", d, err)
		}
		for _, f := range files {
			data, err := fs.ReadFile(dir, f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parse(data); err != nil {
				t.Errorf("%s/%s: %v", d, f, err)
			}
		}
	}
}
