//go:build integration

package integration

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/heavyhitters"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// Heavy hitters (RL-21, CP-22, ADR-0041): heartbeats' top_keys kept as
// one-minute heavy_hitters windows, read through GET /heavy-hitters. Every
// wait allows 30 s, far above the 820 ms host stalls of ADR-0034, so a
// stall alone cannot fail a check.

// listHeavyHitters is GET /heavy-hitters.
func listHeavyHitters(t *testing.T, c cpClient, query string) []apiv1.HeavyHitter {
	t.Helper()
	var l apiv1.HeavyHitterList
	c.must(200, "GET", "/heavy-hitters"+query, nil).decode(t, &l)
	return l.Items
}

// hitter is a heavy hitter's fields, IDs 0 for null, for comparisons.
type hitter struct {
	policy           int64
	digest           string
	consumer, key    int64
	denied, shadowed int64
}

func hittersOf(items []apiv1.HeavyHitter) []hitter {
	out := make([]hitter, len(items))
	for i, h := range items {
		out[i] = hitter{h.PolicyId, h.KeyDigest, 0, 0, h.Denied, h.ShadowDenied}
		if h.ConsumerId != nil {
			out[i].consumer = *h.ConsumerId
		}
		if h.ApiKeyId != nil {
			out[i].key = *h.ApiKeyId
		}
	}
	return out
}

// TestHeavyHitterWindows checks what the control plane keeps of top_keys
// (ADR-0041): counts summed per window over replicas and heartbeats, the
// latest non-zero consumer and key IDs; GET /heavy-hitters' order, its
// range (15 minutes by default, at most 7 days), its policy filter and
// limit, for every role; the prune of windows older than 7 days; and no
// bearer token in a log line.
func TestHeavyHitterWindows(t *testing.T) {
	var logs syncBuffer
	c, iss, db := startControlPlane(t, &logs)
	r := newRoles(t, c, iss)
	st := openStore(t, db)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	ctx := t.Context()
	d1, d2 := rlredis.Digest("client-1"), rlredis.Digest("client-2")
	beat := func(name string, keys ...*configpb.TopKey) {
		t.Helper()
		if _, err := st.RecordHeartbeat(ctx, name, &configpb.HeartbeatRequest{
			ReplicaId: name, SchemaVersion: &configpb.SchemaVersion{Major: 1}, TopKeys: keys,
		}); err != nil {
			t.Fatal(err)
		}
	}
	beat("gateway-1", &configpb.TopKey{PolicyId: 1, KeyDigest: d1, ConsumerId: 3, ApiKeyId: 30, Denied: 5},
		&configpb.TopKey{PolicyId: 2, KeyDigest: d2, ShadowDenied: 7})
	beat("gateway-2", &configpb.TopKey{PolicyId: 1, KeyDigest: d1, Denied: 2, ShadowDenied: 1}) // anonymous: the IDs stay
	beat("gateway-2", &configpb.TopKey{PolicyId: 1, KeyDigest: d1, ConsumerId: 4, ApiKeyId: 40, Denied: 1})
	beat("gateway-3") // no top_keys
	// An hour-old window, outside the default 15 minutes.
	if _, err := owner.Exec(ctx, `INSERT INTO heavy_hitters VALUES (date_trunc('minute', clock_timestamp(), 'UTC') - interval '1 hour', 1, $1, 9, 90, 100, 0)`, d1); err != nil {
		t.Fatal(err)
	}

	want := []hitter{{1, d1, 4, 40, 8, 1}, {2, d2, 0, 0, 0, 7}}
	if got := hittersOf(listHeavyHitters(t, r.viewer, "")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("heavy hitters %v, want %v", got, want)
	}
	if got := hittersOf(listHeavyHitters(t, r.viewer, "?policy_id=2")); fmt.Sprint(got) != fmt.Sprint(want[1:]) {
		t.Errorf("policy 2: %v", got)
	}
	if got := hittersOf(listHeavyHitters(t, r.viewer, "?limit=1")); fmt.Sprint(got) != fmt.Sprint(want[:1]) {
		t.Errorf("limit 1: %v", got)
	}
	since := url.QueryEscape(time.Now().Add(-2 * time.Hour).Format(time.RFC3339))
	if got := hittersOf(listHeavyHitters(t, r.viewer, "?since="+since)); len(got) != 2 || got[0] != (hitter{1, d1, 4, 40, 108, 1}) {
		t.Errorf("the last 2 hours: %v", got)
	}
	until := url.QueryEscape(time.Now().Add(-30 * time.Minute).Format(time.RFC3339))
	if got := hittersOf(listHeavyHitters(t, r.viewer, "?until="+until)); len(got) != 0 {
		t.Errorf("the 15 minutes before 30 minutes ago: %v", got)
	}
	if got := hittersOf(listHeavyHitters(t, r.viewer, "?since="+since+"&until="+until+"&policy_id=1")); len(got) != 1 || got[0] != (hitter{1, d1, 9, 90, 100, 0}) {
		t.Errorf("the hour-old window: %v", got)
	}
	for _, q := range []string{
		"?since=" + until + "&until=" + until,                                             // empty
		"?since=" + url.QueryEscape(time.Now().Add(-8*24*time.Hour).Format(time.RFC3339)), // over 7 days
		"?limit=501", "?since=yesterday", "?policy_id=x",
	} {
		r.viewer.must(400, "GET", "/heavy-hitters"+q, nil)
	}
	for _, cl := range []cpClient{r.editor, r.publisher, r.keyAdmin, r.admin} {
		cl.must(200, "GET", "/heavy-hitters", nil)
	}
	c.must(401, "GET", "/heavy-hitters", nil)

	// The prune: windows older than 7 days go with the hourly prune.
	if _, err := owner.Exec(ctx, `UPDATE heavy_hitters SET window_start = window_start - interval '8 days' WHERE policy_id = 2`); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PruneReplicas(ctx); err != nil || n != 1 {
		t.Errorf("pruned %d rows, %v; want 1", n, err)
	}
	var left int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM heavy_hitters").Scan(&left); err != nil || left != 2 {
		t.Errorf("%d windows left, %v; want 2", left, err)
	}
	for _, cl := range []cpClient{r.editor, r.publisher, r.admin, r.viewer, r.keyAdmin} {
		if sig := cl.token[strings.LastIndexByte(cl.token, '.')+1:]; strings.Contains(logs.String(), sig) {
			t.Error("a bearer token appears in a log line")
		}
	}
}

// TestHeavyHittersReported is RL-21 end to end: a managed gateway
// throttles a key, enforce and shadow, and the key shows up in GET
// /heavy-hitters with its denies and shadow would-denies, under the keyed
// digest of its filled key template (ADR-0053), not the unkeyed one of its
// Redis key; the key itself appears in no answer and in no log of the
// gateway or the control plane.
func TestHeavyHittersReported(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	config := string(limitedConfig(upstreamAnswering(t, "ok"))) + `  - name: per-client-shadow
    level: global
    algorithm: gcra
    local: true
    mode: shadow
    key_template: '{header:x-client}'
    match: {routes: ['*']}
    limits: [{rate: 5, period: 1h, burst: 5}]
`
	v := publishYAML(t, cp.rest, []byte(config), "limited, with a shadow policy")
	gw := startManaged(t, "gateway-1", grpcAddr, t.TempDir())
	eventually(t, "gateway-1 live at the published version", func() bool { return liveAt(listReplicas(t, cp.rest, ""), []string{"gateway-1"}, v, 1) })
	policyID := func(name string) int64 {
		var l apiv1.PolicyList
		cp.rest.must(200, "GET", "/policies?name="+name, nil).decode(t, &l)
		if len(l.Items) != 1 || l.Items[0].Id == nil {
			t.Fatalf("policy %s: %+v", name, l)
		}
		return *l.Items[0].Id
	}
	enforce, shadow := policyID("per-client"), policyID("per-client-shadow")

	// 30 requests: the enforce policy admits 20 and denies 10; the shadow
	// one would deny all but the first 5.
	key := "leaked-" + rand.Text()
	var denied int
	for range 30 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, gw.url+"/r", nil)
		req.Header.Set("X-Client", key)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			denied++
		}
	}
	if denied != 10 {
		t.Fatalf("%d of 30 denied, want 10", denied)
	}
	digest := heavyhitters.Pseudonym([]byte(hitterSecret), ratelimit.Key(key))
	if digest == rlredis.Digest(key) {
		t.Fatal("the reported digest is the limiter key's")
	}
	want := []hitter{{shadow, digest, 0, 0, 0, 25}, {enforce, digest, 0, 0, 10, 0}}
	var got []hitter
	took := eventually(t, "the throttled key in /heavy-hitters", func() bool {
		got = hittersOf(listHeavyHitters(t, cp.rest, ""))
		return fmt.Sprint(got) == fmt.Sprint(want)
	})
	t.Logf("reported %v after the last request", took.Round(10*time.Millisecond))
	// Later heartbeats add nothing: the sketch was reset.
	time.Sleep(6 * time.Second)
	if got := hittersOf(listHeavyHitters(t, cp.rest, "")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after another heartbeat: %v, want %v", got, want)
	}
	body := string(cp.rest.must(200, "GET", "/heavy-hitters", nil).body)
	for name, out := range map[string]string{"the API answer": body, "the gateway's log": gw.out.String(), "the control plane's log": cp.out.String()} {
		if strings.Contains(out, key) || strings.Contains(out, rlredis.Digest(key)) {
			t.Errorf("the raw key or its unkeyed digest appears in %s", name)
		}
	}
}

// lookupHitters is POST /heavy-hitters/lookup's answer to c.
func lookupHitters(t *testing.T, c cpClient, want int, body map[string]any) apiv1.HeavyHitterLookupResult {
	t.Helper()
	var r apiv1.HeavyHitterLookupResult
	a := c.must(want, "POST", "/heavy-hitters/lookup", body)
	if want == 200 {
		a.decode(t, &r)
	}
	return r
}

// TestHeavyHitterLookup is ADR-0053 end to end: a managed gateway behind
// a trusted proxy throttles a client IP, which GET /heavy-hitters shows
// only as its keyed digest; an admin's lookup of the IP computes the same
// digest and finds its windows, an IPv4-mapped form included, and is in
// the audit log with the digest but not the IP; every other role is
// refused; a subject that does not fill the template, a policy that is not
// published and an {api_key} template are refused; neither the IP nor the
// secret reaches the control plane's log.
func TestHeavyHitterLookup(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	r := newRoles(t, cp.rest, iss)
	config := fmt.Sprintf(`upstreams: [{name: u, endpoints: [{address: '%s'}]}]
routes: [{name: r, upstream: u, match: {path_prefix: /r}}]
policies:
  - name: per-ip
    level: global
    algorithm: gcra
    local: true
    key_template: '{ip}'
    match: {routes: ['*']}
    limits: [{rate: 5, period: 1h, burst: 5}]
  - name: per-key
    level: consumer
    algorithm: gcra
    local: true
    key_template: '{api_key}'
    match: {routes: ['*']}
    limits: [{rate: 1000, period: 1s, burst: 1000}]
`, upstreamAnswering(t, "ok"))
	v := publishYAML(t, cp.rest, []byte(config), "per-ip and per-key")
	gw := startManaged(t, "gateway-1", grpcAddr, t.TempDir(), "SPILLWAY_GATEWAY_TRUSTED_PROXIES=127.0.0.0/8")
	eventually(t, "gateway-1 live at the published version", func() bool { return liveAt(listReplicas(t, cp.rest, ""), []string{"gateway-1"}, v, 1) })
	policyID := func(name string) int64 {
		var l apiv1.PolicyList
		cp.rest.must(200, "GET", "/policies?name="+name, nil).decode(t, &l)
		if len(l.Items) != 1 || l.Items[0].Id == nil {
			t.Fatalf("policy %s: %+v", name, l)
		}
		return *l.Items[0].Id
	}
	perIP, perKey := policyID("per-ip"), policyID("per-key")

	// 8 requests from one client IP: 5 admitted, 3 denied.
	const ip = "198.51.100.77"
	var denied int
	for range 8 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, gw.url+"/r", nil)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			denied++
		}
	}
	if denied != 3 {
		t.Fatalf("%d of 8 denied, want 3", denied)
	}
	digest := heavyhitters.Pseudonym([]byte(hitterSecret), ratelimit.Key(ip))
	if digest == rlredis.Digest(ip) || digest == heavyhitters.Pseudonym([]byte("another-"+hitterSecret), ratelimit.Key(ip)) {
		t.Fatal("the keyed digest is the unkeyed one, or the same under another secret")
	}
	want := []hitter{{perIP, digest, 0, 0, 3, 0}}
	eventually(t, "the throttled IP in /heavy-hitters", func() bool {
		return fmt.Sprint(hittersOf(listHeavyHitters(t, r.viewer, ""))) == fmt.Sprint(want)
	})
	if body := string(r.viewer.must(200, "GET", "/heavy-hitters", nil).body); strings.Contains(body, ip) || strings.Contains(body, rlredis.Digest(ip)) {
		t.Errorf("GET /heavy-hitters shows the IP or its unkeyed digest: %s", body)
	}

	// The admin's lookup finds the windows; ::ffff:IP is the same client.
	for _, subject := range []string{ip, "::ffff:" + ip} {
		got := lookupHitters(t, r.admin, 200, map[string]any{"policy_id": perIP, "subject": map[string]any{"ip": subject}})
		var sum int64
		for _, w := range got.Items {
			if w.KeyDigest != digest || w.PolicyId != perIP || w.ConsumerId != nil || w.ApiKeyId != nil || w.ShadowDenied != 0 {
				t.Errorf("window %+v", w)
			}
			sum += w.Denied
		}
		if got.KeyDigest != digest || len(got.Items) == 0 || sum != 3 {
			t.Errorf("lookup of %s: %+v, want %s with 3 denies", subject, got, digest)
		}
	}
	other := lookupHitters(t, r.admin, 200, map[string]any{"policy_id": perIP, "subject": map[string]any{"ip": "198.51.100.78"}, "since": time.Now().Add(-time.Hour).Format(time.RFC3339)})
	if len(other.Items) != 0 || other.KeyDigest == digest {
		t.Errorf("lookup of another IP: %+v", other)
	}
	body := map[string]any{"policy_id": perIP, "subject": map[string]any{"ip": ip}}
	for _, c := range []cpClient{r.viewer, r.editor, r.publisher, r.keyAdmin} {
		c.must(403, "POST", "/heavy-hitters/lookup", body)
	}
	cp.rest.as("").must(401, "POST", "/heavy-hitters/lookup", body)
	r.admin.must(400, "POST", "/heavy-hitters/lookup", map[string]any{"policy_id": perIP, "subject": map[string]any{"route": "r"}})
	r.admin.must(404, "POST", "/heavy-hitters/lookup", map[string]any{"policy_id": perKey + 100, "subject": map[string]any{"ip": ip}})
	if a := r.admin.must(400, "POST", "/heavy-hitters/lookup", map[string]any{"policy_id": perKey, "subject": map[string]any{"ip": ip}}); !strings.Contains(string(a.body), "api_key_id") {
		t.Errorf("an {api_key} template: %s", a.body)
	}

	// Three lookups were answered, each audited with the digest, never
	// the IP; the refused ones are not.
	audit := auditPage(t, r.admin, "?entity=heavy_hitters")
	if len(audit.Items) != 3 {
		t.Fatalf("%d audit records, want 3: %+v", len(audit.Items), audit.Items)
	}
	for i, a := range audit.Items {
		d := a.Diff
		if a.Action != "heavy_hitters.lookup" || a.Actor.Sub != "sub-admin" || a.EntityId == nil || *a.EntityId != fmt.Sprint(perIP) ||
			d["policy_id"] != float64(perIP) || (i > 0 && d["key_digest"] != digest) || (i > 0 && d["windows"] == float64(0)) {
			t.Errorf("audit record %d: %+v", i, a)
		}
	}
	raw := string(r.admin.must(200, "GET", "/audit?entity=heavy_hitters", nil).body)
	for name, out := range map[string]string{"the audit log": raw, "the control plane's log": cp.out.String()} {
		if strings.Contains(out, ip) || strings.Contains(out, "198.51.100.78") {
			t.Errorf("the IP appears in %s", name)
		}
	}
}

// TestHeavyHitterConcurrentHeartbeats checks that replicas reporting the
// same keys at once never deadlock (ADR-0051): five replicas send the same
// 100 keys in different orders, 20 times each, all at once. Every
// heartbeat succeeds and every window holds the exact sum.
func TestHeavyHitterConcurrentHeartbeats(t *testing.T) {
	st := openStore(t, migratedDatabase(t))
	keys := make([]*configpb.TopKey, 100)
	for i := range keys {
		keys[i] = &configpb.TopKey{PolicyId: 1, KeyDigest: rlredis.Digest(fmt.Sprint("client-", i)), Denied: 1}
	}
	const replicas, rounds = 5, 20
	errs := make(chan error, replicas*rounds)
	var wg sync.WaitGroup
	for r := range replicas {
		order := slices.Concat(keys[20*r:], keys[:20*r]) // rotated, every other one reversed
		if r%2 == 1 {
			slices.Reverse(order)
		}
		wg.Go(func() {
			for range rounds {
				_, err := st.RecordHeartbeat(t.Context(), fmt.Sprint("gateway-", r), &configpb.HeartbeatRequest{
					ReplicaId: fmt.Sprint("gateway-", r), SchemaVersion: &configpb.SchemaVersion{Major: 1}, TopKeys: order,
				})
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.TopHeavyHitters(t.Context(), store.HeavyHitterFilter{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range rows {
		total += r.Denied
	}
	// The heartbeats may straddle a minute: the windows' sum is exact.
	if len(rows) != len(keys) || total != replicas*rounds*int64(len(keys)) {
		t.Errorf("%d keys, %d denies; want %d, %d", len(rows), total, len(keys), replicas*rounds*len(keys))
	}
}

// TestUnkeyedHeavyHittersDeleted: migration 00006 deletes the windows
// written before it, whose digests are unkeyed (ADR-0053), and nothing
// else.
func TestUnkeyedHeavyHittersDeleted(t *testing.T) {
	ctx := t.Context()
	name := emptyDatabase(t)
	dsn := pgURL(t, "spillway", name)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = pgOwnerPassword
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(filepath.Join("..", "..", "internal", "controlplane", "store", "migrations")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 5); err != nil {
		t.Fatal(err)
	}
	owner := pgConnect(t, dsn, pgOwnerPassword)
	if _, err := owner.Exec(ctx, `INSERT INTO heavy_hitters VALUES (date_trunc('minute', clock_timestamp(), 'UTC'), 1, $1, NULL, NULL, 5, 0)`, rlredis.Digest("198.51.100.77")); err != nil {
		t.Fatal(err)
	}
	applied, err := store.Migrate(ctx, dsn, pgOwnerPassword, pgAppPassword)
	if err != nil || len(applied) == 0 || applied[0] != 6 {
		t.Fatalf("applied %v, %v; want 6 first", applied, err)
	}
	var n int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM heavy_hitters").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d windows left, %v; want none", n, err)
	}
}
