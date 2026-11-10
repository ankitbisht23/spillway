package config

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
		{"unknown field", "tenants: []", `unknown field "tenants"`},
		{"key in plain text", "api_keys: [{key: " + aliceKey + "}]", `unknown field "key"`},
		{"consumer without a name", "consumers: [{tenant: acme}]", "consumer: name is required"},
		{"consumer twice", "consumers: [{name: a}, {name: a}]", `consumer "a": defined twice`},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseIdentity([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), aliceKey[:12]) {
				t.Errorf("error %q names the key's prefix", err)
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
