# 0087. Observability as code: implementation details

- **Status:** Accepted
- **Date:** 2026-11-26
- **Requirement IDs:** OB-04, OB-05, OB-06, OB-08, NFR-12

## Context

ADR-0075's "Observability as code" section sets the dashboards (OB-04), the alert rules (OB-05), exemplars (OB-06), the runbooks (OB-08) and their tests; under T4 (ADR-0068) the observability task builds them after the helm task, adds ADR-0074's (c6) line to `test/verify/m5.sh` and ticks M7's "Helm chart … dashboards, alert rules and runbooks as code" box. ADR-0085 left two optional ConfigMaps (`prometheus-rules`, `grafana-dashboards`) mounted in kind's Prometheus and Grafana and redis_exporter on kind only. This ADR records what ADR-0075 left open, the one place the implementation differs from its text, and the measured results. No requirement text changes.

## Decision

### Files

- `deploy/grafana/dashboards/{overview,rate-limiting,upstreams,redis,control-plane}.json`: Grafana's classic JSON model (`schemaVersion` 41, which Grafana 13.3.0 loads and migrates), UIDs `spillway-overview`, `spillway-rate-limiting`, `spillway-upstreams`, `spillway-redis`, `spillway-control-plane`, titles `Spillway / …`, tag `spillway` (each dashboard links the others), not editable in the UI (the files are the source), every query on the provisioned datasource uid `prometheus`. Variables: `replica` everywhere (`label_values(gw_requests_total, replica)`, which compose's and kind's Prometheus both set), plus `policy` (Rate limiting), `upstream` (Upstreams) and `node` (Redis). The SLO panels read the recording rules below, so they cover every gateway regardless of `replica`. The panels are ADR-0075's lists; the Overview adds connections capped and a table of Spillway's firing and pending alerts (`ALERTS`, since there is no Alertmanager).
- `deploy/prometheus/rules/spillway.yaml`: three groups, `spillway-slo` (eight recording rules `spillway:gw_availability_errors:ratio_rate{5m,30m,1h,6h}` and `spillway:gw_latency_slow:ratio_rate{…}`, and the four burn-rate alerts), `spillway-dependencies` (`SpillwayRedisErrors`, `SpillwayRedisMemoryHigh`) and `spillway-gateway` (`SpillwayConfigSyncStale`, `SpillwayCircuitOpen`). Thresholds, windows and severities are ADR-0075's.
- `test/obs/rules-test.yaml`: promtool unit tests of every alert, firing and quiet cases. It lives outside `deploy/prometheus/rules`, because kind's Prometheus loads every `*.yaml` there.
- `docs/runbooks/{availability-burn,latency-burn,redis-errors,redis-memory,config-sync-stale,circuit-open}.md` and the index `docs/runbooks/README.md`, each with **What it means**, **First checks**, **Mitigation**.

### Rule details ADR-0075 left open

1. **`SpillwayCircuitOpen` is `gw_circuit_state > 0` for 5 m, not `== 1`.** A failing upstream's breaker goes open, half-open after `open_for` (10 s by default), and open again when the trial requests fail; with `== 1`, every scrape that lands on half-open (2) restarts the 5 minutes, so an upstream that never recovers might never alert. "Circuit open > 5 min" (OB-05) means "not closed for 5 minutes"; the promtool test feeds a breaker flapping between 1 and 2 for 6 minutes and expects the alert.
2. **Burn-rate alerts have no `for`**: the short window (5 m or 30 m) already makes one end soon after the burn does, as in the SRE workbook's multi-window alerts. `SpillwayRedisErrors` and `SpillwayRedisMemoryHigh` have ADR-0075's `for: 5m`; `SpillwayConfigSyncStale` has none (its 60 s is the wait).
3. **SLO ratios are over every gateway together** (`sum(rate(…))`, no labels kept): an SLO is the service's. Without traffic the ratio is NaN and nothing fires. **`SpillwayRedisErrors` is per gateway** (`sum by (job, replica)`), so one replica that cannot reach Redis alerts even when the others can.
4. **Rules every 15 s** (`evaluation_interval: 15s` in compose's and kind's `prometheus.yml`; Prometheus's default is 1 m), so `SpillwayConfigSyncStale` fires within 15 s of its 60 s threshold.
5. **Annotations** show only the series' own labels (`replica`, `upstream`, `node`, `job`, `instance`), never a key, consumer or path (OB-01); `runbook_url` is `https://github.com/ankitbisht23/spillway/blob/main/docs/runbooks/<file>.md`.

### Exemplars (OB-06)

- `telemetry.Request` gains `Sampled`: the handler sets it from the server span's `IsSampled()`. The recorder observes `gw_request_duration_seconds` with the exemplar `{trace_id}` only when `Sampled`; `Recorder.UpstreamAttempt` takes the attempt span's trace ID when that span is sampled, else `""`. A gateway without a tracer adds no exemplar even when the client's `traceparent` asks for sampling: its own spans are not exported, so the link would lead nowhere. A shed request has no span and no exemplar.
- The admin server (both binaries) serves OpenMetrics when asked (`promhttp.HandlerOpts{EnableOpenMetrics: true}`): Prometheus 3's default scrape protocols prefer it, and only that format carries exemplars. Clients that do not ask get the text format as before.
- Cost: an exemplar observation builds a one-label map and the exemplar for sampled requests only. Benchmarks and the latency checks run at the fixed 1% head ratio (ADR-0030), so 1% of their requests pay it; compose's default ratio of 1 makes every dev request pay it.
- Compose's Prometheus runs with `--enable-feature=exemplar-storage` (kind's already did); Grafana's Prometheus datasource (`deploy/compose/grafana/provisioning/datasources/datasources.yaml`, shared with kind) links `trace_id` to the datasource uid `tempo`.

### Compose and kind

- **Compose:** Prometheus mounts `deploy/prometheus/rules` at `/etc/prometheus/rules` and loads `rule_files: /etc/prometheus/rules/*.yaml`; Grafana mounts `deploy/grafana/dashboards` at `/var/lib/grafana-dashboards` with a file provider in `deploy/compose/grafana/provisioning/dashboards/spillway.yaml` (the path and settings of kind's provider). Prometheus reads rule files at start, so a changed rule needs `docker compose restart prometheus` (docs/setup.md); Grafana's provider rereads the files every 10 s.
- **redis_exporter in compose** (ADR-0075 lists it for both): service `redis-exporter`, built from `deploy/compose/redis-exporter/Dockerfile`, which adds the static busybox to `oliver006/redis_exporter:v1.97.0` (kind's image) for the compose healthcheck that `make verify-m0` requires of every service, as for Tempo, Loki and the upstreams (ADR-0006); `REDIS_ADDR=redis://redis-0:6379` for its own `/metrics`, no password (compose's Redis has none, ADR-0040), read-only root filesystem, port 9121 inside the network only (docs/ports.md). Prometheus's job `redis` scrapes each node through `/scrape?target=redis://redis-N:6379` with a `node` label, as on kind (ADR-0085 item 8). `test/kind/kind_test.go` no longer lists it as a kind-only image.
- **kind:** `make kind-up` creates `spillway-obs/prometheus-rules` from `deploy/prometheus/rules/` and `spillway-obs/grafana-dashboards` from `deploy/grafana/dashboards/` (by reference, like every ConfigMap), before the observability Deployments; on a live cluster, a changed rules ConfigMap restarts Prometheus (`rollout restart`), since it does not reload rules by itself. The dashboards total about 97 KB, which `test/obs` keeps under 180 KiB so the ConfigMap and its `kubectl apply` annotation (256 KiB) fit.
- **`make kind-smoke`** gains two lines: Prometheus's `/api/v1/rules` (asked inside its pod) lists as many rules as `deploy/prometheus/rules/*.yaml` defines, none with health `err`; Grafana's `/api/search?tag=spillway&type=dash-db` (asked inside its pod with its own admin password from its environment, never printed) lists one dashboard per file.

### Tests

- **`test/obs/obs_test.go`** (plain Go, so `make test-go` runs it): the five dashboards parse, with their UIDs, titles, tag, unique panel IDs on the 24-column grid and the `prometheus` datasource; every variable a query uses is defined; at least three latency queries ask for exemplars and only on the two latency histograms; every metric a dashboard or rule names is registered in the Go code (`Name:` and `NewDesc` literals under `internal/` and `cmd/`, a histogram's `_bucket`, `_count`, `_sum`) or is one of the seven redis_exporter metrics the test lists; every recording rule used is defined; no query selects on a key, consumer, path, token, email or IP label; every alert ADR-0075 lists exists, with a `page` or `ticket` severity, a summary and description, and the `runbook_url` of its runbook on `main`, whose file exists, names the alert and has the three sections; the runbook index links every runbook; compose and kind mount and load the same rules and dashboards with exemplar storage, 15 s evaluation, the `redis` job, the datasource's exemplar link, and the Makefile's promtool image equal to compose's Prometheus image.
- **`make lint-obs`**: `promtool check rules` and `promtool test rules test/obs/rules-test.yaml` from `prom/prometheus:v3.15.1` (compose's pinned image, run with Docker), then `go test ./test/obs/`. It is part of `make lint`, and CI's integration job runs it (the go job has no other Docker step).
- **Unit tests:** `TestExemplars` (recorder: sampled request and attempt carry the trace ID on the bucket they fall in; unsampled ones carry none), `TestAdminServerExemplars` (OpenMetrics with the exemplar when asked, text without otherwise), `TestTraceExemplars` (handler: sampled, unsampled and tracing-off requests through a retried upstream).
- **Integration tests** (`test/integration/observability_test.go`, compose stack): every rule of the file loaded in Prometheus and healthy; a request with a sampled `traceparent` through gateway-1 shows up as an exemplar with its trace ID in Prometheus's `/api/v1/query_exemplars`; all 81 dashboard queries are valid PromQL against compose's Prometheus with the variables at All, and each listed redis_exporter metric has series for all six nodes; Grafana lists the five dashboards (admin password from `GRAFANA_ADMIN_PASSWORD` in the environment, else compose's dev default; never logged).

### (c6) in `test/verify/m5.sh`

During rule (c)'s outage a background poller asks compose's Prometheus every 5 s for `ALERTS{alertname="SpillwayConfigSyncStale", alertstate="firing", replica="gateway-1"}`, from just before the control plane and Postgres stop until the script starts them again, and notes the first time it sees it. It runs beside the loop that answers the sender, so a slow Prometheus answer never delays the outage's actions. **PASS:** seen firing before the outage ends; the line prints when (seconds after the stop) and the outage's length. Stall tolerance (ADR-0074): the alert fires about 65–80 s into a 600 s outage (60 s threshold, 15 s evaluation, 5 s scrapes and polls), so stalls of up to 820 ms cannot make it miss. `make verify-m5` and `make chaos-cp-outage` run it; `checks` lists `(c6)` so a setup failure fails it too.

### Pending for the user

- **(P36) Metrics table, new rows** (the quota flusher's, ADR-0070, emitted since the quotas task and used by the Control plane dashboard): `cp_quota_flushes_total` · counter · result (ok, error, locked); `cp_quota_flush_duration_seconds` · histogram · none; `cp_quota_reseeds_total` · counter · none; `cp_quota_flush_errors_total` · counter · none; `cp_quota_last_flush_timestamp_seconds` · gauge · none. docs/design.md is not edited; the dashboard uses them either way. (The table also omits the control plane's `cp_active`, `cp_watch_streams` and `cp_stream_*`, which predate M7.)

### Measured

Recorded by this task's runs on the laptop (Docker Desktop, 14 CPUs, about 8.3 GB) on 2026-11-26:

- **(c6), `make chaos-cp-outage` three times: 3 of 3 PASS**, with every other line of rule (c) passing too. The alert was first seen firing for gateway-1 at +65 s, +71 s and +81 s of outages of 600, 601 and 600 s; recovery (c5) after 6.4, 6.3 and 11.3 s; zero failed requests of 347,870, 347,920 and 347,887. Each run took 728 s.
- **kind, three cycles of `make kind-up` → `make kind-smoke` → `make kind-down` from nothing (compose stopped): 3 of 3 PASS**, every smoke line passing, the two new ones as "16 of 16 rules, 0 failing" and "5 of 5" dashboards. kind-up took 146, 139 and 109 s; kind-smoke 14 s each; kind-down 2, 2 and 1 s. A fourth cluster checked the restart path: re-running `make kind-up` with a changed rule file printed "alert rules changed: restarting Prometheus" and finished in 20 s, and kind-smoke passed after.
- **promtool:** `check rules` finds 16 rules; the rule unit tests pass, and fail when an expected annotation or the flapping breaker's series is changed (checked once by hand).
- **Compose:** a sampled request's exemplar appeared in Prometheus 3 s after the first request; 81 dashboard queries valid; `make verify-m0-stack` 20/20 services healthy (redis-exporter included).

## Consequences

- One set of files serves compose and kind; changing a rule or dashboard is a file edit that `make lint-obs` and CI check.
- Every alert has a tested expression and a runbook; with no Alertmanager, someone has to look at Prometheus or Grafana to see them.
- Dev requests (trace ratio 1) each carry an exemplar's small cost; benchmarks are unaffected beyond their 1% sampled share.
- `make lint` now needs Docker (for promtool), like `make test-integration`.
- Compose gains one container (redis-exporter, about 10 MB of memory).

## Alternatives considered

- **`gw_circuit_state == 1` as ADR-0075 wrote it** — misses an upstream whose breaker keeps flapping between open and half-open (rule detail 1).
- **promtool as a pinned binary in `./bin`** (the `download-tool` macro) — another per-platform checksum set for a tool the pinned Prometheus image already contains.
- **Dashboards generated from code (Jsonnet, grafonnet, a Go generator)** — another toolchain to pin and learn for five dashboards; the JSON is the reviewed artifact and the tests check it.
- **Per-replica SLO burn alerts** — an SLO is the fleet's; a single bad replica shows in the Overview's per-replica panels and in the Redis and circuit alerts, which are per replica.
- **Exemplars whenever the client's `traceparent` is sampled, tracing on or off** — without the gateway's tracer its spans are not in Tempo, so the exemplar would link to a trace without the gateway.
