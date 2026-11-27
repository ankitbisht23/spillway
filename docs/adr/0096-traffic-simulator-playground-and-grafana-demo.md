# 0096. Traffic simulator, Playground and the Grafana Demo dashboard

- **Status:** Accepted
- **Date:** 2026-11-27
- **Requirement IDs:** SC-02, SC-04, SC-05, SC-08, UI-03, UI-04, UI-06, UI-07, CP-12, RL-03, RL-05, RL-08, RL-09, RL-11 – RL-14, RL-21 – RL-26, GW-13, GW-14, UI-01, CP-04, CP-20, NFR-15, OB-01

## Context

SC-02 asks for `cmd/demo-traffic --scenario=d1…d7` driving base load plus each scenario's bursts; SC-04 and UI-07 for a Playground screen whose traffic the control plane generates in-network with short-lived demo keys, the browser seeing only results over SSE; SC-05 for a Grafana Demo dashboard with allowed vs denied and p99 per tenant, limiter mode and replica count. The design's Playground row: allowed vs denied per tenant, `RateLimit` headers, a token-bucket visual, live over SSE; actions: pick tenant and rate, fire traffic, run a scenario.

What exists: `cmd/loadgen`'s open-model generator (ADR-0014) and per-tenant slot histograms (ADR-0074, ADR-0083), both in package `main`; ADR-0059's `/api/v1/stream` (fetch with a bearer token, not `EventSource`; `ready`/`result`/`error`/`end` events; keepalives; hand-written handler behind the guard; 10 streams per subject, 200 in all); the RateLimit header syntax of ADR-0018 (`"<policy>";q=<q>;w=<w>` and `"<policy>";r=<r>;t=<t>`); the metrics of the design's table with `tenant` and `replica` labels; compose's Grafana with Prometheus, Loki and Tempo datasources and five dashboards that `make lint-obs` checks and `make kind-smoke` counts.

## Decision

### 1. `cmd/demo-traffic` (D14)

- **One binary, one image.** `cmd/demo-traffic` with a new Dockerfile target `demo-traffic` (multi-arch, non-root, read-only root filesystem, as the others) and image `spillway-demo-traffic:dev`. Subcommands:

  | Subcommand | Used by | Does |
  | --- | --- | --- |
  | `seed` | `demo-seed` one-shot | ADR-0094 §3 |
  | `proxies [--delete]` | `demo-init` one-shot, `make demo-down` | ADR-0094 §5 |
  | `fault <redis-cut\|upstream-slow\|clear> [on\|off]` | `make demo-fault-*` | ADR-0095 §5 |
  | `run --scenario=<base\|d1…d7> [--check] [--out <file>] [--block]` | the `demo-traffic` service (`base`), `make demo-dN`, `make verify-m8` (`--check`) | §2 |
  | `probe --consumer <name> --route <orders\|catalog\|payments> [-n 5]` | vhs scenes (ADR-0099) | sends n requests, prints each status and its `RateLimit`, `RateLimit-Policy` and `Retry-After` |
  | `share-config --url <url>` | `make demo-share` | the Keycloak admin API changes of ADR-0097 §3 |
  | `share-check --url <url> [--after-stop]` | `make demo-share-test` | the checks of ADR-0097 §4 |
  | `stand-in-tunnel --url <url>` | `make demo-share-test` | ADR-0097 §4 |
  | `healthcheck` | compose healthcheck | asks the running `run` process's health listener on `127.0.0.1:8090` |

- **Settings** (environment; the override sets them): `SPILLWAY_DEMO_CP_URL` (`http://controlplane:8080`), `SPILLWAY_DEMO_TOKEN_URL` (Keycloak's in-network token endpoint), `SPILLWAY_DEMO_CLIENT_ID` (`spillway-ci`), `SPILLWAY_DEMO_CLIENT_SECRET` or `SPILLWAY_DEMO_CLIENT_SECRET_FILE` (compose: `${SPILLWAY_CI_CLIENT_SECRET:-dev-only-not-secret}`; logged only as `[redacted]`, SEC-09), `SPILLWAY_DEMO_GATEWAYS` (`http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000`), `SPILLWAY_DEMO_TOXIPROXY_URL` (`http://toxiproxy:8474`, `proxies` only), `SPILLWAY_DEMO_ADMIN_URLS` (`http://gateway-1:8001,…,gateway-3:8001`, D6's and D5's metric scrapes in the compose network).
- **Keys.** `run` resolves each consumer it drives by name, issues it one key at start (`POST /api/v1/consumers/{id}/keys`, no `expires_at`, so 30 minutes in demo mode, ADR-0095 §3) and a new one when the current key has under 10 minutes left; keys live in memory only, are sent only as `X-API-Key`, and output names a key by its ID (`key #412`). A Go test captures all output and logs of a scenario run against a fake control plane and gateway and asserts no key and no token appears.
- **Load engine.** loadgen's open-model scheduler and per-tenant slot histograms move from `cmd/loadgen` to a new package `internal/loadgen` with no behaviour change (loadgen's tests move with them and still pass); `demo-traffic` and `loadgen` both use it. Each consumer stream has its own HTTP transport (ADR-0083: one shared transport would fake interference), keep-alive, a 15 s client timeout (above every route's 6 s total plus the worst stall), at most 1,000 requests in flight per stream (a request due while 1,000 are in flight is dropped and counted, as loadgen does). Requests go round robin over `SPILLWAY_DEMO_GATEWAYS` (ADR-0094 §6) unless a shape says "per replica".
- **The service.** `demo-traffic` (profile `demo`, `read_only`, `ulimits.nofile` 65536, healthcheck `healthcheck`) runs `run --scenario=base` while the demo runs.
- **`make demo-d1` … `make demo-d7`** (script `deploy/demo/scenario.sh <dN>`): stops the `demo-traffic` service, runs `docker compose … run --rm demo-traffic run --scenario=dN` in the foreground (base load plus the scenario, §2), then starts the service again, also on Ctrl-C (trap). So exactly one base load runs at any time. `run` itself switches off any fault it switched on, on every exit path including SIGINT and SIGTERM.

### 2. Base load and the scenarios (D14)

**Base load** (consumers `-web`, open model, round robin, rps; 258 rps in all):

| Tenant | `orders` (GET) | `catalog` (GET) | `payments` (POST) |
| --- | --- | --- | --- |
| `free-co` | 4 | 2 | 0.5 |
| `pro-co` | 35 | 15 | 2 |
| `enterprise-co` | 140 | 50 | 10 |

All under every limit (free 10/s on orders, catalog 200/s, payments 100/s), so the base load alone produces no 429 and no heavy hitter. `enterprise-co-web`'s 140 rps on `orders` is above D4's 50/s shadow limit and `pro-co-web`'s 35 rps below it, which makes D4's report name exactly one key.

**Scenarios** (each runs the base load too; times from the scenario's start):

| # | Bursts, faults and actions | Length | Prints (every second unless noted) |
| --- | --- | --- | --- |
| d1 | `free-co-mobile` 30 rps on `orders` (3× its 10/s); `pro-co-mobile` 50 rps on `orders` (½ of its 100/s) | 60 s | per tenant: 2xx, 429; every 5 s each one's latest `RateLimit`, `RateLimit-Policy` and, on a 429, `Retry-After` |
| d2 | `free-co-mobile` 500 rps on `orders` (50× its limit); quiet probes `pro-co-mobile` 50 rps and `enterprise-co-mobile` 100 rps on `orders` | 60 s (verify: 10 s slots, ADR-0098) | every 5 s: per tenant 2xx, 429 share, p99 |
| d3 | `pro-co-mobile` 200 rps on `payments` (2× `payments-limit`'s 100/s, burst 200, `fail_closed`), round robin over the three replicas, uniform | 60 s | admitted per second against the 100/s line, per-replica split; at the end ADR-0021's accuracy summary (max over-admission per window) |
| d4 | at 0 s create policy `orders-guard` (consumer level, `orders`, `{consumer}`, GCRA 50/s burst 50, **shadow**, `fail_open`), validate, publish (note `D4 shadow`); at 40 s read its shadow report (`window=15m`) and print the would-be-hit keys (consumer name, key ID, would-deny count); at 45 s set it to **enforce** and publish (`D4 promote`); at 75 s roll back to the version before 0 s (`D4 rollback`) | 90 s | 429s per tenant; each step's version and its time to applied on all three replicas |
| d5 | `pro-co-mobile`: `catalog` 100 rps and `payments` 10 rps round robin; `orders` 66 rps to **each** replica separately (2× its degraded 33/s); `redis-cut` on at 10 s, off at 30 s | 45 s | per route: 2xx, 429, 503; per replica the `orders` admitted rate against 33/s |
| d6 | `pro-co-mobile` 60 rps on `orders` round robin; `upstream-slow` on at 10 s, off at 40 s | 70 s | per replica `gw_circuit_state{upstream="orders"}`; retries sent and `budget_exhausted`; the status mix |
| d7 | a fresh key for `free-co-partner`: 200 rps on `orders` (20× its limit); heavy hitters polled every 1 s (`GET /api/v1/heavy-hitters?since=<start>`) | 60 s, or until blocked | when the key first appears: its rank, denied count and key ID; with `--block` (`make demo-d7 BLOCK=1`) it blocks the key (`createAccessList`, deny on the key) and prints, per replica, the time from the request to the first 403 |

Without `--block`, D7's block is the one click on Throttling (ADR-0062: `POST /access-lists` deny on the key, a key-admin or admin action, an identity change). `--check` turns a run into the scenario's verify-m8 check (ADR-0098): its PASS/FAIL lines on stderr and a JSON report to `--out`.

### 3. The Playground (D15, D16)

**API** (OpenAPI-first, behind ADR-0095's demo guard; new schemas only):

- `POST /api/v1/demo/playground/runs` (`startPlaygroundRun`, all five roles, P46). Body, one of:
  - `{"kind": "custom", "tenant": "free-co|pro-co|enterprise-co", "route": "orders|catalog|payments", "rate": 1–200, "duration_s": 5–60}` (`duration_s` default 30);
  - `{"kind": "scenario", "scenario": "d1|d2|d3"}`.

  201 with `PlaygroundRun` `{"id": "<uuid v4>", "kind", "tenant"?, "route"?, "rate"?, "scenario"?, "duration_s", "started_at", "ends_at", "state": "running"}`. A value outside its range → 422 `validation_failed` naming the field; an unknown field or kind → 400 `invalid_request`. A cap (below) → **429** `unavailable` with `Retry-After` (whole seconds until a slot or the hour frees). The seed's playground consumers missing, or every gateway URL failing its first request → 503 `unavailable`.
- `DELETE /api/v1/demo/playground/runs/{id}` (`stopPlaygroundRun`, all five roles): 204; a run that is not the caller's, or does not exist → 404 `not_found`.
- `GET /api/v1/demo/playground/runs/{id}/stream` (`openPlaygroundStream`, all five roles): the run's results over SSE (below); not the caller's or not existing → 404.
- A separate per-run stream, not a `/api/v1/stream` subscription: the stream's `sub` enum is part of response schemas, so a new value would break oasdiff (ADR-0059), and a run belongs to one caller while `/stream`'s subscriptions are shared and cached.

**Who may do what.** Every role may start runs: the Playground is for visitors, and setup.md has visitors on `viewer`. P46 amends CP-12 ("viewer (read only, plus starting Playground runs in demo mode)"). A run belongs to the token's `sub`; only that `sub` may stop it or open its stream, and anyone else gets 404, as if it did not exist. Run IDs are random 128-bit UUIDs, so callers sharing one account (the public demo's `test-viewer`) cannot find each other's runs.

**Caps (abuse bounds)**, counted in the active control plane's memory (a restart ends every run and resets the counts):

| Scope | Active runs | Starts per rolling hour | Load |
| --- | --- | --- | --- |
| One run | — | — | custom ≤ 200 rps and ≤ 60 s; scenarios fixed: d1 80 rps, d2 650 rps, d3 200 rps, 60 s each |
| Per client IP (ADR-0097 §2) | 1 | 30 | — |
| Per caller (`sub`) | 3 | 120 | — |
| All callers | 3 runs, at most 1 of them a scenario run | — | sum of active runs' rates ≤ 700 rps |

A run that would exceed any of them is refused with 429 before anything is sent. Each run has at most 256 requests in flight (a request due while 256 are in flight is dropped and counted), a 5 s timeout per request, and its own HTTP transport.

**Generator.** In the control plane (package `internal/controlplane/demo`), only when demo mode is on. Open model at the run's rate, round robin over `SPILLWAY_CP_DEMO_GATEWAYS`, with the tenant's Playground key (ADR-0095 §3) in `X-API-Key` and `X-Request-Id: pg-<run id>-<n>`; `GET /v1/orders`, `GET /v1/catalog` or `POST /v1/payments` (empty body). Scenario runs drive the playground consumers with the shapes of §2, scaled to the Playground: d1 = `free-co-playground` 30 rps + `pro-co-playground` 50 rps on `orders`; d2 = `free-co-playground` 500 rps + `pro-co-playground` 50 rps + `enterprise-co-playground` 100 rps on `orders`; d3 = `pro-co-playground` 200 rps on `payments`. No Playground run switches a fault, changes config or identity (beyond its own key's issue) or blocks anything; D4–D7 stay with `make demo-dN` and the admin screens.

**Audit and logs.** `startPlaygroundRun` writes one audit record (action `create`, entity `playground_run`, entity id the run ID, diff `{"after": {kind, tenant, route, rate, duration_s, scenario}}`), `stopPlaygroundRun` one (action `delete`, diff `{"before": …}`); a run that ends on time writes none. One info log line at start and at end with the run ID, the caller's `sub`, the shape and the totals; never a client IP, a key, a token or a response body. Go tests capture the logs of a run (start, ticks, stop) and assert none of those appears, and that no SSE event carries a key, a token or a key prefix.

**The stream** (format as ADR-0059: `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`, a `: keepalive` comment every 15 s, token expiry ends it with `end`/`token_expired`; at most 2 open streams per run and 50 Playground streams in all, else 429 `unavailable`):

| Event | When | `data` |
| --- | --- | --- |
| `ready` | at once | the `PlaygroundRun` |
| `tick` | every 1 s while running | `{"t": "<RFC 3339, control plane>", "elapsed_s": n, "tenants": {"<tenant>": {"sent", "ok" (2xx), "limited" (429), "forbidden" (401, 403), "failed" (503), "other", "dropped", "p50_ms", "p99_ms"}}, "replicas": {"gateway-1": n, …}, "last": {"<tenant>": {"status", "replica", "ratelimit", "ratelimit_policy", "retry_after"}}}`, counts for that second |
| `end` | at the end | `{"reason": "done" \| "stopped" \| "token_expired" \| "shutdown", "totals": {…per tenant as in tick, whole run}}` |

`ratelimit`, `ratelimit_policy` and `retry_after` are the response's `RateLimit`, `RateLimit-Policy` and `Retry-After` values copied verbatim (they carry policy names and numbers only, ADR-0018), or null. Nothing else from a response reaches the stream. A run's results stay readable for 5 minutes after it ends (a late stream gets `ready`, then `end` with the totals).

**The screen** (`/playground`, after History in the navigation, shown only when `getDemo` answers 200, ADR-0095 §2):

- **Fire traffic:** tenant (the three demo tenants with their plan and limit from `getDemo`), route, rate (slider 1–200 rps), duration; **Fire** and **Stop**. **Run a scenario:** three buttons, D1, D2 and D3, each with its one-line story from the design's table.
- **Allowed vs denied per tenant:** a uPlot chart per tenant of `ok` and `limited` per second, with the tenant's limit as a line; per-replica split as small counts.
- **RateLimit headers:** each tenant's latest `RateLimit`, `RateLimit-Policy` and `Retry-After`, monospace, as received.
- **Token-bucket visual:** per tenant a bar of `r` from the `consumer-plan` item of the latest `RateLimit` (or the route's limit item for `catalog` and `payments`) against that limit's burst from `getDemo`, refilled as ticks arrive.
- **Faults (admin only, `useCan("setDemoFault")`):** two switches, Redis cut (D5) and Upstream slow (D6), each behind a confirmation dialog with an optional note, showing the state from `getDemo` (refetched every 5 s while the screen is open).
- States per UI-06: loading, empty ("Fire some traffic"), error (the server's message, e.g. a 429 cap with its `Retry-After`). Starting a new run closes the previous run's stream. The screen never writes to the console; a Vitest console spy and Playwright's console listener (zero messages) check it.

### 4. The demo Grafana and its dashboard (D17)

- **Service `grafana-demo`** (profile `demo`, label `spillway.demo`, Grafana image by the digest compose uses, no host port, served by Caddy at `/grafana`, ADR-0097 §1), with `/var/lib/grafana` on tmpfs. Settings: `GF_SERVER_ROOT_URL=${DEMO_PUBLIC_URL:-http://localhost:8888}/grafana/`, `GF_SERVER_SERVE_FROM_SUB_PATH=true`, `GF_AUTH_ANONYMOUS_ENABLED=true`, `GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer`, `GF_AUTH_DISABLE_LOGIN_FORM=true`, `GF_AUTH_BASIC_ENABLED=false`, `GF_USERS_ALLOW_SIGN_UP=false`, `GF_EXPLORE_ENABLED=false`, `GF_SNAPSHOTS_EXTERNAL_ENABLED=false`, `GF_SECURITY_ALLOW_EMBEDDING=false`, `GF_ANALYTICS_REPORTING_ENABLED=false`, `GF_ANALYTICS_CHECK_FOR_UPDATES=false`, `GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana-demo/demo.json`, `GF_SECURITY_ADMIN_PASSWORD=${GRAFANA_DEMO_ADMIN_PASSWORD:-dev-only-not-secret}` (unusable: no login form and no basic auth).
- **Provisioning** from `deploy/grafana/demo/`: `datasources.yaml` with the **Prometheus datasource only** (`http://prometheus:9090`, uid `prometheus`), and a dashboard provider for that directory, which holds only `demo.json`. No Loki and no Tempo: logs and traces carry key-hash prefixes and request details a public visitor must not query. Prometheus holds no keys, consumers or client addresses (OB-01). The dev Grafana on 3000 is unchanged and keeps its five dashboards, so `make kind-smoke`'s count stays five.
- **Dashboard `deploy/grafana/demo/demo.json`** (uid `spillway-demo`, title "Spillway / Demo", tag `spillway`, refresh 5 s, last 15 minutes, rate windows `[30s]` = six 5 s scrapes):

  | Panel | Query |
  | --- | --- |
  | Allowed per tenant (rps) | `sum by (tenant) (rate(gw_requests_total{code_class="2xx"}[30s]))` |
  | Denied per tenant (rps, 429) | `sum by (tenant) (rate(gw_requests_total{reason="ratelimited"}[30s]))` |
  | Refused otherwise per tenant | `sum by (tenant, reason) (rate(gw_requests_total{reason=~"fail_closed\|circuit_open\|shed"}[30s]))` |
  | p99 per tenant | `histogram_quantile(0.99, sum by (tenant, le) (rate(gw_request_duration_seconds_bucket[30s])))` |
  | Limiter mode per policy | `sum by (policy, mode, decision) (rate(gw_ratelimit_decisions_total[30s]))` (enforce vs shadow; allow, deny, `fail_open`, `fail_closed`) |
  | Limiter backend and Redis errors | `sum by (backend) (rate(gw_ratelimit_check_duration_seconds_count[30s]))`; `sum by (kind) (rate(gw_redis_errors_total[30s]))` |
  | Replicas | stat `count(up{job="gateway"} == 1)`; time series `sum by (replica) (rate(gw_requests_total[30s]))` |
  | Upstream protection (D6) | `max by (replica) (gw_circuit_state{upstream="orders"})`; `sum by (outcome) (rate(gw_retries_total{upstream="orders"}[30s]))` |

- `make lint-obs` (`test/obs`) checks this file with the rules `TestDashboards` applies to the five OB-04 dashboards (valid JSON, UID, the "Spillway / " title and `spillway` tag, unique panel IDs on the 24-column grid, every target on datasource uid `prometheus` with metrics that exist, every variable defined), in a test of its own so the OB-04 count stays five, plus that `deploy/grafana/demo/` provisions exactly one datasource, Prometheus.

## Consequences

- One binary carries the seed, the proxies, the faults, the scenarios and the GIF probes, so the make targets, verify-m8 and the vhs scenes all run the same code.
- The Playground load is bounded at 700 rps however many visitors arrive, and every run is attributable in the audit log without storing an address.
- `loadgen` changes only by moving code into `internal/loadgen`; its JSON and behaviour do not change.
- The public demo has its own Grafana with nothing but metrics; the dev Grafana stays as it is.

## Alternatives considered

- **Playground traffic from `demo-traffic` on the control plane's request** — a second process in the request path of the UI, and the keys would cross a process boundary.
- **Results through `/api/v1/stream`** — breaks oasdiff (§3) and mixes owned runs into shared subscriptions.
- **D4–D7 as Playground scenarios** — D4 changes config, D5 and D6 need admin faults, D7 blocks a key: none of that belongs to a viewer; the Playground keeps traffic-only scenarios.
- **A Grafana organisation for anonymous viewers in the dev Grafana** — needs an organisation created through the API at every start; a second small Grafana is simpler and cannot leak the dev datasources.
- **`EventSource` for the run stream** — would put the token in the URL; ADR-0059's fetch-based client is reused.
