package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ankitbisht23/spillway/internal/config"
)

// ADR-0017's example keys, for tests only.
const (
	aliceKey = "alice-example-key-1"
	bobKey   = "bob-example-key-1"
)

// identity compiles an identity file with a key for each named consumer.
func identity(t *testing.T, keys map[string]string) *config.Identity {
	t.Helper()
	yaml := "version: 1\nconsumers: [{name: alice, tenant: acme}, {name: bob}]\napi_keys:\n"
	for consumer, key := range keys {
		sum := sha256.Sum256([]byte(key))
		yaml += "  - {prefix: '" + key[:config.KeyPrefixLen] + "', hash: " + hex.EncodeToString(sum[:]) + ", consumer: " + consumer + "}\n"
	}
	c, err := config.LoadIdentity([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	id, err := config.CompileIdentity(c)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAPIKeys: a request without X-API-Key is anonymous and one with a key
// that resolves is proxied, the key going upstream unchanged; any other key
// is answered 401 by the gateway with a JSON error. The access log carries
// the key's hash prefix, never the key (GW-17, SEC-03).
func TestAPIKeys(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey, "bob": bobKey}))

	for i, tt := range []struct {
		name string
		keys []string // X-API-Key values
		ok   bool
	}{
		{"no key", nil, true},
		{"alice's key", []string{aliceKey}, true},
		{"bob's key", []string{bobKey}, true},
		{"right prefix, wrong key", []string{aliceKey[:config.KeyPrefixLen] + "-wrong-1"}, false},
		{"key with a suffix", []string{aliceKey + "x"}, false},
		{"unknown prefix", []string{"carol-exampl-key-1"}, false},
		{"the prefix alone", []string{aliceKey[:config.KeyPrefixLen]}, false},
		{"short key", []string{"alice"}, false},
		{"empty key", []string{""}, false},
		{"two keys", []string{aliceKey, bobKey}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := "key-" + strconv.Itoa(i)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/orders", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Request-Id", id)
			req.Header["X-Api-Key"] = tt.keys
			resp, err := g.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			wantHash := ""
			if len(tt.keys) > 0 {
				wantHash = config.KeyHash(tt.keys[0])
			}
			line := g.logs.access(t, id)
			if line["key_hash"] != wantHash {
				t.Errorf("access log key_hash = %v, want %q", line["key_hash"], wantHash)
			}
			if tt.ok {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status %d, want 200 from the upstream", resp.StatusCode)
				}
				var e echo
				if err := json.Unmarshal(body, &e); err != nil {
					t.Fatal(err)
				}
				if got := e.Header.Values("X-Api-Key"); strings.Join(got, ",") != strings.Join(tt.keys, ",") {
					t.Errorf("upstream got X-API-Key %q, want %q", got, tt.keys)
				}
				return
			}
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/json" ||
				string(body) != `{"error":"invalid API key"}` || resp.Header.Get("X-Request-Id") != id {
				t.Errorf("response = %d %q %q, request ID %q; want 401 application/json {\"error\":\"invalid API key\"} with request ID %s",
					resp.StatusCode, resp.Header.Get("Content-Type"), body, resp.Header.Get("X-Request-Id"), id)
			}
			if line["upstream"] != "" {
				t.Errorf("access log upstream = %v, want none", line["upstream"])
			}
		})
	}

	m := scrape(t, g.metrics)
	for _, s := range []string{
		`gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="r",tenant="none"} 3`,
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="none",route="r",tenant="none"} 7`,
	} {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
	// Keys never reach the logs, whole or in part beyond the prefix
	// lookups use, and never the metrics.
	for _, k := range []string{aliceKey, bobKey, "-wrong-1", "carol-exampl"} {
		if logs := g.logs.String(); strings.Contains(logs, k) || strings.Contains(m, k) {
			t.Errorf("%q appears in the logs or metrics", k)
		}
	}
}

// TestAPIKeysReload: each request resolves its key against the identity
// current when it arrives; without an identity file every key is invalid.
func TestAPIKeysReload(t *testing.T) {
	u := newUpstream(t, "orders")
	g := newGateway(t, newResolver(u), oneUpstream(u))
	withKey := http.Header{"X-Api-Key": {aliceKey}}

	if status, _, _ := g.get(t, "/", withKey); status != http.StatusUnauthorized {
		t.Errorf("no identity: status %d, want 401", status)
	}
	if status, _, _ := g.get(t, "/", nil); status != http.StatusOK {
		t.Errorf("no identity, no key: status %d, want 200", status)
	}
	g.identity.Store(identity(t, map[string]string{"alice": aliceKey}))
	if status, _, _ := g.get(t, "/", withKey); status != http.StatusOK {
		t.Errorf("alice's key added: status %d, want 200", status)
	}
	g.identity.Store(identity(t, map[string]string{"bob": bobKey}))
	if status, _, _ := g.get(t, "/", withKey); status != http.StatusUnauthorized {
		t.Errorf("alice's key removed: status %d, want 401", status)
	}
}
