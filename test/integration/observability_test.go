//go:build integration

package integration

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Observability as code on the compose stack (OB-04 – OB-06, ADR-0087):
// compose's Prometheus (host 9091) and Grafana (host 3000).

// promGet decodes the data of a Prometheus API answer from compose's
// Prometheus.
func promGet(t *testing.T, path string, q url.Values, data any) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:9091"+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.Status != "success" {
		return fmt.Errorf("%s: %s %s", path, out.Status, out.Error)
	}
	return json.Unmarshal(out.Data, data)
}

// TestPrometheusRulesLoaded: compose's Prometheus loaded every rule of
// deploy/prometheus/rules and evaluates each without error (OB-05).
func TestPrometheusRulesLoaded(t *testing.T) {
	b, err := os.ReadFile("../../deploy/prometheus/rules/spillway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Rules []struct {
				Record string `yaml:"record"`
				Alert  string `yaml:"alert"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, g := range file.Groups {
		for _, r := range g.Rules {
			want = append(want, r.Record+r.Alert)
		}
	}
	slices.Sort(want)
	// Rules are evaluated every 15 s; a Prometheus that just started
	// reports health "unknown" until then.
	var got, bad []string
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(time.Second) {
		var data struct {
			Groups []struct {
				Rules []struct {
					Name      string `json:"name"`
					Health    string `json:"health"`
					LastError string `json:"lastError"`
				} `json:"rules"`
			} `json:"groups"`
		}
		if err := promGet(t, "/api/v1/rules", nil, &data); err != nil {
			t.Fatal(err)
		}
		got, bad = nil, nil
		unknown := 0
		for _, g := range data.Groups {
			for _, r := range g.Rules {
				got = append(got, r.Name)
				switch r.Health {
				case "ok":
				case "unknown":
					unknown++
				default:
					bad = append(bad, r.Name+": "+r.Health+" "+r.LastError)
				}
			}
		}
		slices.Sort(got)
		if unknown == 0 || time.Now().After(deadline) {
			break
		}
	}
	if !slices.Equal(got, want) || len(bad) > 0 {
		t.Errorf("Prometheus has rules %v (failing: %v), want %v, all healthy", got, bad, want)
	}
}

// TestExemplarInPrometheus: a sampled request through gateway-1 (compose's
// trace ratio is 1, and the client asks for sampling) leaves its trace ID as
// an exemplar on gw_request_duration_seconds in compose's Prometheus, which
// scrapes OpenMetrics with exemplar storage on (OB-06). Other requests
// replace a bucket's exemplar, so it sends one a second until Prometheus
// shows one of its own, for up to 60 s (scrapes are every 5 s).
func TestExemplarInPrometheus(t *testing.T) {
	var id [16]byte
	_, _ = rand.Read(id[:])
	trace := hex.EncodeToString(id[:])
	start := time.Now().Add(-time.Minute)
	client := &http.Client{Timeout: 10 * time.Second}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(time.Second) {
		var span [8]byte
		_, _ = rand.Read(span[:])
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/bytes/16", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Traceparent", "00-"+trace+"-"+hex.EncodeToString(span[:])+"-01")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /bytes/16 = %d", resp.StatusCode)
		}
		var data []struct {
			SeriesLabels map[string]string `json:"seriesLabels"`
			Exemplars    []struct {
				Labels map[string]string `json:"labels"`
			} `json:"exemplars"`
		}
		q := url.Values{
			"query": {`gw_request_duration_seconds_bucket{replica="gateway-1"}`},
			"start": {strconv.FormatInt(start.Unix(), 10)},
			"end":   {strconv.FormatInt(time.Now().Unix()+1, 10)},
		}
		if err := promGet(t, "/api/v1/query_exemplars", q, &data); err != nil {
			t.Fatal(err)
		}
		for _, s := range data {
			for _, e := range s.Exemplars {
				if e.Labels["trace_id"] == trace {
					t.Logf("measure: exemplar trace_id=%s on %s le=%s after %s", trace, s.SeriesLabels["route"], s.SeriesLabels["le"], time.Since(start.Add(time.Minute)).Round(time.Second))
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no exemplar with trace_id %s in Prometheus after 60 s (%d series with exemplars)", trace, len(data))
		}
	}
}

// TestDashboardQueries: every query of every dashboard in
// deploy/grafana/dashboards is valid PromQL for compose's Prometheus, with
// the dashboards' variables at All, and every redis_exporter metric they use
// has series for all six Redis nodes (OB-04).
func TestDashboardQueries(t *testing.T) {
	files, err := filepath.Glob("../../deploy/grafana/dashboards/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no dashboards (%v)", err)
	}
	vars := strings.NewReplacer("$__rate_interval", "1m", "$__interval", "15s", "$__range", "1h",
		"$replica", ".*", "$policy", ".*", "$upstream", ".*", "$node", ".*")
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: a dashboard file of this repo
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Panels []struct {
				Title   string `json:"title"`
				Targets []struct {
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				n++
				var res json.RawMessage
				if err := promGet(t, "/api/v1/query", url.Values{"query": {vars.Replace(tg.Expr)}}, &res); err != nil {
					t.Errorf("%s, %q: %v", filepath.Base(f), p.Title, err)
				}
			}
		}
	}
	t.Logf("measure: %d dashboard queries in %d dashboards valid", n, len(files))

	for _, m := range []string{"redis_up", "redis_memory_used_bytes", "redis_memory_max_bytes", "redis_commands_processed_total", "redis_connected_clients", "redis_db_keys", "redis_cluster_state"} {
		var res struct {
			Result []json.RawMessage `json:"result"`
		}
		if err := promGet(t, "/api/v1/query", url.Values{"query": {`count by (__name__) (count by (__name__, node) (` + m + `{job="redis"}))`}}, &res); err != nil {
			t.Fatal(err)
		}
		var v struct {
			Value [2]any `json:"value"`
		}
		if len(res.Result) == 1 {
			_ = json.Unmarshal(res.Result[0], &v)
		}
		if fmt.Sprint(v.Value[1]) != "6" {
			t.Errorf("%s: series for %v Redis nodes, want 6", m, v.Value[1])
		}
	}
}

// TestGrafanaDashboards: compose's Grafana provisioned the five dashboards
// (OB-04). The admin password is GRAFANA_ADMIN_PASSWORD from this
// environment, else compose's dev default; it is never logged.
func TestGrafanaDashboards(t *testing.T) {
	password := os.Getenv("GRAFANA_ADMIN_PASSWORD")
	if password == "" {
		password = "dev-only-not-secret" //nolint:gosec // G101: compose's dev default
	}
	var uids []string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:3000/api/search?tag=spillway&type=dash-db", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("admin", password)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var found []struct {
			UID string `json:"uid"`
		}
		err = json.NewDecoder(resp.Body).Decode(&found)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("Grafana search: %d (%v)", resp.StatusCode, err)
		}
		uids = nil
		for _, f := range found {
			uids = append(uids, f.UID)
		}
		slices.Sort(uids)
		if len(uids) == 5 || time.Now().After(deadline) {
			break
		}
	}
	want := []string{"spillway-control-plane", "spillway-overview", "spillway-rate-limiting", "spillway-redis", "spillway-upstreams"}
	if !slices.Equal(uids, want) {
		t.Errorf("Grafana's spillway dashboards %v, want %v", uids, want)
	}
}
