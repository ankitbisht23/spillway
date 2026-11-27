//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/demo"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// toxiproxyAPI is compose's toxiproxy API on the host (docs/ports.md).
const toxiproxyAPI = "http://127.0.0.1:8474"

// proxyToxics returns the toxics of compose's toxiproxy proxy name.
func proxyToxics(t *testing.T, name string) []demo.Toxic {
	t.Helper()
	resp, err := http.Get(toxiproxyAPI + "/proxies/" + name + "/toxics") //nolint:gosec,noctx // G107: compose's toxiproxy
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out []demo.Toxic
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatalf("toxiproxy %s: status %d", name, resp.StatusCode)
	}
	return out
}

// toxicNames is "proxy: toxic toxic …" for each of proxies, in order.
func toxicNames(t *testing.T, proxies ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range proxies {
		var names []string
		for _, x := range proxyToxics(t, p) {
			names = append(names, x.Name)
		}
		slices.Sort(names)
		fmt.Fprintf(&b, "%s: %s; ", p, strings.Join(names, " "))
	}
	return b.String()
}

var (
	demoRedisProxies    = []string{"demo-redis-0", "demo-redis-1", "demo-redis-2", "demo-redis-3", "demo-redis-4", "demo-redis-5"}
	demoUpstreamProxies = []string{"demo-upstream-1", "demo-upstream-2", "demo-upstream-3"}
)

// TestDemoFaults: a demo-mode control plane on Postgres switches the
// faults in compose's toxiproxy (SC-03, ADR-0095 §5). redis-cut on puts
// demo-cut-up and demo-cut-down on the six demo-redis proxies, upstream-slow
// demo-slow on the three demo-upstream proxies, off removes them; the same
// state again changes nothing and is still recorded; getDemo reads the
// states back; only an admin may switch (the four other roles 403); each
// switch is one audit record (demo_fault.update, the name as entity ID,
// before and after, the note, the actor); a missing proxy is 503 naming
// it, with no record, and getDemo shows that fault unknown. The demo
// proxies are created for the test and deleted after it, also on failure,
// which removes any toxic left.
func TestDemoFaults(t *testing.T) {
	tp := demo.NewToxiproxy(toxiproxyAPI, 5*time.Second)
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // t.Context() has ended
		defer cancel()
		for _, p := range demo.DemoProxies() {
			if err := tp.Delete(ctx, p.Name); err != nil {
				t.Errorf("deleting %s: %v", p.Name, err)
			}
		}
	}
	t.Cleanup(cleanup)
	for _, p := range demo.DemoProxies() {
		if err := tp.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	c, iss, _ := startControlPlaneWith(t, &logs, func(s *api.Server) *api.Server { return s.WithDemo(true).WithDemoToxiproxy(toxiproxyAPI) })
	adminTok := iss.token(t, "service-account-spillway-ci", "service-account-spillway-ci", "admin")
	admin := c.as(adminTok)
	states := func() string {
		var d apiv1.Demo
		c.as(iss.token(t, "sub-viewer", "test-viewer", "viewer")).must(200, "GET", "/demo", nil).decode(t, &d)
		var out []string
		for _, f := range d.Faults {
			st := "unknown"
			if f.Active != nil {
				st = fmt.Sprint(*f.Active)
			}
			out = append(out, fmt.Sprintf("%s=%s", f.Name, st))
		}
		return strings.Join(out, " ")
	}
	set := func(cl cpClient, want int, name string, body map[string]any) cpAnswer {
		t.Helper()
		return cl.must(want, "PUT", "/demo/faults/"+name, body)
	}
	if s := states(); s != "redis-cut=false upstream-slow=false" {
		t.Fatalf("before: %s", s)
	}
	for _, role := range []string{"viewer", "editor", "publisher", "key-admin"} {
		set(c.as(iss.token(t, "sub-"+role, "test-"+role, role)), 403, "redis-cut", map[string]any{"active": true})
	}
	none := "demo-redis-0: ; demo-redis-1: ; demo-redis-2: ; demo-redis-3: ; demo-redis-4: ; demo-redis-5: ; demo-upstream-1: ; demo-upstream-2: ; demo-upstream-3: ; "
	all := append(slices.Clone(demoRedisProxies), demoUpstreamProxies...)
	if got := toxicNames(t, all...); got != none {
		t.Fatalf("a refused switch changed toxiproxy: %s", got)
	}

	var f apiv1.DemoFault
	set(admin, 200, "redis-cut", map[string]any{"active": true, "note": "D5 by hand"}).decode(t, &f)
	if f.Name != "redis-cut" || f.Active == nil || !*f.Active {
		t.Errorf("redis-cut on answered %+v", f)
	}
	cut := "demo-redis-0: demo-cut-down demo-cut-up; demo-redis-1: demo-cut-down demo-cut-up; demo-redis-2: demo-cut-down demo-cut-up; " +
		"demo-redis-3: demo-cut-down demo-cut-up; demo-redis-4: demo-cut-down demo-cut-up; demo-redis-5: demo-cut-down demo-cut-up; " +
		"demo-upstream-1: ; demo-upstream-2: ; demo-upstream-3: ; "
	if got := toxicNames(t, all...); got != cut {
		t.Errorf("after redis-cut on: %s", got)
	}
	for _, x := range proxyToxics(t, "demo-redis-4") {
		if x.Type != "timeout" || x.Toxicity != 1 || x.Attributes["timeout"] != 0 || x.Stream != map[string]string{"demo-cut-up": "upstream", "demo-cut-down": "downstream"}[x.Name] {
			t.Errorf("demo-redis-4: %+v", x)
		}
	}
	if s := states(); s != "redis-cut=true upstream-slow=false" {
		t.Errorf("getDemo after redis-cut on: %s", s)
	}
	set(admin, 200, "redis-cut", map[string]any{"active": true}) // the same state: nothing changes
	if got := toxicNames(t, all...); got != cut {
		t.Errorf("after redis-cut on again: %s", got)
	}
	set(admin, 200, "upstream-slow", map[string]any{"active": true})
	for _, p := range demoUpstreamProxies {
		x := proxyToxics(t, p)
		if len(x) != 1 || x[0].Name != "demo-slow" || x[0].Type != "latency" || x[0].Stream != "downstream" || x[0].Attributes["latency"] != 4000 {
			t.Errorf("%s: %+v", p, x)
		}
	}
	if s := states(); s != "redis-cut=true upstream-slow=true" {
		t.Errorf("getDemo with both on: %s", s)
	}
	set(admin, 200, "redis-cut", map[string]any{"active": false})
	set(admin, 200, "upstream-slow", map[string]any{"active": false, "note": "D6 done"})
	if got := toxicNames(t, all...); got != none {
		t.Errorf("after both off: %s", got)
	}
	if s := states(); s != "redis-cut=false upstream-slow=false" {
		t.Errorf("getDemo after both off: %s", s)
	}

	// A missing proxy.
	if err := tp.Delete(t.Context(), "demo-upstream-2"); err != nil {
		t.Fatal(err)
	}
	e := set(admin, 503, "upstream-slow", map[string]any{"active": true}).apiErr(t)
	if e.Code != apiv1.ErrorCodeUnavailable || e.Error != "toxiproxy has no proxy demo-upstream-2 (make demo creates it)" {
		t.Errorf("a missing proxy: %+v", e)
	}
	time.Sleep(time.Second) // getDemo's states are at most a second old
	if s := states(); s != "redis-cut=false upstream-slow=unknown" {
		t.Errorf("getDemo with a missing proxy: %s", s)
	}

	// The audit log: one record per successful switch, newest first.
	var audit apiv1.AuditList
	admin.must(200, "GET", "/audit?entity=demo_fault", nil).decode(t, &audit)
	var got []string
	for _, r := range audit.Items {
		diff, _ := json.Marshal(r.Diff)
		note := ""
		if r.Note != nil {
			note = *r.Note
		}
		if r.Action != "demo_fault.update" || r.Actor.Sub != "service-account-spillway-ci" || !slices.Equal(r.Actor.Roles, []string{"admin"}) || r.RequestId == "" ||
			r.ConfigVersion != nil || r.IdentityVersion != nil {
			t.Errorf("record %+v", r)
		}
		got = append(got, fmt.Sprintf("%s %s %q", *r.EntityId, diff, note))
	}
	want := []string{
		`upstream-slow {"after":{"active":false},"before":{"active":true}} "D6 done"`,
		`redis-cut {"after":{"active":false},"before":{"active":true}} ""`,
		`upstream-slow {"after":{"active":true},"before":{"active":false}} ""`,
		`redis-cut {"after":{"active":true},"before":{"active":true}} ""`,
		`redis-cut {"after":{"active":true},"before":{"active":false}} "D5 by hand"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit records:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(logs.String(), adminTok) || strings.Contains(logs.String(), "eyJ") {
		t.Error("a token appears in the control plane's log")
	}
}
