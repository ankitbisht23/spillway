# 0060. Admin UI: screens, states and API additions

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** UI-01, UI-02, UI-04, UI-06, CP-02, CP-04, CP-13, CP-14, RL-21, RL-26, SEC-08

## Context

The design's screen table lists what each screen shows and does. UI-01 asks for a shadow report: would-be-rejected count and share for a shadow policy over a chosen window, broken down by key from the heavy-hitter windows (RL-21). UI-02 asks for a diff preview before publish, `If-Match` with the previewed working-set revision, and a required change note stored in the audit log (CP-02). UI-06 asks for loading, empty and error states on every screen and server-side pagination of large tables. The user set D8 (shadow windows 15 min, 1 h, 24 h, 7 d; promote to enforce is a working-set edit followed by the normal diff, change note and `If-Match` publish, no shortcut) and D9 (server-side pagination with the M5 cursor convention, 50 rows per page).

What M5 provides (ADR-0036, ADR-0037, ADR-0051, ADR-0053): every list except heavy hitters is cursor-paginated (`limit` default 50, `next_cursor`), checked against today's spec: upstreams, routes, policies, plans, tenants, consumers, a consumer's keys, overrides, access lists, config versions, replicas and the audit log; heavy hitters are a ranked top list (at most 500) by design. Validate returns errors, a changes list, a unified YAML diff and the revision; versions have a diff endpoint and an `applied` list; drain, key issue/rotate/revoke, overrides and access lists exist. The policy label on `gw_ratelimit_decisions_total` is the policy's name; the `heavy_hitters` windows keep `denied` and `shadow_denied` per (window, policy ID, keyed digest) for 7 days, only for the up to 100 keys each replica reports per heartbeat. Compose's Prometheus has no volume until ADR-0059's live-screens change.

## Decision

### The shadow report (UI-01, D8)

**`GET /api/v1/policies/{id}/shadow-report?window=15m|1h|24h|7d`** (`getShadowReport`; default `1h`; all five roles; a read, not audited). REST, not a stream subscription: the report is reviewed, not watched, and a window of up to 7 days does not change in 5 s.

- **Which policy.** `{id}` must be a policy in the **latest published config version**, which is what gateways run; otherwise 404 ("policy N is not in the latest published config: publish it first"). The report uses that version's name for the policy (the metric label) and its ID (the heavy-hitter windows). The metric's label is the name, so a policy renamed within the window has its earlier name's counts left out; the key breakdown, keyed by ID, is unaffected. The screen's help text says so; detecting renames would mean reading every snapshot in the window, for a rare case.
- **The numbers**, all over `[now − window, now]`:
  - from Prometheus (`SPILLWAY_CP_PROMETHEUS_URL`, ADR-0059), with `P` = `policy="<name>"`: `evaluated` = `sum(increase(gw_ratelimit_decisions_total{P,mode="shadow"}[w]))` (every decision the policy made in shadow mode: allow, deny, `fail_open`, `fail_closed`), `would_deny` = the same with `decision="deny"`, `share` = `would_deny / evaluated` (null when `evaluated` is 0); and the same three for `mode="enforce"` as `enforce_evaluated`, `denied` and `enforce_share`, so the report also shows what happened after a promotion.
  - from Postgres: `keys` = the policy's heavy-hitter windows in the range, summed per digest: `key_digest`, `consumer_id`, `api_key_id`, `shadow_denied`, `denied`, ordered by `shadow_denied` then `denied`, at most 50 (ADR-0051's query with `policy_id`, the `(policy_id, window_start)` index), and `keys_shadow_denied_total`.
- **Coverage, honestly.** `since` and `until` are the window. `metrics_since` = the later of the window's start and Prometheus's oldest sample (`min(prometheus_tsdb_lowest_timestamp_seconds)`, from Prometheus's own scrape job); `keys_since` = the later of the window's start and the oldest heavy-hitter window kept for the policy. The screen prints "Prometheus has data from …" / "Key data from …" whenever either is later than the window's start. In compose this is the common case for 7 d (a laptop's stack rarely runs a week; Prometheus keeps 15 d once it has a volume, ADR-0059). Heavy-hitter windows live in Postgres for 7 days (CP-22) and survive restarts, so the per-key breakdown covers 7 d whenever the gateways were reporting.
- **What the per-key breakdown is.** Heavy hitters are each replica's top 100 throttled keys per heartbeat (RL-21), so `keys` covers the heaviest keys, not all of them: `keys_shadow_denied_total` can be below `would_deny`, and the screen says "top keys; N would-denies not attributed" when it is. Each key shows its digest, its consumer and key IDs (links to Consumers & keys), and, for admins only, a "Who is this?" action that opens the audited lookup (ADR-0053).
- **Prometheus down or not configured**: 200 with the Prometheus fields null and `metrics_error` set; the key breakdown still answers. Postgres down: 503 as usual.
- Answer schema `ShadowReport`, additive for oasdiff.

### Promote to enforce and publish (UI-01, UI-02, CP-02, D8)

- **Promote** on a shadow policy's page or report is an edit, nothing more: `PUT /policies/{id}` with the policy's fields and `mode: enforce`, `If-Match` with the policy's ETag (`updatePolicy`: editor, admin). It changes the working set only. The screen then offers "Review and publish", which goes to the publish page; there is no promote-and-publish button.
- **The publish page** (`/publish`, every role can open it; `validateConfig` is all roles):
  1. On open, and on "Refresh preview", `POST /config/validate`: the `working_set_revision`, `base_version`, `errors`, `changes` and the unified `diff` (ADR-0037). Errors are listed with links to the entity at fault; the changes list summarises; the diff is shown in a hand-written diff view (added and removed lines by CSS class, no HTML from the server: the diff is text).
  2. A required change note (1–2,000 characters, counted as the user types).
  3. **Publish** (`publishConfig`: publisher, admin; disabled with its reason otherwise; also disabled while `valid` is false, with "Fix the errors above first") sends `POST /config/publish` with `If-Match: "<working_set_revision>"` **from the preview on screen**, never from a newer read, and the note.
  4. On success: the new version, a link to it in History, and "applied on n of m live replicas", polled from `GET /config/versions/{v}` every second for up to 30 s.
- **412 on publish** (the revision moved since the preview): "The working set changed since your preview: someone edited, imported or rolled back. Review the new diff." (A publish alone does not move the revision, ADR-0037; it makes the next publish of the same preview a 409.) The page validates again, shows the new diff and changes, keeps the typed note, and requires another click. It never re-sends by itself.
- **409 `conflict`** on publish, whose only 409 is nothing to publish (ADR-0037; the server's message starts "nothing to publish"): "Nothing to publish: the working set is already version N" (someone published the same edits); the preview refreshes.
- **422**: the validation errors, as in step 1.
- **Rollback** (History, `rollbackConfig`: publisher, admin): a dialog shows `GET /config/versions/{v}/diff?against=<latest>`, warns when the working set has unpublished changes ("rolling back discards N unpublished changes", from a validate call whose revision becomes the `If-Match`), requires a note, and handles 412 and 422 as above (a 422 names the identity rows that block it, ADR-0037).
- **Entity edits** (`PUT`, `DELETE`, drain) send the entity's ETag as `If-Match`. A 412 shows "This <entity> changed since you opened it" with a Reload button that loads the current version into the form; the user re-applies the change. Nothing is overwritten silently.

### Screens, by task

| Screen | Data | Actions (operation; disabled with a reason when not allowed) | Task |
| --- | --- | --- | --- |
| Overview | stream: `overview.requests`, `.ratelimited`, `.latency`, `.routes`, `.tenants` | filters route, tenant, range (search params) | live-screens |
| Throttling | stream: `throttling.policies`, `throttling.heavy_hitters` | open policy; add override (`createOverride`); block key (`createAccessList`, deny on the key); who is this (`lookupHeavyHitters`, admin) | live-screens |
| Policies | `listPolicies` (paginated), `getPolicy` | create, edit, delete (`createPolicy`, `updatePolicy`, `deletePolicy`); shadow report; promote (`updatePolicy`); validate | policies |
| Publish | `validateConfig` | publish (`publishConfig`) | policies |
| Routes & upstreams | `listRoutes`, `listUpstreams` (paginated), `getRoute`, `getUpstream`; stream: `routes.endpoints`, `routes.circuits` | edit routes and upstreams (`updateRoute`, `updateUpstream`, create, delete); drain (`drainEndpoint`); undrain (`updateUpstream` with `draining: false`) | routes |
| Consumers & keys | `listConsumers`, `listConsumerKeys`, `listOverrides`, `listAccessLists`, `listPlans`, `listTenants` (paginated) | consumers CRUD; issue, rotate, revoke keys; overrides and access lists CRUD (key-admin, admin); quota usage is M7 | consumers-keys |
| Tenants (admin) | `listTenants` | tenants CRUD (`createTenant`, …) | consumers-keys |
| Replicas | stream: `replicas` | none (view only) | replicas-history |
| History | `listConfigVersions` (paginated), `getConfigVersion`, `diffConfigVersion` | roll back (`rollbackConfig`) | replicas-history |
| Audit (admin) | `listAudit` (paginated; filters entity, entity ID, actor, since, until) | none | replicas-history |

Drain and route edits change the working set; after one, a banner says "Unpublished changes: review and publish" with a link to `/publish`. Every action that removes or revokes something confirms first (revoke a key, delete an entity, roll back), naming what it does.

### States on every screen (UI-06)

One shared set in `src/components`:

- **`QueryState`** wraps a query or a subscription: **loading** (a skeleton shown after 200 ms, so fast loads do not flicker), **empty** (a message and, when the role may create, the create action; otherwise the reason it cannot), **error** (the mapped message, the request ID, a Retry button for retryable errors), else the content. Live screens add **reconnecting** (last values greyed) and per-chart **error** from `error` events (ADR-0059).
- A route-level **error boundary** catches render errors and shows the error state with a Reload button (React also logs such errors to the console, which fails Playwright flows: a crash cannot pass a test).
- **Not allowed** (403, or a hidden screen's URL) and **Not found** (404, unknown route) are full screens with a link home.

**Error mapping** (`ApiError` from ADR-0057):

| Answer | Shown | Retry |
| --- | --- | --- |
| network error, 502, 503, 504 | "Can't reach the control plane" (503: "unavailable, try again shortly", honouring `Retry-After`) | queries: automatically twice (1 s, 2 s), then a Retry button; mutations: never automatically |
| 401 | handled by the session (one refresh, one retry, then "Your session ended", ADR-0058) | once, by `authFetch` |
| 403 | "Not allowed: this needs the <roles> role" (from the generated table) | never |
| 404 | "Not found: it may have been deleted" | never |
| 409 | the server's message; on publish, nothing to publish as above | never |
| 412 | "Changed since you opened it" with Reload; publish and rollback as above | never by itself |
| 422 | `details` mapped to form fields by `path`, plus a list | never |
| 400, 413, 415, 428 | "The request was rejected: <message>" with the request ID (a 428 is a UI bug) | never |
| 500 | "Something went wrong" with the request ID | button only |

TanStack Query defaults: `retry` only for network errors and 502–504, at most twice; `staleTime` 10 s for lists and entities; no refetch on window focus (a refetch mid-edit would surprise); mutations never retry. `/me` is fetched once per session.

### Pagination and filters (UI-06, D9)

- Every table on a paginated list uses the M5 cursor convention with **`limit=50`**: a `Pager` with Next (from `next_cursor`) and Previous (the browser's history: each page's `cursor` is a search param, so Back returns to the page before), First (no cursor), and no page numbers or totals (keyset pagination has none). Filters are the server's exact-match ones (ADR-0036: `name`, `tenant_id`, `consumer_id`, `policy_id`, `action`, `subject_type`, the audit filters); a name filter is an exact name, not a search. Changing a filter resets the cursor.
- **No list endpoint lacks pagination today** except heavy hitters, which is a ranked top list by design (ADR-0036: at most 500, no cursor); the UI asks for 50. Nothing is added.
- Small embedded lists stay whole: an upstream's endpoints, a version's `applied` replicas, validate's changes and errors.

### API additions in M6

All OpenAPI-first in `api/openapi.yaml`, additive for oasdiff (new paths, new schemas, no new value in an existing enum), each with `x-spillway-roles`, regenerated Go stubs (`make api`) and UI client (`make web-gen`). **M6 adds no mutation**: drain, key lifecycle, overrides, access lists, publish and rollback exist and are audited (ADR-0040, ADR-0043). Every addition is a read, so none writes an audit record; the RBAC table test covers each.

| Task | Addition | Roles |
| --- | --- | --- |
| shell | `GET /api/v1/me` (`getMe`, schema `Me`, ADR-0058); `GET /api/ui-config` outside the spec (ADR-0057) | all five; ui-config unauthenticated |
| stream | `GET /api/v1/stream` (`openStream`, ADR-0059) and its event schemas | all five |
| live-screens | the catalogue's Prometheus queries (no new path); Prometheus scrapes all five gateways with a `replica` label, and a data volume (ADR-0059) | — |
| policies | `GET /api/v1/policies/{id}/shadow-report` (`getShadowReport`, schema `ShadowReport`) | all five |
| routes | `gw_upstream_endpoint_ejected` gauge in the gateway; `routes.endpoints` and `routes.circuits` in the catalogue (ADR-0059) | — |
| consumers-keys | none | — |
| replicas-history | none (`replicas` subscription carries `staleness_s` on Postgres's clock, ADR-0059) | — |

Endpoint health, ejection and circuit state come from Prometheus, as the gateways already export them per replica, rather than from heartbeats: heartbeats would need a proto change and a table for state that Prometheus already holds per replica and per endpoint, and the screen wants it live, which the stream gives. If Prometheus is down, the Routes screen still shows the route and upstream config, with the live columns in an error state.

## Consequences

- The shadow report answers every window, and says plainly when Prometheus or the heavy-hitter windows cover less of it.
- There is exactly one way to change a policy's mode in production: edit, preview, note, publish with the previewed revision.
- No new mutation means no new audit or RBAC surface beyond reads; the M5 tests keep covering every write.
- Follow-up: the policies, routes, consumers-keys and replicas-history tasks build their rows of the screen table with all four states and role-aware actions.

## Alternatives considered

- **The shadow report as a stream subscription** — a 7-day `increase` every 5 s per viewer key for numbers a reviewer reads once.
- **Would-deny counts from heavy hitters only** — a lower bound (top 100 keys per heartbeat) and no denominator for the share.
- **A 7 d window served only when Prometheus has 7 d** — hides the heavy-hitter breakdown that Postgres does have; the coverage fields say what each part covers.
- **A promote button that also publishes** — D8 rules it out; it would skip the diff and the note.
- **Endpoint state in heartbeats** — a proto change and a new table to duplicate what Prometheus has.
- **Offset pagination with page numbers** — ADR-0036 chose keyset; totals would need a count scan.
- **Retrying mutations on 5xx** — a publish or a revoke could run twice; the user retries knowingly.
