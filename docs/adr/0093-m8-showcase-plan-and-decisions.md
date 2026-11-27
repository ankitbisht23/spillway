# 0093. M8 showcase: plan and decisions

- **Status:** Accepted
- **Date:** 2026-11-27
- **Requirement IDs:** SC-01 – SC-09, UI-07, CP-12, CP-20, SEC-05, RL-03, RL-05, RL-08, RL-09, RL-11 – RL-14, RL-21 – RL-26, GW-13, GW-14, UI-01, CP-04, NFR-15

## Context

M8 turns M0–M7 into something a reviewer can see in five minutes: `make demo`, seven one-minute scenarios D1–D7 (docs/design.md, Showcase), a Playground screen, a Grafana Demo dashboard, a public demo behind one tunnel, README GIFs and `docs/demo.md`. Its exit is `make verify-m8` running D1–D7 and checking each. The user approved the task order — design-adrs, checkpoint, demo-seed, demo-guards, fault-switches, traffic-sim, playground, grafana-demo, demo-share, verify, readme, demo-doc, then review and verify-main — and asked this run to settle the design in ADRs before any code, shown at a checkpoint.

Facts from the code on `main` (5eb35c8) that shape the plan:

- There is **no `operator` role**. ADR-0040 removed it (five roles: viewer, editor, publisher, key-admin, admin), and ADR-0043 proposed P12 (SC-03's "Operator role only" → "Admin role only"), which the user never decided; design.md still reads "Operator role only". M6 (ADR-0056) and M7 (ADR-0068) carried "the Playground and SC-03's wording" to M8.
- Plans, overrides, keys, access lists and heavy hitters exist only in **managed mode** (ADR-0041, ADR-0051). `make dev` runs gateway-1 in static mode; verify-m5/m6 switch gateways to managed mode by env and back.
- Config is **one global working set** per database: importing a demo config would replace whatever the dev database holds (verify-m5/m6 data included).
- The gateway already rejects a managed key past its `expires_at` on every request (internal/config/identity.go), and the control plane leaves expired and revoked keys out of snapshots.
- Redis nodes announce hostnames (`redis-N:6379`), so every `CLUSTER SLOTS`, `MOVED` and `ASK` answer names an address the ADR-0004 address map can rewrite. ADR-0074's `make chaos-redis-latency` already runs a gateway through per-node toxiproxy proxies this way (`test/chaos/latency.compose.yml`).
- Adding a value to an enum that a **response** uses is an oasdiff error (ADR-0059); the stream's `sub` enum is such a schema, and the `Error.code` enum is closed.
- The host / Docker Desktop VM stalls: 4–9 ms about every 15 s under load, 100–820 ms at worst, and twice in one NFR-04 run about 25 s apart on `main` a69b856 (ADR-0027, ADR-0034, README; the user recorded that as a macOS / Docker Desktop constraint on 2026-11-27, no rule change).

## Decision

### The M8 ADRs

| ADR | Topic | Tasks that implement it |
| --- | --- | --- |
| 0093 (this) | Plan, task order, requirement map, P-items, decisions | all |
| [0094](0094-demo-stack-seed-database-and-redis-path.md) | Demo stack: compose override, demo database, seed, `make demo` / `demo-reset` / `demo-down`, demo gateways, toxiproxy Redis and upstream paths, D3's load balancer | demo-seed |
| [0095](0095-demo-mode-guards-demo-keys-and-fault-switches.md) | `DEMO_MODE` wiring and the 404 list, demo keys (30 min, limits, cleanup), fault switches (API, toxics, RBAC, make targets) | demo-guards, fault-switches |
| [0096](0096-traffic-simulator-playground-and-grafana-demo.md) | `cmd/demo-traffic` (base load, D1–D7 shapes), the Playground (API, generator, per-run SSE, caps, UI), the demo Grafana and its dashboard | traffic-sim, playground, grafana-demo |
| [0097](0097-public-demo-caddy-tunnel-and-demo-share.md) | Caddy, cloudflared, `make demo-share` / `demo-share-stop`, public-demo hardening, `make demo-share-test` without a tunnel | demo-share |
| [0098](0098-make-verify-m8.md) | `make verify-m8`: per-scenario load, pass rules, stall tolerance, time | verify |
| [0099](0099-readme-gifs-and-recording.md) | README GIFs: vhs and Playwright recording, masking, sizes, regeneration | readme, demo-doc |

These ADRs are binding for every later M8 task. A later change goes in a new ADR.

### Task order

Confirmed as approved, with no split or re-order:

| # | Task (branch `m8-<key>`) | Builds | ADR |
| --- | --- | --- | --- |
| 1 | design-adrs | these ADRs, docs/ports.md notes for the demo's in-network ports | 0093–0099 |
| — | checkpoint | the user reviews D1–D31 and P42–P54 | — |
| 2 | demo-seed | `deploy/compose/demo.compose.yml`, `deploy/demo/config.yaml` and `seed.yaml` (with the `service` route and `pro-co-service` consumer of ADR-0097 §3), the `spillway_demo` database, `demo-traffic seed` and `demo-traffic proxies` (a first `cmd/demo-traffic` with those two subcommands and the image target), `make demo`, `make demo-reset`, `make demo-down`, `make dev`'s demo clean-up, realm URIs for `localhost:8888` | 0094 |
| 3 | demo-guards | `SPILLWAY_CP_DEMO_MODE`, the `/api/v1/demo/` 404 guard, `GET /api/v1/demo`, the 30-minute key rule, the expired-key janitor, the UI's demo probe (nav hidden on 404) | 0095 |
| 4 | fault-switches | `PUT /api/v1/demo/faults/{name}`, the control plane's toxiproxy client, `demo-traffic fault`, `make demo-fault-redis`, `make demo-fault-upstream`, `make demo-fault-clear` | 0095 |
| 5 | traffic-sim | `demo-traffic run --scenario=base\|d1…d7` (with `--check`), `demo-traffic probe`, the `demo-traffic` compose service, `make demo-d1` … `make demo-d7` | 0096 |
| 6 | playground | the Playground API, generator and per-run SSE in the control plane, caps, the Playground screen with admin fault buttons | 0096 |
| 7 | grafana-demo | `grafana-demo`, `deploy/grafana/demo/demo.json`, `make lint-obs` over it, Caddy's `/grafana` route (Caddy itself lands here, local-only) | 0096, 0097 |
| 8 | demo-share | the tunnel and stand-in services, `demo-traffic share-config`, `share-check` and `stand-in-tunnel`, `make demo-share`, `make demo-share-stop`, `make demo-share-test`, `SPILLWAY_CP_TRUSTED_PROXIES`, Keycloak hardening, `deploy/demo/run/` in `.gitignore`, setup.md Steps 4, 6, 7 and 8 | 0097 |
| 9 | verify | `make verify-m8`, at least three full runs with their pass rate in the PR | 0098 |
| 10 | readme | `make demo-gifs`, the seven GIFs, README tables and quick start | 0099 |
| 11 | demo-doc | `docs/demo.md` live-demo script and video shot list (the video itself is the user's) | 0099 |

Then the review and verify-main steps.

### Checklist boxes and requirements

| M8 checklist box (text unchanged) | Ticked by |
| --- | --- |
| Demo seed data, `make demo` and `make demo-reset` (SC-01) | demo-seed |
| Traffic simulator and toxiproxy-only fault switches for D1–D7 (SC-02, SC-03) | playground (SC-03's Playground buttons complete it) |
| Playground screen with server-side traffic, and Grafana Demo dashboard (SC-04, SC-05, UI-07) | grafana-demo |
| Demo-mode guards and expiring demo keys (SC-08) | playground (the last task that adds a guarded surface; each earlier task adds its own guard tests) |
| Public demo: Caddy on 8888 behind one tunnel, `make demo-share` (SC-09) | demo-share |
| README GIFs (vhs for terminal scenes, a screen recorder for the UI), tables and quick start (SC-06) | readme |
| Walkthrough video and `docs/demo.md` live-demo script (SC-07) | never in this run: the video is the user's; demo-doc writes `docs/demo.md` |

| Requirement | Implemented by |
| --- | --- |
| SC-01 | demo-seed |
| SC-02 | traffic-sim |
| SC-03 | fault-switches (API, make targets), playground (buttons) |
| SC-04 | playground |
| SC-05 | grafana-demo |
| SC-06 | readme |
| SC-07 | demo-doc (`docs/demo.md`); video by the user |
| SC-08 | demo-guards (guard, keys, janitor), fault-switches and playground (their surfaces' 404 tests) |
| SC-09 | grafana-demo (Caddy, local), demo-share (tunnel, settings, test) |
| UI-07 | playground |

The M8 exit line (`make verify-m8` runs D1–D7 and checks each; a newcomer reproduces every scenario from `docs/demo.md`) is met by verify and demo-doc together.

### What exists between tasks

- After demo-seed: `make demo` boots the demo stack (three managed gateways through toxiproxy, seeded `spillway_demo`), but `DEMO_MODE` has no effect yet and nothing generates traffic; `make demo-reset` and `make demo-down` work; `make dev` and every earlier verify target behave exactly as before.
- After demo-guards: `/api/v1/demo/` answers 404 when demo mode is off and `GET /api/v1/demo` answers when on; keys issued in demo mode expire within 30 minutes.
- After fault-switches: D5 and D6 faults switch from make targets; after traffic-sim, every scenario runs from make; after playground, from the UI too; after grafana-demo, the Demo dashboard shows them at `http://localhost:8888/grafana`.

### Proposed design-text changes (for the user's approval)

Each item quotes the current text and the proposed text; none is applied by this PR. "Applied by" names the task that edits docs/design.md once the user approves it.

- **(P42) SC-03** — supersedes ADR-0043's P12, which was never decided. Applied by fault-switches.
  - Current: "**SC-03** (M8) Fault switches for D5 and D6 as `make` targets and Playground buttons, toxiproxy only: Redis traffic runs through per-node toxiproxy proxies via the ADR-0004 address map, and upstream latency is added in toxiproxy. Operator role only, `DEMO_MODE=true` only; no container gets the Docker socket."
  - Proposed: "**SC-03** (M8) Fault switches for D5 and D6 as `make` targets and Playground buttons, both through one audited control-plane endpoint, toxiproxy only: the demo gateways' Redis traffic runs through per-node toxiproxy proxies via the ADR-0004 address map, which D5 cuts, and D6's upstream latency is added in toxiproxy. Admin role only, `DEMO_MODE=true` only; no container gets the Docker socket."
- **(P43) SC-08** — applied by demo-guards.
  - Current: "**SC-08** (M8) Demo mode is off by default: Playground, demo endpoints and fault switches return 404 unless `DEMO_MODE=true`, and demo keys expire after 30 minutes with their own limits."
  - Proposed: "**SC-08** (M8) Demo mode is off by default: the Playground, demo endpoints and fault switches (everything under `/api/v1/demo/`) return 404 unless the control plane runs with `DEMO_MODE=true`, and the UI then hides the Playground. In demo mode every API key the control plane issues or rotates expires within 30 minutes; Playground keys never leave the control plane, belong to per-tenant playground consumers with their own counters, and Playground runs are capped per run, per caller, per client IP and in total."
- **(P44) UI-07 and the Playground row** — applied by playground.
  - Current UI-07: "**UI-07** (M8) Playground traffic is generated server-side: the control plane drives the real gateway in-network with short-lived demo keys, and the browser only shows results over SSE, so there are no cross-origin calls. Available only when `DEMO_MODE=true` (SC-08)."
  - Proposed UI-07: "**UI-07** (M8) Playground traffic is generated server-side: the control plane drives the real gateways in-network, round robin, with short-lived demo keys that never leave its memory, and the browser only shows results over a per-run SSE stream, so there are no cross-origin calls. Every role may start runs; only the caller who started a run may watch or stop it. Available only when `DEMO_MODE=true` (SC-08)."
  - Current row: "| Playground (demo mode) | Allowed vs denied per tenant, `RateLimit` headers, token-bucket visual, live over SSE | Pick tenant and rate, fire traffic (generated server-side, UI-07), run a scenario |"
  - Proposed row: "| Playground (demo mode) | Allowed vs denied per tenant, `RateLimit` headers, token-bucket visual, live over SSE | Pick tenant, route and rate, fire traffic (generated server-side, UI-07), run a traffic scenario (D1–D3); admins switch the D5 and D6 faults (SC-03) |"
- **(P45) Admin API table** — applied row by row by the task that adds each operation (demo-guards: `GET /api/v1/demo`; fault-switches: faults; playground: the run rows and the stream row's change).
  - Current: "| GET | `/api/v1/stream` | SSE: live metrics, replica status, heavy hitters, Playground results (demo mode) |"
  - Proposed: "| GET | `/api/v1/stream` | SSE: live metrics, replica status, heavy hitters |", plus after it:
    - "| GET | `/api/v1/demo` | Demo mode: tenants, routes, scenarios, fault states and the caller's Playground limits (404 when demo mode is off, SC-08) |"
    - "| POST | `/api/v1/demo/playground/runs` | Start a Playground run (UI-07) |"
    - "| DELETE | `/api/v1/demo/playground/runs/{id}` | Stop one's own run |"
    - "| GET | `/api/v1/demo/playground/runs/{id}/stream` | SSE: one run's results |"
    - "| PUT | `/api/v1/demo/faults/{name}` | Switch a D5 or D6 fault on or off (admin, SC-03) |"
- **(P46) CP-12** — applied by playground.
  - Current: "**CP-12** (M5) RBAC with five global roles, each able to read everything except the audit log: viewer (read only), editor (config working set and import), publisher (publish and rollback), key-admin (consumers, API keys, overrides and access lists), admin (everything, including tenants and the audit log)."
  - Proposed: "**CP-12** (M5, M8 Playground) RBAC with five global roles, each able to read everything except the audit log: viewer (read only, plus starting Playground runs in demo mode, UI-07), editor (config working set and import), publisher (publish and rollback), key-admin (consumers, API keys, overrides and access lists), admin (everything, including tenants and the audit log)."
- **(P47) D3 row** — applied by traffic-sim.
  - Current: "| D3 | Distributed accuracy | 3 replicas behind a load balancer; allowed rate tracks the limit line exactly | RL-11 – RL-14 |"
  - Proposed: "| D3 | Distributed accuracy | 3 replicas behind a round-robin load balancer (the traffic simulator's); allowed rate tracks the limit line exactly | RL-11 – RL-14 |"
- **(P48) SC-01** — applied by demo-seed.
  - Current: "**SC-01** (M8) `make demo` boots the stack with seeded tenants, plans, keys and policies; `make demo-reset` restores it in one command."
  - Proposed: "**SC-01** (M8) `make demo` boots the stack in demo mode with seeded tenants, consumers, plans, routes and policies in its own database (`spillway_demo`), and demo keys are issued at run time (SC-08); `make demo-reset` restores it in one command without touching the dev database, and `make demo-down` returns to `make dev`."
- **(P49) SC-02** — applied by traffic-sim.
  - Current: "**SC-02** (M8) Traffic simulator `cmd/demo-traffic --scenario=d1…d7` drives base load plus each scenario’s bursts."
  - Proposed: "**SC-02** (M8) Traffic simulator `cmd/demo-traffic --scenario=base|d1…d7` drives each tenant's base load (`base`, running while the demo runs) and, for one minute per scenario, base load plus that scenario's bursts, faults or actions (`make demo-d1` … `make demo-d7`)."
- **(P50) SC-05** — applied by grafana-demo.
  - Current: "**SC-05** (M8) Grafana Demo dashboard: allowed vs denied and p99 per tenant, limiter mode, replica count."
  - Proposed: "**SC-05** (M8) Grafana Demo dashboard: allowed vs denied and p99 per tenant, limiter mode, replica count, in a demo Grafana (`grafana-demo`: anonymous viewers, Prometheus only, no login) that the demo proxy serves at `/grafana`."
- **(P51) SC-09** — applied by demo-share. Everything before "restarts what changed" stays as it is; only the end changes:
  - Current: "… and restarts what changed; a named tunnel gives a stable link."
  - Proposed: "… and restarts what changed. For the share it disables every realm test user except `test-viewer`, sets `test-admin`'s password from `.env.demo`, gives `spillway-ci` and `gateway-tests` new secrets, disables `rbac-tests` and turns on brute-force detection; the proxy refuses `/auth/admin`; it asks for confirmation before it opens a tunnel. `make demo-share-test` checks every one of these settings against a stand-in tunnel without opening one. A named tunnel gives a stable link."
- **(P52) SEC-05** — applied by demo-share.
  - Current: "**SEC-05** (M2) `X-Forwarded-For` trusted only from a configured proxy CIDR list, so clients cannot spoof IP-based limits."
  - Proposed: "**SEC-05** (M2, M8 public demo) `X-Forwarded-For` trusted only from a configured proxy CIDR list, so clients cannot spoof IP-based limits; in the public demo the control plane trusts `CF-Connecting-IP` only from the demo proxy, which trusts it only from the tunnel."
- **(P53) Stack table, Demo row** — applied by readme.
  - Current: "| Demo | vhs for terminal GIFs; Caddy reverse proxy behind a Cloudflare tunnel (quick or named) for live sharing |"
  - Proposed: "| Demo | vhs for terminal GIFs, Playwright video for UI GIFs; Caddy reverse proxy behind a Cloudflare tunnel (quick or named) for live sharing |"
- **(P54) Repo layout** — applied by demo-seed (`deploy/`) and readme (`docs/`).
  - Current `deploy/` line: "├── deploy/               # compose/ (service configs), kind/ (cluster, dependency and observability manifests, values), helm/spillway/, grafana/dashboards/, prometheus/rules/"
  - Proposed: "├── deploy/               # compose/ (service configs), demo/ (demo seed, Caddyfile, demo scripts), kind/ (cluster, dependency and observability manifests, values), helm/spillway/, grafana/dashboards/, grafana/demo/, prometheus/rules/"
  - Current `docs/` line: "├── docs/                 # design.md, setup.md, demo.md, ports.md, adr/, runbooks/"
  - Proposed: "├── docs/                 # design.md, setup.md, demo.md, ports.md, adr/, runbooks/, media/ (README GIFs and their tapes)"

### Questions for the user at the checkpoint

1. P42–P54: approve, change or reject each.
2. D16 (viewers start Playground runs, P46): the Playground exists for visitors, who are viewers; the alternative is no Playground for visitors.
3. D23 (D5's "within one budget" checked by medians and a p99, not ADR-0027's every-check line) and D24 (D2 over 20 slot pairs, not NFR-15's 30): see ADR-0098.
4. D20: GitHub-brokered visitors are viewers only, and the owner's admin login in the public demo is `test-admin` with `DEMO_ADMIN_PASSWORD`, which changes setup.md Step 4.6.

### Decisions taken for the user (review)

- **D1 Demo database** — the demo runs on its own database `spillway_demo` in compose's Postgres (same volume, same roles); the control plane points at it only in demo mode; `make demo-reset` drops and recreates it (ADR-0094 §1). *Why:* config is one global working set, so only a separate database keeps demo and dev data apart and makes reset one safe command.
- **D2 Seed path** — the REST API, from `deploy/demo/config.yaml` (imported and published) and `deploy/demo/seed.yaml` (tenants and consumers, created by name if absent), by the one-shot `demo-seed` (`demo-traffic seed`); idempotent; no API key is seeded (ADR-0094 §3). *Why:* the API validates, versions and audits exactly as for users, and keys must be runtime demo keys.
- **D3 Demo compose override** — everything demo-only lives in `deploy/compose/demo.compose.yml` (new services under profiles `demo`, `share`, `share-test`; overrides of controlplane and gateway-1…3); `compose.yaml` does not change; every container it creates or overrides carries `spillway.demo=true`, and `make dev` removes such containers before `up` (ADR-0094 §2). *Why:* `make dev`, verify-m1…m7 and the bench can never run on a demo container.
- **D4 Demo gateways** — gateway-1, gateway-2 and gateway-3 in managed mode, always three, and only they use the proxied Redis address map (ADR-0094 §4). *Why:* D3 needs three replicas, plans and heavy hitters need managed mode, and keeping it fixed makes every scenario's numbers fixed.
- **D5 Toxiproxy demo proxies** — `demo-redis-0`…`5` on 26380–26385 and `demo-upstream-1`…`3` on 26391–26393 inside the network, created by the one-shot `demo-init`, removed by `make demo-down`; disjoint from ADR-0074's transient `redis-N` proxies (ADR-0094 §5). *Why:* the demo and the chaos test can never delete each other's proxies.
- **D6 D3's load balancer** — client-side round robin over gateway-1…3 in `demo-traffic` and in the Playground generator, as the accuracy harness does (ADR-0094 §6, P47). *Why:* no extra hop or component, and D3 reuses the harness's accuracy method unchanged.
- **D7 `DEMO_MODE` wiring** — compose variable `DEMO_MODE` (default `false`, in `.env.example`), mapped only by the demo override to the control plane's `SPILLWAY_CP_DEMO_MODE`; `make demo` sets it `true`; only the control plane reads it; gateways and Helm never do (ADR-0095 §1). *Why:* one process owns every demo surface, and the hot path does not change.
- **D8 404 list** — when demo mode is off, every request under `/api/v1/demo/` gets 404 `not_found` before authentication: `getDemo`, `startPlaygroundRun`, `stopPlaygroundRun`, `openPlaygroundStream`, `setDemoFault`; the UI hides the Playground when `GET /api/v1/demo` is 404 (ADR-0095 §2). *Why:* a single prefix check is easy to prove complete, and off means absent.
- **D9 Demo keys** — in demo mode every key the control plane issues or rotates expires within 30 minutes (no `expires_at` = exactly 30 minutes; later = 422); ordinary `api_keys` rows; the gateway's existing expiry check enforces it; plaintext only in the issuer's memory (ADR-0095 §3). *Why:* one invariant covers Playground keys, simulator keys and keys a user issues, with no schema change.
- **D10 Demo keys' own limits** — Playground keys belong to per-tenant `<tenant>-playground` consumers (own counters under the tenant's plan, inside the tenant ceiling); simulator keys to the `-web`, `-mobile` and `-partner` consumers; Playground runs are capped (ADR-0095 §3, ADR-0096 §3). *Why:* visitors never eat a scenario's budget, and the Playground still shows each plan's limit.
- **D11 Expired-key janitor** — demo mode only, active replica, every 10 minutes: deletes keys that expired over an hour ago and that no key rotated from, as one identity change with one audit record per key (ADR-0095 §4). *Why:* bounded tables without touching live keys.
- **D12 Fault switches** — two faults, `redis-cut` and `upstream-slow`, one operation `PUT /api/v1/demo/faults/{name}`, admin only (P42; no operator role exists), audited; the make targets call the same operation through `demo-traffic fault`; the control plane talks to toxiproxy's API (ADR-0095 §5). *Why:* one path, one RBAC rule and one audit trail for buttons and targets, and no Docker socket.
- **D13 Toxics** — Redis cut = a `timeout` toxic with `timeout: 0` on both streams of all six demo Redis proxies (a black hole, like `docker pause` for the client); upstream slow = a 4,000 ms `latency` toxic downstream on the three demo upstream proxies (ADR-0095 §5). *Why:* D5 then exercises the time budget like the chaos table's full outage, and D6's latency is above every per-try timeout.
- **D14 Traffic simulator** — `demo-traffic run --scenario=base|d1…d7`, fixed base load per tenant (258 rps in all), a fixed shape of 45–90 s per scenario, `make demo-dN` stops the base service, runs dN (base + bursts) and restarts it (ADR-0096 §1–2). *Why:* exactly one base load at any time, and every number in the scenarios is fixed.
- **D15 Playground** — a generator inside the control plane; operations under `/api/v1/demo/playground/runs`; a per-run SSE stream as its own operation (not a `/stream` subscription, which would break oasdiff); owner-only stream and stop; caps per run (≤ 200 rps, ≤ 60 s), per client IP (1 active, 30 an hour), per caller (3 active, 120 an hour) and in total (3 runs, 700 rps, one scenario run) (ADR-0096 §3). *Why:* bounded, attributable server-side load with no cross-origin call.
- **D16 Viewers may start Playground runs** — P46 amends CP-12 (ADR-0096 §3). *Why:* visitors are viewers (setup.md Step 4); without it the public Playground is unusable.
- **D17 Demo Grafana** — a second Grafana, `grafana-demo` (profile `demo`, no host port), anonymous Viewer, login disabled, only the Prometheus datasource and only the Demo dashboard (`deploy/grafana/demo/demo.json`), served by Caddy at `/grafana` (ADR-0096 §4, P50). *Why:* the dev Grafana's Loki and Tempo hold logs a public visitor must not query.
- **D18 Caddy and cloudflared** — `caddy:2.11.9-alpine@sha256:003a97808e781aa9f7e75cc2f43d34d461099031faf5e20faefb9f61126836f0` and `cloudflare/cloudflared:2026.11.0@sha256:b94b96edf2c9c4360f8879c8580cfaa99077f19c849ec595ca2f711b27d4f344`, static addresses 172.29.255.10 and 172.29.255.11; Caddy routes `/auth` (without `/auth/admin`), `/grafana` and the rest to the control plane (ADR-0097 §1). *Why:* exact, multi-arch, over two weeks old, and the only public surface is what SC-09 lists.
- **D19 Client IP** — Caddy trusts `CF-Connecting-IP` only from the tunnel's address and sets it on every request; the control plane trusts it only from Caddy's (`SPILLWAY_CP_TRUSTED_PROXIES`), uses it only for the Playground's per-IP caps, never logs or stores it (ADR-0097 §2, P52). *Why:* setup.md Step 7's rule, with each hop trusting only the next.
- **D20 Share hardening** — `make demo-share` needs `.env.demo` with non-dev `KEYCLOAK_ADMIN_PASSWORD` and `DEMO_ADMIN_PASSWORD`; disables test users but `test-viewer`, sets `test-admin`'s password, gives `spillway-ci` and `gateway-tests` new secrets kept in 0600 files, disables `rbac-tests`, turns on brute-force detection; asks the user to type `share`; a named tunnel with `TUNNEL=named`; GitHub login (named tunnel only) brokers visitors as viewers (ADR-0097 §3). *Why:* the realm's dev secrets and passwords are public in the repository.
- **D21 Stand-in share test** — `make demo-share-test` runs the whole share path with a stand-in tunnel container (`demo-traffic stand-in-tunnel`) and generated throwaway secrets, checks every setting SC-09 names plus the hardening, SEC-05 and the restore, and never contacts Cloudflare; verify-m8 runs it (ADR-0097 §4). *Why:* the share path is tested end to end without publishing anything.
- **D22 `make verify-m8`** — compose only, about 31 minutes plus build time: guards off, demo keys, D1–D7, the stand-in share test, clean-up and guards off again; one PASS/FAIL line per rule with the measured number; no re-run (ADR-0098). *Why:* every scenario is checked by its proven requirement's own rule.
- **D23 D5 budget rule** — RL-22's budget is checked as ADR-0074's Redis-latency chaos checks it (each route's median during the cut ≤ its pre-cut median + 10 ms) plus ≥ 99% of each gateway's Redis checks in the 10 ms bucket, not ADR-0027's every-check line (ADR-0098 §D5). *Why:* the every-check line cannot be made stall-proof; this form is the one the user approved for the toxiproxy Redis path. **Flagged for review.**
- **D24 D2 pairs** — 20 (off, on) pairs of 10 s slots instead of NFR-15's 30; the rule (median p99 ratio < 1.10 per quiet tenant) is unchanged (ADR-0098 §D2). *Why:* keeps the run near 30 minutes; a median of 20 still ignores up to 9 stalled pairs. **Flagged for review.**
- **D25 Controlled load in verify** — verify-m8 stops the base-load service and each check sends its own base load plus its bursts (ADR-0098 §1). *Why:* every measured request is from a specified schedule.
- **D26 GIFs** — vhs `ghcr.io/charmbracelet/vhs:v0.12.1@sha256:ea49a6a1c529be83153e88321892b5585964418f0b9055e8c1e0d732194234a3` for terminal scenes (in the compose network, a mounted static `demo-traffic`, no Docker socket), Playwright 1.64.1 video for UI scenes, ffmpeg from the same vhs image; seven GIFs in `docs/media/`, ≤ 2 MB each; `make demo-gifs`; leak checks on every scene (ADR-0099). *Why:* pinned, reproducible, and no frame can carry a key or token.
- **D27 Box ticks** — box 2 and the SC-08 box are ticked by playground, box 3 by grafana-demo, the SC-07 box by nobody in this run (this ADR). *Why:* a box is ticked by the task that completes it.
- **D28 Realm for `localhost:8888`** — demo-seed adds `http://localhost:8888/oidc/callback`, `/signed-out` and the origin `http://localhost:8888` to `admin-ui` in `realm.json` (ADR-0094 §7). *Why:* the UI works through the demo proxy locally too.
- **D29 Short demo periods, no quotas, flusher off** — every demo limit has a period of 1 s, demo plans have no quotas, and the demo control plane runs without `SPILLWAY_CP_REDIS_ADDRS` (flusher off) (ADR-0094 §3). *Why:* limiter state from before a reset expires within seconds, so `demo-reset` never touches Redis.
- **D30 Demo-share secrets** — the agent and tests never read `.env.demo`; `make demo-share-test` uses generated values in a 0700 temporary directory, removed on exit (ADR-0097 §4). *Why:* CLAUDE.md and the run's secret rules.
- **D31 D7's one-click block** — the existing Block key on Throttling: `POST /access-lists` deny on the key (key-admin or admin, audited, an identity change, ADR-0062), or `make demo-d7 BLOCK=1` with the same call; verify-m8 times it per replica with 50 ms probes from just before the call to the first 403, ten rounds, every sample ≤ 5 s (CP-20; ADR-0096 §2, ADR-0098 §D7). *Why:* the action already exists; the check is ADR-0042's revocation method applied to a block.

### Carried, not M8

No CRL for stream certificates; the `consumer_id` filter on `listAccessLists`; stretch items; and the open M7 review items (kind Redis 64Mi, P35–P41, OB-06 exemplars vs tail sampling, NFR-05 gate design, x64 baselines on more CPU models), which the orchestrator lists to the user separately. `make verify-m3`'s accepted laptop pass rate stays as recorded.

## Consequences

- Later M8 tasks implement without design questions; anything not settled here is a small choice recorded in the task's PR description.
- The demo never shares a database, a gateway configuration or a toxiproxy proxy with dev, verify or bench runs.
- Thirteen design-text changes (P42–P54) wait for the user; until approved, tasks build to these ADRs and leave design.md's text as it is.
- The SC-07 box stays open after M8's run; the user records the video.

## Alternatives considered

- **Re-ordering demo-guards after playground** — the guard is cheaper to build once, before the surfaces it guards; each later task then adds its own 404 tests.
- **Splitting traffic-sim into simulator and scenarios** — the scenarios are the simulator's whole point; one task keeps the shapes and their checks together.
- **An `operator` role for SC-03** — a sixth role changes CP-12, the realm, the RBAC tables and the UI's role model for two buttons; admin already holds every other dangerous action.
