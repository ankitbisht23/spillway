# 0100. Demo guards: implementation details

- **Status:** Accepted
- **Date:** 2026-11-27
- **Requirement IDs:** SC-08, UI-07, CP-12, CP-13, CP-15, CP-16, CP-20

## Context

ADR-0095 settles demo mode: `SPILLWAY_CP_DEMO_MODE`, one 404 guard for `/api/v1/demo`, `getDemo`, the 30-minute key rule, the expired-key janitor and the UI's demo probe. Building them (task demo-guards, ADR-0093) left some details open, and a few of ADR-0095's literal values clash with what the code already does. Accepted ADRs are not edited, so the choices are recorded here.

## Decision

1. **The 404 is the unknown path's, byte for byte.** With demo mode off, a request under `/api/v1/demo` gets the handler that answers every unknown path: 404, `{"error": "no such path", "code": "not_found", "request_id": …}`, the same headers. ADR-0095 §2 quotes the message as `"not found"`; the point of the guard is that "off" cannot be told apart from an unknown path, which only the identical answer gives. A unit test compares the two answers (status, headers, body without the request ID) for every demo target, method and token, and checks the authenticator was never called.
2. **`getDemo` grows with the tasks that own its parts.** This task's `Demo` schema has `tenants` (every tenant, at most 500 by ID, with its plan and the plan's first limit for a `from_plan` policy, burst filled in from the rate when unset), `routes` (`orders`, `catalog` and `payments` when the latest published config has them, each with the first consumer- or route-level policy that matches it and that policy's first limit, null for a `from_plan` policy) and `scenarios` (`d1`, `d2`, `d3`). `faults` comes with fault-switches (it needs the toxiproxy client), and `playground` and `client_ip` come with playground (the caps) and demo-share (`SPILLWAY_CP_TRUSTED_PROXIES`). Each is a new response property, which oasdiff accepts. `scenarios` is a plain string array, not an enum, so a later value does not break oasdiff (ADR-0059).
3. **Demo-only settings arrive with their users.** `SPILLWAY_CP_DEMO_GATEWAYS` and `SPILLWAY_CP_DEMO_TOXIPROXY_URL` (ADR-0095 §1) are read and checked by the tasks that use them (playground and fault-switches), not here, so no setting is checked before anything reads it.
4. **The 30-minute rule is `demo.Keys.Expiry`** in the new package `internal/controlplane/demo`, where the Playground generator will live (ADR-0096 §3). `IssueKey` passes the requested expiry and answers its error as 422 `validation_failed` on `expires_at`; `RotateKey` passes none and hands the answer to `store.RotateKey` as `latest`: the new key's `expires_at`, and a third term of the old key's `LEAST(…)` in `ExpireAPIKey` (NULL when demo mode is off, which `LEAST` skips). The clock is the control plane's (`time.Now`, replaceable in tests); the rotation's grace stays on Postgres's clock as before.
5. **The janitor's audit records follow the log's conventions.** Each deleted key gets one record with action `key.delete` and entity `key` (ADR-0095 §4 wrote `delete` and `api_key`; the audit log's actions are `<entity>.<verb>`, and key records already use entity `key`, so the UI's filter finds them together), actor sub `system:demo-janitor` with name `demo-janitor` and no roles, request ID `demo-janitor-<uuid>` (one per sweep), the key's REST form as `before`, and, as a consumer delete does for its cascade, `removed: {"access_list_ids": […]}` for the access-list entries that went with it, instead of a record per entry. `github.com/google/uuid` v1.6.0, already in the module graph, becomes a direct dependency.
6. **The janitor's query reads the whole of `api_keys`** (no index on `expires_at`, and ADR-0095 wants no schema change). `TestControlPlaneQueriesUseIndexes` lists it among the intended whole-table reads: demo mode only, every 10 minutes, on the demo database, whose keys it keeps few. It runs inside the identity transaction like every writer of `api_keys`; with nothing to delete the transaction rolls back, so no identity version and no record.
7. **The janitor runs beside the quota flusher** in the active replica's `Active` function and stops with it; with demo mode off `RunDemoJanitor` returns at once. It logs counts only.
8. **The UI's probe and the Playground route.** `useDemo` (TanStack Query, key `demo`, `staleTime: Infinity`, the default retry, which never retries a 404) runs in the app shell, so once per session. Navigation entries may carry `demo: true`; the Playground entry, after History, shows only while the probe succeeded. `/playground` renders Not found until the probe answers 200 and, until the playground task adds traffic, lists the demo's tenants and routes with their limits from `getDemo`. Vitest's MSW server answers `GET /api/v1/demo` with 404 by default, as the compose stack does.
9. **The Playwright leak guard excuses exactly the demo probe's 404.** Chromium logs every failed load as a console error, and with demo mode off (the compose stack Playwright runs against) every page load's probe is a 404 by design, so the guard (ADR-0061) would fail every flow. It excuses the console line `Failed to load resource: … status of 404` for `http://localhost:8080/api/v1/demo` only; any other failed load, and any other status at that URL, is still judged. `e2e/demo.spec.ts` checks that each page load sends one probe and gets 404, with no Playground entry and Not found at `/playground`. In a browser's developer console on a stack without demo mode, that one line per page load is the visible cost of ADR-0095's probe.
10. **The SC-08 checklist box stays unticked** in this task: ADR-0093 assigns it to playground, the last task that adds a guarded surface.

## Consequences

- fault-switches adds `faults` to `Demo` and its two settings; playground adds `playground`, the generator's use of `demo.Keys` (with a unit test of its path) and ticks the SC-08 box; demo-share adds `client_ip`.
- A later operation under `/api/v1/demo` is behind the guard by its path, and `TestEveryDemoPathIsGuarded` fails if it is not tagged `demo` or not under `/demo`.
- The integration role matrix runs against a control plane in each mode; with demo mode off it expects 404 for every demo operation and caller, which also checks `make dev`'s control plane (TestComposeRBAC).

## Alternatives considered

- **ADR-0095's literal `"not found"` message** — distinguishable from an unknown path's `"no such path"`, which the guard exists to avoid.
- **The whole `getDemo` answer now, with constant fault states and caps** — answers that would claim what nothing yet measures.
- **One audit record per cascaded access-list entry** — the consumer delete already records cascades as `removed`; two styles in one log for the same kind of change.
- **An index on `api_keys.expires_at`** — a migration (and a schema version step for every gateway) for a demo-only sweep over a small table.
- **Telling the UI about demo mode in its boot config instead of a probe** — no 404 in the console, but an anonymous caller would then learn whether demo mode is on, and ADR-0095 chose the probe.
