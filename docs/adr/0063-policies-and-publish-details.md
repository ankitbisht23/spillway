# 0063. Policies and publish: details

- **Status:** Accepted
- **Date:** 2026-11-16
- **Requirement IDs:** UI-01, UI-02, UI-04, UI-06, CP-02, CP-03, CP-14, RL-21

## Context

The policies task builds the Policies screens, the shadow report and the publish page with ADR-0060's design. Building them left questions ADR-0056 – ADR-0062 do not settle:

- ADR-0060 names the report's numbers but not their types, the shape of its Prometheus error, or what `metrics_since` is when Prometheus answers but does not report its oldest sample. `increase()` extrapolates, so its counts are not whole numbers.
- `keys_since` is "the later of the window's start and the oldest heavy-hitter window kept for the policy"; heavy-hitter windows exist only for minutes in which a key was throttled.
- The server's policy errors (422) mostly carry no `path`: `config.CheckPolicy` returns one message, which starts with the field it is about (`level …`, `match.routes: …`, `limits[0].burst: …`).
- ADR-0061's `publish.spec.ts` edits a route header in the Routes screen and checks History; both screens arrive later (routes, replicas-history). `policy.spec.ts`'s rollback is History's.
- The publish page polls "applied on n of m live replicas"; in `make dev` the gateways are static and no replica reports.
- A new policy's mode: the server's default is enforce (RL-26), the M6 exit and the design's D4 story create policies in shadow first.

## Decision

- **Report schema** (`ShadowReport`, additive): the six counts (`evaluated`, `would_deny`, `share`, `enforce_evaluated`, `denied`, `enforce_share`) are nullable doubles, Prometheus's `increase()` estimates; the UI rounds counts to whole numbers and shows shares to one decimal. `metrics_error` is a nullable enum `prometheus_not_configured | prometheus_unavailable | query_failed` (as the stream's error codes, ADR-0059); a refused connection, a 5xx or the 4 s deadline is `prometheus_unavailable`. `metrics_since` is null without Prometheus or when `min(prometheus_tsdb_lowest_timestamp_seconds)` has no value. The report also carries `policy_name`, `mode` and `config_version` from the latest published config, so the screen can say which version runs the policy in which mode. One Prometheus query, `sum by (mode, decision) (increase(gw_ratelimit_decisions_total{policy="<name>"}[<window>s]))`, gives both modes; the name goes through the catalogue's `promString`.
- **Key data**: `keys` are the top 50 by `shadow_denied` then `denied` then digest, with each key's latest consumer and API key IDs (as `listHeavyHitters`); `keys_shadow_denied_total` sums every key in the windows; `keys_since` is the later of the window's start and the policy's oldest window kept (any age up to the 7-day prune), null when the policy has none. Three sqlc queries on `heavy_hitters_policy_id_window_start`, covered by the EXPLAIN test. The window's start is the control plane's clock; windows starting at or after it count, as in `listHeavyHitters`.
- **A policy not in the latest published config** answers 404 "policy N is not in the latest published config: publish it first"; the screen shows that sentence and a link to the policy instead of Not found.
- **Validation in the browser** mirrors `compilePolicy` and `compileLimits` where they need only the policy: name, level, algorithm, mode, routes (`*` alone, no duplicates), key template placeholders, failure mode only for Redis policies, local-only algorithms, limits (rate, whole-second periods 1 s – 8760 h, duplicate periods, burst only for GCRA and required over 1 min, one rate-only limit for concurrency), and `from_plan`'s per-consumer rule. Route existence, route costs and conflicts stay the server's.
- **Server errors in place**: a 422 detail goes to the field its `path` names, else to the field its message starts with (`name`, `level`, `algorithm`, `mode`, `failure_mode`, `match.routes`, `match.methods`, `key_template`, `limits…` and `route "…" costs…` to Limits, `from_plan`); anything else is listed under the form. No change to the server's messages.
- **New policies start in shadow mode** in the editor; the user can pick enforce or off.
- **Promote** sends the policy as loaded (its writable fields) with `mode: enforce` and its ETag, from the policy page or the report; it is disabled with "Save or discard your edits first" while the form has unsaved edits, so promote never drops them. After it, the page says "Promoted to enforce in the working set. Unpublished changes: review and publish" with a link to `/publish`. A 412 shows "This policy changed since you opened it" with a Reload button that loads the current version (ADR-0060's entity-edit rule), never a page reload, which would mean signing in again (D3).
- **Publish page**: the preview is a query that never refetches by itself (`staleTime` infinite, no refetch on reconnect, dropped when the page closes), so the diff on screen always is the revision Publish sends; Refresh preview, a 412, a 409 and a success refetch it. Publish is disabled with its reason while the preview refreshes, while it has errors ("Fix the errors above first") and when there is nothing to publish (empty diff against a published version), besides the role. The note is checked on submit (non-blank, at most 2,000 code points, as the server counts); a missing note sends nothing. "Applied on n of m live replicas" counts the live replicas of `listReplicas`; with none it says static gateways do not report, and polls nothing.
- **Shared components**: `DiffView` (one element per line, classed `diff-add`/`diff-del`/`diff-hunk`/`diff-file`/`diff-ctx`, text only), `ChangesList` and `ValidationErrors` (entities linked to their screens), `ChangeNote`, `ErrorLine` and the `Unpublished` banner, for routes and History's rollback dialog to reuse.
- **Who is this?** on the report is shown per key to admins only; other roles see one line saying it needs the admin role, rather than 50 disabled buttons.
- **Playwright**: `publish.spec.ts` edits the seeded policy's rate in the policy editor (the Routes screen is m6-routes) and the 412 comes from an API edit of the route's `priority`; the published version and note are checked through `listConfigVersions` until History exists. `policy.spec.ts` runs create (editor) → publish (publisher) → report → promote and publish (admin), checking each role's disabled action on the way, and rolls back through the API (`rollbackConfig`, spillway-ci); m6-replicas-history replaces that step with History's dialog and finishes the spec. `ciApi` is exported from `seed.ts` for these API steps, and `signOut` moves from `login.spec.ts` to `fixtures.ts`. Timeouts are ADR-0061's; the flows add none.

## Consequences

- The report is honest about its sources: null counts with a reason, and coverage dates, rather than zeros.
- Routes and History reuse the diff, changes, note and banner components.
- `policy.spec.ts` is half UI, half API until replicas-history.

## Alternatives considered

- **Integer counts in the report** — rounding in the API hides that `increase()` estimates; the UI rounds for display.
- **`keys_since` from the oldest window inside the window** — would claim missing coverage whenever a policy simply throttled nothing early in the window.
- **Path on every server policy error** — a change to `config.CheckPolicy`'s error type shared with the gateway's compile; the message prefix carries the field already.
- **A promote that overwrites unsaved edits, or merges them** — either surprises; asking to save or discard first is plain.
- **Building a temporary History rollback for `policy.spec.ts`** — that is replicas-history's screen.
