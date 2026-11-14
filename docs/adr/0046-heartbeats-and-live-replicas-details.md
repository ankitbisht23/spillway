# 0046. Heartbeats and live replicas: details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** CP-08, CP-09, CP-18, CP-22, RL-24, RL-31, NFR-08, NFR-12

## Context

M5's heartbeat task (`m5-heartbeat`, the second half of ADR-0035's T1 split, ADR-0045) builds what ADR-0038 decides for heartbeats: `ConfigService.Heartbeat`, the `replicas` and `replica_versions` tables and their prune, `live_replicas` in the limiter with its fallback, `gw_config_sync_age_seconds`, `GET /api/v1/replicas` and rejected versions. Building it raised points ADR-0038 leaves open:

- The per-replica limits of local policies are divided when a config compiles (RL-31, ADR-0017); degraded limiters divide at reconcile time by the snapshot's replica count. How does a new `live_replicas` reach both, and what if the config does not compile with it (a route whose cost is above a limit's per-replica capacity)?
- When the on-disk snapshot's `live_replicas` is written, given that `synced_at` must stay an upper bound of the sync age.
- How a snapshot file that cannot be used is reported, which ADR-0038 asks for ("reports `rejected_reason`").
- gRPC's own redial backoff grows to 120 s. The Watch stream's reconnects (100 ms doubling to 5 s, ADR-0038) cannot connect faster than the channel redials, so after a long outage a gateway could reach a control plane that is back up to two minutes late, against ADR-0042's (c5) rule of 60 s. The short outage test measured 17 s for a 30 s outage.
- The prune's batches and the `EXPLAIN` test (ADR-0036): a volatile `clock_timestamp()` cannot be an index condition.

## Decision

### Gateway

- **Live count in the limits.** A managed gateway compiles every config with `ratelimit.replicas` replaced by the last known live count, on a copy (the published config, the on-disk snapshot and the stream are unchanged). So local limits, degraded limiters and the compile checks all use it, exactly as a static file with that `ratelimit.replicas` would. When a heartbeat answers a count that differs from the one the config in use was compiled with, the gateway compiles the same config again through the apply path (`config.Prepare`: pools and limiters reconciled, state kept, GW-29) and swaps it in; the config version does not change. The last known count is the latest answer's (0 counts as 1), else the on-disk snapshot's `live_replicas` when the gateway booted from that file, else none, and the config's own `ratelimit.replicas` applies (ADR-0038, ADR-0045).
- **A count the config does not compile with.** The config in use stays, with its limits for the previous count, and the error is logged; nothing is reported as rejected, because no version was refused. A new version that does not compile with the live count is refused and reported like any other compile error (ADR-0038).
- **Snapshot file.** The file carries the current live count. A changed count is written at once only when the heartbeat that brought it was a sync (sent with the Watch stream established), so the file's `synced_at` stays the time of a sync; otherwise the count is written with the next apply.
- **Heartbeat loop.** A ticker every 5 s from the start, plus one at once after each apply, config or identity, from the stream or the file, and after each refused config (so the refusal shows at once). One heartbeat is in flight at a time; requests for one coalesce. Each has a 4 s deadline; failures are logged at most once a minute.
- **Sync.** A heartbeat is a sync when the Watch stream was established (its headers received, ADR-0045) as it was sent, and the answer arrived. `gw_config_sync_age_seconds` is now minus the last sync; before one, the snapshot file's `synced_at` when the gateway booted from it, else the process start. Static mode does not export it.
- **Rejected versions.** `rejected_config_version` and `rejected_reason` hold the last refused config version and its compile error until a config applies. A snapshot file that cannot be used at boot (corrupt, another schema major) is reported as `rejected_reason` "on-disk snapshot: …" with `rejected_config_version` 0, until a config applies. Identity refusals are logged only (the proto has no field for them).
- **Redials.** The ConfigService connection's gRPC backoff is 100 ms doubling to 5 s with 20 % jitter (`grpc.WithConnectParams`), the Watch stream's own range; the minimum connect timeout stays gRPC's 20 s. The short outage test now recovers in 7–12 s after a 30 s outage (every sync age under 10 s and every replica live, against 17 s before).

### Control plane

- **Heartbeat.** The replica name is the request's `replica_id` until mtls-secrets (ADR-0045): not a DNS label is `INVALID_ARGUMENT`. A heartbeat of any schema version is recorded (the replicas list then shows a gateway that cannot watch). A failed transaction is `UNAVAILABLE`, logged at most once a minute. The answer is at least 1. `rejected_reason` is stored cut to 2,000 bytes at a rune boundary.
- **Tables** (migration `00004_replicas`): as ADR-0036, with the versions `NOT NULL` (0 = none, as in the proto), `rejected_*` NULL for none, and indexes `replicas (last_heartbeat_at)`, `replica_versions (stream, version)` and `replica_versions (first_reported_at)`. Every time is `clock_timestamp()`; `last_synced_at` moves only on a heartbeat with `watch_connected`; `replica_versions` rows are inserted with `ON CONFLICT DO NOTHING` for each version above 0, so a version's first report is set once. Cut-offs (`clock_timestamp() - interval '15 seconds'`, `'7 days'`) are scalar subqueries so the planner can use them as index conditions.
- **Prune.** On becoming active and then every hour, while active: batches of 10,000 rows (`ctid` / `id = ANY(ARRAY(SELECT … LIMIT 10000))`) until a batch is short, first `replica_versions` older than 7 days, then `replicas` without a heartbeat for 7 days. `heavy_hitters` joins it with the heavy-hitters task.
- **REST.** `GET /api/v1/replicas` (`listReplicas`, every role, ADR-0040) lists replicas by name, cursor-paginated by the last name (base64url), with `live` computed on Postgres's clock. `GET /config/versions/{v}` gains ADR-0036's `applied: [{replica, at}]` (an additive response field), which ADR-0042's NFR-08 check polls.

## Consequences

- `live_replicas` changes the per-replica limits within one heartbeat (≤ 5 s) of the change; a stopped gateway leaves the count 10–15 s after its last heartbeat (measured: 15.0 s in 4 runs, where the stop came right after a heartbeat).
- A config that publishes cleanly can be refused by gateways when enough replicas are live that a route's cost exceeds a per-replica capacity, the same as a static file with that `ratelimit.replicas`; the replicas list shows the refusal.
- NFR-08's measure (ADR-0042) is available from `GET /config/versions/{v}`; the integration test logs it (20 publishes × 5 host gateways: p50 3 ms, max 6–10 ms over 4 runs, from `published_at` to the last apply heartbeat).

## Alternatives considered

- **Divide by the live count at request time, outside the snapshot** — every limiter and the compile checks read the count from the snapshot; a second source would need its own reconcile and would skip the checks.
- **Report a count the config cannot compile with as a rejection of the running version** — the version is not refused; the gateway runs it, with the previous count's limits.
- **Write the count to the file on every change** — would set `synced_at` to a time that may not be a sync, so a later boot could understate the sync age.
- **Keep gRPC's default redial backoff** — up to 2 minutes from a control plane's return to the first Watch, beyond ADR-0042's 60 s recovery rule.
- **`DELETE … WHERE (key) IN (SELECT … LIMIT n)`** — Postgres plans it as a hash join over a whole index scan, which the `EXPLAIN` test refuses.
