# 0095. Demo mode guards, demo keys and fault switches

- **Status:** Accepted
- **Date:** 2026-11-27
- **Requirement IDs:** SC-03, SC-08, UI-07, CP-12, CP-13, CP-15, CP-17, CP-20, RL-22 – RL-24, GW-13, GW-14, SEC-02, SEC-08

## Context

SC-08: demo mode is off by default; the Playground, demo endpoints and fault switches return 404 unless `DEMO_MODE=true`; demo keys expire after 30 minutes with their own limits. SC-03: fault switches for D5 and D6 as `make` targets and Playground buttons, toxiproxy only, "Operator role only", `DEMO_MODE=true` only, no container with the Docker socket.

What exists: settings are `SPILLWAY_CP_*` environment variables read at start; every `/api/v1` operation is in `api/openapi.yaml` with `x-spillway-roles` and is authenticated and role-checked before the request is read (ADR-0043); the `Error.code` enum is closed (adding a value breaks oasdiff, ADR-0059) and has `not_found`, `validation_failed` and `unavailable`; `IssueKey` already takes an optional `expires_at` in the future; the gateway refuses a managed key at or after its expiry on its own clock, and the control plane leaves expired keys out of snapshots; the audit log's `entity_id` is text and `request_id` is required. There is no `operator` role (ADR-0040); ADR-0043's P12 ("Admin role only") was never decided.

## Decision

### 1. `DEMO_MODE` (D7)

- **Name and default.** `DEMO_MODE` is a compose variable, `false` in `.env.example`. Only `deploy/compose/demo.compose.yml` maps it, to the control plane's **`SPILLWAY_CP_DEMO_MODE: ${DEMO_MODE:-false}`**. `make demo` runs compose with `DEMO_MODE=true`. `make dev` uses `compose.yaml` alone, which never sets the control-plane variable, so demo mode is off there even if `.env.local` sets `DEMO_MODE=true`.
- **Parsing.** `strconv.ParseBool`; unset is false; any other value stops the control plane at start with `SPILLWAY_CP_DEMO_MODE: want true or false`.
- **Who reads it.** The control plane only. Gateways never read it, so the request path is the same in and out of demo mode; Helm's chart has no value for it and kind never sets it. `demo-traffic` does not read it: it calls the control plane and stops with `demo mode is off (GET /api/v1/demo answered 404)` when that is the answer.
- **Settings used only when it is true**, each checked at start (missing or invalid → the process stops with a message naming the setting): `SPILLWAY_CP_DEMO_GATEWAYS` (comma-separated `http`/`https` base URLs; the override sets `http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000`) and `SPILLWAY_CP_DEMO_TOXIPROXY_URL` (the override sets `http://toxiproxy:8474`). `SPILLWAY_CP_TRUSTED_PROXIES` is ADR-0097's and is read in both modes (empty by default: trust nobody). When demo mode is false these demo settings are ignored.

### 2. What answers 404 when demo mode is off (D8)

- **One guard for one prefix.** A handler in front of ADR-0043's guard answers every request whose path is `/api/v1/demo` or starts with `/api/v1/demo/` with **404** and the standard `Error` body (`{"error": "not found", "code": "not_found", "request_id": "…"}`) when demo mode is off, **before authentication**, for every method. Off means the operations are absent, so an anonymous caller learns nothing it could not learn from any unknown path. When demo mode is on, requests go through the usual guard (401, 403, then the handler).
- **The operations under it** (all new, OpenAPI-first, in the spec whatever the mode; oasdiff stays additive: new paths, new schemas, no new enum value in an existing response):

  | Operation | Method, path | Roles (`x-spillway-roles`) | Audited | Added by |
  | --- | --- | --- | --- | --- |
  | `getDemo` | `GET /api/v1/demo` | all five | no (read) | demo-guards |
  | `setDemoFault` | `PUT /api/v1/demo/faults/{name}` | admin | yes | fault-switches |
  | `startPlaygroundRun` | `POST /api/v1/demo/playground/runs` | all five (P46) | yes | playground |
  | `stopPlaygroundRun` | `DELETE /api/v1/demo/playground/runs/{id}` | all five, own runs only | yes | playground |
  | `openPlaygroundStream` | `GET /api/v1/demo/playground/runs/{id}/stream` | all five, own runs only | no (read) | playground |

- **Also off when demo mode is off:** the 30-minute key rule (§3), the janitor (§4), the Playground generator and the toxiproxy client (no goroutine, no connection), and the UI's Playground. `/api/v1/stream` gets no demo subscription at all (ADR-0096 §3), so it needs no guard.
- **`getDemo`** answers `{"tenants": [{"name", "plan", "limit": {"rate", "burst", "period"}}], "routes": [{"name": "orders" | "catalog" | "payments", "policy": "consumer-plan" | "catalog-limit" | "payments-limit", "limit": {"rate", "burst", "period"} or null}], "scenarios": ["d1", "d2", "d3"], "faults": [{"name": "redis-cut", "active": bool}, {"name": "upstream-slow", "active": bool}], "playground": {"max_rate": 200, "max_duration_s": 60, "active_runs": <the caller's>, "runs_left_this_hour": <the smaller of the caller's and its IP's>}, "client_ip": "<the address the caps attribute the caller to>"}`. `routes[].limit` is null for `orders`, whose limit is each tenant's plan. The limits come from the latest published config; fault states from toxiproxy (§5), cached for 1 s. `client_ip` is shown to that caller only, never logged (ADR-0097 §2).
- **The UI.** At session start the UI calls `getDemo` once (TanStack Query, no retry on 404). On 404 the navigation has no Playground entry and `/playground` renders Not found; on 200 the Playground entry appears after History. Any other error hides the entry and shows nothing else (the Playground is optional). The admin fault buttons render only when `useCan("setDemoFault")`.
- **Tests per item.** Go: for each of the five operations and for `/api/v1/demo`, `/api/v1/demo/`, `/api/v1/demo/anything`, with demo mode off, every method, with no token, a viewer's and an admin's token → 404 `not_found` and no handler or store call; with demo mode on → the normal 401/403/2xx. A spec test asserts every path under `/api/v1/demo` is behind the guard (so a later operation cannot miss it). Vitest: navigation and route with `getDemo` 404 (hidden, Not found) and 200 (shown), fault buttons only for admin. Playwright (compose, demo off): no Playground entry, `/playground` shows Not found. verify-m8 repeats the 404s against the running stack (ADR-0098 §G).

### 3. Demo keys (D9, D10)

- **The rule.** When demo mode is on, every key the control plane issues or rotates **expires within 30 minutes** of its creation, on the control plane's clock:
  - `IssueKey` without `expires_at` → `expires_at` = now + 30 min;
  - `IssueKey` with `expires_at` later than now + 30 min → 422 `validation_failed`, field `expires_at`, message `expires_at: at most 30 minutes ahead in demo mode`; earlier is kept (verify-m8 uses a 15-second key, ADR-0098 k3);
  - `RotateKey` → the new key gets now + 30 min; the old key's end of grace becomes the earlier of its own and that same instant.
  When demo mode is off nothing changes. The rule lives in one control-plane function that the `IssueKey` and `RotateKey` handlers and the Playground generator (which issues through the store, below) all call, so every issuer meets it: the Playground generator, `demo-traffic`, and a key-admin in the UI. A unit test covers the generator's path as well as the handlers'.
- **Enforcement point.** The gateway, which already refuses a managed key at or after `expires_at` on every request with 401 (internal/config/identity.go). The control plane also leaves expired keys out of the next snapshot it builds. No schema change: demo keys are ordinary `api_keys` rows (prefix and hash only).
- **Who issues.**
  - The **Playground generator** (ADR-0096 §3) issues one key per demo tenant, for `<tenant>-playground`, through the store with the actor `system:playground` (`Name: "Playground"`, no roles), the first time a run needs it and again when it has under 5 minutes left. The plaintext stays in the generator's memory; it is never in a response, an SSE event, a log line or an audit record (the audit records the key's ID and prefix, as for every issue).
  - **`demo-traffic`** issues keys for its consumers through `POST /api/v1/consumers/{id}/keys` (no `expires_at`) with its `spillway-ci` token, at start and again when a key has under 10 minutes left, and keeps them in memory only; its output names a key by ID only.
  - A **key-admin** may issue keys in the UI as usual; in demo mode they expire within 30 minutes like the others.
- **Never shown in full.** Playground keys are never shown at all (UI-07). Simulator keys are never printed. The GIFs show neither (ADR-0099).
- **Their own limits.** Each Playground key belongs to its tenant's `<tenant>-playground` consumer, so it has its own `consumer-plan`, `catalog-limit` and `payments-limit` counters (they are keyed `{consumer}`) under its tenant's plan, and shares only the tenant's `tenant-ceiling` (1,500/s). Visitors therefore never consume a scenario consumer's budget, and the Playground still shows each plan's limit. On top, the generator's caps (ADR-0096 §3) bound how much traffic any key can carry: at most 200 rps per custom run.

### 4. Cleanup of expired keys (D11)

- **The janitor.** Demo mode only, active control-plane replica only (CP-22), every 10 minutes (first sweep 1 minute after start): deletes up to 1,000 `api_keys` rows whose `expires_at` is more than 1 hour in the past and that no other key's `rotated_from` names, in one transaction that is one identity change (the identity version goes up once, CP-20) with one audit record per deleted key: actor `system:demo-janitor`, action `delete`, entity `api_key`, entity id the key's ID, diff the key's REST form (ID, prefix, consumer, expiry; never a secret), request ID `demo-janitor-<uuid>`. Access-list entries naming a deleted key go with it (`ON DELETE CASCADE`) and are audited the same way. Nothing to delete → no transaction, no version.
- **Why one hour.** A key just past its expiry still shows as expired in the UI for a while (useful in D7's story); after an hour nobody needs it. `make demo-reset` removes everything anyway.
- **Tests.** Store integration test on a scratch database: expired-over-an-hour keys deleted, younger and unexpired ones kept, a `rotated_from` target kept, one version bump, one audit record per key; not running when demo mode is off.

### 5. Fault switches (D12, D13)

- **Faults.** Exactly two, named in an OpenAPI enum `DemoFaultName`:

  | Name | Scenario | On | Off |
  | --- | --- | --- | --- |
  | `redis-cut` | D5 | on each of `demo-redis-0` … `5`: toxics `demo-cut-up` (`type: timeout`, `stream: upstream`, `toxicity: 1`, `attributes: {timeout: 0}`) and `demo-cut-down` (the same, `stream: downstream`) | delete both toxics on all six |
  | `upstream-slow` | D6 | on each of `demo-upstream-1` … `3`: toxic `demo-slow` (`type: latency`, `stream: downstream`, `toxicity: 1`, `attributes: {latency: 4000, jitter: 0}`) | delete it on all three |

  A `timeout` toxic with `timeout: 0` stops all data in that direction and keeps the connection open until the toxic is removed: for the gateway it is a black hole, the same as the chaos table's paused nodes, so every Redis call ends at its time budget (RL-22) and the per-node breakers open (ADR-0021, ADR-0025), while the cluster itself keeps running and needs no repair. A 4,000 ms delay is above the `orders` route's 2.5 s per-try timeout (ADR-0094 §3), so every attempt through the slowed upstream ends at its deadline, a breaker failure (ADR-0034's table).
- **One operation.** `PUT /api/v1/demo/faults/{name}` (`setDemoFault`) with body `{"active": <bool>, "note": "<optional, 1–500 characters>"}` answers 200 with `{"name", "active"}`. The control plane applies the change through toxiproxy's API at `SPILLWAY_CP_DEMO_TOXIPROXY_URL` (each toxiproxy call with a 2 s timeout); setting the state it already has is allowed and changes nothing in toxiproxy. If a proxy is missing or toxiproxy fails, it answers 503 `unavailable` with what failed (proxy name, not data) and leaves the toxics it already changed as they are, so a second PUT completes the change; the state in `getDemo` is always read back from toxiproxy, the one source of truth.
- **RBAC: admin only** (`x-spillway-roles: [admin]`). There is no operator role (ADR-0040); P42 proposes "Admin role only" in place of "Operator role only" and supersedes ADR-0043's P12. Admin already holds every other action that can take traffic down.
- **Audit.** Every successful PUT writes one record: actor, action `update`, entity `demo_fault`, entity id the fault's name, diff `{"before": {"active": …}, "after": {"active": …}}`, the note, the request ID. Toxiproxy is not transactional, so the record is written after toxiproxy confirmed the change.
- **Make targets** (the same operation, so the same RBAC, audit and 404): `make demo-fault-redis ON=1|0`, `make demo-fault-upstream ON=1|0` and `make demo-fault-clear` (both off) run `docker compose … run --rm --no-deps demo-traffic fault <redis-cut|upstream-slow|clear> [on|off]`, which gets a `spillway-ci` token (admin) and calls `setDemoFault`, printing `redis-cut: on` and so on. With demo mode off they fail with the 404 message of §1.
- **No Docker socket anywhere.** The control plane and `demo-traffic` only speak HTTP to toxiproxy and the control plane. `make demo-down` and `make demo-reset` switch both faults off; `make demo-down` also deletes the proxies (ADR-0094 §5), which removes any toxic left.
- **Tests.** Go: a fake toxiproxy (httptest) for on, off, idempotence, a missing proxy (503) and read-back; RBAC table rows (admin 200, the four other roles 403); the audit record; 404 when off. Integration (compose): `redis-cut` on with a demo-mapped gateway → its Redis-policy requests take their failure modes within the budget, off → Redis decisions again within 5 s; `upstream-slow` on → attempts end at the per-try timeout.

## Consequences

- Demo mode is a property of one process and one URL prefix, which makes "off" easy to prove and keeps gateways identical in and out of the demo.
- In demo mode, keys issued in the UI also expire within 30 minutes: correct for a public demo, and visible in the issue dialog's answer.
- The demo database's `api_keys` stays bounded by the janitor; its audit log keeps a record of every deleted key.
- An admin's fault switch is one audited change whatever the client; a viewer can watch faults in `getDemo` but never switch them.

## Alternatives considered

- **Gateways read `DEMO_MODE` too** (to enforce demo-key expiry themselves) — the expiry check already exists for every managed key; a second switch on the hot path adds nothing.
- **Per-operation demo checks after authentication** — easier to forget on a new operation, and the answer would differ by role (403 vs 404) while "off" should mean absent.
- **A `/api/v1/stream` subscription for Playground results** — its `sub` enum is shared with response schemas, so a new value breaks oasdiff (ADR-0059), and ownership of a run does not fit the shared, cached subscriptions.
- **A `demo` flag column on consumers or keys** — a migration for something the demo database and the 30-minute rule already express.
- **Revoking expired keys instead of deleting them** — rows would still pile up; revocation is meant for live keys.
- **Disabling the toxiproxy proxies for the cut** (connection refused) — calls would fail in microseconds and never exercise the time budget, which is what the chaos table's full outage checks.
- **Make targets calling toxiproxy directly from the host** — a second path without RBAC, audit or the demo-mode check, against SC-03's "Operator role only, `DEMO_MODE=true` only".
