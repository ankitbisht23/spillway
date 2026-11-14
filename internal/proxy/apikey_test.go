package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
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

// managedIdentity compiles a managed identity (ADR-0039) with consumer 3 of
// tenant acme and the given key entries.
func managedIdentity(t *testing.T, keys ...*configpb.ApiKey) *config.Identity {
	t.Helper()
	id, err := config.CompileIdentity(&configpb.Identity{
		Version: 1, KeyFormat: config.KeyFormatManaged,
		Tenants:   []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2, Status: "active"}},
		Consumers: []*configpb.Consumer{{Id: 3, TenantId: 1, Name: "alice", Status: "active"}},
		ApiKeys:   keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newManagedKey makes a managed key at run time (a literal one would trip
// the gitleaks rule) and its identity entry, expiring at expires unless
// zero.
func newManagedKey(t *testing.T, id int64, expires time.Time) (apikey.Key, *configpb.ApiKey) {
	t.Helper()
	k, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e := &configpb.ApiKey{Id: id, ConsumerId: 3, Prefix: k.Prefix, Hash: hex.EncodeToString(k.Hash[:])}
	if !expires.IsZero() {
		e.ExpiresAt = timestamppb.New(expires)
	}
	return k, e
}

// TestManagedAPIKeys: on a managed gateway a valid key is proxied with its
// tenant as the metric label; a revoked (absent), expired, unknown or
// malformed key, or a wrong secret, is 401 {"error":"invalid API key"};
// and the key, its secret and its stored hash appear in no access log,
// event log, span, metric or error body (CP-15 - CP-17, GW-17, GW-19,
// SEC-03).
func TestManagedAPIKeys(t *testing.T) {
	u := newUpstream(t, "orders")
	sr, tracer := newTracer(t, sdktrace.AlwaysSample())
	g := newTracedGateway(t, newResolver(u), oneUpstream(u), nil, nil, false, tracer)
	valid, validEntry := newManagedKey(t, 1, time.Time{})
	revoked, _ := newManagedKey(t, 2, time.Time{})
	expired, expiredEntry := newManagedKey(t, 3, time.Now().Add(-time.Second))
	g.identity.Store(managedIdentity(t, validEntry, expiredEntry))

	key := valid.Secret.Reveal()
	wrong := key[:len(key)-1] + map[bool]string{true: "1", false: "0"}[key[len(key)-1] == '0']
	var bodies []string
	for i, tt := range []struct {
		name string
		key  string
		ok   bool
	}{
		{"valid", key, true},
		{"revoked", revoked.Secret.Reveal(), false},
		{"expired", expired.Secret.Reveal(), false},
		{"wrong secret", wrong, false},
		{"malformed", key + "0", false},
	} {
		id := "managed-" + strconv.Itoa(i)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/orders", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", tt.key)
		req.Header.Set("X-Request-Id", id)
		resp, err := g.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if line := g.logs.access(t, id); line["key_hash"] != config.KeyHash(tt.key) {
			t.Errorf("%s: access log key_hash = %v, want %s", tt.name, line["key_hash"], config.KeyHash(tt.key))
		}
		if tt.ok {
			var e echo
			if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &e) != nil || e.Header.Get("X-Api-Key") != tt.key {
				t.Errorf("%s: status %d; want 200 with the key passed upstream unchanged", tt.name, resp.StatusCode)
			}
			continue
		}
		bodies = append(bodies, string(body))
		if resp.StatusCode != http.StatusUnauthorized || string(body) != `{"error":"invalid API key"}` {
			t.Errorf("%s: status %d %q; want 401 {\"error\":\"invalid API key\"}", tt.name, resp.StatusCode, body)
		}
	}

	m := scrape(t, g.metrics)
	if want := `gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="r",tenant="acme"} 1`; !strings.Contains(m, "\n"+want+"\n") {
		t.Errorf("/metrics has no %s", want)
	}
	var spans strings.Builder
	for _, s := range sr.Ended() {
		fmt.Fprintf(&spans, "%s %v %v %v\n", s.Name(), s.Attributes(), s.Events(), s.Status())
	}
	if !strings.Contains(spans.String(), "spillway.authn.result") {
		t.Fatalf("no authn span recorded:\n%s", spans.String())
	}
	outputs := map[string]string{"access log": g.logs.String(), "event log": g.events.String(), "metrics": m, "spans": spans.String(), "error bodies": strings.Join(bodies, "\n")}
	for _, k := range []apikey.Key{valid, revoked, expired} {
		whole := k.Secret.Reveal()
		_, sec, _ := apikey.Parse(whole)
		for what, out := range outputs {
			for part, s := range map[string]string{"key": whole, "secret": sec, "stored hash": hex.EncodeToString(k.Hash[:])} {
				if strings.Contains(out, s) {
					t.Errorf("a %s appears in the %s", part, what)
				}
			}
		}
	}
	if strings.Contains(outputs["access log"]+outputs["error bodies"], wrong) {
		t.Error("the wrong-secret key appears in the logs or an error body")
	}
}
