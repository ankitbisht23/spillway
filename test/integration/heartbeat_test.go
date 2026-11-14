//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Heartbeats, replicas, live_replicas and the sync age (CP-08, CP-09,
// CP-22, RL-24, ADR-0038). Every wait allows 30 s or more, far above the
// 820 ms host stalls of ADR-0034, so a stall alone cannot fail a check.

// listReplicas is GET /replicas, by name.
func listReplicas(t *testing.T, c cpClient, query string) apiv1.ReplicaList {
	t.Helper()
	var l apiv1.ReplicaList
	c.must(200, "GET", "/replicas"+query, nil).decode(t, &l)
	return l
}

// TestControlPlaneReplicas checks what the control plane records from
// heartbeats (CP-08, CP-22, ADR-0036, ADR-0038): the replicas rows and
// their live flag, live_replicas (< 15 s), each version's first report set
// once, the applied list of a config version, GET /replicas for every role
// with its pages, the prune after 7 days, and no bearer token in a log
// line.
func TestControlPlaneReplicas(t *testing.T) {
	var logs syncBuffer
	c, iss, db := startControlPlane(t, &logs)
	r := newRoles(t, c, iss)
	st := openStore(t, db)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	ctx := t.Context()

	importYAML(t, r.editor, 200, []byte("plans: [{name: free}]\n"), `"1"`, "")
	_, tag := validate(t, r.viewer)
	var v apiv1.ConfigVersion
	r.publisher.must(200, "POST", "/config/publish", note("a plan"), "If-Match", tag).decode(t, &v)

	beat := func(name string, cv int64, connected bool, rejected int64, reason string) int32 {
		t.Helper()
		live, err := st.RecordHeartbeat(ctx, name, &configpb.HeartbeatRequest{
			ReplicaId: name, ConfigVersion: cv, IdentityVersion: 1, SchemaVersion: &configpb.SchemaVersion{Major: 1},
			WatchConnected: connected, RejectedConfigVersion: rejected, RejectedReason: reason,
		})
		if err != nil {
			t.Fatal(err)
		}
		return live
	}
	if live := beat("gateway-1", v.Version, true, 0, ""); live != 1 {
		t.Fatalf("live_replicas %d, want 1", live)
	}
	beat("gateway-2", v.Version, true, v.Version+1, `policy "p":`+strings.Repeat("é", 1500)) // cut at 2,000 bytes, inside an é
	if live := beat("gateway-3", 0, false, 0, ""); live != 3 {
		t.Fatalf("live_replicas %d, want 3", live)
	}
	var first time.Time
	if err := owner.QueryRow(ctx, "SELECT first_reported_at FROM replica_versions WHERE replica_id = 'gateway-1' AND stream = 'config'").Scan(&first); err != nil {
		t.Fatal(err)
	}
	// gateway-3's last heartbeat 20 s ago: not live, not counted.
	if _, err := owner.Exec(ctx, "UPDATE replicas SET last_heartbeat_at = clock_timestamp() - interval '20 seconds' WHERE id = 'gateway-3'"); err != nil {
		t.Fatal(err)
	}
	if live := beat("gateway-1", v.Version, true, 0, ""); live != 2 {
		t.Errorf("live_replicas %d with gateway-3 stale, want 2", live)
	}

	l := listReplicas(t, r.viewer, "")
	if len(l.Items) != 3 || l.NextCursor != nil {
		t.Fatalf("replicas %+v", l)
	}
	g1, g2, g3 := l.Items[0], l.Items[1], l.Items[2]
	if g1.Id != "gateway-1" || !g1.Live || g1.ConfigVersion != v.Version || g1.IdentityVersion != 1 || !g1.WatchConnected ||
		g1.LastSyncedAt == nil || g1.RejectedConfigVersion != nil || g1.RejectedReason != nil || g1.SchemaVersion.Major != 1 {
		t.Errorf("gateway-1: %+v", g1)
	}
	if g2.RejectedConfigVersion == nil || *g2.RejectedConfigVersion != v.Version+1 || g2.RejectedReason == nil ||
		len(*g2.RejectedReason) > 2000 || !strings.HasPrefix(*g2.RejectedReason, `policy "p":é`) || !strings.HasSuffix(*g2.RejectedReason, "é") {
		t.Errorf("gateway-2's refusal: %v %v", g2.RejectedConfigVersion, g2.RejectedReason)
	}
	if g3.Id != "gateway-3" || g3.Live || g3.WatchConnected || g3.LastSyncedAt != nil || g3.ConfigVersion != 0 {
		t.Errorf("gateway-3: %+v", g3)
	}
	// Pages by name.
	p1 := listReplicas(t, r.admin, "?limit=2")
	if len(p1.Items) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1: %+v", p1)
	}
	if p2 := listReplicas(t, r.admin, "?limit=2&cursor="+*p1.NextCursor); len(p2.Items) != 1 || p2.Items[0].Id != "gateway-3" || p2.NextCursor != nil {
		t.Errorf("page 2: %+v", p2)
	}
	r.viewer.must(400, "GET", "/replicas?cursor=***", nil)
	for _, cl := range []cpClient{r.editor, r.publisher, r.keyAdmin} {
		cl.must(200, "GET", "/replicas", nil)
	}
	c.must(401, "GET", "/replicas", nil)

	// The applied list: each replica's first report, set once.
	var got apiv1.ConfigVersion
	r.viewer.must(200, "GET", fmt.Sprintf("/config/versions/%d", v.Version), nil).decode(t, &got)
	if got.Applied == nil || len(*got.Applied) != 2 || (*got.Applied)[0].Replica != "gateway-1" || !(*got.Applied)[0].At.Equal(first) ||
		(*got.Applied)[1].Replica != "gateway-2" || (*got.Applied)[0].At.Before(got.PublishedAt) {
		t.Errorf("applied %+v, gateway-1 first at %v", got.Applied, first)
	}

	// The prune: reports older than 7 days, replicas silent for 7 days.
	if _, err := owner.Exec(ctx, `UPDATE replica_versions SET first_reported_at = clock_timestamp() - interval '8 days' WHERE replica_id = 'gateway-2';
		UPDATE replicas SET last_heartbeat_at = clock_timestamp() - interval '8 days' WHERE id = 'gateway-3'`); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneReplicas(ctx)
	if err != nil || n != 3 { // gateway-2's two reports, gateway-3's row
		t.Errorf("pruned %d rows, %v; want 3", n, err)
	}
	if l := listReplicas(t, r.viewer, ""); len(l.Items) != 2 || l.Items[1].Id != "gateway-2" {
		t.Errorf("after the prune: %+v", l.Items)
	}
	for _, cl := range []cpClient{r.editor, r.publisher, r.admin, r.viewer, r.keyAdmin} {
		if sig := cl.token[strings.LastIndexByte(cl.token, '.')+1:]; strings.Contains(logs.String(), sig) {
			t.Error("a bearer token appears in a log line")
		}
	}
}

// limitedConfig is a route r to an upstream at addr with a local GCRA
// policy of 20 an hour, burst 20, per x-client header: 20 / replicas per
// gateway (RL-31).
func limitedConfig(addr string) []byte {
	return []byte(fmt.Sprintf(`upstreams: [{name: u, endpoints: [{address: '%s'}]}]
routes: [{name: r, upstream: u, match: {path_prefix: /r}}]
policies:
  - name: per-client
    level: global
    algorithm: gcra
    local: true
    key_template: '{header:x-client}'
    match: {routes: ['*']}
    limits: [{rate: 20, period: 1h, burst: 20}]
`, addr))
}

// publishYAML imports body into the working set and publishes it.
func publishYAML(t *testing.T, c cpClient, body []byte, msg string) int64 {
	t.Helper()
	_, ws := exportYAML(t, c, "")
	importYAML(t, c, 200, body, ws, "")
	_, tag := validate(t, c)
	var v struct{ Version int64 }
	c.must(200, "POST", "/config/publish", map[string]any{"note": msg}, "If-Match", tag).decode(t, &v)
	return v.Version
}

var clientKeys atomic.Int64

// admitted sends GET url with a new x-client key until the first 429, at
// most 30 times, and returns how many were admitted: the gateway's
// per-replica burst.
func admitted(t *testing.T, url string) int {
	t.Helper()
	key := fmt.Sprint("client-", clientKeys.Add(1))
	for n := range 30 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		req.Header.Set("X-Client", key)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			return n
		default:
			t.Fatalf("GET %s: %d", url, resp.StatusCode)
		}
	}
	return 30
}

// liveAt reports whether the replicas named are live, synced, at config
// version v and identity version iv.
func liveAt(l apiv1.ReplicaList, names []string, v, iv int64) bool {
	found := 0
	for _, r := range l.Items {
		for _, n := range names {
			if r.Id == n && r.Live && r.WatchConnected && r.LastSyncedAt != nil && r.ConfigVersion == v && r.IdentityVersion == iv {
				found++
			}
		}
	}
	return found == len(names)
}

// TestHeartbeatsAndLiveReplicas checks heartbeats end to end with five
// managed host gateways (CP-08, RL-24, RL-31, ADR-0038): all five are
// listed live at the applied versions and in the version's applied list;
// live_replicas divides the per-replica limits (20 / 5 = 4); four that
// stop drop out after 15 s and the fifth's limit goes back to 20; a
// version a gateway refuses is reported.
func TestHeartbeatsAndLiveReplicas(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	v := publishYAML(t, cp.rest, limitedConfig(upstreamAnswering(t, "ok")), "limited")
	names := []string{"gateway-1", "gateway-2", "gateway-3", "gateway-4", "gateway-5"}
	var gws []*hostGW
	for _, n := range names {
		gws = append(gws, startManaged(t, n, grpcAddr, t.TempDir()))
	}
	eventually(t, "five replicas live at the published version", func() bool { return liveAt(listReplicas(t, cp.rest, ""), names, v, 1) })
	var got apiv1.ConfigVersion
	cp.rest.must(200, "GET", fmt.Sprintf("/config/versions/%d", v), nil).decode(t, &got)
	if got.Applied == nil || len(*got.Applied) != 5 {
		t.Errorf("version %d applied on %v", v, got.Applied)
	}
	eventually(t, "gateway-1 admitting 20 / 5 live", func() bool { return admitted(t, gws[0].url+"/r") == 4 })

	// Four stop: they leave live_replicas once their last heartbeat is 15 s
	// old, 10 to 15 s after the stop.
	stopped := time.Now()
	var wg sync.WaitGroup
	for _, g := range gws[1:] {
		wg.Go(func() { g.stop(syscall.SIGTERM) })
	}
	wg.Wait()
	eventually(t, "gateway-2 … gateway-5 not live", func() bool {
		for _, r := range listReplicas(t, cp.rest, "").Items {
			if r.Id != "gateway-1" && r.Live {
				return false
			}
		}
		return true
	})
	dropped := time.Since(stopped)
	t.Logf("the stopped gateways left live_replicas %v after the stop", dropped.Round(10*time.Millisecond))
	if dropped < 5*time.Second {
		t.Errorf("the stopped gateways left after %v; they stay live 15 s from their last heartbeat, at most 5 s before the stop", dropped)
	}
	eventually(t, "gateway-1 admitting 20 / 1 live", func() bool { return admitted(t, gws[0].url+"/r") == 20 })

	// A version with a Redis policy, which these gateways (no Redis) refuse.
	bad := strings.Replace(string(limitedConfig(upstreamAnswering(t, "ok"))), "local: true", "local: false", 1)
	v2 := publishYAML(t, cp.rest, []byte(bad), "a Redis policy")
	eventually(t, "gateway-1 reporting the refused version", func() bool {
		for _, r := range listReplicas(t, cp.rest, "").Items {
			if r.Id == "gateway-1" {
				return r.Live && r.ConfigVersion == v && r.RejectedConfigVersion != nil && *r.RejectedConfigVersion == v2 &&
					r.RejectedReason != nil && strings.Contains(*r.RejectedReason, "SPILLWAY_GATEWAY_REDIS_ADDRS is not set")
			}
		}
		return false
	})
}

// TestControlPlaneOutage is ADR-0042's control-plane outage at 30 s, the
// short form CI runs (NFR-12, CP-08): three managed gateways under load
// while the control plane is down. Every request succeeds; the versions
// and readiness stay; gw_config_sync_age_seconds grows with the outage
// (at least its length minus 10 s); once the control plane is back, every
// sync age is under 10 s and all three are listed live, within 60 s.
func TestControlPlaneOutage(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	v := publishRoute(t, cp.rest, upstreamAnswering(t, "ok"), "base")
	names := []string{"gateway-1", "gateway-2", "gateway-3"}
	var gws []*hostGW
	for _, n := range names {
		gws = append(gws, startManaged(t, n, grpcAddr, t.TempDir()))
	}
	eventually(t, "three replicas live", func() bool { return liveAt(listReplicas(t, cp.rest, ""), names, v, 1) })

	// 150 rps in all, 50 per gateway, until the end.
	var sent, failed atomic.Int64
	ctx, stopLoad := context.WithCancel(context.Background())
	var load sync.WaitGroup
	client := &http.Client{Timeout: 20 * time.Second}
	for _, g := range gws {
		load.Go(func() {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				sent.Add(1)
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, g.url+"/r", nil)
				resp, err := client.Do(req)
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					failed.Add(1)
				}
			}
		})
	}
	defer func() {
		stopLoad()
		load.Wait()
	}()

	time.Sleep(3 * time.Second)
	tStop := time.Now()
	cp.stop(syscall.SIGTERM)
	const outage = 30 * time.Second
	var lastAge []float64
	for time.Since(tStop) < outage {
		time.Sleep(time.Second)
		lastAge = lastAge[:0]
		for _, g := range gws {
			sentAt := time.Now()
			age := g.metric("gw_config_sync_age_seconds")
			if !g.ready() || g.metric("gw_config_version") != float64(v) || g.metric("gw_identity_version") != 1 {
				t.Fatalf("%s during the outage: ready %v, versions %v and %v", g.name, g.ready(), g.metric("gw_config_version"), g.metric("gw_identity_version"))
			}
			if floor := sentAt.Sub(tStop).Seconds() - 10; age < floor {
				t.Fatalf("%s: sync age %.1f s at %.1f s into the outage, want at least %.1f", g.name, age, sentAt.Sub(tStop).Seconds(), floor)
			}
			lastAge = append(lastAge, age)
		}
	}
	for i, age := range lastAge {
		if age < (outage - 10*time.Second).Seconds() {
			t.Errorf("%s: sync age %.1f s at the end of a %v outage", names[i], age, outage)
		}
	}

	cp = startCP(t, "controlplane-2", db, grpcAddr, iss)
	tHealthy := time.Now()
	for time.Since(tHealthy) < 60*time.Second {
		synced := liveAt(listReplicas(t, cp.rest, ""), names, v, 1)
		for _, g := range gws {
			synced = synced && g.metric("gw_config_sync_age_seconds") < 10
		}
		if synced {
			break
		}
		time.Sleep(time.Second)
	}
	recovered := time.Since(tHealthy)
	if recovered >= 60*time.Second {
		t.Errorf("not recovered 60 s after the control plane came back: %+v", listReplicas(t, cp.rest, "").Items)
	}
	time.Sleep(2 * time.Second) // load after the recovery
	stopLoad()
	load.Wait()
	t.Logf("outage %v: sync ages at its end %v; recovered %v after the control plane was ready; %d requests, %d failed",
		outage, lastAge, recovered.Round(10*time.Millisecond), sent.Load(), failed.Load())
	if failed.Load() != 0 {
		t.Errorf("%d of %d requests failed", failed.Load(), sent.Load())
	}
}
