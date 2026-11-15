//go:build integration

package integration

import (
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/controlplane/live"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/proxy"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// The shadow report (UI-01, ADR-0060): GET /policies/{id}/shadow-report on
// Postgres, its counts from compose's Prometheus.

// serveWithPrometheus serves the API over database db with the shadow
// report's counts from the Prometheus at promURL ("" for none).
func serveWithPrometheus(t *testing.T, db, promURL string, iss *testIssuer, log *syncBuffer) cpClient {
	t.Helper()
	allowed, err := proxy.ParseAllowlist("172.29.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	s := api.New(openStore(t, db), allowed, net.DefaultResolver, api.OIDC{Verifier: iss.verifier(t)}, secret.New(hitterSecret), slog.New(slog.NewJSONHandler(log, nil)))
	if promURL != "" {
		u, err := live.ParsePrometheusURL(promURL)
		if err != nil {
			t.Fatal(err)
		}
		s.WithPrometheus(live.NewPrometheus(u, nil))
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return cpClient{t: t, base: srv.URL}
}

func shadowReport(t *testing.T, c cpClient, id int64, window string) apiv1.ShadowReport {
	t.Helper()
	var r apiv1.ShadowReport
	c.must(200, "GET", fmt.Sprintf("/policies/%d/shadow-report%s", id, window), nil).decode(t, &r)
	return r
}

// TestShadowReport checks the report: only a policy of the latest
// published version (404 otherwise, also before the first publish); its
// window (1h by default, 15m to 7d, nothing else); its keys from the
// heavy-hitter windows by policy ID, most shadow would-denies first, at
// most 50, with the total of all; keys_since; the counts from compose's
// Prometheus, which accepts the query (a policy no gateway ran has 0
// evaluated and no share); null counts with the reason without Prometheus
// or with one that is down; every role; and no token in a log line.
func TestShadowReport(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	var logs syncBuffer
	c := serveWithPrometheus(t, db, "http://127.0.0.1:9091", iss, &logs)
	r := newRoles(t, c, iss)
	st := openStore(t, db)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	ctx := t.Context()

	created(t, r.editor, "/upstreams", ordersUpstream)
	created(t, r.editor, "/routes", ordersRoute)
	pol := gcraPolicy("shadow-per-consumer", "orders-api")
	pol["mode"] = "shadow"
	id, _ := created(t, r.editor, "/policies", pol)
	if e := r.viewer.must(404, "GET", fmt.Sprintf("/policies/%d/shadow-report", id), nil).apiErr(t); !strings.Contains(e.Error, "not in the latest published config") {
		t.Errorf("before the first publish: %+v", e)
	}
	_, tag := validate(t, r.publisher)
	var v apiv1.ConfigVersion
	r.publisher.must(200, "POST", "/config/publish", note("shadow-per-consumer in shadow"), "If-Match", tag).decode(t, &v)
	unpublished, _ := created(t, r.editor, "/policies", gcraPolicy("not-yet", "orders-api"))
	r.viewer.must(404, "GET", fmt.Sprintf("/policies/%d/shadow-report", unpublished), nil)
	for _, w := range []string{"?window=2h", "?window=", "?window=7D"} {
		r.viewer.must(400, "GET", fmt.Sprintf("/policies/%d/shadow-report%s", id, w), nil)
	}

	// No windows yet.
	rep := shadowReport(t, r.viewer, id, "")
	if rep.PolicyId != id || rep.PolicyName != "shadow-per-consumer" || rep.Mode != "shadow" || rep.ConfigVersion != v.Version ||
		rep.Window != "1h" || rep.Until.Sub(rep.Since) != time.Hour || len(rep.Keys) != 0 || rep.KeysShadowDeniedTotal != 0 || rep.KeysSince != nil {
		t.Errorf("an empty report: %+v", rep)
	}
	if rep.MetricsError != nil || rep.Evaluated == nil || *rep.Evaluated != 0 || rep.WouldDeny == nil || *rep.WouldDeny != 0 || rep.Share != nil ||
		rep.EnforceEvaluated == nil || *rep.EnforceEvaluated != 0 || rep.EnforceShare != nil {
		t.Errorf("counts from Prometheus for a policy no gateway ran: %+v", rep)
	}
	if rep.MetricsSince == nil || rep.MetricsSince.Before(rep.Since) || rep.MetricsSince.After(rep.Until) {
		t.Errorf("metrics_since %v, want within [%v, %v]", rep.MetricsSince, rep.Since, rep.Until)
	}

	// Windows now (two heartbeats), 2 hours and 3 days ago, and another
	// policy's, which the report leaves out.
	d1, d2, d3 := rlredis.Digest("client-1"), rlredis.Digest("client-2"), rlredis.Digest("client-3")
	beat := func(keys ...*configpb.TopKey) {
		t.Helper()
		if _, err := st.RecordHeartbeat(ctx, "gateway-1", &configpb.HeartbeatRequest{ReplicaId: "gateway-1", SchemaVersion: &configpb.SchemaVersion{Major: 1}, TopKeys: keys}); err != nil {
			t.Fatal(err)
		}
	}
	beat(&configpb.TopKey{PolicyId: id, KeyDigest: d1, ConsumerId: 3, ApiKeyId: 30, ShadowDenied: 5},
		&configpb.TopKey{PolicyId: id, KeyDigest: d2, Denied: 9},
		&configpb.TopKey{PolicyId: id + 100, KeyDigest: d3, ShadowDenied: 50})
	beat(&configpb.TopKey{PolicyId: id, KeyDigest: d2, ShadowDenied: 5, Denied: 1})
	for _, w := range []struct {
		ago    string
		digest string
		shadow int
	}{{"2 hours", d3, 40}, {"3 days", d1, 100}} {
		if _, err := owner.Exec(ctx, `INSERT INTO heavy_hitters VALUES (date_trunc('minute', clock_timestamp(), 'UTC') - $1::interval, $2, $3, NULL, NULL, 0, $4)`, w.ago, id, w.digest, w.shadow); err != nil {
			t.Fatal(err)
		}
	}
	type key struct {
		digest           string
		consumer, apiKey int64
		shadow, denied   int64
	}
	keysOf := func(r apiv1.ShadowReport) []key {
		out := make([]key, len(r.Keys))
		for i, k := range r.Keys {
			out[i] = key{digest: k.KeyDigest, shadow: k.ShadowDenied, denied: k.Denied}
			if k.ConsumerId != nil {
				out[i].consumer = *k.ConsumerId
			}
			if k.ApiKeyId != nil {
				out[i].apiKey = *k.ApiKeyId
			}
		}
		return out
	}
	for _, tc := range []struct {
		window string
		want   []key
		total  int64
		oldest bool // keys_since is the oldest window, 3 days ago, not the window's start
	}{
		{"?window=15m", []key{{d2, 0, 0, 5, 10}, {d1, 3, 30, 5, 0}}, 10, false},
		{"?window=1h", []key{{d2, 0, 0, 5, 10}, {d1, 3, 30, 5, 0}}, 10, false},
		{"?window=24h", []key{{d3, 0, 0, 40, 0}, {d2, 0, 0, 5, 10}, {d1, 3, 30, 5, 0}}, 50, false},
		{"?window=7d", []key{{d1, 3, 30, 105, 0}, {d3, 0, 0, 40, 0}, {d2, 0, 0, 5, 10}}, 150, true},
	} {
		rep := shadowReport(t, r.viewer, id, tc.window)
		if got := keysOf(rep); fmt.Sprint(got) != fmt.Sprint(tc.want) || rep.KeysShadowDeniedTotal != tc.total {
			t.Errorf("%s: keys %v total %d, want %v total %d", tc.window, got, rep.KeysShadowDeniedTotal, tc.want, tc.total)
		}
		switch {
		case rep.KeysSince == nil:
			t.Errorf("%s: no keys_since", tc.window)
		case !tc.oldest && !rep.KeysSince.Equal(rep.Since):
			t.Errorf("%s: keys_since %v, want the window's start %v: older windows are kept", tc.window, rep.KeysSince, rep.Since)
		case tc.oldest && (time.Since(*rep.KeysSince) > 72*time.Hour+2*time.Minute || time.Since(*rep.KeysSince) < 72*time.Hour-time.Minute):
			t.Errorf("%s: keys_since %v, want the oldest window's start, about 3 days ago", tc.window, rep.KeysSince)
		}
	}
	// Once the 3-day-old window is pruned, the 7 d window's keys start 2 hours ago.
	if _, err := owner.Exec(ctx, `DELETE FROM heavy_hitters WHERE window_start < clock_timestamp() - interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	if rep := shadowReport(t, r.viewer, id, "?window=7d"); rep.KeysSince == nil || time.Since(*rep.KeysSince) > 2*time.Hour+2*time.Minute || time.Since(*rep.KeysSince) < 2*time.Hour-time.Minute {
		t.Errorf("7d after the prune: keys_since %v, want about 2 hours ago", rep.KeysSince)
	}

	// At most 50 keys; the total counts them all.
	if _, err := owner.Exec(ctx, `INSERT INTO heavy_hitters
		SELECT date_trunc('minute', clock_timestamp(), 'UTC') - interval '5 minutes', $1, 'k' || lpad(i::text, 21, '0'), NULL, NULL, 0, 1
		FROM generate_series(1, 60) i`, id); err != nil {
		t.Fatal(err)
	}
	if rep := shadowReport(t, r.viewer, id, "?window=15m"); len(rep.Keys) != 50 || rep.KeysShadowDeniedTotal != 70 || rep.Keys[0].KeyDigest != d2 {
		t.Errorf("61 keys: %d listed, total %d, first %s; want 50, 70, %s", len(rep.Keys), rep.KeysShadowDeniedTotal, rep.Keys[0].KeyDigest, d2)
	}

	for _, cl := range []cpClient{r.editor, r.publisher, r.keyAdmin, r.admin} {
		cl.must(200, "GET", fmt.Sprintf("/policies/%d/shadow-report", id), nil)
	}
	c.must(401, "GET", fmt.Sprintf("/policies/%d/shadow-report", id), nil)

	// Without Prometheus, or with one that does not answer: the keys, and
	// null counts with the reason.
	for _, tc := range []struct {
		prom string
		want apiv1.ShadowReportMetricsError
	}{{"", apiv1.PrometheusNotConfigured}, {"http://" + freeAddr(t), apiv1.PrometheusUnavailable}} {
		c2 := serveWithPrometheus(t, db, tc.prom, iss, &logs).as(iss.token(t, "sub-viewer", "test-viewer", "viewer"))
		rep := shadowReport(t, c2, id, "?window=15m")
		if rep.MetricsError == nil || *rep.MetricsError != tc.want || rep.Evaluated != nil || rep.WouldDeny != nil || rep.Share != nil ||
			rep.EnforceEvaluated != nil || rep.Denied != nil || rep.EnforceShare != nil || rep.MetricsSince != nil || len(rep.Keys) != 50 {
			t.Errorf("Prometheus %q: %+v", tc.prom, rep)
		}
	}
	if !strings.Contains(logs.String(), "shadow report: Prometheus query failed") {
		t.Error("the failed Prometheus query was not logged")
	}
	for _, cl := range []cpClient{r.editor, r.publisher, r.admin, r.viewer, r.keyAdmin} {
		if sig := cl.token[strings.LastIndexByte(cl.token, '.')+1:]; strings.Contains(logs.String(), sig) {
			t.Error("a bearer token appears in a log line")
		}
	}
}
