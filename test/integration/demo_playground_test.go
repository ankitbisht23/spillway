//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	cpdemo "github.com/ankitbisht23/spillway/internal/controlplane/demo"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// playgroundConfig is the demo's three Playground routes on one upstream,
// with plan free at 10/s, burst 10, for every tenant's consumers.
func playgroundConfig(addr string) []byte {
	return fmt.Appendf(nil, `upstreams: [{name: u, endpoints: [{address: '%s'}]}]
routes:
  - {name: orders, upstream: u, match: {path: /v1/orders, methods: [GET]}, auth_mode: api_key}
  - {name: catalog, upstream: u, match: {path: /v1/catalog, methods: [GET]}, auth_mode: api_key}
  - {name: payments, upstream: u, match: {path: /v1/payments, methods: [POST]}, auth_mode: api_key}
policies:
  - name: consumer-plan
    level: consumer
    algorithm: gcra
    local: true
    from_plan: true
    key_template: '{consumer}'
    match: {routes: [orders, catalog, payments]}
plans:
  - name: free
    default_limits: [{policy: consumer-plan, limits: [{rate: 10, period: 1s, burst: 10}]}]
`, addr)
}

// playgroundEvent is one SSE event.
type playgroundEvent struct{ name, data string }

// TestPlaygroundRun: a control plane in demo mode drives a managed gateway
// through the Playground (UI-07, ADR-0096 §3). A viewer starts a custom
// run of free-co on orders at 30/s for 5 s, three times the plan's 10/s;
// its stream shows ticks with allowed and limited answers, the RateLimit
// headers and the replica, then end with totals; the start is one audit
// record as the viewer and the key's issue one as system:playground with
// a 30-minute expiry; another caller cannot see or stop the run; and no
// key appears in either process's log or in any event.
func TestPlaygroundRun(t *testing.T) {
	iss := newTestIssuer(t)
	database := migratedDatabase(t)
	grpcAddr, gwAddr := freeAddr(t), freeAddr(t)
	cp := startCP(t, "controlplane", database, grpcAddr, iss, "SPILLWAY_CP_DEMO_MODE=true",
		"SPILLWAY_CP_DEMO_TOXIPROXY_URL="+toxiproxyAPI, "SPILLWAY_CP_DEMO_GATEWAYS=http://"+gwAddr)
	v := publishYAML(t, cp.rest, playgroundConfig(upstreamAnswering(t, "ok")), "playground")
	tenant, _ := created(t, cp.rest, "/tenants", map[string]any{"name": "free-co", "plan_id": planID(t, cp.rest)})
	created(t, cp.rest, "/consumers", map[string]any{"tenant_id": tenant, "name": "free-co-playground"})
	gw := startManaged(t, "gateway-1", grpcAddr, t.TempDir(), "SPILLWAY_GATEWAY_HTTP_ADDR="+gwAddr)
	gw.waitVersion(t, int(v))

	viewer := cp.rest.as(iss.token(t, "viewer-1", "test-viewer", "viewer"))
	other := cp.rest.as(iss.token(t, "viewer-2", "another-viewer", "viewer"))
	var d apiv1.Demo
	viewer.must(200, "GET", "/demo", nil).decode(t, &d)
	if d.Playground != (apiv1.DemoPlayground{MaxRate: 200, MaxDurationS: 60, ActiveRuns: 0, RunsLeftThisHour: 30}) {
		t.Errorf("getDemo playground %+v", d.Playground)
	}

	var run apiv1.PlaygroundRun
	viewer.must(201, "POST", "/demo/playground/runs", map[string]any{"kind": "custom", "tenant": "free-co", "route": "orders", "rate": 30, "duration_s": 5}).decode(t, &run)
	if run.State != "running" || run.Kind != "custom" || run.DurationS != 5 || run.EndsAt.Sub(run.StartedAt) != 5*time.Second {
		t.Errorf("run %+v", run)
	}
	viewer.must(200, "GET", "/demo", nil).decode(t, &d)
	if d.Playground.ActiveRuns != 1 || d.Playground.RunsLeftThisHour != 29 {
		t.Errorf("getDemo playground during the run %+v", d.Playground)
	}
	other.must(404, "DELETE", "/demo/playground/runs/"+run.Id, nil)
	other.must(404, "GET", "/demo/playground/runs/"+run.Id+"/stream", nil)

	events := readPlayground(t, cp.rest.base, viewer.token, run.Id)
	if len(events) < 4 || events[0].name != "ready" || events[len(events)-1].name != "end" {
		t.Fatalf("events %v", events)
	}
	var ok, limited, forbidden int64
	var last cpdemo.Last
	for _, e := range events[1 : len(events)-1] {
		var tk cpdemo.Tick
		if err := json.Unmarshal([]byte(e.data), &tk); err != nil || e.name != "tick" {
			t.Fatalf("%s %s: %v", e.name, e.data, err)
		}
		c := tk.Tenants["free-co"]
		ok, limited, forbidden = ok+c.OK, limited+c.Limited, forbidden+c.Forbidden
		if l, has := tk.Last["free-co"]; has {
			last = l
		}
	}
	var end cpdemo.End
	if err := json.Unmarshal([]byte(events[len(events)-1].data), &end); err != nil {
		t.Fatal(err)
	}
	tot := end.Totals["free-co"]
	// 150 requests in 5 s at 10/s with a burst of 10: about 60 allowed
	// (each GCRA burst holds a second of the rate, so a host stall does
	// not change the count by itself, ADR-0094 §3).
	if end.Reason != "done" || tot.Sent != 150 || tot.OK < 45 || tot.OK > 75 || tot.Limited < 75 || tot.Forbidden != 0 || tot.OK+tot.Limited != 150 {
		t.Errorf("end %s, totals %+v", end.Reason, tot)
	}
	// The stream opened just after the start; a host stall between the
	// two could cost it the first tick, so the ticks may sum to less.
	if ok > tot.OK || limited > tot.Limited || ok+limited < tot.Sent-60 || forbidden != 0 {
		t.Errorf("ticks: ok %d limited %d forbidden %d; totals %+v", ok, limited, forbidden, tot)
	}
	if last.Replica == "" || last.RateLimit == nil || !strings.Contains(*last.RateLimit, `"consumer-plan"`) || last.RateLimitPolicy == nil {
		t.Errorf("last %+v", last)
	}
	t.Logf("totals %+v, last %+v", tot, last)

	// Audit: the start as the viewer; the key's issue as system:playground,
	// 30 minutes.
	var audit apiv1.AuditList
	cp.rest.must(200, "GET", "/audit?entity=playground_run", nil).decode(t, &audit)
	if len(audit.Items) != 1 || audit.Items[0].Action != "playground_run.create" || audit.Items[0].Actor.Sub != "viewer-1" ||
		audit.Items[0].EntityId == nil || *audit.Items[0].EntityId != run.Id {
		t.Errorf("playground audit %+v", audit.Items)
	}
	cp.rest.must(200, "GET", "/audit?entity=key", nil).decode(t, &audit)
	if len(audit.Items) != 1 || audit.Items[0].Action != "key.issue" || audit.Items[0].Actor.Sub != "system:playground" {
		t.Fatalf("key audit %+v", audit.Items)
	}
	var keys apiv1.ApiKeyList
	consumers := cp.rest.must(200, "GET", "/consumers?name=free-co-playground", nil)
	var cl struct{ Items []struct{ Id int64 } }
	consumers.decode(t, &cl)
	cp.rest.must(200, "GET", fmt.Sprintf("/consumers/%d/keys", cl.Items[0].Id), nil).decode(t, &keys)
	if len(keys.Items) != 1 || keys.Items[0].ExpiresAt == nil || keys.Items[0].ExpiresAt.Sub(keys.Items[0].CreatedAt).Round(time.Second) != 30*time.Minute {
		t.Errorf("the Playground key %+v", keys.Items)
	}
	prefix := keys.Items[0].Prefix

	// Stop after the end: 204, one more record.
	viewer.must(204, "DELETE", "/demo/playground/runs/"+run.Id, nil)
	cp.rest.must(200, "GET", "/audit?entity=playground_run", nil).decode(t, &audit)
	if len(audit.Items) != 2 || !slices.ContainsFunc(audit.Items, func(r apiv1.AuditRecord) bool { return r.Action == "playground_run.delete" }) {
		t.Errorf("after the stop: %+v", audit.Items)
	}

	gw.stop(syscall.SIGTERM)
	cp.stop(syscall.SIGTERM)
	for _, e := range events {
		if strings.Contains(e.data, "spw_") || strings.Contains(e.data, prefix) {
			t.Errorf("an event carries a key or its prefix: %s", e.data)
		}
	}
	for what, out := range map[string]string{"the control plane's log": cp.out.String(), "the gateway's log": gw.out.String()} {
		if strings.Contains(out, "spw_") {
			t.Errorf("%s has a key", what)
		}
	}
	if !strings.Contains(cp.out.String(), "playground run ended") {
		t.Error("the control plane did not log the run's end")
	}
}

// readPlayground reads run id's stream until its end event.
func readPlayground(t *testing.T, base, token, id string) []playgroundEvent {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", base+"/api/v1/demo/playground/runs/"+id+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Fatalf("stream: %d", res.StatusCode)
	}
	var out []playgroundEvent
	var e playgroundEvent
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		switch line := sc.Text(); {
		case strings.HasPrefix(line, "event: "):
			e.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			e.data = strings.TrimPrefix(line, "data: ")
		case line == "" && e.name != "":
			out = append(out, e)
			if e.name == "end" {
				return out
			}
			e = playgroundEvent{}
		}
	}
	return out
}
