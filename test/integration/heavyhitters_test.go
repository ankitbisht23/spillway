//go:build integration

package integration

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
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
// /heavy-hitters with its denies and shadow would-denies, under the
// digest of its filled key template; the key itself appears in no answer
// and in no log of the gateway or the control plane.
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
	digest := rlredis.Digest(key)
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
		if strings.Contains(out, key) {
			t.Errorf("the raw key appears in %s", name)
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
