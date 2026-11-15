# 0062. Live screens: details

- **Status:** Accepted
- **Date:** 2026-11-16
- **Requirement IDs:** UI-03, UI-04, UI-06, RL-21

## Context

The live-screens task builds Overview and Throttling on the stream (ADR-0059) with the screen table of ADR-0060. Building them left a few questions ADR-0056 – ADR-0061 do not settle or settle differently from what the code needs:

- ADR-0056 says Throttling's actions "link to placeholders until consumers-keys". Blocking a key is one `createAccessList` call (deny on the API key, ADR-0060) behind a confirmation; adding an override needs a limits form, which the Consumers screen will have.
- The route filter must name a route of the latest published config (ADR-0059's validation), which `listRoutes` (the working set) does not give; heavy-hitter windows carry a policy ID, the deny-rate series a policy name.
- ADR-0061's global setup works "through the REST API". The Throttling screen's actions act on a row of the top throttled keys, which only a managed gateway's heartbeat writes (ADR-0051); `make dev`'s gateways are static, so in CI and in a plain `make dev` no row exists.
- The leak guard reads every `/api/v1/` answer's body to collect API keys; a stream's body does not end while the page is open, so reading it never finishes.
- TanStack Router merges a route's validated search over the raw search: a key the validator leaves out keeps its raw value.

## Decision

- **Block key works now**: on Throttling, after a confirmation dialog with an optional note, `POST /access-lists` with `{action: deny, subject_type: key, api_key_id}`; the answer's entry ID is shown, and errors show the server's message. **Add override** navigates to `/consumers/<consumer_id>?override_policy=<policy_id>`; the consumers-keys task reads that search parameter to prefill its override form. **Who is this?** (admin) is a dialog with one field per placeholder of the policy's key template (from the latest published snapshot), sending `lookupHeavyHitters` in a POST body; the subject lives in component state only and is gone when the dialog closes. It is disabled with a reason when the policy is not in the latest published config or its template uses `{api_key}` (the lookup cannot fill it; the API key column identifies such keys). A key with no consumer or no API key disables Add override or Block key with that reason. Actions sit in a panel for the selected row, not on every row, so 50 rows do not repeat three reasons each.
- **Filters and names from the latest published config**: `listConfigVersions?limit=1` then `getConfigVersion` (both all roles); its routes are the route filter's options, its policies give names to policy IDs and IDs to the deny-rate series' names (Open policy). Tenant filter options: `*`, `none`, `other` and the first 500 tenant names (`listTenants?limit=500`); a value already in the URL stays selectable. A table row whose route or tenant is a valid filter value links to the filtered screen.
- **Search validators set every key**, `undefined` when absent or invalid, so a hand-edited URL never reaches the stream with a malformed value.
- **Charts**: one `LineChart` over uPlot 1.6.32 with an HTML legend of each line's latest value (readable as text and by tests); a fixed seven-colour palette with matching `.swatch-N` classes, no inline styles. The status mix is a table with `<meter>` elements. Latency in ms, shares in %, rates per second.
- **Playwright global setup** (ADR-0061's, added here) also inserts one heavy-hitter window (`denied` 42) for the run's policy `e2e-per-client`, consumer and key into compose's Postgres with `docker compose exec postgres psql`, under a digest starting `e2e-`; setup and teardown delete every such row, and every `e2e-` tenant with its consumers, their keys' access-list entries and keys. Only integers from API answers and generated digests reach the SQL.
- **The leak guard skips reading `text/event-stream` bodies** (the stream carries no token or key, ADR-0059); their URLs are still checked. The login helper matches the path with its query.
- Prometheus's retention is set explicitly (`--storage.tsdb.retention.time=15d`, with the image's two default flags), not left to the default.

## Consequences

- Throttling's block action is usable before the Consumers screen exists; consumers-keys builds the override form behind `override_policy`.
- `live.spec.ts` and `roles.spec.ts` act on a real row in the real Postgres through the real stream; the e2e job needs `docker compose exec`, which it has.
- Later screens with filters reuse `useLatestConfig` and the every-key validator pattern.

## Alternatives considered

- **Block key as a link to the placeholder (ADR-0056's sequencing)** — leaves the one cheap action untestable until a later task.
- **A heartbeat from a test client to seed heavy hitters** — the real path, but it registers a replica, which changes `live_replicas` and so per-replica limits on a laptop running managed gateways.
- **Mocking the stream in Playwright for the rows** — the flows are meant to run against the real stack.
- **Route filter options from `listRoutes`** — the working set; an unpublished route would be refused by the stream with a 400.
