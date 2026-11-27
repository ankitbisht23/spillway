//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.yaml.in/yaml/v3"

	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/demo"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/secret"
)

const demoClientSecret = "dev-only-demo-seed-test" //nolint:gosec // G101: a test value

// demoState is what a second seed run must leave as it was: the latest
// published version, the working set's revision, the newest audit record
// and every tenant's and consumer's version.
type demoState struct {
	Version  int64
	Revision string
	Audit    int64
	Entities map[string]int64 // "tenant <id>" or "consumer <id>" → version
}

func readDemoState(t *testing.T, c cpClient) demoState {
	t.Helper()
	s := demoState{Revision: c.must(200, "GET", "/config/export", nil).etag(), Entities: map[string]int64{}}
	var versions struct{ Items []struct{ Version int64 } }
	c.must(200, "GET", "/config/versions?limit=1", nil).decode(t, &versions)
	if len(versions.Items) == 1 {
		s.Version = versions.Items[0].Version
	}
	var audit struct{ Items []struct{ ID int64 } }
	c.must(200, "GET", "/audit?limit=1", nil).decode(t, &audit)
	if len(audit.Items) == 1 {
		s.Audit = audit.Items[0].ID
	}
	for _, kind := range []string{"tenant", "consumer"} {
		var list struct {
			Items []struct{ ID, Version int64 }
		}
		c.must(200, "GET", "/"+kind+"s?limit=500", nil).decode(t, &list)
		for _, e := range list.Items {
			s.Entities[fmt.Sprintf("%s %d", kind, e.ID)] = e.Version
		}
	}
	return s
}

// TestDemoSeed: demo-traffic seed's logic (SC-01, ADR-0094 §3) against a
// control plane on a scratch database, with a client-credentials token
// endpoint, using deploy/demo's files. The first run publishes the demo
// config and creates the three tenants and eleven consumers on their plans,
// with no API key; the second run changes nothing: the same version and
// working-set revision, no audit record, no entity version. Neither run
// prints the client secret or the token.
func TestDemoSeed(t *testing.T) {
	c, iss, _ := startControlPlane(t, io.Discard)
	admin := iss.token(t, "service-account-spillway-ci", "service-account-spillway-ci", "admin")
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.PostForm.Get("client_id") != "spillway-ci" || r.PostForm.Get("client_secret") != demoClientSecret {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": admin})
	}))
	t.Cleanup(tokens.Close)
	client := demo.NewClient(c.base+api.BasePath, tokens.URL, "spillway-ci", secret.New(demoClientSecret), 10*time.Second)
	cfg, err := os.ReadFile("../../deploy/demo/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../deploy/demo/seed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := demo.ParseSeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep, err := demo.Seed(t.Context(), client, cfg, seed, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !rep.Published || rep.Created != 14 || rep.Updated != 0 || rep.Unchanged != 0 {
		t.Fatalf("first run: %+v\n%s", rep, out.String())
	}
	c = c.as(admin)

	// The published config: four routes, four policies, three plans.
	names := func(collection string) map[string]map[string]any {
		var list struct{ Items []map[string]any }
		c.must(200, "GET", "/"+collection+"?limit=100", nil).decode(t, &list)
		m := map[string]map[string]any{}
		for _, e := range list.Items {
			m[e["name"].(string)] = e
		}
		return m
	}
	routes := names("routes")
	for name, mode := range map[string]string{"orders": "api_key", "catalog": "api_key", "payments": "api_key", "service": "jwt"} {
		if routes[name] == nil || routes[name]["auth_mode"] != mode {
			t.Errorf("route %s: %v", name, routes[name])
		}
	}
	if p := names("policies"); len(p) != 4 || p["consumer-plan"]["from_plan"] != true || p["payments-limit"]["failure_mode"] != "fail_closed" {
		t.Errorf("policies: %v", slices.Sorted(maps.Keys(p)))
	}
	plans := names("plans")
	planName := map[int64]string{}
	for name, p := range plans {
		planName[int64(p["id"].(float64))] = name
	}
	if len(plans) != 3 || plans["free"] == nil || plans["pro"] == nil || plans["enterprise"] == nil {
		t.Errorf("plans: %v", slices.Sorted(maps.Keys(plans)))
	}

	// The identity: each tenant on its plan, each consumer on its tenant's,
	// pro-co-service with gateway-tests' subject, and no key at all.
	tenants := names("tenants")
	for tn, plan := range map[string]string{"free-co": "free", "pro-co": "pro", "enterprise-co": "enterprise"} {
		if tenants[tn] == nil || planName[int64(tenants[tn]["plan_id"].(float64))] != plan {
			t.Errorf("tenant %s: %v", tn, tenants[tn])
		}
	}
	consumers := names("consumers")
	if len(consumers) != 11 {
		t.Errorf("%d consumers: %v", len(consumers), slices.Sorted(maps.Keys(consumers)))
	}
	for name, cs := range consumers {
		if cs["plan_id"] != nil || cs["status"] != "active" || (cs["jwt_subject"] != nil) != (name == "pro-co-service") {
			t.Errorf("consumer %s: %v", name, cs)
		}
		var keys struct{ Items []any }
		c.must(200, "GET", fmt.Sprintf("/consumers/%d/keys", int64(cs["id"].(float64))), nil).decode(t, &keys)
		if len(keys.Items) != 0 {
			t.Errorf("consumer %s has %d keys", name, len(keys.Items))
		}
	}
	if consumers["pro-co-service"]["jwt_subject"] != gatewayTestsSubject {
		t.Errorf("pro-co-service: %v", consumers["pro-co-service"])
	}

	before := readDemoState(t, c)
	rep, err = demo.Seed(t.Context(), client, cfg, seed, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Published || rep.ConfigVersion != before.Version || rep.Unchanged != 14 || rep.Created+rep.Updated != 0 {
		t.Errorf("second run: %+v\n%s", rep, out.String())
	}
	if after := readDemoState(t, c); after.Version != before.Version || after.Revision != before.Revision || after.Audit != before.Audit || !maps.Equal(after.Entities, before.Entities) {
		t.Errorf("the second run changed something:\nbefore %+v\nafter  %+v", before, after)
	}
	if s := out.String(); strings.Contains(s, demoClientSecret) || strings.Contains(s, admin) {
		t.Errorf("a secret or token was printed:\n%s", s)
	}
}

// demoAddrMap is the SPILLWAY_REDIS_ADDR_MAP that
// deploy/compose/demo.compose.yml gives gateway-1 … gateway-3.
func demoAddrMap(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/compose/demo.compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	for _, g := range []string{"gateway-1", "gateway-2", "gateway-3"} {
		got, err := rlredis.ParseAddrMap(f.Services[g].Environment[rlredis.AddrMapEnv])
		if err != nil || len(got) != 6 || (m != nil && !maps.Equal(m, got)) {
			t.Fatalf("%s: map %v %v", g, got, err)
		}
		m = got
	}
	proxies := map[string]string{} // toxiproxy:port → upstream
	for _, p := range demo.DemoProxies() {
		_, port, _ := net.SplitHostPort(p.Listen)
		proxies["toxiproxy:"+port] = p.Upstream
	}
	for from, to := range m {
		if proxies[to] != from {
			t.Errorf("%s goes to %s, the proxy of %q", from, to, proxies[to])
		}
	}
	return m
}

// TestDemoRedisAddrMap: every address a cluster client dials with the demo
// gateways' map is one the map sends through a demo proxy (ADR-0094 §5):
// the seeds, every primary that scripts on keys of all three primaries
// reach, and the node a forced MOVED names. Redis Cluster names nodes by
// hostname in slot maps and redirects, so no connection bypasses the map.
// toxiproxy's in-network ports cannot be reached from the host, so each
// mapped dial goes to the node's host port instead (ADR-0004).
func TestDemoRedisAddrMap(t *testing.T) {
	demoMap := demoAddrMap(t)
	hostMap, err := rlredis.ParseAddrMap(os.Getenv(rlredis.AddrMapEnv))
	if err != nil || hostMap == nil {
		t.Fatalf("%s: %v", rlredis.AddrMapEnv, err)
	}
	var mu sync.Mutex
	dialed := map[string]bool{}
	var unmapped []string
	var d net.Dialer
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		dialed[addr] = true
		if _, ok := demoMap[addr]; !ok {
			unmapped = append(unmapped, addr)
			return nil, fmt.Errorf("%s is not in the demo map", addr)
		}
		return d.DialContext(ctx, network, hostMap[addr])
	}
	seeds := slices.Sorted(maps.Keys(demoMap))
	rdb := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: seeds, Dialer: dial, MaxRetries: 0})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := t.Context()
	shards, err := rdb.ClusterShards(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	type slotRange struct {
		start, end int64
		primary    string // as the cluster names it: hostname:port
	}
	var ranges []slotRange
	primaries := map[string]bool{}
	for _, s := range shards {
		for _, n := range s.Nodes {
			if n.Role != "master" || n.Health != "online" {
				continue
			}
			addr := net.JoinHostPort(n.Endpoint, fmt.Sprint(n.Port))
			for _, r := range s.Slots {
				ranges = append(ranges, slotRange{r.Start, r.End, addr})
				primaries[addr] = true
			}
		}
	}
	owner := func(slot int64) string {
		for _, r := range ranges {
			if slot >= r.start && slot <= r.end {
				return r.primary
			}
		}
		return ""
	}
	if len(primaries) != 3 {
		t.Fatalf("primaries %v", primaries)
	}
	script := goredis.NewScript(`redis.call('SET', KEYS[1], '1', 'PX', 10000) return redis.call('GET', KEYS[1])`)
	reached := map[string]bool{}
	for i := 0; len(reached) < 3 && i < 200; i++ {
		key := fmt.Sprintf("demo-map-test:{%d}", i)
		if err := script.Run(ctx, rdb, []string{key}).Err(); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		slot, err := rdb.ClusterKeySlot(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		reached[owner(slot)] = true
	}
	if !maps.Equal(reached, primaries) {
		t.Fatalf("scripts reached %v, want %v", reached, primaries)
	}

	// A forced MOVED: a client whose slot map puts every slot on one
	// primary sends a script for another primary's key there, and follows
	// the redirect to the node it names.
	first := ranges[0].primary
	wrong := goredis.NewClusterClient(&goredis.ClusterOptions{Dialer: dial, MaxRetries: 0, MaxRedirects: 3,
		ClusterSlots: func(context.Context) ([]goredis.ClusterSlot, error) {
			return []goredis.ClusterSlot{{Start: 0, End: 16383, Nodes: []goredis.ClusterNode{{Addr: first}}}}, nil
		}})
	t.Cleanup(func() { _ = wrong.Close() })
	moved := ""
	for i := 0; moved == "" && i < 200; i++ {
		key := fmt.Sprintf("demo-map-test:{%d}", i)
		slot, err := rdb.ClusterKeySlot(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if o := owner(slot); o != first {
			mu.Lock()
			delete(dialed, o)
			mu.Unlock()
			if err := script.Run(ctx, wrong, []string{key}).Err(); err != nil {
				t.Fatalf("%s through a wrong slot map: %v", key, err)
			}
			moved = o
		}
	}
	// And the raw redirect names the node by hostname.
	node := goredis.NewClient(&goredis.Options{Addr: first, Dialer: dial, MaxRetries: 0})
	t.Cleanup(func() { _ = node.Close() })
	var redirect string
	for i := 0; redirect == "" && i < 200; i++ {
		if err := node.Get(ctx, fmt.Sprintf("demo-map-test:{%d}", i)).Err(); err != nil && strings.HasPrefix(err.Error(), "MOVED ") {
			redirect = strings.Fields(err.Error())[2]
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if moved == "" || !dialed[moved] {
		t.Errorf("the MOVED to %q was not followed through the map; dialed %v", moved, slices.Sorted(maps.Keys(dialed)))
	}
	if _, ok := demoMap[redirect]; !ok {
		t.Errorf("MOVED names %q, which the demo map does not send through a proxy", redirect)
	}
	if len(unmapped) > 0 {
		t.Errorf("dialed addresses outside the demo map: %v", unmapped)
	}
}
