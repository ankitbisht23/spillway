// Package obs holds static tests of observability as code (OB-04, OB-05,
// OB-06, OB-08; ADR-0075, ADR-0087): the Grafana dashboards in
// deploy/grafana/dashboards, the alert rules in deploy/prometheus/rules and
// their runbooks in docs/runbooks, and how compose and kind load them. They
// run with go test, without Docker; make lint-obs adds promtool's checks
// and rule unit tests (rules-test.yaml), and test/integration checks the
// running compose stack.
package obs

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// repo is the repository root.
var repo = os.DirFS("../..")

// dashboards are the five dashboards OB-04 names, by file, with their UIDs.
var dashboards = map[string]string{
	"overview.json":      "spillway-overview",
	"rate-limiting.json": "spillway-rate-limiting",
	"upstreams.json":     "spillway-upstreams",
	"redis.json":         "spillway-redis",
	"control-plane.json": "spillway-control-plane",
}

// alerts are the alerts OB-05 asks for (ADR-0075), each with its runbook.
var alerts = map[string]string{
	"SpillwayAvailabilityBurnFast": "availability-burn.md",
	"SpillwayAvailabilityBurnSlow": "availability-burn.md",
	"SpillwayLatencyBurnFast":      "latency-burn.md",
	"SpillwayLatencyBurnSlow":      "latency-burn.md",
	"SpillwayRedisErrors":          "redis-errors.md",
	"SpillwayRedisMemoryHigh":      "redis-memory.md",
	"SpillwayConfigSyncStale":      "config-sync-stale.md",
	"SpillwayCircuitOpen":          "circuit-open.md",
}

// redisExporterMetrics are the redis_exporter (v1.97.0) metrics the
// dashboards and rules use, each seen in its /scrape output for a Redis
// Cluster node; test/integration checks each has series in compose's
// Prometheus.
var redisExporterMetrics = []string{
	"redis_up", "redis_memory_used_bytes", "redis_memory_max_bytes", "redis_commands_processed_total",
	"redis_connected_clients", "redis_db_keys", "redis_cluster_state",
}

// runbookBase is where every runbook_url points: the file on main on GitHub.
const runbookBase = "https://github.com/ankitbisht23/spillway/blob/main/"

// maxDashboardBytes keeps the grafana-dashboards ConfigMap, which kind-up
// applies with kubectl apply (whose last-applied annotation holds a copy,
// at most 256 KiB), well within its limits.
const maxDashboardBytes = 180 << 10

// dashboard is the part of a Grafana dashboard's JSON model these tests read.
type dashboard struct {
	UID        string   `json:"uid"`
	Title      string   `json:"title"`
	Tags       []string `json:"tags"`
	Panels     []panel  `json:"panels"`
	Templating struct {
		List []struct {
			Name       string     `json:"name"`
			Datasource datasource `json:"datasource"`
			Definition string     `json:"definition"`
		} `json:"list"`
	} `json:"templating"`
}

// panel is a dashboard panel; rows have no targets.
type panel struct {
	ID         int        `json:"id"`
	Type       string     `json:"type"`
	Title      string     `json:"title"`
	Datasource datasource `json:"datasource"`
	GridPos    struct {
		X, Y, W, H int
	} `json:"gridPos"`
	Targets []struct {
		Expr     string     `json:"expr"`
		Exemplar bool       `json:"exemplar"`
		Data     datasource `json:"datasource"`
	} `json:"targets"`
}

// datasource is a datasource reference.
type datasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// loadDashboards decodes every dashboard file, by file name.
func loadDashboards(t *testing.T) map[string]dashboard {
	t.Helper()
	files, err := fs.Glob(repo, "deploy/grafana/dashboards/*")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]dashboard{}
	total := 0
	for _, f := range files {
		b, err := fs.ReadFile(repo, f)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b)
		var d dashboard
		dec := json.NewDecoder(strings.NewReader(string(b)))
		if err := dec.Decode(&d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[path.Base(f)] = d
	}
	if total > maxDashboardBytes {
		t.Errorf("dashboards are %d bytes, over %d (the ConfigMap's apply annotation)", total, maxDashboardBytes)
	}
	return out
}

// ruleFile is deploy/prometheus/rules/spillway.yaml.
type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []rule `yaml:"rules"`
	} `yaml:"groups"`
}

// rule is a recording or an alerting rule.
type rule struct {
	Record      string            `yaml:"record"`
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// loadRules decodes the rule file.
func loadRules(t *testing.T) []rule {
	t.Helper()
	b, err := fs.ReadFile(repo, "deploy/prometheus/rules/spillway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var r ruleFile
	if err := yaml.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	var out []rule
	for _, g := range r.Groups {
		out = append(out, g.Rules...)
	}
	return out
}

var (
	metricName   = regexp.MustCompile(`(?:^|[^a-zA-Z0-9_:])((?:gw|cp|redis)_[a-zA-Z0-9_]+)\b`)
	recordedName = regexp.MustCompile(`spillway:[a-zA-Z0-9_:]+`)
	registered   = regexp.MustCompile(`(?:Name:\s*|NewDesc\(\s*)"((?:gw|cp)_[a-z0-9_]+)"`)
	histogram    = regexp.MustCompile(`(?s)HistogramOpts\{\s*Name:\s*"((?:gw|cp)_[a-z0-9_]+)"`)
)

// codeMetrics returns every gw_ and cp_ metric the Go code registers, with
// a histogram's _bucket, _count and _sum series.
func codeMetrics(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := fs.WalkDir(repo, dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := fs.ReadFile(repo, p)
			if err != nil {
				return err
			}
			for _, m := range registered.FindAllSubmatch(b, -1) {
				out[string(m[1])] = true
			}
			for _, m := range histogram.FindAllSubmatch(b, -1) {
				for _, s := range []string{"_bucket", "_count", "_sum"} {
					out[string(m[1])+s] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The design's metrics table (Observability) and what the dashboards
	// lean on most; finding them proves the scan works.
	for _, m := range []string{"gw_requests_total", "gw_request_duration_seconds_bucket", "gw_circuit_state", "gw_config_sync_age_seconds", "cp_active", "cp_quota_flushes_total"} {
		if !out[m] {
			t.Fatalf("the code scan found no %s (found %d metrics)", m, len(out))
		}
	}
	return out
}

// checkExpr reports every metric in expr that neither the code nor
// redis_exporter provides, every recording rule it uses that the rule
// file does not define, and any label that could hold a key, a consumer
// or a path (OB-01).
func checkExpr(t *testing.T, where, expr string, known, recorded map[string]bool) {
	t.Helper()
	if strings.TrimSpace(expr) == "" {
		t.Errorf("%s: empty expression", where)
		return
	}
	for _, m := range metricName.FindAllStringSubmatch(expr, -1) {
		if !known[m[1]] {
			t.Errorf("%s: metric %s is neither registered in the code nor a listed redis_exporter metric", where, m[1])
		}
	}
	for _, r := range recordedName.FindAllString(expr, -1) {
		if !recorded[r] {
			t.Errorf("%s: recording rule %s is not defined in spillway.yaml", where, r)
		}
	}
	if m := regexp.MustCompile(`\b(key|key_hash|api_key|consumer|consumer_id|path|token|email|ip)\s*(=|!=|=~|!~)`).FindString(expr); m != "" {
		t.Errorf("%s: selects on %q; keys, consumers and paths are never labels (OB-01)", where, m)
	}
}

// known returns every metric name an expression may use.
func known(t *testing.T) map[string]bool {
	t.Helper()
	k := codeMetrics(t)
	for _, m := range redisExporterMetrics {
		k[m] = true
	}
	return k
}

// recordings returns the recording rules' names.
func recordings(rules []rule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		if r.Record != "" {
			out[r.Record] = true
		}
	}
	return out
}

// TestDashboards: the five dashboards of OB-04, each valid JSON with its
// UID, the spillway tag, unique panel IDs on a 24-column grid, every
// target on the provisioned Prometheus datasource (uid prometheus) with a
// query whose metrics exist; the latency panels ask for exemplars (OB-06);
// every variable a query uses is defined.
func TestDashboards(t *testing.T) {
	ds := loadDashboards(t)
	k, rec := known(t), recordings(loadRules(t))
	if len(ds) != len(dashboards) {
		t.Errorf("deploy/grafana/dashboards holds %d files, want the %d of OB-04", len(ds), len(dashboards))
	}
	uids := map[string]string{}
	exemplars := 0
	for file, uid := range dashboards {
		d, ok := ds[file]
		if !ok {
			t.Errorf("no dashboard %s", file)
			continue
		}
		if prev, dup := uids[d.UID]; dup {
			t.Errorf("%s and %s share uid %s", prev, file, d.UID)
		}
		uids[d.UID] = file
		exemplars += checkDashboard(t, file, d, uid, k, rec)
	}
	if exemplars < 3 {
		t.Errorf("%d latency queries ask for exemplars, want those of the route, tenant and upstream latency panels (OB-06)", exemplars)
	}
}

// checkDashboard applies TestDashboards' rules to one dashboard: its UID,
// a "Spillway / " title and the spillway tag, unique panel IDs on a
// 24-column grid, every variable and target on the provisioned Prometheus
// datasource (uid prometheus) with a query whose metrics exist and whose
// variables are defined, and exemplars only on the latency histograms. It
// returns how many targets ask for exemplars.
func checkDashboard(t *testing.T, file string, d dashboard, uid string, k, rec map[string]bool) (exemplars int) {
	t.Helper()
	if d.UID != uid || !strings.HasPrefix(d.Title, "Spillway / ") || !slices.Contains(d.Tags, "spillway") {
		t.Errorf("%s: uid %q, title %q, tags %v; want uid %q, a \"Spillway / \" title and the spillway tag", file, d.UID, d.Title, d.Tags, uid)
	}
	vars := map[string]bool{"__rate_interval": true, "__interval": true, "__range": true}
	for _, v := range d.Templating.List {
		vars[v.Name] = true
		if v.Datasource.UID != "prometheus" {
			t.Errorf("%s: variable %s uses datasource %q, want prometheus", file, v.Name, v.Datasource.UID)
		}
		checkExpr(t, file+" variable "+v.Name, v.Definition, k, rec)
	}
	ids := map[int]bool{}
	targets := 0
	for _, p := range d.Panels {
		where := file + " panel " + p.Title
		if ids[p.ID] || p.ID == 0 {
			t.Errorf("%s: id %d missing or not unique", where, p.ID)
		}
		ids[p.ID] = true
		if g := p.GridPos; g.W <= 0 || g.H <= 0 || g.X < 0 || g.X+g.W > 24 {
			t.Errorf("%s: gridPos %+v outside the 24-column grid", where, g)
		}
		if p.Type == "row" {
			continue
		}
		if p.Datasource.UID != "prometheus" || len(p.Targets) == 0 {
			t.Errorf("%s: datasource %q with %d targets, want prometheus and at least one", where, p.Datasource.UID, len(p.Targets))
		}
		for _, tg := range p.Targets {
			targets++
			if tg.Data.UID != "prometheus" {
				t.Errorf("%s: target datasource %q, want prometheus", where, tg.Data.UID)
			}
			checkExpr(t, where, tg.Expr, k, rec)
			for _, v := range regexp.MustCompile(`\$\{?(\w+)`).FindAllStringSubmatch(tg.Expr, -1) {
				if !vars[v[1]] {
					t.Errorf("%s: uses $%s, which the dashboard does not define", where, v[1])
				}
			}
			if tg.Exemplar {
				exemplars++
				if !strings.Contains(tg.Expr, "gw_request_duration_seconds_bucket") && !strings.Contains(tg.Expr, "gw_upstream_duration_seconds_bucket") {
					t.Errorf("%s: asks for exemplars of %q; only the latency histograms carry them", where, tg.Expr)
				}
			}
		}
	}
	if targets == 0 {
		t.Errorf("%s: no queries", file)
	}
	return exemplars
}

// TestRules: every alert OB-05 asks for exists with a severity, every
// rule's metrics exist, and every alert links to a runbook on main whose
// file exists, names the alert, and has the three sections OB-08 asks for.
func TestRules(t *testing.T) {
	rules := loadRules(t)
	k, rec := known(t), recordings(rules)
	seen := map[string]bool{}
	for _, r := range rules {
		name := r.Alert + r.Record
		checkExpr(t, "rule "+name, r.Expr, k, rec)
		if r.Record != "" {
			if !strings.HasPrefix(r.Record, "spillway:") {
				t.Errorf("recording rule %s: want a spillway: prefix", r.Record)
			}
			continue
		}
		seen[r.Alert] = true
		if want, ok := alerts[r.Alert]; !ok {
			t.Errorf("alert %s is not one ADR-0075 lists; add it to this test with its runbook", r.Alert)
		} else if got := r.Annotations["runbook_url"]; got != runbookBase+"docs/runbooks/"+want {
			t.Errorf("alert %s: runbook_url %q, want %s", r.Alert, got, runbookBase+"docs/runbooks/"+want)
		}
		if s := r.Labels["severity"]; s != "page" && s != "ticket" {
			t.Errorf("alert %s: severity %q, want page or ticket", r.Alert, s)
		}
		if r.Annotations["summary"] == "" || r.Annotations["description"] == "" {
			t.Errorf("alert %s: needs a summary and a description", r.Alert)
		}
		// Annotations show only the series' own labels, none of which can
		// hold a key, a consumer or a path (OB-01).
		for _, a := range r.Annotations {
			for _, m := range regexp.MustCompile(`\$labels\.(\w+)`).FindAllStringSubmatch(a, -1) {
				if !slices.Contains([]string{"replica", "upstream", "node", "job", "instance"}, m[1]) {
					t.Errorf("alert %s: an annotation shows $labels.%s", r.Alert, m[1])
				}
			}
		}
		url := r.Annotations["runbook_url"]
		file, ok := strings.CutPrefix(url, runbookBase)
		if !ok {
			t.Errorf("alert %s: runbook_url %q is not under %s", r.Alert, url, runbookBase)
			continue
		}
		b, err := fs.ReadFile(repo, file)
		if err != nil {
			t.Errorf("alert %s: runbook %s: %v", r.Alert, file, err)
			continue
		}
		for _, want := range []string{"## What it means", "## First checks", "## Mitigation", "`" + r.Alert + "`"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("alert %s: runbook %s has no %q", r.Alert, file, want)
			}
		}
	}
	for a := range alerts {
		if !seen[a] {
			t.Errorf("no alert %s in spillway.yaml", a)
		}
	}
}

// TestRunbookIndex: docs/runbooks/README.md links every runbook, and every
// runbook has the three sections (OB-08).
func TestRunbookIndex(t *testing.T) {
	index, err := fs.ReadFile(repo, "docs/runbooks/README.md")
	if err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(repo, "docs/runbooks/*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := path.Base(f)
		if name == "README.md" {
			continue
		}
		if !strings.Contains(string(index), "]("+name+")") {
			t.Errorf("docs/runbooks/README.md does not link %s", name)
		}
		b, err := fs.ReadFile(repo, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"## What it means", "## First checks", "## Mitigation"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s has no %q", f, want)
			}
		}
	}
}

// TestProvisioning: compose and kind load the same rules and dashboards,
// with exemplar storage on, and Grafana's Prometheus datasource links an
// exemplar's trace_id to Tempo (OB-04 – OB-06).
func TestProvisioning(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := fs.ReadFile(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	compose := read("compose.yaml")
	for _, want := range []string{
		"./deploy/prometheus/rules:/etc/prometheus/rules:ro",
		"./deploy/grafana/dashboards:/var/lib/grafana-dashboards:ro",
		"./deploy/compose/grafana/provisioning/dashboards:/etc/grafana/provisioning/dashboards:ro",
		"--enable-feature=exemplar-storage",
		"build: ./deploy/compose/redis-exporter",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose.yaml has no %q", want)
		}
	}
	// make lint-obs's promtool is compose's (and kind's) Prometheus.
	img := regexp.MustCompile(`(?m)^\s*image:\s*(prom/prometheus:\S+)`).FindStringSubmatch(compose)
	if mk := read("Makefile"); img == nil || !strings.Contains(mk, "PROMETHEUS_IMAGE := "+img[1]+"\n") {
		t.Errorf("the Makefile's PROMETHEUS_IMAGE is not compose's Prometheus image %v", img)
	}
	for _, f := range []string{"deploy/compose/prometheus/prometheus.yml", "deploy/kind/config/prometheus.yml"} {
		var cfg struct {
			Global struct {
				Evaluation string `yaml:"evaluation_interval"`
			} `yaml:"global"`
			RuleFiles []string `yaml:"rule_files"`
			Scrape    []struct {
				Job string `yaml:"job_name"`
			} `yaml:"scrape_configs"`
		}
		if err := yaml.Unmarshal([]byte(read(f)), &cfg); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var jobs []string
		for _, s := range cfg.Scrape {
			jobs = append(jobs, s.Job)
		}
		// 15 s: the staleness alert fires within 15 s of its 60 s threshold.
		if !slices.Equal(cfg.RuleFiles, []string{"/etc/prometheus/rules/*.yaml"}) || cfg.Global.Evaluation != "15s" ||
			!slices.Contains(jobs, "redis") || !slices.Contains(jobs, "gateway") || !slices.Contains(jobs, "controlplane") {
			t.Errorf("%s: rule_files %v, evaluation_interval %q, jobs %v; want the rules directory, 15s, and the redis, gateway and controlplane jobs", f, cfg.RuleFiles, cfg.Global.Evaluation, jobs)
		}
	}
	kindProm := read("deploy/kind/obs/prometheus.yaml")
	up := read("deploy/kind/up.sh")
	for _, want := range []string{"--enable-feature=exemplar-storage", "name: prometheus-rules"} {
		if !strings.Contains(kindProm, want) {
			t.Errorf("deploy/kind/obs/prometheus.yaml has no %q", want)
		}
	}
	for _, want := range []string{"prometheus-rules --from-file=deploy/prometheus/rules/", "grafana-dashboards --from-file=deploy/grafana/dashboards/"} {
		if !strings.Contains(up, want) {
			t.Errorf("deploy/kind/up.sh has no %q: kind would not load what compose loads", want)
		}
	}
	var dsFile struct {
		Datasources []struct {
			UID      string `yaml:"uid"`
			JSONData struct {
				Exemplars []struct {
					Name          string `yaml:"name"`
					DatasourceUID string `yaml:"datasourceUid"`
				} `yaml:"exemplarTraceIdDestinations"`
			} `yaml:"jsonData"`
		} `yaml:"datasources"`
	}
	if err := yaml.Unmarshal([]byte(read("deploy/compose/grafana/provisioning/datasources/datasources.yaml")), &dsFile); err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, d := range dsFile.Datasources {
		for _, e := range d.JSONData.Exemplars {
			linked = linked || d.UID == "prometheus" && e.Name == "trace_id" && e.DatasourceUID == "tempo"
		}
	}
	if !linked {
		t.Error("the Prometheus datasource does not link exemplars' trace_id to Tempo (OB-06)")
	}
}

// demoQueries are the Demo dashboard's queries, in ADR-0096 §4's order:
// allowed, denied and refused per tenant, p99 per tenant, limiter mode,
// limiter backend and Redis errors, replicas, upstream protection (SC-05).
var demoQueries = []string{
	`count(up{job="gateway"} == 1)`,
	`sum by (replica) (rate(gw_requests_total[30s]))`,
	`sum by (tenant) (rate(gw_requests_total{code_class="2xx"}[30s]))`,
	`sum by (tenant) (rate(gw_requests_total{reason="ratelimited"}[30s]))`,
	`histogram_quantile(0.99, sum by (tenant, le) (rate(gw_request_duration_seconds_bucket[30s])))`,
	`sum by (tenant, reason) (rate(gw_requests_total{reason=~"fail_closed|circuit_open|shed"}[30s]))`,
	`sum by (policy, mode, decision) (rate(gw_ratelimit_decisions_total[30s]))`,
	`sum by (backend) (rate(gw_ratelimit_check_duration_seconds_count[30s]))`,
	`sum by (kind) (rate(gw_redis_errors_total[30s]))`,
	`max by (replica) (gw_circuit_state{upstream="orders"})`,
	`sum by (outcome) (rate(gw_retries_total{upstream="orders"}[30s]))`,
}

// TestDemoDashboard: the demo Grafana's one dashboard,
// deploy/grafana/demo/demo.json (SC-05, ADR-0096 §4), passes the rules
// TestDashboards applies to the five OB-04 dashboards, has uid
// spillway-demo, refreshes every 5 s over the last 15 minutes, and asks
// exactly ADR-0096's queries. It is the only dashboard under
// deploy/grafana/demo, and the OB-04 count stays five.
func TestDemoDashboard(t *testing.T) {
	files, err := fs.Glob(repo, "deploy/grafana/demo/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	_ = fs.WalkDir(repo, "deploy/grafana/demo", func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(p, ".json") {
			all = append(all, p)
		}
		return err
	})
	if !slices.Equal(files, []string{"deploy/grafana/demo/demo.json"}) || len(all) != 1 {
		t.Fatalf("deploy/grafana/demo holds dashboards %v, want only demo.json", all)
	}
	b, err := fs.ReadFile(repo, files[0])
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		dashboard
		Refresh string `json:"refresh"`
		Time    struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"time"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("demo.json: %v", err)
	}
	checkDashboard(t, "demo.json", d.dashboard, "spillway-demo", known(t), recordings(loadRules(t)))
	if d.Title != "Spillway / Demo" || d.Refresh != "5s" || d.Time.From != "now-15m" || d.Time.To != "now" {
		t.Errorf("demo.json: title %q, refresh %q, time %s to %s; want Spillway / Demo, 5s, now-15m to now", d.Title, d.Refresh, d.Time.From, d.Time.To)
	}
	var got []string
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			got = append(got, tg.Expr)
		}
	}
	if !slices.Equal(got, demoQueries) {
		t.Errorf("demo.json asks\n  %s\nwant ADR-0096 §4's\n  %s", strings.Join(got, "\n  "), strings.Join(demoQueries, "\n  "))
	}
}

// TestDemoGrafana: the demo Grafana (SC-05, ADR-0096 §4) provisions exactly
// one datasource, compose's Prometheus, and the Demo dashboard alone; the
// demo override runs it from compose's Grafana image, in profile demo, with
// no host port and ADR-0096's anonymous-Viewer settings, behind Caddy on
// 127.0.0.1:8888 only (ADR-0097 §1); and the dev Grafana stays as it was:
// no anonymous access, its own provisioning.
func TestDemoGrafana(t *testing.T) {
	read := func(name string, out any) {
		t.Helper()
		b, err := fs.ReadFile(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var dsFile struct {
		Datasources []map[string]any `yaml:"datasources"`
	}
	read("deploy/grafana/demo/provisioning/datasources/datasources.yaml", &dsFile)
	if len(dsFile.Datasources) != 1 || dsFile.Datasources[0]["uid"] != "prometheus" || dsFile.Datasources[0]["type"] != "prometheus" ||
		dsFile.Datasources[0]["url"] != "http://prometheus:9090" {
		t.Errorf("the demo Grafana's datasources: %v; want exactly one, Prometheus (uid prometheus, http://prometheus:9090)", dsFile.Datasources)
	}
	var prov struct {
		Providers []struct {
			Type    string            `yaml:"type"`
			Options map[string]string `yaml:"options"`
		} `yaml:"providers"`
	}
	read("deploy/grafana/demo/provisioning/dashboards/demo.yaml", &prov)
	if len(prov.Providers) != 1 || prov.Providers[0].Type != "file" || prov.Providers[0].Options["path"] != "/var/lib/grafana-demo" {
		t.Errorf("the demo Grafana's dashboard providers: %+v; want one file provider for /var/lib/grafana-demo", prov.Providers)
	}

	type service struct {
		Image       string            `yaml:"image"`
		Profiles    []string          `yaml:"profiles"`
		Labels      map[string]string `yaml:"labels"`
		Ports       []string          `yaml:"ports"`
		Environment map[string]string `yaml:"environment"`
		Volumes     []string          `yaml:"volumes"`
	}
	var base, demo struct {
		Services map[string]service `yaml:"services"`
	}
	read("compose.yaml", &base)
	read("deploy/compose/demo.compose.yml", &demo)
	dev, g, caddy := base.Services["grafana"], demo.Services["grafana-demo"], demo.Services["caddy"]
	if g.Image == "" || g.Image != dev.Image {
		t.Errorf("grafana-demo's image %q, want compose's Grafana image %q", g.Image, dev.Image)
	}
	if !slices.Equal(g.Profiles, []string{"demo"}) || g.Labels["spillway.demo"] != "true" || len(g.Ports) != 0 {
		t.Errorf("grafana-demo: profiles %v, labels %v, ports %v; want profile demo, spillway.demo=true, no host port", g.Profiles, g.Labels, g.Ports)
	}
	for k, want := range map[string]string{ //nolint:gosec // G101: compose's dev-only default, not a credential
		"GF_SERVER_ROOT_URL":                        "${DEMO_PUBLIC_URL:-http://localhost:8888}/grafana/",
		"GF_SERVER_SERVE_FROM_SUB_PATH":             "true",
		"GF_AUTH_ANONYMOUS_ENABLED":                 "true",
		"GF_AUTH_ANONYMOUS_ORG_ROLE":                "Viewer",
		"GF_AUTH_DISABLE_LOGIN_FORM":                "true",
		"GF_AUTH_BASIC_ENABLED":                     "false",
		"GF_USERS_ALLOW_SIGN_UP":                    "false",
		"GF_EXPLORE_ENABLED":                        "false",
		"GF_SNAPSHOTS_EXTERNAL_ENABLED":             "false",
		"GF_SECURITY_ALLOW_EMBEDDING":               "false",
		"GF_ANALYTICS_REPORTING_ENABLED":            "false",
		"GF_ANALYTICS_CHECK_FOR_UPDATES":            "false",
		"GF_PLUGINS_PREINSTALL_DISABLED":            "true",
		"GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH": "/var/lib/grafana-demo/demo.json",
		"GF_SECURITY_ADMIN_PASSWORD":                "${GRAFANA_DEMO_ADMIN_PASSWORD:-dev-only-not-secret}",
	} {
		if got := g.Environment[k]; got != want {
			t.Errorf("grafana-demo: %s=%q, want %q", k, got, want)
		}
	}
	if want := []string{
		"./deploy/grafana/demo/provisioning/datasources:/etc/grafana/provisioning/datasources:ro",
		"./deploy/grafana/demo/provisioning/dashboards:/etc/grafana/provisioning/dashboards:ro",
		"./deploy/grafana/demo/demo.json:/var/lib/grafana-demo/demo.json:ro",
	}; !slices.Equal(g.Volumes, want) {
		t.Errorf("grafana-demo mounts %v, want only the demo provisioning and demo.json, read-only: %v", g.Volumes, want)
	}
	if !slices.Equal(caddy.Ports, []string{"127.0.0.1:8888:8888"}) || !slices.Equal(caddy.Profiles, []string{"demo"}) {
		t.Errorf("caddy: ports %v, profiles %v; want 127.0.0.1:8888:8888 (docs/ports.md) and profile demo", caddy.Ports, caddy.Profiles)
	}
	for k := range dev.Environment {
		if strings.HasPrefix(k, "GF_AUTH_") {
			t.Errorf("the dev Grafana sets %s; its access stays as it was (ADR-0096 §4)", k)
		}
	}
	if slices.ContainsFunc(dev.Volumes, func(v string) bool { return strings.Contains(v, "deploy/grafana/demo") }) {
		t.Errorf("the dev Grafana mounts the demo Grafana's files: %v", dev.Volumes)
	}
}
