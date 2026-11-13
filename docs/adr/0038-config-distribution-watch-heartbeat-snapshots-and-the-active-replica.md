# 0038. Config distribution: Watch, Heartbeat, snapshots and the active replica

- **Status:** Accepted
- **Date:** 2026-11-13
- **Requirement IDs:** CP-06 – CP-09, CP-18, CP-21, CP-22, RL-02, RL-21, RL-23, RL-24, RL-31, GW-17, GW-21, GW-24, GW-29, OB-05, SEC-10, NFR-08, NFR-12

## Context

The design names `ConfigService.Watch` and `ConfigService.Heartbeat`, their main fields, the 5 s heartbeat plus one on apply, `live_replicas` (< 15 s), the on-disk last good snapshot, schema versions with one minor of skew, one active control-plane replica, and `LISTEN/NOTIFY` on publish. The user settled D6 (an advisory lock picks the active replica; standbys take over), D9 (protobuf snapshot written by temp file, fsync and rename; served when the control plane is unreachable; age in `gw_config_sync_age_seconds`) and D10 (NFR-08 from apply heartbeats). Still open:

- the proto: files, messages, field numbers, and the config and identity fields that M5's entities add to `config.proto` (all additive, `buf breaking`);
- what the server sends when, how a gateway reconnects, and how a missed notification is caught;
- what a heartbeat carries and what the control plane records, so NFR-08 can be timed on one clock;
- what exactly `gw_config_sync_age_seconds` measures;
- how schema versions are compared and what "one minor behind" allows;
- where the snapshot lives in compose given read-only root filesystems, and how a gateway chooses between static files and the control plane;
- which advisory lock, what standbys do, how fast a standby takes over, and what happens when the active replica loses Postgres. CP-22 says Helm `replicas: 1` and HA is stretch; D6 adds standbys.

## Decision

### Proto (CP-06, CP-08, CP-18, CP-21)

`config.proto` gains fields for M5's entities, all additive:

```proto
message Config    { /* 1–5 */ repeated Plan plans = 6; }
message Upstream  { /* 1–8 */ int64 id = 9; }
message Endpoint  { string address = 1; int64 id = 2; bool draining = 3; }   // a draining endpoint gets no new requests
message Route     { /* 1–14 */ int64 id = 15; string auth_mode = 16; }      // none | api_key | jwt | either (ADR-0039)
message Policy    { /* 1–9 */ int64 id = 10; bool from_plan = 11; }         // ADR-0041
message Plan      { int64 id = 1; string name = 2; repeated PlanLimits default_limits = 3; }
message PlanLimits { string policy = 1; repeated Limit limits = 2; }        // policy by name, as in YAML

message Identity {
  /* 1–3 */
  string key_format = 4;                  // "" = static keys (ADR-0017); "spw1" = managed keys (ADR-0039)
  repeated Tenant tenants = 5;
  repeated AccessListEntry access_lists = 6;
  repeated Override overrides = 7;
}
message Tenant   { int64 id = 1; string name = 2; int64 plan_id = 3; string status = 4; }
message Consumer { /* 1–2 */ int64 id = 3; int64 tenant_id = 4; int64 plan_id = 5; string status = 6; string jwt_subject = 7; }
message ApiKey   { /* 1–3 */ int64 id = 4; int64 consumer_id = 5; google.protobuf.Timestamp expires_at = 6; }
message AccessListEntry { int64 id = 1; string action = 2; string subject_type = 3; int64 api_key_id = 4; int64 consumer_id = 5; string cidr = 6; }
message Override { int64 id = 1; int64 consumer_id = 2; int64 policy_id = 3; repeated Limit limits = 4;
                   google.protobuf.Timestamp starts_at = 5; google.protobuf.Timestamp ends_at = 6; }
```

IDs are 0 (unset) in static mode, where names keep their ADR-0017/0021 roles. In managed mode a policy's Redis key part and a tenant's hash tag are the decimal IDs (ADR-0036). Draining is honoured in static mode too.

A new file `api/proto/config_service.proto`, same package `spillway.config.v1` and Go package `configpb`:

```proto
service ConfigService {
  rpc Watch(WatchRequest) returns (stream WatchResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
}
message SchemaVersion { int32 major = 1; int32 minor = 2; }
message WatchRequest {
  string replica_id = 1;            // optional; must equal the client certificate's name (ADR-0040)
  int64 config_version = 2;         // 0 = none
  int64 identity_version = 3;       // 0 = none
  SchemaVersion schema_version = 4; // what the gateway was built with
}
message WatchResponse { oneof update { ConfigUpdate config = 1; IdentityUpdate identity = 2; } }
message ConfigUpdate   { SchemaVersion schema_version = 1; Config config = 2; google.protobuf.Timestamp published_at = 3; }
message IdentityUpdate { SchemaVersion schema_version = 1; Identity identity = 2; }
message HeartbeatRequest {
  string replica_id = 1;
  int64 config_version = 2;
  int64 identity_version = 3;
  SchemaVersion schema_version = 4;
  bool watch_connected = 5;
  int64 rejected_config_version = 6;   // 0 = none; the last version the gateway refused to apply
  string rejected_reason = 7;          // its compile error, no secrets (they never reach a snapshot)
  google.protobuf.Timestamp applied_at = 8;      // gateway clock, informational only
  google.protobuf.Timestamp interval_start = 9;  // start of the top_keys interval
  repeated TopKey top_keys = 10;                 // ADR-0041
}
message TopKey { int64 policy_id = 1; string key_digest = 2; int64 consumer_id = 3; int64 api_key_id = 4; int64 denied = 5; int64 shadow_denied = 6; }
message HeartbeatResponse { int32 live_replicas = 1; }
message SnapshotFile {               // the on-disk last good snapshot
  SchemaVersion schema_version = 1; Config config = 2; Identity identity = 3;
  google.protobuf.Timestamp synced_at = 4; int32 live_replicas = 5;
}
```

The design writes `stream Update` and `Heartbeat(Heartbeat{…})`; `buf lint`'s STANDARD rules require `WatchResponse` and `HeartbeatRequest`, so those are the names (ADR-0035 lists the wording). `buf.gen.yaml` adds `protoc-gen-go-grpc` (v1.6.2, already in `make tools`), and `make proto` / `make lint-proto` cover the new file. `buf breaking` against `origin/main` keeps every change additive.

### Schema versions (CP-18)

- The schema version is `{major, minor}`, a constant in `internal/config` (M5 ships **1.0**). It travels in every `ConfigUpdate`, `IdentityUpdate`, `config_versions` row and snapshot file.
- **Minor** bump: additive fields whose absence keeps the previous meaning and which a gateway one minor behind can ignore safely. **Major** bump: anything else (a changed meaning, a removal, a field that would be unsafe to ignore); it also gets a new proto package (`spillway.config.v2`), which `buf breaking` forces anyway.
- **The control plane** accepts a Watch whose major equals its own and whose minor is at least its own minus one (one minor behind during a rolling upgrade, CP-18); a newer minor is accepted too (gateways upgraded first read older snapshots, which lack the new fields). Anything else is refused with `FAILED_PRECONDITION` and logged. Before the first minor bump ships a field that an older gateway must not ignore, that task adds a check that refuses to publish a config using it while a live replica runs the older minor; M5 has no such field.
- **A gateway** never applies a snapshot of another major, from the stream or from disk: it keeps the last good one, logs the error and reports `rejected_reason` in its heartbeats.

### Watch (CP-06)

- **Server.** On a Watch, the active control plane (below) takes the replica name from the client certificate (ADR-0040), checks `replica_id` (empty or equal) and the schema version, and registers the stream. A second stream with the same replica name replaces the first, which is closed with `ABORTED` and logged. It then sends the current config if its version differs from the request's `config_version`, then the current identity likewise; afterwards, each new version as it appears. It sends whatever version is current when the two differ, lower ones included: Postgres is the source of truth (after a database restore, gateways follow it down).
- **Fan-out.** One broadcaster in the active replica holds the latest config and identity, each as marshalled bytes built once per version: config from `config_versions.snapshot`, identity from the `REPEATABLE READ` build of ADR-0037. It learns of new versions from a dedicated `LISTEN` connection on channels **`spillway_config`** and **`spillway_identity`** (payload: the version, decimal), and every 10 s it also reads `max(config_versions.version)` and `identity_state.version`, which catches a notification lost while the `LISTEN` connection was down. After that connection fails it reconnects with backoff (100 ms doubling to 5 s) and re-reads both versions. Each stream has a one-slot mailbox per section that keeps only the latest version, so a slow gateway is sent the newest snapshot and never blocks the others.
- **Connections.** gRPC over mTLS only (ADR-0040). Keepalive: client pings after 10 s idle with a 5 s timeout; the server permits pings every 5 s and does the same to its clients. Maximum message size 64 MiB both ways. Gateways reconnect with full-jitter backoff from 100 ms doubling to 5 s, reset once a stream has stayed up 30 s.
- **Gateway apply.** A config message is compiled and applied exactly like a file reload (`config.Compile`, registry reconciliation, atomic swap; GW-21, GW-29); identity as ADR-0037 says. A config that fails to compile or apply (including the gateway's own setting checks: Redis seeds, JWT settings) is not applied: the gateway keeps the last good config, logs the error, and reports `rejected_config_version` and `rejected_reason` until a later version applies.

### Heartbeat (CP-08, RL-24, NFR-08)

- **Gateway.** A unary `Heartbeat` every 5 s, and one immediately after each config or identity apply (after the swap; the disk write runs alongside). Each has a 4 s deadline. A failed heartbeat is logged (at most once a minute) and the next one goes at its time; `top_keys` keep accumulating until one succeeds (ADR-0041).
- **Control plane**, one transaction per heartbeat, active replica only:
  1. upsert `replicas` (`last_heartbeat_at = clock_timestamp()`; `last_synced_at = clock_timestamp()` only when `watch_connected`; versions, schema version, rejection fields);
  2. `INSERT INTO replica_versions (replica_id, stream, version, first_reported_at) VALUES (…, 'config', v, clock_timestamp()), (…, 'identity', w, clock_timestamp()) ON CONFLICT DO NOTHING`. The first heartbeat that reports a version, normally the immediate one, sets its time once; this is "the replica's apply heartbeat" for NFR-08, on the same Postgres clock as `published_at` (ADR-0037, ADR-0042);
  3. add `top_keys` to `heavy_hitters` (ADR-0041);
  4. `live_replicas = SELECT count(*) FROM replicas WHERE last_heartbeat_at > clock_timestamp() - interval '15 seconds'` (three intervals, RL-24), the caller included, returned in the response.
- **`live_replicas` in the gateway** (RL-24, RL-31). In managed mode the replica count that divides local and degraded limits (RL-02, RL-23) is the latest `live_replicas` (at least 1). Before the first response it is the disk snapshot's `live_replicas` when the gateway booted from one, else the config's `ratelimit.replicas` (default 1). A failed heartbeat keeps the last known count. A changed count reconciles the limiter registry the way a config change of `ratelimit.replicas` does: state is kept (GW-29). Static mode keeps `ratelimit.replicas`.
- **`GET /api/v1/replicas`** lists the rows with `live` (heartbeat under 15 s old) and both timestamps; the UI shows staleness as now − `last_synced_at` (CP-08).

### `gw_config_sync_age_seconds` (CP-08, OB-05)

In managed mode, the gauge is now minus the gateway's **last sync**: the time it received a successful `HeartbeatResponse` to a heartbeat sent while its Watch stream was established. A gateway in sync therefore reads 0–5 s. While the control plane is unreachable, or the stream is down while heartbeats still succeed, it grows. After a boot from the disk snapshot it starts from the file's `synced_at` (the time the file was last written, after an apply), so it is an upper bound of the true age. Before any sync and with no snapshot it counts from process start. Static mode does not export it. `gw_identity_version` (design metrics table) is exported in both modes. OB-05's alert at 60 s is M7.

### On-disk last good snapshot (CP-07, D9)

- **Setting** `SPILLWAY_GATEWAY_SNAPSHOT`: the file path, default `/var/lib/spillway/snapshot.pb`. Managed mode only.
- **Write.** After each apply, a background writer (coalescing: only the newest state is written) marshals a `SnapshotFile` with both sections, `synced_at` = now and the current `live_replicas`, writes it to a temporary file in the same directory, `fsync`s it, renames it over the path and `fsync`s the directory. A failed write is logged at error level (at most once a minute) and serving continues.
- **Boot in managed mode** (D9):
  1. Connect the Watch and heartbeats in the background.
  2. If both a config and an identity arrive within **5 s**, apply them and become ready.
  3. Otherwise, if the snapshot file exists, parses and has the gateway's schema major, apply it, start the sync age from its `synced_at`, become ready, and keep reconnecting; the first Watch messages replace it.
  4. Otherwise stay not ready (`/readyz` 503), keep reconnecting, and become ready on the first config and identity from the stream. A corrupt or unreadable file is logged and treated as absent.
- **Readiness** in managed mode: a config and an identity are loaded and the gateway is not draining (GW-24 unchanged otherwise).
- **Compose.** Each gateway gets a named volume `gateway-N-state` mounted at `/var/lib/spillway`; the root filesystem stays read-only (SEC-10). The gateway image creates `/var/lib/spillway` owned by uid 65532, so Docker gives a new named volume that owner and the non-root process can write it. `docker compose down -v` removes the snapshots with the other volumes. Helm (M7) mounts a volume there, emptyDir by default, with CP-07's runbook for a replacement pod started during an outage.

### Static and managed mode

- **Managed mode** is on when `SPILLWAY_GATEWAY_CONTROL_PLANE` (the ConfigService address, `host:port`) is set. It requires the client certificate settings (ADR-0040) and forbids `SPILLWAY_GATEWAY_CONFIG` and `SPILLWAY_GATEWAY_IDENTITY` (set together, the gateway refuses to start). Static mode is today's: `SPILLWAY_GATEWAY_CONFIG` required, identity file optional, no control plane, no snapshot file.
- **Compose** keeps the gateways in static mode by default, so `make verify-m1` … `m4` and their config-file swaps are unchanged. The gateway environment becomes `SPILLWAY_GATEWAY_CONFIG: ${SPILLWAY_GATEWAY_CONFIG-/etc/spillway/config.yaml}`, the same for `SPILLWAY_GATEWAY_IDENTITY`, and `SPILLWAY_GATEWAY_CONTROL_PLANE: ${SPILLWAY_GATEWAY_CONTROL_PLANE-}` (the `-` form keeps a value that is set but empty). `make verify-m5` exports the two file settings empty and the control plane as `controlplane:9090`, recreates the gateways in managed mode, and recreates them in static mode on exit (ADR-0042). The compose `controlplane` and `migrate` services always run, so `make dev` and `make verify-m0` cover them. Gateways do not `depends_on` the control plane: they must start without it.

### One active control-plane replica (CP-22, D6)

- **Election.** Each control-plane process keeps one dedicated Postgres connection for the lock and tries the session-level `pg_try_advisory_lock(1397774156, 1)` every 1 s until it holds it (1397774156 is `0x5350574C`, "SPWL"; the second key 1 is "active replica"; key 2 is reserved for M7's quota flusher, RL-10; goose's own lock uses the one-key form, a separate key space). The holder is **active**. The lock connection sets `tcp_keepalives_idle = 5`, `tcp_keepalives_interval = 1` and `tcp_keepalives_count = 5` on its session, so Postgres ends a vanished holder's session, and frees the lock, about 10 s after its peer disappears; the client side uses the same TCP keepalive settings.
- **Active only:** ConfigService (Watch and Heartbeat), the broadcaster and its `LISTEN` connection, and the hourly prune (`heavy_hitters` and `replica_versions` older than 7 days, `replicas` without a heartbeat for 7 days, in batches of 10,000 rows). M6's SSE fan-out and M7's flusher join this list.
- **Standbys** serve the whole REST API (every read and write is a Postgres transaction, and publish's `NOTIFY` reaches the active replica's `LISTEN`) and report ready, but **do not listen on the gRPC port**: the active replica opens the gRPC listener when it takes the lock and closes it when it steps down. A gateway that dials a standby gets a refused connection and dials again, and behind a Kubernetes Service each new connection can land on another pod; an open port that refused calls would keep a gateway's one HTTP/2 connection pinned to the standby. Being ready is what lets M7's single-replica Deployment roll over (a surge pod waits as a standby, then takes the lock when the old pod exits).
- **Losing the lock.** The active replica pings its lock connection every 1 s with a 5 s timeout. A ping that fails (connection error or timeout) means the lock may be gone: it closes every Watch stream with `UNAVAILABLE`, stops the broadcaster and the prune, closes the gRPC listener and the lock connection, and goes back to polling as a standby. A replica that was paused and resumes after a standby took over finds its connection broken on the next ping. In the overlap both may serve Watch streams; both read the same committed rows, and heartbeat writes are upserts, so nothing conflicts. M7's flusher writes absolute values (RL-10) for the same reason.
- **Failover time.** A clean exit releases the lock at once: a standby takes it within 1 s, gateways reconnect within their 5 s backoff, so about 6 s. A crashed or vanished active replica: Postgres frees the lock within about 10 s, plus 1 s and the backoff, so about 16 s. Gateways serve on their last good snapshot throughout (NFR-12).
- **Shutdown** (SIGTERM): stop accepting gRPC, close streams, release the lock, give REST 5 s to finish, exit.
- **CP-22 reconciled with D6.** v1 has one *active* replica, as CP-22 says; Helm stays at `replicas: 1` (M7), and running more replicas for HA, with a measured failover SLO, stays stretch. The lock makes a second process (a rolling update's surge pod, or an accident) a harmless standby instead of a second writer. No requirement text changes.
- **Metrics** on the control plane's admin port: `cp_active` (1 while holding the lock) and `cp_watch_streams` (open streams).

## Consequences

- A publish reaches gateways in one notification, one marshalled snapshot and one send per stream; nothing polls per gateway. The 10 s safety read bounds how late a lost notification can be.
- Heartbeat times and publish times are both Postgres `clock_timestamp()`, so NFR-08 needs no clock comparison between machines.
- Gateways in compose stay static by default until M6 or M8 makes managed mode the default dev setup; managed mode is exercised by `make verify-m5` and the integration tests.
- A replacement gateway started during a control-plane outage without its volume has no snapshot and stays unready (CP-07 runbook, M7).
- One stuck heartbeat transaction cannot stall streams: Watch reads only the broadcaster's cached bytes.
- Every gateway receives every identity change in full; ADR-0037 gives the size at which that changes.

## Alternatives considered

- **The design's message names** (`Update`, `Heartbeat`) — fail `buf lint` STANDARD.
- **Heartbeat as a second stream** — a stream can't give a per-heartbeat success, which the sync age needs, and a unary call is simpler to time out.
- **The gateway clock for apply times** — NFR-08 would compare two clocks; Postgres time on both ends needs no skew bound.
- **Waiting for the control plane forever at boot** — a restart during an outage would never become ready; D9 wants the snapshot served.
- **Serving the disk snapshot at once, before trying the control plane** — a restarted gateway could serve a stale identity (a key revoked while it was down) while the control plane is up.
- **Standbys not ready** — a single-replica Deployment could not roll over without `Recreate`.
- **Transaction-level advisory locks** — held only for one transaction; leadership needs a session.
- **Leader election through a lease row** — clock-based expiry and a renewal loop; the session lock is freed by Postgres itself.
- **A tmpfs for the snapshot in compose** — emptied on restart, so a restarted gateway could not boot from it.
