//go:build demo

package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The demo Grafana and the demo proxy on the running demo (SC-05, SC-09,
// ADR-0096 §4, ADR-0097 §1), from the host through Caddy on 8888, as an
// anonymous visitor sees them.

const (
	proxyURL      = "http://127.0.0.1:8888"
	demoGrafana   = proxyURL + "/grafana"
	devGrafanaURL = "http://127.0.0.1:3000"
)

// web is the visitor's client: no cookies are kept, and a redirect is
// returned, not followed.
var web = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// visit sends one anonymous request (basic auth when user is set) and
// returns the status, the headers and up to 1 MiB of the body.
func visit(t *testing.T, method, url, body, user, password string) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := web.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, b
}

// TestDemoProxyAndGrafana: through the demo proxy an anonymous visitor
// gets the UI and Keycloak's public endpoints but not Keycloak's admin
// console or admin API (ADR-0097 §1), and the demo Grafana as a Viewer
// (ADR-0096 §4): its root URL is the proxy's /grafana/, its only
// datasource is Prometheus, its only dashboard is the Demo dashboard,
// which is also the home page and which the visitor cannot change; no
// login form, no basic auth, no Loki or Tempo. The dev Grafana still
// refuses anonymous requests.
func TestDemoProxyAndGrafana(t *testing.T) {
	password := setting(t, "GRAFANA_DEMO_ADMIN_PASSWORD").Reveal() // the demo Grafana's admin, unusable from outside
	for _, c := range []struct {
		path string
		want int
	}{
		{"/", 200}, {"/api/ui-config", 200}, {"/auth/realms/spillway/.well-known/openid-configuration", 200},
		{"/auth/admin", 404}, {"/auth/admin/", 404}, {"/auth/admin/realms/spillway", 404}, {"/auth/admin/master/console/", 404},
		{"/grafana/api/health", 200},
	} {
		code, h, _ := visit(t, "GET", proxyURL+c.path, "", "", "")
		if code != c.want {
			t.Errorf("GET %s through the demo proxy: %d, want %d", c.path, code, c.want)
		}
		if s := h.Get("Server"); s != "" {
			t.Errorf("GET %s: Server: %s, want none", c.path, s)
		}
	}

	var settings struct {
		AppURL      string `json:"appUrl"`
		Datasources map[string]struct {
			UID  string `json:"uid"`
			Type string `json:"type"`
		} `json:"datasources"`
		AnonymousEnabled bool `json:"anonymousEnabled"`
		DisableLogin     bool `json:"disableLoginForm"`
		ExploreEnabled   bool `json:"exploreEnabled"`
	}
	code, _, b := visit(t, "GET", demoGrafana+"/api/frontend/settings", "", "", "")
	if err := json.Unmarshal(b, &settings); code != 200 || err != nil {
		t.Fatalf("the demo Grafana's settings: %d %v", code, err)
	}
	var sources []string
	for name, d := range settings.Datasources {
		if d.Type != "datasource" { // not one of Grafana's built-ins (-- Grafana --, -- Mixed --, -- Dashboard --)
			sources = append(sources, name+"/"+d.UID+"/"+d.Type)
		}
	}
	if !slices.Equal(sources, []string{"Prometheus/prometheus/prometheus"}) {
		t.Errorf("the demo Grafana's datasources: %v, want exactly Prometheus", sources)
	}
	if settings.AppURL != "http://localhost:8888/grafana/" || !settings.AnonymousEnabled || !settings.DisableLogin || settings.ExploreEnabled {
		t.Errorf("the demo Grafana: appUrl %q, anonymous %v, login form disabled %v, Explore %v; want http://localhost:8888/grafana/, true, true, false",
			settings.AppURL, settings.AnonymousEnabled, settings.DisableLogin, settings.ExploreEnabled)
	}

	var found []struct {
		UID  string `json:"uid"`
		Type string `json:"type"`
	}
	code, _, b = visit(t, "GET", demoGrafana+"/api/search", "", "", "")
	if err := json.Unmarshal(b, &found); code != 200 || err != nil || fmt.Sprint(found) != "[{spillway-demo dash-db}]" {
		t.Errorf("the demo Grafana's dashboards: %d %v %+v, want only spillway-demo", code, err, found)
	}
	var dash struct {
		Meta struct {
			CanEdit, CanSave, CanAdmin, Provisioned bool
		} `json:"meta"`
	}
	code, _, b = visit(t, "GET", demoGrafana+"/api/dashboards/uid/spillway-demo", "", "", "")
	if err := json.Unmarshal(b, &dash); code != 200 || err != nil || dash.Meta.CanEdit || dash.Meta.CanSave || dash.Meta.CanAdmin || !dash.Meta.Provisioned {
		t.Errorf("the Demo dashboard for a visitor: %d %v %+v, want readable, provisioned, not editable", code, err, dash.Meta)
	}
	if code, _, b = visit(t, "GET", demoGrafana+"/api/dashboards/home", "", "", ""); code != 200 || !bytes.Contains(b, []byte("spillway-demo")) {
		t.Errorf("the demo Grafana's home: %d %.200s, want the Demo dashboard", code, b)
	}

	// Nothing a visitor may change or open beyond the dashboard.
	for _, c := range []struct{ method, path, body, user, password, what string }{
		{"POST", "/api/dashboards/db", `{"dashboard":{"title":"x"}}`, "", "", "saving a dashboard"},
		{"POST", "/api/datasources", `{"name":"x","type":"loki","url":"http://loki:3100","access":"proxy"}`, "", "", "adding a datasource"},
		{"POST", "/api/snapshots", `{"dashboard":{"title":"x"}}`, "", "", "a snapshot"},
		{"POST", "/login", `{"user":"admin","password":"` + password + `"}`, "", "", "the login form with the admin password"},
		{"GET", "/api/admin/settings", "", "admin", password, "basic auth with the admin password"},
		{"GET", "/api/datasources/uid/loki", "", "", "", "Loki"},
		{"GET", "/api/datasources/uid/tempo", "", "", "", "Tempo"},
	} {
		code, h, _ := visit(t, c.method, demoGrafana+c.path, c.body, c.user, c.password)
		if code < 400 || strings.Contains(h.Get("Set-Cookie"), "grafana_session=") {
			t.Errorf("%s (%s %s): %d, cookie %v; want refused", c.what, c.method, c.path, code, h.Get("Set-Cookie") != "")
		}
	}

	if code, _, _ := visit(t, "GET", devGrafanaURL+"/api/search", "", "", ""); code != 401 {
		t.Errorf("the dev Grafana answered an anonymous search with %d, want 401 (no anonymous access)", code)
	}
}

// panelTarget is one query of the Demo dashboard.
type panelTarget struct {
	panel, refID, expr string
}

// demoTargets reads every panel query of deploy/grafana/demo/demo.json.
func demoTargets(t *testing.T) []panelTarget {
	t.Helper()
	b, err := os.ReadFile("../../deploy/grafana/demo/demo.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				RefID string `json:"refId"`
				Expr  string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	var out []panelTarget
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			out = append(out, panelTarget{p.Title, tg.RefID, tg.Expr})
		}
	}
	return out
}

// series is one series a query returned: its labels and its values, NaN
// and missing points left out.
type series struct {
	labels map[string]string
	values []float64
}

// grafanaQuery runs expr as the dashboard does, through the demo Grafana's
// query API as an anonymous Viewer, over [from, to] in 5 s steps.
func grafanaQuery(t *testing.T, expr string, from, to time.Time) ([]series, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"from": strconv.FormatInt(from.UnixMilli(), 10),
		"to":   strconv.FormatInt(to.UnixMilli(), 10),
		"queries": []map[string]any{{
			"refId":         "A",
			"datasource":    map[string]string{"type": "prometheus", "uid": "prometheus"},
			"expr":          expr,
			"range":         true,
			"intervalMs":    5000,
			"maxDataPoints": 2000,
		}},
	})
	code, _, b := visit(t, "POST", demoGrafana+"/api/ds/query", string(body), "", "")
	if code != 200 {
		return nil, fmt.Errorf("status %d: %.300s", code, b)
	}
	var r struct {
		Results map[string]struct {
			Error  string `json:"error"`
			Frames []struct {
				Schema struct {
					Fields []struct {
						Labels map[string]string `json:"labels"`
					} `json:"fields"`
				} `json:"schema"`
				Data struct {
					Values [][]*float64 `json:"values"`
				} `json:"data"`
			} `json:"frames"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	res := r.Results["A"]
	if res.Error != "" {
		return nil, fmt.Errorf("query error: %s", res.Error)
	}
	var out []series
	for _, f := range res.Frames {
		for i := 1; i < len(f.Data.Values) && i < len(f.Schema.Fields); i++ { // values[0] is the time field
			s := series{labels: f.Schema.Fields[i].Labels}
			for _, v := range f.Data.Values[i] {
				if v != nil { // Grafana sends NaN and Inf as null
					s.values = append(s.values, *v)
				}
			}
			if len(s.values) > 0 {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// TestGrafanaDemoPanels: every query of every Demo dashboard panel returns
// data through the demo Grafana, as an anonymous visitor's dashboard asks
// it, while demo traffic runs (SC-05): the base load, then make demo-d5
// (429s, Redis errors, fail_closed) and make demo-d6 (the breaker opens,
// retries), the scenarios whose effects the base load alone never shows.
// Each query must give at least one series with a value above 0 over the
// run;
// allowed per tenant shows the three demo tenants, the replica count is 3
// and the per-replica panels show gateway-1 … gateway-3.
//
// Host stalls (ADR-0026, ADR-0027, ADR-0034) cannot fail it: it asks for
// any value over minutes of traffic, Prometheus scrapes every 5 s with 30 s
// rate windows, and a stall of under a second only delays a few samples.
func TestGrafanaDemoPanels(t *testing.T) {
	targets := demoTargets(t)
	if len(targets) != 11 {
		t.Fatalf("demo.json has %d queries, want ADR-0096 §4's 11", len(targets))
	}
	start := time.Now()
	// The base load is running (make demo); wait for a minute of it in
	// Prometheus at most, then the two fault scenarios.
	for deadline := start.Add(60 * time.Second); ; time.Sleep(2 * time.Second) {
		s, err := grafanaQuery(t, `sum by (tenant) (rate(gw_requests_total{code_class="2xx"}[30s]))`, time.Now().Add(-30*time.Second), time.Now())
		if err == nil && len(s) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no allowed traffic of three tenants in the demo Grafana within 60 s: %v %v", s, err)
		}
	}
	noLeak(t, "make demo-d5", runMake(t, "demo-d5"))
	noLeak(t, "make demo-d6", runMake(t, "demo-d6"))
	// The last scrape (5 s) and its 30 s rate window include the end.
	time.Sleep(10 * time.Second)
	end := time.Now()

	for _, tg := range targets {
		s, err := grafanaQuery(t, tg.expr, start, end)
		where := fmt.Sprintf("panel %q query %s (%s)", tg.panel, tg.refID, tg.expr)
		if err != nil {
			t.Errorf("%s: %v", where, err)
			continue
		}
		top := 0.0
		var labels []string
		for _, x := range s {
			labels = append(labels, fmt.Sprint(x.labels))
			top = max(top, slices.Max(x.values))
		}
		// Above zero: a series left at 0 by an earlier run is no proof of
		// this run's traffic (every query here is a rate, a count or a
		// circuit state that D6 opens).
		if top <= 0 {
			t.Errorf("%s: no data above 0 while the demo traffic ran (%s to %s; %d series)", where, start.Format(time.TimeOnly), end.Format(time.TimeOnly), len(s))
			continue
		}
		t.Logf("%s: %d series, max %.3g: %s", where, len(s), top, strings.Join(labels, " "))
		values := func(label string) []string {
			var v []string
			for _, x := range s {
				v = append(v, x.labels[label])
			}
			slices.Sort(v)
			return slices.Compact(v)
		}
		switch tg.expr {
		case `sum by (tenant) (rate(gw_requests_total{code_class="2xx"}[30s]))`:
			if got := values("tenant"); !slices.Equal(got, []string{"enterprise-co", "free-co", "pro-co"}) {
				t.Errorf("%s: tenants %v, want the three demo tenants", where, got)
			}
		case `count(up{job="gateway"} == 1)`:
			if v := s[0].values; len(s) != 1 || v[len(v)-1] != 3 {
				t.Errorf("%s: %v, want 3 at the end", where, s)
			}
		case `sum by (replica) (rate(gw_requests_total[30s]))`, `max by (replica) (gw_circuit_state{upstream="orders"})`:
			if got := values("replica"); !slices.Equal(got, []string{"gateway-1", "gateway-2", "gateway-3"}) {
				t.Errorf("%s: replicas %v, want gateway-1 … gateway-3", where, got)
			}
		}
	}
}
