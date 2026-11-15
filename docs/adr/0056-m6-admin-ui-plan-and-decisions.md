# 0056. M6 admin UI: plan and decisions

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** UI-01 – UI-06, UI-08, UI-09, NFR-08, CP-02, CP-10, CP-12, SC-09

## Context

M6 builds the admin UI over the M5 control plane. The user approved the task order — design-adrs, shell, stream, live-screens, policies, routes, consumers-keys, replicas-history, verify — and set eleven defaults, D1–D11. This run settles the design in five ADRs so later tasks implement without guessing; the user sees them at a checkpoint before any code. This ADR indexes them, maps every M6 checklist box and requirement to a task, describes what the UI shows between tasks, lists the design-text changes the ADRs propose, and lists every decision taken for the user. UI-07 (the Playground) is M8's.

## Decision

### The M6 ADRs

| ADR | Settles |
| --- | --- |
| [0057](0057-admin-ui-app-toolchain-embedding-and-serving.md) | every dependency and version, the layout under `web/`, code routes, TypeScript and ESLint, make targets and CI jobs, the typed client (errors, ETags), the `go:embed` package and its placeholder, the Dockerfile's Node stage, serving at `/` with cache and security headers, the exact CSP, the runtime config |
| [0058](0058-admin-ui-session-and-roles.md) | the Keycloak client (`admin-ui` kept) and realm changes, code flow with PKCE, where the verifier lives, in-memory tokens, refresh with rotation, reload, tabs, logout, 401 and 403, `GET /me`, the generated role table, hide vs disable |
| [0059](0059-live-data-the-sse-stream-and-named-queries.md) | `GET /stream`, its wire format and limits, the named-query catalogue and its validation, one evaluation per key per 5 s, backpressure, deadlines, the client's reconnects, replicas and heavy hitters on the stream, its metrics |
| [0060](0060-admin-ui-screens-states-and-api-additions.md) | the shadow report, promote and publish (412, 409), the screens per task, loading/empty/error states and the error mapping, pagination, the API additions per task |
| [0061](0061-admin-ui-tests-and-make-verify-m6.md) | Vitest, MSW and coverage; Playwright, the leak guard, the flows and the CI job; `make verify-m6`'s setup, pass rules, stall tolerance and run time |

### Task plan check

Every M6 box and requirement has a task. A box that spans tasks is ticked by the task that completes it; no box is ticked by this PR.

| M6 checklist box (abridged) | IDs | Built by | Ticked by |
| --- | --- | --- | --- |
| App shell embedded at `/`, Vite proxy in dev; OIDC login with in-memory tokens, role-aware navigation; typed client from `api/openapi.yaml` | UI-04, UI-05, UI-08, UI-09 | shell | shell |
| Overview and Throttling screens with SSE live data from named server-side queries | UI-03 | stream (endpoint, hub, catalogue, client), live-screens (screens, Prometheus scrape) | live-screens |
| Policy editor: validation, diff, publish with `If-Match` on the previewed revision, shadow report, promote to enforce | UI-01, UI-02 | policies | policies |
| Routes & upstreams, Consumers & keys (quota usage waits for M7), Replicas, History with rollback | UI-06 | routes, consumers-keys, replicas-history (UI-06's shared states from shell) | replicas-history |
| Vitest and Playwright tests for the main flows, run in CI from M6 | — | shell (Vitest, the `e2e` CI job, login and roles), then each task its flow (ADR-0061's table) | replicas-history, whose rollback completes the last main flow (shadow → promote → rollback) |
| Exit: `make verify-m6` | NFR-08, the exit | verify | verify (its output in the PR) |

| Requirement | Tasks |
| --- | --- |
| UI-01 shadow report | policies |
| UI-02 diff preview, `If-Match`, change note | policies (publish page); replicas-history reuses it for rollback |
| UI-03 live charts over SSE | stream, live-screens; routes and replicas-history use the same stream |
| UI-04 role-aware UI | shell (table, navigation, `ActionButton`), then every screen task |
| UI-05 typed client, TanStack Query | shell |
| UI-06 states and pagination | shell (shared components), then every screen task |
| UI-08 session model | shell |
| UI-09 embedded build, Vite proxy | shell |
| UI-07 | M8, not M6 |

### What each task delivers, and what the UI shows meanwhile

| Task | Builds | UI once merged |
| --- | --- | --- |
| design-adrs | ADR-0056 – ADR-0061 | none (the `web/` skeleton) |
| shell | ADR-0057 entire (dependencies, layout, make targets, CI changes, embed package and placeholder, Dockerfile stage, serving and headers, `/api/ui-config`), ADR-0058 entire (realm changes, session, `getMe`, role table, hide/disable), the shared components, Vitest setup, the `e2e` CI job with `login.spec.ts` and `roles.spec.ts` | `http://localhost:8080/` logs in through Keycloak; the navigation shows each role's screens; every screen is a placeholder "Arrives with m6-<task>" that calls no API; Not found, Not allowed and Signed out work |
| stream | `openStream`, the hub, the catalogue's Postgres and Prometheus queries, `SPILLWAY_CP_PROMETHEUS_URL` in compose, the stream client and `useSubscription`, the stream metrics | unchanged screens; the stream is tested by Go and Vitest |
| live-screens | Overview and Throttling; Prometheus scrapes all five gateways with a `replica` label and keeps data on a volume; `live.spec.ts` | Overview and Throttling live; Throttling's actions link to placeholders until consumers-keys |
| policies | Policies (list, editor, delete), the publish page, `getShadowReport` and its screen, promote; `publish.spec.ts`, the first half of `policy.spec.ts` | Policies and Publish work end to end; History is still a placeholder, so rollback waits |
| routes | Routes & upstreams with live endpoint and circuit state, drain and undrain; `gw_upstream_endpoint_ejected`; `drain.spec.ts` | as before plus Routes & upstreams |
| consumers-keys | Consumers (keys, overrides, access lists), Access lists, Tenants (admin); `keys.spec.ts` | as before plus Consumers & keys; Throttling's override and block actions work |
| replicas-history | Replicas (live), History (versions, diffs, rollback), Audit (admin); `policy.spec.ts` completed | every M6 screen |
| verify | `test/verify/m6.sh`, `make verify-m6`, `chaos m6 setup` and `chaos m6 probe`, `m6-exit.spec.ts`, docs/setup.md's M6 paragraph | the M6 exit |

### Proposed changes to the task plan

None is required. One optional change for the user:

- **(T3) Split shell.** As planned, shell is the largest task (toolchain, embedding, serving, session, roles, shared components, the CI job). It could be split into `shell` (toolchain, embed, serving, headers, client, navigation with a fixed role, components) and `session` (realm, OIDC, `getMe`, role table, `login.spec.ts`, `roles.spec.ts`), the second ticking box 1. Recommended only if the shell PR grows past review size; the order is otherwise unchanged.

### Proposed design-text changes (for the user's approval)

docs/design.md is not edited in this PR. Each item names the task that would apply it once approved; if the user declines one, the ADR's decision still binds the code where it can and the design text stays as is.

- **(P13) Stack row "UI"** (shell): "React, TypeScript, Vite, TanStack Query and Router, uPlot, pnpm" in place of "Recharts or ECharts". If declined, live-screens uses Recharts 3.x under the same CSP (ADR-0057).
- **(P14) SC-09** (M8 wording, applied by shell since it changes the realm): "… and adds `<url>/oidc/callback` to the `admin-ui` client's redirect URIs, `<url>/signed-out` to its post-logout redirect URIs and `<url>` to its web origins through the Keycloak admin API …" in place of "adds `<url>/*` to the `admin-ui` client's redirect URIs and web origins" (ADR-0058's exact URIs).
- **(P15) Admin API table, `GET /api/v1/metrics/query`**: drop the row; named queries are served only on the stream (ADR-0059). If the user keeps it, the stream task builds it as a thin REST view of the same hub.
- **(P16) Admin API table**: a new row "`GET /api/v1/me` — the caller's subject, display name and Spillway roles (UI-04)" (shell).
- **(P17) Metrics table**: a new row "`gw_upstream_endpoint_ejected` · gauge · upstream, endpoint" (routes; ADR-0059).
- **(P18) Testing table, row "UI"**: "Screens and hooks against a mocked network (MSW) with Vitest and Testing Library; main flows against the compose stack with a real Keycloak login (Playwright)" in place of "Screens and flows against a mocked API", which D10 changed (shell).

### Questions for the user at the checkpoint

- P13 – P18 above, and T3.
- **D3's client name**: the ADRs keep `admin-ui` instead of creating `spillway-ui` (ADR-0058 gives the reasons). Confirm, or ask for the rename (then P14 and docs/setup.md, ADR-0004 and ADR-0040's references change with it).

### Decisions taken for the user (review)

**D1–D11, as adopted**

- **D1 Stack** — adopted: React 19.3.1, TypeScript 6.0.3 strict, Vite 8.3.5, TanStack Router 1.170.46 with **code-based routes** (no route generator for about twenty routes) and TanStack Query 5.105.1, pnpm; no component kit; **one plain stylesheet**; chart library **uPlot 1.6.32** (small, no dependencies, canvas, CSSOM styles), which differs from the design's Stack row (P13) (ADR-0057).
- **D2 Typed client** — adopted: openapi-typescript 7.13.0 and openapi-fetch 0.17.0; `web/src/api/schema.gen.ts` committed; `make web-gen`; `make lint-web` regenerates and diffs in CI. Problem found and handled: openapi-typescript's peer range is `typescript ^5.x`; generation was measured to work under 6.0.3, and `pnpm-workspace.yaml` allows that pairing explicitly (ADR-0057).
- **D3 Session** — adopted with one change: the existing public client **`admin-ui`** is kept instead of a new `spillway-ui`, because SC-09, ADR-0004, ADR-0040 and docs/setup.md name it and it already has PKCE S256 and no implicit or direct grants. Code flow with PKCE, tokens in memory only, silent refresh with **refresh-token rotation**, reload = SSO redirect (ADR-0058).
- **D4 Roles** — adopted: `GET /api/v1/me`; the capability table generated from `x-spillway-roles`; disabled with a reason; hidden: Audit and the Tenants management screen (tenant names stay visible: `listTenants` is open to every role) (ADR-0058).
- **D5 SSE auth** — adopted: fetch with a streaming body and `Authorization`; reconnect on every refresh; the server also ends a stream at its token's expiry (ADR-0059).
- **D6 Named queries** — adopted: ten catalogue names; route and tenant validated against published routes and existing tenants plus a strict regexp and PromQL escaping; range from `15m`, `1h`, `6h`, `24h`, `7d`; one evaluation per distinct key per 5 s with a 5 s cache across unsubscribe (ADR-0059).
- **D7 Embedding** — adopted: `internal/controlplane/webui` with a `webui` build tag and a committed placeholder; hashed assets immutable for a year, `index.html` no-cache, SPA fallback, 404 for missing assets and unknown `/api` paths; the CSP is `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self' <issuer-origin>; form-action 'self' <issuer-origin>; frame-src 'none'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; worker-src 'none'; manifest-src 'self'` — `form-action` names Keycloak because logout is a POST (ADR-0057).
- **D8 Shadow windows** — adopted: 15 m, 1 h, 24 h, 7 d; promote is a `PUT` with `mode: enforce` followed by the normal publish. 7 d is served honestly: the per-key breakdown from Postgres (7 days kept), the counts from Prometheus (15 d retention on a new volume), and the report says when either covers less (ADR-0060).
- **D9 Tables** — adopted: cursor pagination, 50 rows; no endpoint lacked it (heavy hitters is a ranked top list by design) (ADR-0060).
- **D10 Tests** — adopted: Vitest 5.0.5 with **jsdom** 30.1.3, Testing Library, MSW 3.0.3 only at the fetch boundary; Playwright 1.64.1, Chromium only, against the compose stack and a real Keycloak login: D10's five flows plus a roles spec and a live-screens spec. In CI the gateways are static, so the flows check UI and control plane; gateway behaviour is verify-m6's (ADR-0061).
- **D11 verify-m6** — adopted: NFR-08 over **23 UI publishes** (20 route edits plus the three exit publishes), each sample measured as the sum of two single-clock intervals so clock skew cannot enter; the gateways' behaviour checked by an independent probe through all five gateways at each stage; zero console errors and zero leaked tokens are PASS lines (ADR-0061).

**New decisions the requirement text leaves open**, each with its reason:

- Dependency pins: the newest release on or before 2026-11-01 (two weeks before), as ADR-0035's pins — avoids day-old releases; the set installs with `pnpm audit` clean (ADR-0057).
- oauth4webapi 3.8.8 for OIDC — no dependencies, stores nothing, does the checks a hand-written client would get wrong (ADR-0058).
- Exact redirect URIs `<origin>/oidc/callback`, post-logout `<origin>/signed-out`, for 8080 and 5173 only — exact matching per the OAuth security BCP (ADR-0058).
- Realm lifetimes 5 min access, 30 min SSO idle, 10 h SSO max, rotation with no reuse — Keycloak 26.8.1 measured to keep each tab's refresh chain separate (ADR-0058).
- The PKCE verifier, state, nonce and return path in a sessionStorage pending record for at most 10 minutes, deleted at the callback — the only way to survive the redirect without a popup; it holds no token (ADR-0058).
- Code and state removed from the URL with `history.replaceState` before the token exchange; the return path accepted only as a same-origin relative path (ADR-0058).
- Refresh at max(`expires_in` − 60 s, half), timed on `performance.now()`; one refresh and one retry on a 401; never a retry on a 403 (ADR-0058).
- Logout by POST to the end-session endpoint with `id_token_hint`, so the ID token is in no URL; sign-out broadcast to other tabs on a `BroadcastChannel` (ADR-0058).
- `GET /me` answers `subject`, `name`, `roles` only (ADR-0058).
- The role table generated by `web/scripts/gen-roles.mjs` from `x-spillway-roles` and checked for drift (ADR-0058).
- Key secrets kept out of the TanStack Query caches by the mutations themselves (ADR-0058).
- `internal/controlplane/webui` with a `webui` build tag and a committed placeholder; Vite builds straight into its gitignored `dist` (ADR-0057).
- Docker stage `node:26.13.0-trixie-slim` with pnpm 12.16.1 from npm (Node 26 has no corepack); a `build-cp` stage so only the control plane waits for Node (ADR-0057).
- `GET /api/ui-config` (issuer, client ID) outside `/api/v1`, unauthenticated, no build-time values; the CSP's issuer origin from the same setting (ADR-0057).
- No HSTS from the plain-HTTP listener; `nosniff` and `Cache-Control: no-store` on every API answer (ADR-0057).
- `make test` = `test-go` + `test-web`; the CI `go` job runs `test-go`, the `web` job `test-web` (ADR-0057).
- One stream per tab with the screen's subscriptions as repeated `sub` parameters, served by its own handler behind the operation's guard (ADR-0059).
- Stream limits 8 subscriptions, 10 streams per subject, 200 in all, answered 503 `unavailable` — a new error code would break oasdiff (measured) (ADR-0059).
- Backpressure by coalescing to the latest result per subscription and disconnecting after a 10 s write deadline (ADR-0059).
- The stream clears the server's 30 s deadlines, ends at its token's expiry, keeps alive every 15 s, and ends cleanly on shutdown; the client treats 45 s of silence as dead and backs off 1–30 s with jitter; a tab hidden for 60 s closes its stream (ADR-0059).
- Evaluations time out at 4 s, at most 8 at once; Prometheus errors are events, never a dropped stream (ADR-0059).
- `SPILLWAY_CP_PROMETHEUS_URL`; Prometheus scrapes all five gateways with a `replica` label and keeps data on a named volume (ADR-0059).
- Endpoint health, ejection and circuit state from Prometheus, with a new `gw_upstream_endpoint_ejected` gauge, not from heartbeats (ADR-0059, ADR-0060).
- Replica staleness computed on Postgres's clock, not the browser's (ADR-0059).
- `GET /metrics/query` not built unless the user keeps it (P15) (ADR-0059).
- The shadow report as REST over the latest published policy, from Prometheus (counts, share, also enforce counts after a promotion) and the heavy-hitter windows (top keys), with coverage fields and a partial answer when Prometheus is down (ADR-0060).
- The publish page always sends the previewed revision; 412 re-validates and keeps the note; 409 `nothing_to_publish` refreshes; rollback warns about discarded edits (ADR-0060).
- One shared state component set; queries retried only for network errors and 502–504, at most twice; mutations never (ADR-0060).
- M6 adds no mutation, so no new audit record; every addition is a read with RBAC (ADR-0060).
- Coverage thresholds of 80 % lines and branches for `src/auth`, `src/stream`, `src/api` only (ADR-0061).
- Playwright: one worker, no retries, `expect` 10 s, action 15 s, navigation 30 s, traces and screenshots only on failure into gitignored directories (ADR-0061).
- The leak guard on every Playwright test, and console errors judged on the control plane's pages, with only declared expected statuses excused; Keycloak's pages reported, not judged (ADR-0061).
- The CI `e2e` job: its own `make dev`, cached Chromium, about 10–12 minutes, in `ci-ok` (ADR-0061).
- verify-m6's probe: a local header-keyed GCRA policy (50/s, burst 50; 10/s per gateway), heavy 200 rps and light 10 rps over five gateways, 20 s windows starting 1 s after the last apply, 25 s holds; enforce needs ≥ 50 % heavy 429s (about 74 % expected) (ADR-0061).
- verify-m6 runs about 10 minutes and is not in CI (ADR-0061).

## Consequences

- Later M6 tasks implement these ADRs; a task that finds one wrong writes a new ADR that supersedes the part, as usual.
- P13 – P18 change docs/design.md only after the user approves them; T3 and the client-name question are for the same checkpoint.
- Carried items stay out of M6 unless a task needs them: the NFR-01 re-baseline, the per-client-IP connection cap (M7), the accuracy test's boundary 503s (M7), `overrides.policy_id` nullable (M7), quota usage (M7), the CP-07 restore runbook and Helm parts (M7), no CRL for stream certificates, the Playground and SC-03's wording (M8), and `make verify-m3`'s accepted laptop pass rate.

## Alternatives considered

- **One large M6 ADR** — over a thousand lines; the session, the stream and the exit check are reviewed by different questions.
- **Editing the design text now** — requirement text changes only with the user's approval.
- **Reordering tasks (screens before the stream, or verify earlier)** — the live screens need the stream, and the exit needs History's rollback; the given order works with placeholders in between.
