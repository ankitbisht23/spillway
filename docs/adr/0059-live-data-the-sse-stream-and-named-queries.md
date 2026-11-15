# 0059. Live data: the SSE stream and named queries

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** UI-03, UI-08, CP-08, CP-22, RL-21, OB-01, SEC-08

## Context

UI-03: live charts refresh every 5 s over SSE from the control plane, which runs named queries against Prometheus server-side; no PromQL in the browser. The design's API table lists `GET /api/v1/stream` (live metrics, replica status, heavy hitters; Playground results in M8) and `GET /api/v1/metrics/query`. The user set D5 (fetch with a streaming body and `Authorization`, not `EventSource`, so tokens never go in a URL; the stream reconnects when the token is refreshed) and D6 (a fixed server-side catalogue; the browser sends a query name and parameters validated against known values: route, tenant, a window from a fixed list; each distinct (query, parameters) runs once per 5 s however many clients watch it).

Facts that shape the design:

- The REST listener has `ReadTimeout` and `WriteTimeout` of 30 s (cmd/controlplane/main.go). A stream outlives both, and in Go a read deadline that passes while a handler runs can cancel the request's context.
- Every `/api/v1` operation is guarded before the request is read (ADR-0043) and served by oapi-codegen's strict server, which answers with one response object and cannot flush per event.
- The gateway's metrics (design, Observability): `gw_requests_total{route, method, code_class, origin, reason, tenant}`, `gw_request_duration_seconds{route, tenant}`, `gw_ratelimit_decisions_total{policy, decision, mode}` (the policy's **name**), `gw_upstream_endpoint_healthy{upstream, endpoint}` ("1 while the endpoint takes traffic: healthy and not ejected"), `gw_circuit_state{upstream}` (0 closed, 1 open, 2 half-open). The `tenant` label is the tenant's name for the top 50, else `other`, or `none` (OB-01).
- Compose's Prometheus (v3.15.1, host 9091) scrapes only `gateway-1`, keeps its data in the container (no volume), and scrapes every 5 s.
- `prometheus/client_golang` v1.25.0, already in `go.mod`, has the HTTP API client (`api`, `api/prometheus/v1`); no new Go module is needed.
- Adding a value to the `Error` schema's `code` enum is an oasdiff **error** (`response-property-enum-value-added`, checked 2026-11-15 against today's spec: 273 errors), so new error answers must use today's codes.

## Decision

### The endpoint

- **`GET /api/v1/stream`** (`openStream`), OpenAPI-first, `x-spillway-roles` all five roles (every role reads everything but the audit log), not audited (a read). Query parameters: `sub` (required, repeated, 1–8 values, each a catalogue name, declared as an enum so the typed client knows them), and the parameters below. The `200` answer is `text/event-stream`; its payload schemas (`StreamResult`, `StreamError`, `StreamEnd`, and each query's data) are components, so the UI client is typed from them.
- **One stream per tab, carrying the current screen's subscriptions.** A screen opens one stream with all its subscriptions and the shared parameters; navigating to another screen or changing a filter closes it and opens another. Subscriptions are fixed for the life of a stream (SSE is one-way; changing them in place would need a second endpoint). Reconnecting costs one request, and the first data arrives at once from the shared cache.
- The URL holds only catalogue names and filter values (route and tenant names, a range), never a token: the token goes in `Authorization: Bearer` on a `fetch` (D5). No cookie, no `EventSource`.
- **Served by its own handler behind the same guard.** The operation is in the spec and in ADR-0043's operation table, so the guard authenticates the token and checks `x-spillway-roles` before anything else, exactly as for every operation; then a hand-written handler streams, because the strict server cannot flush per event. The guard passes the token's `exp` to the handler with the actor (the stream task makes the `Authenticator` return it; nothing else changes).
- **Validation before streaming.** Every `sub` must be a catalogue name, every parameter known and used by at least one requested query, and every value valid (below); anything else is **400** `invalid_request` naming the parameter (never echoing a value), and no stream starts.
- **Limits**: at most 8 subscriptions per stream; at most **10 open streams per subject** (`sub`) and **200 in all** per control plane. Past either: **503** `unavailable` with `Retry-After: 10` (today's codes only: a new `too_many_streams` code would break oasdiff, measured above). 10 per subject allows a person ten tabs; 200 is far above what one control plane's operators open.

### Wire format

`Content-Type: text/event-stream; charset=utf-8`, `Cache-Control: no-store`, `X-Accel-Buffering: no`, no compression. Events, each a `data:` line of JSON:

| Event | When | `data` |
| --- | --- | --- |
| `ready` | first, at once | `{"subscriptions": ["overview.requests", …], "interval_s": 5}` |
| `result` | each evaluation of a subscription | `{"sub": "<name>", "at": "<RFC 3339, the control plane's clock>", "data": {…}}` |
| `error` | a failed evaluation (the stream stays open) | `{"sub": "<name>", "at": "…", "code": "prometheus_unavailable" \| "prometheus_not_configured" \| "query_failed" \| "unavailable", "message": "…"}` |
| `end` | just before the server closes | `{"reason": "token_expired" \| "shutdown"}` |

- A comment line `: keepalive` every 15 s. No `id:` and no `retry:`: there is no resumption (a reconnect gets current data within a second from the cache), and the client, not `EventSource`, decides retries.
- `data` shapes: Prometheus range queries `{"kind": "matrix", "step_s": 15, "series": [{"labels": {…}, "points": [[<unix s>, <value or null>], …]}]}`; instant ones `{"kind": "vector", "series": [{"labels": {…}, "value": <number or null>}]}`; NaN and ±Inf become `null`. Postgres-backed queries: the REST objects (below).

### The named-query catalogue (D6)

**Parameters** (all validated before streaming):

| Parameter | Allowed values | Validation |
| --- | --- | --- |
| `route` | `*` (default) or a route name | `*`, or the name of a route in the **latest published config** (what gateways run); and in every case `^[A-Za-z0-9._-]{1,64}$` (ADR-0036's name rule) |
| `tenant` | `*` (default), `none`, `other`, or a tenant name | the name of an existing tenant (indexed lookup by unique name), or one of the three fixed values; and the same regexp |
| `range` | `15m` (default), `1h`, `6h`, `24h`, `7d` | the fixed list |
| `policy_id` | absent (default) or a policy ID | an integer ≥ 1 |

Values reach PromQL only inside double-quoted label matchers, through one escaping function (backslash, double quote and newline escaped as PromQL strings require), even though the regexp already excludes them. `*` means no matcher. Filter F below is `route="<route>"` and `tenant="<tenant>"` as given.

**Range → step and rate window** (fixed): `15m` → 15 s step, `[1m]`; `1h` → 1 m, `[2m]`; `6h` → 5 m, `[10m]`; `24h` → 15 m, `[30m]`; `7d` → 1 h, `[2h]` (at most 169 points per series).

| Name | Source | Parameters | Query | Screen |
| --- | --- | --- | --- | --- |
| `overview.requests` | Prometheus, range | route, tenant, range | `sum by (code_class) (rate(gw_requests_total{F}[W]))` | Overview: RPS and status mix |
| `overview.ratelimited` | Prometheus, range | route, tenant, range | `sum(rate(gw_requests_total{F,reason="ratelimited"}[W])) / sum(rate(gw_requests_total{F}[W]))` | Overview: 429 share (OB-01's 429 rate) |
| `overview.latency` | Prometheus, range | route, tenant, range | `histogram_quantile(q, sum by (le) (rate(gw_request_duration_seconds_bucket{F}[W])))` for q = 0.5 and 0.99, merged into series labelled `quantile` | Overview: p50, p99 |
| `overview.routes` | Prometheus, instant | tenant, range | per route over the whole range R: `sum by (route) (rate(gw_requests_total{tenant}[R]))`, the same with `reason="ratelimited"` over it, and `histogram_quantile(0.99, sum by (route, le) (rate(gw_request_duration_seconds_bucket{tenant}[R])))`, merged by route, top 10 by rate | Overview: per-route table |
| `overview.tenants` | Prometheus, instant | route, range | as `overview.routes`, by `tenant`, filtered by route | Overview: per-tenant table |
| `throttling.policies` | Prometheus, range | range | `sum by (policy, mode) (rate(gw_ratelimit_decisions_total{decision="deny"}[W]))` | Throttling: deny rate per policy (shadow would-denies as `mode="shadow"`) |
| `throttling.heavy_hitters` | Postgres | range, policy_id | `listHeavyHitters` with `since` = now − range, limit 50 (ADR-0051) | Throttling: top throttled keys |
| `replicas` | Postgres | none | `listReplicas` (all pages), plus `staleness_s` per replica = Postgres's `clock_timestamp()` − `last_synced_at`, both on Postgres's clock | Replicas |
| `routes.endpoints` | Prometheus, instant | none | `gw_upstream_endpoint_healthy` and `gw_upstream_endpoint_ejected`, by (`replica`, `upstream`, `endpoint`) | Routes & upstreams: endpoint state per replica |
| `routes.circuits` | Prometheus, instant | none | `gw_circuit_state` by (`replica`, `upstream`) | Routes & upstreams: circuit per replica |

`routes.*` need two small additions, both in the routes task: a gauge **`gw_upstream_endpoint_ejected{upstream, endpoint}`** (1 while outlier detection has the endpoint ejected, else 0; the same series lifetime and labels as `gw_upstream_endpoint_healthy`, so no new cardinality), which lets the screen tell "health check failing" (healthy 0, ejected 0) from "ejected" (ejected 1); and a `replica` target label in Prometheus (below). The metric is proposed for the design's metrics table in ADR-0056 (P17).

**Prometheus in compose** (live-screens task): scrape `gateway-1` … `gateway-5` (`gateway-2` … `5` are down unless `--profile replicas` runs; a down target costs nothing), with a relabel rule that sets `replica` to the target's host (`gateway-N`, which is also its replica name, ADR-0040); and a named volume `prometheus-data` at `/prometheus`, so history survives restarts until `docker compose down -v`. Retention stays Prometheus's default 15 d, which covers the 7 d range (ADR-0060 says what a shorter history means).

**Setting**: `SPILLWAY_CP_PROMETHEUS_URL` (an `http` or `https` URL, not a secret; compose sets `http://prometheus:9090`). Unset: Prometheus-backed subscriptions get `error` events `prometheus_not_configured` and the stream still serves the Postgres-backed ones; a URL that does not parse stops the process at start.

### Evaluation and fan-out

- **One evaluation per distinct key per 5 s.** A key is the query name plus the parameters that query uses, in a fixed order (so `overview.routes` ignores `route`, and two streams that differ only in `route` share it). Each key has one loop: it evaluates as soon as its first subscriber arrives, then every 5 s; every subscriber gets each result. A subscriber joining a running key gets its latest result at once. When the last subscriber leaves, the loop stops at once, and its last result and time are kept for 5 s: a subscriber returning within them gets that result and the next evaluation waits for the 5 s mark, so no key is ever evaluated twice within 5 s.
- Evaluations have a **4 s timeout** (below the 5 s cadence); at most **8 run at once** across keys; a key whose previous evaluation is still running skips the tick (counted, below). Prometheus queries use its HTTP API with the range and step above; a Prometheus answer over 1 MiB is an error (`query_failed`), which the top-10 limits make impossible in practice.
- **Prometheus down** (connection refused, timeout, 5xx): each affected subscription gets an `error` event with `prometheus_unavailable` at each tick, and recovers by itself; the stream is never dropped for it. Postgres down: `error` with `unavailable` for `replicas` and `throttling.heavy_hitters`.
- **Backpressure: coalesce to the latest, then disconnect.** Each stream has one writer goroutine and one slot per subscription. A new result replaces an unwritten one in its slot (counted as coalesced), so a slow client never makes the server queue more than one pending event per subscription. Each write gets a **10 s** write deadline (`http.ResponseController.SetWriteDeadline`); a client that does not take an event within it is disconnected (counted as slow). Memory per stream is bounded by 8 results.
- **Server deadlines.** At the start the handler clears the connection's read and write deadlines (`SetReadDeadline` and `SetWriteDeadline` with the zero time) so the server's 30 s `ReadTimeout` and `WriteTimeout` cannot end a stream or cancel its context; the per-write 10 s deadline replaces them. A test proves a stream outlives a server with a 1 s `WriteTimeout` and `ReadTimeout`.
- **The token's expiry ends the stream.** The handler closes the stream at the token's `exp` (sending `end` with `token_expired`), so a stream cannot outlive its token; the client has already reconnected with a refreshed token by then (refresh at `exp` − 60 s, ADR-0058).
- **Shutdown.** The stream hub is registered with `http.Server.RegisterOnShutdown`: on SIGTERM every stream gets `end` with `shutdown` and closes, so `Shutdown` is not held for its whole timeout.

### The client (D5)

- `src/stream/client.ts`: `fetch("/api/v1/stream?…", { headers: { Authorization: "Bearer …", Accept: "text/event-stream" }, signal, cache: "no-store" })`, the body read through `TextDecoderStream` and parsed by a small SSE line parser (`event:`, `data:`, comments, blank-line dispatch), about 60 lines with unit tests.
- `useSubscription(subs, params)` opens the screen's stream, keeps the latest `result` or `error` per subscription in component state (not in the TanStack Query cache), and closes the stream on unmount (`AbortController`; an abort is not an error).
- **Reconnect**: on a network error, an ended body or no bytes for **45 s** (three keepalives), after a backoff of 1, 2, 4, 8, 16, 30 s (capped) with ±20 % jitter, reset once a stream has stayed open for 30 s; **at once, with the new token, after every token refresh** (D5) and after `end` with `token_expired`; on `end` with `shutdown` after the backoff. HTTP answers: 401 makes one session refresh (ADR-0058) and reconnects; 403 and 400 stop with an error state (403: not allowed; 400: a bug, the request ID shown); 503 waits for `Retry-After`. A tab hidden for 60 s closes its stream and reopens it when visible again.
- Charts show the last values while reconnecting, greyed, with "Reconnecting…"; a subscription in `error` shows the message over its chart (ADR-0060's states).

### Replica status and heavy hitters

Both come from Postgres through the same hub at the same 5 s cadence: `replicas` from the `replicas` table (heartbeats every 5 s, ADR-0038) and `throttling.heavy_hitters` from the `heavy_hitters` windows (one-minute windows written by heartbeats, ADR-0051). Their `data` is the REST answer's `items` (the same schemas as `listReplicas` and `listHeavyHitters`) plus `staleness_s` for replicas, so the screens use one type for REST and stream. Heavy-hitter digests are the keyed ones (ADR-0053); "who is this?" stays the admin-only REST lookup.

### The design's `GET /api/v1/metrics/query`

Not built in M6 unless the user keeps it (ADR-0056, P15). Every named query the UI needs is live and goes through the stream; a second, one-shot path to the same catalogue would be a second door to Prometheus with no screen using it. If the user keeps the row, the stream task adds it as a thin REST view of the same hub (the key's cached result, or one shared evaluation), same parameters, same validation, same roles.

### Metrics of the stream (on the admin port)

`cp_stream_connections` (gauge), `cp_stream_keys` (gauge: keys being evaluated), `cp_stream_evaluations_total{query, outcome}` (counter; outcome `ok`, `error`, `skipped`; `query` is a catalogue name, at most 10 values), `cp_stream_evaluation_duration_seconds{source}` (histogram; `prometheus`, `postgres`), `cp_stream_coalesced_total` (counter), `cp_stream_closed_total{reason}` (counter; `client`, `slow`, `token_expired`, `shutdown`), `cp_stream_rejected_total{reason}` (counter; `invalid`, `limit`). No subject, route or tenant is a label.

### Tests (stream task)

Go, with a fake clock, a fake Prometheus (an `httptest` server) and the store stubbed where it helps: two streams on one key make one evaluation per tick; distinct keys evaluate separately; the loop stops with the last subscriber and resumes from the cache within 5 s without a second evaluation; coalescing keeps only the latest result; a blocked client is disconnected after the write deadline; the limits answer 503; every invalid `sub`, parameter and value answers 400 before any byte of stream; Prometheus down yields `error` events and the stream stays; the stream ends at the token's `exp`; shutdown sends `end`; the stream outlives 1 s server timeouts; viewer can open it, no token is 401; logs and errors never contain the token. Vitest: the SSE parser (split chunks, multi-line `data`, comments), the reconnect schedule, the reconnect on refresh, 401/403/503 handling, the dead-stream timer, abort on unmount.

## Consequences

- Prometheus load is bounded by the number of distinct keys, not viewers: at most one query set per key per 5 s, and at most 8 at once.
- Changing a filter reconnects the stream; the shared cache makes that cost one request and no extra query.
- A stalled or slow client costs at most one pending event per subscription and is dropped after 10 s.
- Compose's Prometheus keeps its data across restarts and scrapes all five gateways; the gateway grows one gauge.

## Alternatives considered

- **`EventSource`** — cannot send `Authorization`; the token would need a URL or a cookie (D5).
- **A stream per query** — up to six connections per screen, and HTTP/1.1's six-per-origin limit would starve API calls.
- **Subscriptions changed in place** (`POST /stream/{id}/subscriptions`) — a second endpoint and stream state on the server, to save a reconnect.
- **A global 5 s ticker for all keys** — a new key would wait up to 5 s for data, or, evaluated at once, could run twice within 5 s.
- **Dropping events for slow clients without disconnecting** — keeps a dead connection forever; coalescing plus a write deadline bounds both memory and time.
- **Per-client buffers of N events** — memory grows with N for data that is stale after 5 s anyway.
- **New error codes for the limits** — an oasdiff error on every operation (measured).
- **The browser polling REST every 5 s** — UI-03 asks for SSE, and per-viewer polling would defeat D6's one evaluation per key.
