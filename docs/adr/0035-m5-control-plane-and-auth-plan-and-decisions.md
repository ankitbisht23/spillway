# 0035. M5 control plane and auth: plan and decisions

- **Status:** Accepted; the `auth_mode` interim ended by [ADR-0049](0049-gateway-jwt-and-auth-modes-details.md); the `from_plan` and `default_limits` interim ended by [ADR-0050](0050-plans-overrides-and-access-lists-details.md)
- **Date:** 2026-11-13
- **Requirement IDs:** CP-01 – CP-22, GW-17 – GW-20, RL-08, RL-09, RL-21, RL-24, RL-31, SEC-02, SEC-04, SEC-06, SEC-08, SEC-09, NFR-08, NFR-09, NFR-12

## Context

M5 builds the control plane (Postgres, REST admin API, config and identity streams to gateways) and authentication (managed API keys, JWT, RBAC). The user approved the task order — design-adrs, schema-api, rbac-audit, publish, import-export, watch, mtls-secrets, keys-identity, jwt, plans, heavy-hitters, verify — and set ten defaults, D1–D10. This run settles the design in seven ADRs so later tasks implement without guessing; the user sees them at a checkpoint before any code. This ADR indexes them, maps every M5 checklist box and requirement to a task, describes how gateways behave between tasks, lists the design-text changes the ADRs propose, and lists every decision taken for the user.

## Decision

### The M5 ADRs

| ADR | Settles |
| --- | --- |
| [0036](0036-control-plane-data-model-migrations-and-rest-api.md) | binary and settings, Postgres tables and indexes, IDs, migrations and expand/contract, the REST conventions and endpoint list, OpenAPI tooling |
| [0037](0037-config-publish-rollback-and-the-identity-path.md) | working-set revision, CP-03 validation and SEC-06, publish, rollback, import/export, the identity path and its versions |
| [0038](0038-config-distribution-watch-heartbeat-snapshots-and-the-active-replica.md) | proto, schema versions, Watch, Heartbeat, `live_replicas`, sync age, on-disk snapshot, static vs managed mode, the active replica |
| [0039](0039-gateway-authentication-managed-api-keys-jwt-and-auth-modes.md) | key format and lifecycle, gateway key lookup, JWT/JWKS, identity resolution, auth modes, 401/403 answers, no credentials in logs |
| [0040](0040-control-plane-security-mtls-oidc-rbac-audit-and-secrets.md) | mTLS and certificates, OIDC tokens and the dev realm, the RBAC matrix, the audit log, Postgres roles, secrets |
| [0041](0041-limit-precedence-plans-overrides-access-lists-and-heavy-hitters.md) | plans, overrides and burst grants, access lists, precedence with worked examples, heavy hitters |
| [0042](0042-m5-exit-check.md) | `make verify-m5`: NFR-08, NFR-09, NFR-12 pass rules, stall tolerance, run time |

### Task plan check

Every M5 box and requirement has a task. A box that spans tasks is ticked by the task that completes it.

| M5 checklist box (abridged) | IDs | Built by | Ticked by |
| --- | --- | --- | --- |
| Postgres schema; CRUD admin API, OpenAPI-first, oasdiff; tenants, access lists, key listing, endpoint drain | CP-01, CP-10, CP-14 | schema-api (key listing included: it reads `api_keys`, empty until keys-identity) | schema-api |
| Migrations as `controlplane migrate`; compose before the API; Helm Job; expand, then contract | CP-19 | schema-api | schema-api, on the compose step plus ADR-0036's Helm contract (P9) |
| YAML import and export through the API | CP-05 | import-export | import-export |
| Validate, publish with note and `If-Match`, version, rollback; allow-list in API validation | CP-02 – CP-04, SEC-06 | schema-api (per-entity SEC-06), publish | publish |
| Identity changes applied at once, own version, audited, never rolled back, separate Watch messages | CP-20 | schema-api (versions), rbac-audit (audit), watch (delivery), keys-identity (keys and revocation) | keys-identity |
| gRPC Watch and Heartbeat, `replicas`, `live_replicas`, sync age, on-disk snapshot, versioned schema | CP-06 – CP-09, CP-18, RL-24 (RL-31's fallback) | watch | watch |
| One active control-plane replica, state in Postgres, `LISTEN/NOTIFY` | CP-22 | publish (`NOTIFY`), watch (lock, `LISTEN`) | watch |
| Heavy hitters in `top_keys`, kept as `heavy_hitters` windows | RL-21 | heavy-hitters | heavy-hitters |
| mTLS for the stream; certs from a Secret in Helm; API bound to internal networks | SEC-02 | mtls-secrets | mtls-secrets, on compose plus ADR-0040's Helm contract (P9) |
| Key issue, list, rotate, revoke; managed keys replace static | CP-15 – CP-17, GW-17 | keys-identity | keys-identity |
| JWT/JWKS, allowlist, browser-facing `iss`, own JWKS URL; identity → tenant and plan | GW-18 – GW-20, SEC-04 | jwt (GW-19 for keys in keys-identity) | jwt |
| Plans, `from_plan`, overrides, allow/deny lists; Keycloak login, confidential clients, RBAC everywhere, append-only audit | RL-08, RL-09, CP-11 – CP-13, SEC-08 | rbac-audit (CP-11 – CP-13, SEC-08), plans (RL-08, RL-09) | plans |
| Postgres, Redis and Keycloak secrets from env or files | SEC-09 | mtls-secrets | mtls-secrets |
| Exit: `make verify-m5` | NFR-08, NFR-09, NFR-12 | verify | verify (its output in the PR) |

CP-21 is M1's; M5 only adds fields. Every endpoint added after rbac-audit lands with its `x-spillway-roles` and audit records; rbac-audit adds both to schema-api's endpoints.

### What each task delivers, and gateways meanwhile

| Task | Builds | Gateways and API once merged |
| --- | --- | --- |
| design-adrs | ADR-0035 – ADR-0042 | unchanged |
| schema-api | `cmd/controlplane` (`serve`, `migrate`, `healthcheck`), its image, compose `migrate` and `controlplane` (REST 8080 and admin 8081 on 127.0.0.1; no gRPC yet); both Postgres roles; migrations for the config, identity and `config_versions` tables; sqlc store; `api/openapi.yaml` with every M5 CRUD path, key listing and drain (roles already declared); oapi-codegen, `make api`, `make lint-api`, oasdiff in CI; ETags, pagination, errors, entity rules, identity versions; proto config fields (IDs, `draining`, `Plan`, `from_plan`, `auth_mode`); gateways skip draining endpoints (all draining: 503, ADR-0008's "no usable address"); the proto-parity and `EXPLAIN` tests | Gateways unchanged (static). **Interim:** REST has no authentication until rbac-audit (loopback only in compose, no deployment exists); `from_plan`, plans and an `auth_mode` other than empty or `none` fail compile with "not supported until <task>", so nothing half-built can be published |
| rbac-audit | `internal/auth` (JWT checks, JWKS cache), bearer tokens and the RBAC middleware, `audit_log` with grants and triggers, audit records on every existing mutation, `GET /audit`, the realm changes; CP-12's wording if the user approved P1 | REST requires tokens and roles |
| publish | validate, publish, versions, diff, rollback, CP-03 and SEC-06 checks, `NOTIFY spillway_config` | versions accumulate; nobody listens yet |
| import-export | config import and export, identity export | as before |
| watch | `config_service.proto`, identity proto fields, ConfigService, broadcaster and `LISTEN`, advisory lock and standbys, prune, heartbeats, `replicas`, `replica_versions`, `GET /replicas`, `live_replicas` in the limiter; gateway managed mode, on-disk snapshot and its volumes, `gw_identity_version`, `gw_config_sync_age_seconds`; a short outage integration test | Managed mode works (opt-in). **Interim:** gRPC is plaintext and reachable only inside the compose network (9090 not published) until mtls-secrets; managed identity has tenants and consumers but no keys, so a keyed request on a managed gateway is 401 |
| mtls-secrets | stream CA and certificates in `make certs`, mTLS on both ends with reload, replica name from the certificate, 9090 published on 127.0.0.1; the `_FILE` convention and redacted secret type everywhere; Redis username and password settings and their testcontainer test | managed mode requires mTLS; no plaintext gRPC exists |
| keys-identity | key generation, issue, rotate, revoke, managed lookup (`spw1`), expiry, the gitleaks rule, a revocation integration test, no-credential-in-logs tests | managed keys work end to end; revocation through identity |
| jwt | gateway JWT settings and checks, `auth_mode` (all four), `jwt_subject`, the `gateway-tests` audience and fixed service-account ID | auth modes live |
| plans | plans, `from_plan`, overrides and burst grants, access lists and statuses, precedence in the pipeline, per-call local limits, the plan and override validation rules | RL-08 and RL-09 complete |
| heavy-hitters | the sketch, `top_keys`, `heavy_hitters` upserts and reads | heavy hitters reported |
| verify | `chaos controlplane`, `test/verify/m5.sh` and `m5.yaml`, `make verify-m5`, `make chaos-cp-outage`, measured runs | M5 exit |

### Proposed changes to the task plan

None is required: CP-20's gateway side needs Watch, and keys-identity already follows watch; plans already follows rbac-audit and publish. Two optional changes for the user:

- **(T1) Split watch.** As planned, watch is the largest task (server, fan-out, election, heartbeats, gateway managed mode, snapshot). It could be split into `watch` (proto, ConfigService.Watch, broadcaster, lock, gateway managed mode, snapshot) and `heartbeat` (Heartbeat, `replicas`, `replica_versions`, `live_replicas`, sync age, `GET /replicas`), the second ticking the watch box. Recommended only if the watch PR grows past review size.
- **(T2) Authentication before CRUD.** schema-api's REST API is unauthenticated until rbac-audit merges (interim above). The alternative is to move the token check and RBAC middleware into schema-api. Not recommended: the interim is one task long, compose binds it to loopback, and the order keeps schema-api reviewable.

### Proposed design-text changes (for the user's approval)

docs/design.md is not edited in this PR. Each item names the task that would apply it once approved; if the user declines one, the ADR's decision binds the code and the design text stays as is (the ADRs record why).

- **(P1) CP-12** (rbac-audit), to match D4: "**CP-12** (M5) RBAC with five global roles, each able to read everything except the audit log: viewer (read only), editor (config working set and import), publisher (publish and rollback), key-admin (consumers, API keys, overrides and access lists), admin (everything, including tenants and the audit log)." Includes the placement of consumers, overrides and access lists, which D4 does not name (ADR-0040).
- **(P2) CP-03's conflict rule** (publish): "policy conflicts (two enforce-mode policies at the same level, with the same algorithm and key template, matching the same route and method)". As written, CP-03 would reject a shadow policy beside the enforce policy it replaces (demo scenario D4's promote flow) and ADR-0017's own example (ADR-0037).
- **(P3) Identity version** (watch; Data model paragraph under the Postgres table): "The identity version is a single-row counter (`identity_state.version`), incremented first in the same transaction as each identity change; its row lock orders identity changes, so versions follow commit order." A sequence does not (ADR-0037).
- **(P4) gRPC message names** (watch; Admin API section, last paragraph): `ConfigService.Watch(WatchRequest{…}) → stream WatchResponse` (a full config snapshot or a full identity snapshot) and `ConfigService.Heartbeat(HeartbeatRequest{…, top_keys}) → HeartbeatResponse{live_replicas}`, as `buf lint` requires (ADR-0038).
- **(P5) Export** (import-export; Admin API table): "`GET /api/v1/config/export` — config as YAML (CP-05)" and a new row "`GET /api/v1/identity/export` — identity as YAML (admin)", so key hashes stay admin-only by operation (ADR-0036).
- **(P6) Postgres schema table** (schema-api): config entity rows become "id, name, spec (jsonb: the proto message's fields), version" (`endpoints`: id, upstream\_id, address, draining); new rows `identity_state` (version, changed\_at) and `replica_versions` (replica\_id, stream, version, first\_reported\_at); `api_keys` adds rotated\_from and version; `replicas` adds schema version, watch\_connected, last\_synced\_at and the rejection fields; `heavy_hitters` adds api\_key\_id; `audit_log` adds actor roles, request\_id, config\_version and identity\_version (ADR-0036).
- **(P7) GW-20** (jwt): "Auth mode per route: none (no credential required; an `X-API-Key` that is sent is still checked), API key, JWT, or either (an API key, when sent, is used)." Keeps every existing config's meaning (ADR-0039).

### Questions for the user at the checkpoint

- **(P8)** D4's gap: consumers, overrides and access lists placed with key-admin (part of P1).
- **(P9) Helm parts of two M5 boxes.** The migrate box names "Helm as a pre-install/pre-upgrade Job" and the mTLS box "certs from a Secret in Helm", but the chart is M7. Proposed: M5 ticks both boxes for the compose part plus the Helm contract written in ADR-0036 and ADR-0040, and M7's Helm chart box implements them (a box left open would block "finish the checklist before the next milestone"). The alternative is to leave both boxes open until M7.
- **(P10) D7 in Kubernetes.** With the certificate name as the replica name, every gateway pod needs its own certificate; pods of one Deployment sharing a Secret would all be one replica. M7 must pick a StatefulSet with a Secret per ordinal, or a changed rule (for example a shared certificate name plus the pod name as the replica ID). Nothing changes in M5, where compose has one certificate per gateway.
- **(P11)** The compose Redis Cluster stays without a password; the gateway's Redis password path is tested on a single-node testcontainer (ADR-0040).

### Decisions taken for the user (review)

**D1–D10, as adopted**

- **D1 Key format** — adopted: `spw_` + 8 base62 + `_` + 43 base62 (the 32-byte secret as a base62 number); stored prefix and SHA-256 of the 43 secret characters; shown once with `Cache-Control: no-store` (ADR-0039).
- **D2 Rotation** — adopted: grace `0s`–`168h`, default `24h`, per rotation; the old key's `expires_at` set to the earlier of its own and now + grace; expiry checked by gateways on their clock, no message needed; revocation immediate through identity (ADR-0039). Allowing `0s` is an addition (D2 gives only the default and maximum).
- **D3 Precedence** — adopted; an allow entry exempts from every not-yet-run policy, concurrency included, and never overrides a deny or a suspended status (ADR-0041).
- **D4 Roles** — adopted with the gap filled (P1, P8); every role reads everything except the audit log; roles come from Keycloak's `realm_access.roles` (ADR-0040).
- **D5 Rollback** — adopted: a new version copying the old one, working tables reset to it, history never rewritten, identity untouched; a rollback needs `If-Match` on the working-set revision and a note, and is refused (422) when identity refers to config the target lacks (ADR-0037).
- **D6 Active replica** — adopted: session advisory lock `(1397774156, 1)`; standbys serve REST, are ready, and open no gRPC listener; failover about 6 s after a clean exit, about 16 s after a crash (ADR-0038).
- **D7 Gateway identity** — adopted for compose (one certificate per replica from a dedicated stream CA); Kubernetes needs P10 (ADR-0040).
- **D8 JWT** — adopted: go-jose v4.1.7, RS256 and ES256, 5 min refresh, one unknown-`kid` refresh per 30 s with requests waiting up to 2 s, last good keys kept with no age limit, 503 when none were ever fetched (ADR-0039).
- **D9 Snapshot** — adopted: `SnapshotFile` protobuf at `/var/lib/spillway/snapshot.pb`, temp file + fsync + rename + directory fsync; at boot the stream gets 5 s, then the file is served; no file and no control plane means not ready (ADR-0038).
- **D10 NFR-08** — adopted: 60 publishes, 5 gateways, both times on Postgres's clock, p99 by nearest rank (the maximum at N = 60) < 5 s (ADR-0042).

**New decisions the requirement text leaves open**, each with its reason:

- Integer IDs; managed Redis keys, hash tags and `{consumer}`/`{tenant}` use decimal IDs — short keys, renames keep counters (ADR-0036, ADR-0039).
- One `spec jsonb` per config entity instead of per-field columns — the proto stays the one schema (P6, ADR-0036).
- Forward-only goose migrations; `serve` refuses an older schema and accepts a newer one — rolling deploys only ever fix forward (ADR-0036).
- Two Postgres roles; `migrate` sets the app role's password as a SCRAM verifier — the app cannot drop the audit triggers, and no plaintext password reaches the server (ADR-0040).
- Cursor (keyset) pagination, default 50, max 500 — the audit log only grows (ADR-0036).
- Error body `{"error", "code", "request_id", "details"}` — keeps the gateway's `error` key (ADR-0036).
- `If-Match` required on `PUT`, `DELETE`, drain, import, publish, rollback; not on rotate and revoke — revocation must be one click and is idempotent (ADR-0036).
- `409 nothing_to_publish` when the working set has not changed since the last version — two publishers of one preview make one version (ADR-0037).
- Validate always answers 200 with errors, a change list and a unified YAML diff — the UI preview needs all three (ADR-0037).
- Import matches by ID, then by name; refuses identity; identity export is its own admin-only path (P5, ADR-0037).
- Full identity snapshot per change, incremental above 1 MB — simple and self-healing at M5's sizes (ADR-0037).
- Single-row counter for identity versions; row lock for config versions — commit order (P3, ADR-0037).
- Policy conflict = same level, algorithm and template, both enforce (P2, ADR-0037).
- SEC-06 in the API resolves host names with a 2 s timeout; unresolvable names pass (dial-time checks remain) (ADR-0037).
- Proto: `config_service.proto` in the same package; schema 1.0; one minor of skew accepted both ways (P4, ADR-0038).
- Heartbeats with a 4 s deadline; `replica_versions` records first reports for NFR-08 and the History screen; gateways report rejected versions (ADR-0038).
- Sync age = time since a successful heartbeat while the Watch stream was up; static mode does not export it (ADR-0038).
- Compose gateways stay static by default; managed mode is opt-in by env; `make verify-m5` switches and restores — M1–M4 checks keep working (ADR-0038).
- Snapshot volumes `gateway-N-state`, directory owned by uid 65532 in the image (ADR-0038).
- A dedicated mkcert CA for the stream, TLS 1.3, one DNS label per client certificate (ADR-0040).
- RBAC from `x-spillway-roles` in the spec, with a test over every operation and role (ADR-0040).
- Audit: one record per successful mutation in its transaction; notes from publish, rollback, import, override and access-list entries, rotate and revoke; append-only by grants and triggers (ADR-0040).
- Secrets: `X` or `X_FILE`, both is an error, read once at start, redacted type (ADR-0040).
- A gitleaks detection rule for the `spw_` key grammar — it adds findings and leaves the allowlist untouched (ADR-0039).
- JWT settings from env, `SPILLWAY_GATEWAY_JWT_CA` for M7's HTTPS Keycloak; `sub` → `consumers.jwt_subject` (ADR-0039).
- `auth_mode` unset means `none`, which still checks a sent key; `either` prefers the key (P7, ADR-0039).
- 403 `{"error":"forbidden"}` for deny entries and suspended consumers or tenants (ADR-0039, ADR-0041).
- Plans and overrides only on consumer- or route-level policies keyed by `{consumer}` or `{api_key}`; every tenant has a plan; every plan covers every `from_plan` policy (ADR-0041).
- Among active overrides, a burst grant beats a standing override, then the latest start (ADR-0041).
- Heavy hitters: one 100-counter sketch per replica, count 1 per deny or shadow would-deny, failure modes excluded, 22-character digests with the consumer and key IDs, one-minute windows (ADR-0041).
- NFR-09: 20 revocations, probes every 50 ms per replica, sender clock, every sample < 5 s (ADR-0042).
- NFR-12: control plane and Postgres stopped for 600 s under 500 rps over five gateways, gateway-3 restarted from its snapshot at +300 s, five rules (ADR-0042).
- `make verify-m5`'s timeouts: the routes keep the default 5 s connect and 15 s total, probes and scrapes get 4 s, REST calls 10 s, the load client 20 s — every timeout at least 4× the 820 ms planning stall, and a failed probe or scrape fails its check (ADR-0042).
- The `spillway-ci` client's service account holds `admin`: `make verify-m5` creates a tenant, which only admin may; RBAC tests use the per-role users (ADR-0040).
- Library pins: releases about two weeks old or older on 2026-11-13 (pgx v5.12.0, go-jose v4.1.7, oapi-codegen runtime v1.8.0, testcontainers v0.44.0, goose v3.29.0 to match the CLI, kin-openapi v0.142.0 as oapi-codegen requires) (ADR-0036, ADR-0039).

## Consequences

- Later M5 tasks implement these ADRs; a task that finds one wrong writes a new ADR that supersedes the part, as usual.
- P1–P7 change docs/design.md only after the user approves them; P8–P11 and T1–T2 are questions for the same checkpoint.
- Carried items stay out of M5 unless a task needs them: the NFR-01 re-baseline, the per-client-IP connection cap (M7), the accuracy test's boundary 503s (M7), and `make verify-m3`'s accepted laptop pass rate.

## Alternatives considered

- **One large M5 ADR** — over a thousand lines; topics reviewed by different questions (schema, security, exit rules) are easier to approve separately.
- **Editing CP-12 and the other texts now** — requirement text changes only with the user's approval.
- **Reordering tasks (RBAC first, or mTLS inside watch)** — T1 and T2 above; the given order works with one-task interims.
