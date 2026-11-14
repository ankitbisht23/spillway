# 0045. Config settings, and the watch task's details

- **Status:** Accepted; the interim until mtls-secrets ended by [ADR-0047](0047-stream-mtls-and-secrets-details.md); managed identity without key lookup ended by [ADR-0048](0048-managed-api-keys-and-the-identity-path-details.md)
- **Date:** 2026-11-14
- **Requirement IDs:** CP-02, CP-05, CP-06, CP-07, CP-18, CP-20, CP-22, RL-22, RL-24, RL-31, SEC-02

## Context

M5's watch task builds ADR-0038's config stream. Building it raised these points:

- **The `ratelimit` section.** ADR-0044 found that the control plane cannot keep a config file's `ratelimit` section (`replicas`, `redis_budget`): import drops it and export never writes it. A managed gateway would then run with the default Redis budget (5 ms) whatever the operator wants, and before its first heartbeat it would have no replica count but the default 1 (RL-31, ADR-0038). ADR-0044 left it to a later ADR. The run decided for the user (flagged at the M5 checkpoint notes) that the control plane keeps the section.
- **Size.** ADR-0035's T1, approved by the user, splits the task if it grows past review size: `m5-watch` (proto, Watch, broadcaster and `LISTEN`, advisory lock and standbys, gateway managed mode, on-disk snapshot) and `m5-heartbeat` (Heartbeat, `replicas`, `replica_versions`, `live_replicas`, sync age, `GET /replicas`).
- **Before mTLS.** ADR-0035 lets the stream run plaintext inside the compose network until mtls-secrets, but the replica name comes from the client certificate (ADR-0040), which does not exist yet.
- **The changes list.** The preview's and import's `changes` list config entities with a closed `entity` enum (`upstream`, `route`, `policy`, `plan`); `oasdiff` reports a new value as a breaking change of three responses.

## Decision

### Config settings (the `ratelimit` section)

- The control plane keeps the `ratelimit` section as one row of the config working set: table `config_settings` (`id` = 1, `spec` jsonb = the `RateLimitSettings` message's protojson, `version`, timestamps), migration `00003`. It starts empty (`{}`, version 1).
- It is a config edit like any other (ADR-0037): `PUT /api/v1/config/settings` (editor, admin; `If-Match` on its version; body `{"ratelimit": {"replicas", "redis_budget"}}`) bumps the working-set revision, which becomes its version, and writes an audit record `settings.update` on entity `settings`, ID 1, with the settings before and after. `GET /api/v1/config/settings` is open to every role, with an ETag. The settings are checked as a static file's are (`config.CheckRateLimit`: replicas ≥ 0, `redis_budget` a duration in (0, 1s]); a failure is 422.
- The working set read by validate, publish, export and import has the settings as its `ratelimit` section (empty settings are no section). So every published version carries them in its snapshot, the stream sends them, a rollback restores them (only when they differ), and export writes them.
- **Import** keeps the section: the file's `ratelimit` replaces the settings, and a file without one empties them, as for every other section. It is checked like the PUT (400 for an unknown field, 422 for a broken rule, with the path `ratelimit`). The answer's `ignored` stays in the API and is always empty. An import that changes only the settings is a change (revision bump, audit record); its audit record holds `"settings": {"before", "after"}`.
- **Not in `changes`.** A settings change does not appear in the preview's, the version diff's or the import's `changes` list: adding `settings` to the closed `entity` enum is an `oasdiff` breaking change the user has not approved. It shows in the preview's unified YAML diff, in the import's audit record and, with every other change, in the version's snapshot. M6 (the UI) may propose the enum change with the user's approval.
- **Replica count in managed mode** (RL-31, ADR-0038): until heartbeats land (`m5-heartbeat`), a managed gateway divides by the published `ratelimit.replicas`. `m5-heartbeat` adds `live_replicas`, with the fallback the run decided: the last known `live_replicas` (persisted in the snapshot file), then `ratelimit.replicas`.

ADR-0044's statements that import drops the section and export never writes it are superseded by this section.

### The split (T1)

The watch task ships as two PRs. `m5-watch`:

- `api/proto/config_service.proto` with ADR-0038's whole service and messages (`Heartbeat` included, so the proto changes once), the managed `Identity` fields in `config.proto`, `protoc-gen-go-grpc` in `make proto` and `make lint-proto`;
- the config settings above; `GET /api/v1/identity/export` (moved here by ADR-0044); `NOTIFY spillway_identity` in every identity change's transaction;
- the broadcaster, `LISTEN`, the 10 s safety read, `ConfigService.Watch`, the advisory-lock election with standbys, `cp_active` and `cp_watch_streams`, `SPILLWAY_CP_GRPC_ADDR`;
- gateway managed mode, the on-disk snapshot, boot from it, `gw_identity_version`, the snapshot volumes;
- docs/design.md P3 and P4 (approved).

`m5-heartbeat`: `ConfigService.Heartbeat` (UNIMPLEMENTED until then; gateways do not call it yet), the `replicas` and `replica_versions` tables and their hourly prune, `live_replicas` in the limiter with the fallback above, `gw_config_sync_age_seconds`, `GET /api/v1/replicas`, rejected versions in heartbeats. It ticks both M5 boxes this task names: the Watch and Heartbeat box needs heartbeats, `live_replicas` and the sync age, and the CP-22 box's "state in Postgres" is the `replicas` table it adds.

### Interim until mtls-secrets

- The gRPC listener is plaintext; compose does not publish 9090, so only the compose network reaches it (ADR-0035). The gateway dials with insecure transport credentials.
- The replica name is the request's `replica_id`, which must be a DNS label (`INVALID_ARGUMENT` otherwise). The gateway's is `SPILLWAY_GATEWAY_REPLICA_ID` (managed mode only), default the host name in lower case; compose sets `hostname: gateway-N` on each gateway. mtls-secrets takes the name from the certificate and makes a differing `replica_id` `PERMISSION_DENIED` (ADR-0040).

### Implementation details

- **Pre-encoded messages.** The broadcaster marshals each version's `WatchResponse` once; a server codec sends those bytes as they are and falls back to the proto codec for everything else (ADR-0038).
- **Stream registration.** After registering a stream, Watch sends its headers; the gateway counts the stream as established when they arrive, which mtls-secrets and heartbeats (`watch_connected`) use.
- **Broadcaster reads.** A notification, the 10 s tick and each `LISTEN` (re)connect kick a refresh of the section; one pending kick per section coalesces the rest. A refresh reads both versions first and rebuilds a section only when its version differs. A version that cannot be read (another schema major) is logged and the last one kept; the next notification or tick tries again.
- **Election.** A standby retries the lock every second; a replica that was active waits a second after stepping down before it tries again, so a standby can take over. On SIGTERM the control plane stops the election first (streams end with `UNAVAILABLE`, the gRPC listener closes, the lock session closes), then gives REST 5 s.
- **Gateway boot.** `Boot` waits 5 s for both sections from the stream, then applies from the file only the sections the stream has not delivered. Applies from the file are not written back, so the file's `synced_at` stays the time of the last sync.
- **Before the first config** a managed gateway answers every request 503 `{"error":"no config loaded yet"}` and `/readyz` fails.
- **Static mode refuses managed fields.** An identity file with `key_format` other than unset, tenants, access lists, overrides, or consumer or key IDs, statuses, JWT subjects or expiry is refused: they come from the control plane only. Managed identity (`key_format: spw1`) compiles with no key lookup until keys-identity (ADR-0035's interim).

## Consequences

- Managed gateways get the operator's Redis budget and a replica count from published config; a rollback restores both.
- The GitOps import of ADR-0044 now round-trips the `ratelimit` section, and a re-import of the same file is still a no-op.
- `m5-heartbeat` runs as its own step; until it merges, the control plane records nothing per replica and the two M5 boxes stay open.
- Compose gateways stay static by default; `make verify-m5` (M5's verify task) and the integration tests switch them to managed mode with `SPILLWAY_GATEWAY_CONFIG=`, `SPILLWAY_GATEWAY_IDENTITY=` and `SPILLWAY_GATEWAY_CONTROL_PLANE=controlplane:9090`.

## Alternatives considered

- **Settings in the `changes` list** — needs the user's approval of an `oasdiff` breaking change; the diff text already shows them.
- **Keep dropping `ratelimit` (ADR-0044)** — managed gateways could not get a Redis budget other than 5 ms, nor a replica count before heartbeats.
- **A gateway setting for the Redis budget** — would make one config value differ per replica; ADR-0008 keeps settings for what a config must not widen.
- **One PR for the whole task** — over twice the review size of earlier M5 PRs.
- **A replica name setting with no default** — every compose gateway would need its own environment map, which YAML merge cannot give the shared gateway block; the host name, set per service, can.
