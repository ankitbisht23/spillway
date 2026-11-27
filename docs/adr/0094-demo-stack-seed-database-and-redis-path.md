# 0094. Demo stack: seed, demo database and the toxiproxy Redis path

- **Status:** Accepted
- **Date:** 2026-11-27
- **Requirement IDs:** SC-01, SC-03, RL-03, RL-05, RL-08, RL-13, RL-22 – RL-24, GW-13, GW-14, CP-02, CP-20, UI-07

## Context

SC-01 asks for `make demo` with seeded tenants, plans and policies and a one-command `make demo-reset`; SC-03 asks for Redis traffic through per-node toxiproxy proxies via the ADR-0004 address map; D3 asks for three replicas behind a load balancer. The demo must not disturb anything else: `make dev`, verify-m1 … m7 and the bench must behave exactly as before, and dev data in Postgres (for example verify-m5's and verify-m6's tenants and published versions) must survive a demo and a reset.

What exists: compose's Postgres (`pgdata` volume, owner role `spillway`, app role `spillway_app` whose password `migrate` sets), the `migrate` one-shot, managed gateways by env (`SPILLWAY_GATEWAY_CONFIG=''`, `SPILLWAY_GATEWAY_IDENTITY=''`, `SPILLWAY_GATEWAY_CONTROL_PLANE=controlplane:9090`, as `test/verify/m6.sh` does), stream certificates for gateway-1 … gateway-5 from `make certs`, toxiproxy 2.13.0 with only its API published, and ADR-0074's test override that already points gateway-4 at proxies `redis-0` … `redis-5` (26370–26375) with `SPILLWAY_REDIS_ADDR_MAP`. The control plane's config is one global working set per database (ADR-0037), so importing the demo config into the dev database would replace whatever is there. ADR-0055 makes gateways resync when the stream epoch changes, which is what happens when a gateway meets a control plane on another database.

## Decision

### 1. The demo database (D1)

- The demo uses its own database, **`spillway_demo`**, in compose's Postgres: same container, same `pgdata` volume, same roles. The dev database `spillway` is never read or written by the demo.
- One-shot **`demo-db`** (profile `demo`, Postgres image by the digest compose already uses, label `spillway.oneshot: "true"`): connects to `postgres:5432` as `spillway` with `POSTGRES_PASSWORD` and runs `CREATE DATABASE spillway_demo OWNER spillway` only if `pg_database` has no such row; exits 0.
- One-shot **`demo-migrate`** (profile `demo`, the control-plane image, command `migrate`, `read_only`): as compose's `migrate` but with `SPILLWAY_CP_MIGRATE_DATABASE_URL=postgres://spillway@postgres:5432/spillway_demo?sslmode=disable`; depends on `demo-db` completing.
- The demo override sets the control plane's `SPILLWAY_CP_DATABASE_URL` to `postgres://spillway_app@postgres:5432/spillway_demo?sslmode=disable` and makes it depend on `demo-migrate` completing.
- **`make demo-reset`** (one command): (1) switch both faults off through the fault API (ADR-0095) if the control plane answers; (2) stop `demo-traffic` and `controlplane`; (3) `docker compose exec -T postgres psql -U spillway -d spillway -c 'DROP DATABASE IF EXISTS spillway_demo WITH (FORCE)'`; (4) `up -d --wait` of `demo-db`, `demo-migrate`, `controlplane`, `demo-seed`; (5) `up -d --wait --force-recreate gateway-1 gateway-2 gateway-3` (they start from the new config, not from a snapshot of the dropped database); (6) start `demo-traffic` again. It prints `demo-reset: done in <s> s`. It never runs `docker compose down`, never removes a volume, and never touches the `spillway` database or Redis (§3, D29).
- Dropping the database also drops its audit log. The demo database's audit trail therefore lives from one reset to the next; the dev database's audit log is untouched (its append-only rule, ADR-0040, is per database).

### 2. The compose override and how the demo leaves (D3)

- All demo-only compose content is in **`deploy/compose/demo.compose.yml`**, used only by the demo targets as a second `-f`. `compose.yaml` is unchanged by M8.
- New services, each with label **`spillway.demo: "true"`**: `demo-db`, `demo-migrate`, `demo-init`, `demo-seed` (one-shots), `demo-traffic` (long-running, ADR-0096), `caddy` and `grafana-demo` (profile `demo`, from the grafana-demo task, ADR-0096 §4, ADR-0097), `tunnel` (profile `share`) and `tunnel-stand-in` (profile `share-test`) (ADR-0097). One-shots also carry `spillway.oneshot: "true"`.
- Overridden services, which get the same label: `controlplane` (§1, ADR-0095 §1, ADR-0096 §3), `gateway-1`, `gateway-2`, `gateway-3` (§4).
- **`make demo`**: `make certs`, then `DEMO_MODE=true docker compose --env-file .env.example [--env-file .env.local] -f compose.yaml -f deploy/compose/demo.compose.yml --profile replicas --profile demo up -d --build --wait --wait-timeout 300` naming every long-running base service plus `controlplane gateway-1 gateway-2 gateway-3 demo-traffic` (and `caddy grafana-demo` once they exist). Naming the services keeps gateway-4 and gateway-5 down although `--profile replicas` is on. It ends by printing the UI, Grafana and Caddy URLs and `make demo-d1` … `make demo-d7`. Running it again on a running demo recreates only what changed.
- **`make demo-down`**: (1) faults off and `demo-traffic proxies --delete` (§5) while the stack is up; (2) `docker rm -f` of every container with labels `spillway.demo=true` and `com.docker.compose.project=spillway`; (3) `make dev`. It prints `demo-down: done` after checking that no such container is left and toxiproxy lists no `demo-*` proxy.
- **`make dev` gains one step before `up`**: it removes, with `docker rm -f`, every container labelled `spillway.demo=true` in the `spillway` project, and prints `removed <n> demo containers` when there were any. Containers only, never volumes. Every verify, chaos, NFR and bench target that has `dev` as a prerequisite therefore starts with no demo container running: no demo gateway, no base load, no managed-mode leftover. Without demo containers the step does nothing, so `make dev` behaves as today.
- `.env.example` gains `DEMO_MODE=false` (and the other demo variables of ADR-0095 – ADR-0097 with dev-safe defaults).
- Memory: the demo adds gateway-2 and gateway-3 (about 60 MB each at demo load), `demo-traffic` (about 30 MB), Caddy (about 30 MB) and `grafana-demo` (about 150 MB) to the dev stack, well inside Docker Desktop's 8.3 GB. kind and the demo are never run together (`make kind-up` already refuses while compose runs).

### 3. The seed (D2, D29)

- **Path:** the REST API, as a user would, so validation (CP-03), versions (CP-02, CP-20) and audit (CP-13) apply unchanged. The one-shot **`demo-seed`** (`demo-traffic seed`, ADR-0096) gets a `spillway-ci` token by client credentials from `http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/token` (secret from `SPILLWAY_CI_CLIENT_SECRET`, never printed) and talks to `http://controlplane:8080/api/v1`. It depends on the control plane and Keycloak being healthy; the gateways depend on it completing.
- **Config:** `deploy/demo/config.yaml` is imported (`POST /config/import` with the working set's `If-Match`, ADR-0044), then validated and published with note `demo seed` unless publish answers 409 `nothing_to_publish`. **Identity:** `deploy/demo/seed.yaml` lists tenants and consumers; each is looked up by its exact name (ADR-0036's filter) and created if absent, or updated (`If-Match`) if its plan differs. No API key is seeded: every key is a demo key issued at run time (ADR-0095 §3). Running it twice changes nothing the second time (an integration test runs it twice and compares versions).
- **Upstreams** (`orders` goes through toxiproxy for D6; the others go direct):

  | Upstream | Endpoints | Other settings |
  | --- | --- | --- |
  | `orders` | `toxiproxy:26391`, `toxiproxy:26392`, `toxiproxy:26393` (`demo-upstream-1` … `3` → `upstream-1` … `3:8080`) | `lb: p2c`; no health check; breaker, outlier detection and retry budget at their defaults (ADR-0030: `error_rate` 0.5, `window` 10 s, `min_requests` 20, `open_for` 10 s, `half_open_probes` 5; budget 20% + 3/s) |
  | `catalog`, `payments` | `upstream-1:8080`, `upstream-2:8080`, `upstream-3:8080` | as `orders` |

- **Routes** (`auth_mode: api_key` except `service`, which exists so `make demo-share-test` can check the gateways' issuer, ADR-0097 §3; each removes `X-API-Key` from the upstream request with `request_headers`; go-httpbin paths that never echo a request):

  | Route | Match | Upstream path | Timeouts | Retry |
  | --- | --- | --- | --- | --- |
  | `orders` | `GET /v1/orders` (exact) | `strip_prefix: /v1/orders`, `add_prefix: /bytes/512` | connect 1 s, per_try 2.5 s, total 6 s | `attempts: 2` |
  | `catalog` | `GET /v1/catalog` | `/bytes/256` likewise | as orders | none |
  | `payments` | `POST /v1/payments` | `/status/201` likewise | as orders | none |
  | `service` | `GET /v1/service`, **`auth_mode: jwt`** | `/status/204` likewise, upstream `catalog` | as orders | none |

- **Policies** (all GCRA, strict, `lease_k: 0`, every period 1 s):

  | Policy | Level | Match | Key | Limits | Mode | Failure mode |
  | --- | --- | --- | --- | --- | --- | --- |
  | `tenant-ceiling` | tenant | all four routes | `{tenant}` | 1,500/s, burst 1,500 | enforce | `fail_open` |
  | `consumer-plan` | consumer | `orders` | `{consumer}` | `from_plan: true` | enforce | `degraded` |
  | `catalog-limit` | route | `catalog` | `{consumer}` | 200/s, burst 200 | enforce | `fail_open` |
  | `payments-limit` | route | `payments` | `{consumer}` | 100/s, burst 200 | enforce | `fail_closed` |

  So during D5's cut, `catalog` fails open, `payments` fails closed and `orders` is limited locally by `consumer-plan`'s degraded limiters (`tenant-ceiling` fails open everywhere, ADR-0021's precedence). D2 is hierarchical (RL-05): a tenant level and a consumer level in one tenant-slot call. D3 runs on `payments`: its only failure outcome is a 503, so every 2xx there is a Redis decision that the accuracy check can place on Redis time (ADR-0098 §D3).
- **Plans** (`consumer-plan` entries; no quotas, so no `q:v1:` keys exist in the demo):

  | Plan | `consumer-plan` limit | Per replica when degraded (3 live) |
  | --- | --- | --- |
  | `free` | 10/s, burst 20 | 3/s, burst 6 |
  | `pro` | 100/s, burst 200 | 33/s, burst 66 |
  | `enterprise` | 1,000/s, burst 1,000 | 333/s, burst 333 |

- **Tenants and consumers** (`seed.yaml`; consumers use their tenant's plan, `plan_id` null):

  | Tenant (plan) | Consumers |
  | --- | --- |
  | `free-co` (`free`) | `free-co-web` (base load), `free-co-mobile` (D1, D2), `free-co-partner` (D7), `free-co-playground` |
  | `pro-co` (`pro`) | `pro-co-web`, `pro-co-mobile` (D1, D3, D5, D6), `pro-co-playground`, `pro-co-service` (`jwt_subject` = `gateway-tests`'s service account `0b5e7c1a-5f1d-4c3e-9a00-0000000000c2`, ADR-0049) |
  | `enterprise-co` (`enterprise`) | `enterprise-co-web` (D4's would-be-hit consumer), `enterprise-co-mobile` (D2), `enterprise-co-playground` |

- **Why the numbers.** Every GCRA burst covers at least one second of its rate (free 2 s, pro 2 s, degraded pro 2 s, payments 2 s, ceiling 1 s), which ADR-0098 relies on to make admissions independent of host stalls. Every period is 1 s, so limiter keys live at most a few seconds (RL-15) and a reset needs no Redis clean-up. With no quotas there is nothing to flush, and the demo override leaves `SPILLWAY_CP_REDIS_ADDRS` unset (flusher off, ADR-0070), so the demo control plane never writes quota epochs for tenant IDs that the dev database also uses.

### 4. The demo gateways (D4)

- The override sets, for **gateway-1, gateway-2 and gateway-3** only: `SPILLWAY_GATEWAY_CONFIG: ""`, `SPILLWAY_GATEWAY_IDENTITY: ""`, `SPILLWAY_GATEWAY_CONTROL_PLANE: controlplane:9090` (managed mode, ADR-0038) and `SPILLWAY_REDIS_ADDR_MAP: redis-0:6379=toxiproxy:26380,redis-1:6379=toxiproxy:26381,redis-2:6379=toxiproxy:26382,redis-3:6379=toxiproxy:26383,redis-4:6379=toxiproxy:26384,redis-5:6379=toxiproxy:26385`; and `depends_on` `demo-init` and `demo-seed` completing. Everything else is compose's: dev mode (`Spillway-Decision-Time`, `Spillway-Redis-Error`, `Spillway-Attempts`, used by ADR-0098), trace ratio 1, the heavy-hitter secret, the JWT settings, their state volumes and stream certificates.
- The demo always runs these three, so `live_replicas` is 3 (RL-24) and the degraded limits above hold. gateway-4 and gateway-5 stay down.

### 5. The toxiproxy paths (D5)

- **Proxies.** The one-shot **`demo-init`** (`demo-traffic proxies`, profile `demo`, depends on toxiproxy healthy) creates through `http://toxiproxy:8474`, deleting any of the same name first:
  - `demo-redis-0` … `demo-redis-5`: `listen 0.0.0.0:2638N`, `upstream redis-N:6379`;
  - `demo-upstream-1` … `demo-upstream-3`: `listen 0.0.0.0:2639N`, `upstream upstream-N:8080`.
  They listen inside the compose network only (docs/ports.md). `demo-traffic proxies --delete` removes them (`make demo-down`). ADR-0074's transient `redis-0` … `redis-5` (26370–26375) keep their names and ports, so neither path can delete the other's proxies.
- **Slot map, `MOVED` and `ASK`.** The gateway's go-redis `ClusterClient` starts from its seeds (`redis-N:6379`, `SPILLWAY_GATEWAY_REDIS_ADDRS`), which the Dialer maps to `toxiproxy:2638N`, which forwards to `redis-N:6379`. Each node announces its hostname (`cluster-announce-hostname redis-N`, `cluster-preferred-endpoint-type hostname`), so `CLUSTER SLOTS`/`SHARDS` replies and every `MOVED` and `ASK` redirect name `redis-N:6379`; the client keys its node table by that name and dials it through the same Dialer, so it reaches `redis-N` through `demo-redis-N`. No reply names an IP, so no connection can bypass the map. The breaker per node (ADR-0021), `gw_redis_errors_total` and `Spillway-Redis-Error` keep the logical `redis-N:6379` names. The demo-seed task adds an integration test: a cluster client with the demo map and a Dialer wrapper records every address it dials while it runs scripts on keys of all three primaries and follows a forced `MOVED` (a slot's owner asked for directly on the wrong node); every dial must be a mapped one.
- **What the proxies cost and touch.** One extra hop inside the compose network on each demo Redis call (well under a millisecond, inside the 5 ms budget); the NFRs are never measured on the demo stack. Nothing else uses these proxies: the dev and verify gateways, the dev control plane's flusher and `redis-exporter` keep dialling `redis-N:6379` directly, and the cluster itself never sees a fault (a cut is a client-side black hole, ADR-0095 §5), so no failover happens and nothing needs repairing afterwards.

### 6. D3's load balancer (D6)

The "load balancer" in front of the three replicas is **client-side round robin** over `http://gateway-1:8000`, `http://gateway-2:8000` and `http://gateway-3:8000`, in `demo-traffic` (every scenario and the base load) and in the Playground generator: request i goes to replica i mod 3. It is the accuracy harness's balancer (ADR-0021), so D3 reuses its accuracy method unchanged, adds no hop and no component, and the Demo dashboard shows the even split per replica (`replica` label, ADR-0059). Caddy is not used for gateway traffic: the gateway is never behind the public proxy (SC-09, setup.md Step 7). P47 proposes the D3 wording.

### 7. Realm additions (D28)

demo-seed adds `http://localhost:8888/oidc/callback` to `admin-ui`'s redirect URIs, `http://localhost:8888/signed-out` to its post-logout redirect URIs and `http://localhost:8888` to its web origins in `deploy/compose/keycloak/realm.json`, so the UI also works through the demo proxy on the laptop. Keycloak re-imports the realm only on recreate (setup.md Step 4), so `make demo` runs `up -d --force-recreate --wait keycloak` first when `realm.json` is newer than the running container's start time (`docker inspect -f '{{.State.StartedAt}}'`, which prints that one field and no environment).

### Tests the demo-seed task adds

- Go unit tests for the seed file parsing and the idempotent create-or-update logic (fake API).
- Integration: the seed twice against a control plane on a scratch database (testcontainers), identical versions after the second run; the map test of §5.
- A `make demo` → `make demo-reset` → `make demo-down` → `make dev` cycle run three times by hand, with the times in the PR, and `make verify-m0` passing after it (no stopped long-running service, no demo container).

## Consequences

- The demo and dev share containers (Postgres, Redis, Keycloak, observability, toxiproxy) but no database, no gateway configuration and no proxy.
- `make demo-reset` is safe to run at any time and restores exactly the seed; it loses the demo's audit log and identity history, by design.
- A reviewer's `make demo` starts from whatever dev state exists; `make dev` afterwards removes every trace of the demo except its database, which `docker compose down -v` (the documented full reset) removes with everything else.
- Every new verify or chaos target of a later milestone inherits the demo clean-up through its `dev` prerequisite.

## Alternatives considered

- **Seed SQL migration** — would put demo rows into every database, including dev and Helm, and bypass validation, versions and audit.
- **A seed subcommand of the control plane writing to the store** — skips the REST API's validation path and needs its own audit actor; the API path is the one users exercise.
- **Demo data in the dev database with name prefixes** — the config working set is global, so the demo config would still replace dev's; a reset could not avoid touching dev data.
- **A second Postgres container for the demo** — more memory and a second volume to explain; a second database in the same server isolates just as well.
- **Caddy or a small Go proxy as D3's balancer** — an extra hop in every measured request, another failure point, and the client IP would have to be trusted through it (SEC-05).
- **Proxying the dev gateways' Redis too** — would put toxiproxy's hop into every NFR measurement.
