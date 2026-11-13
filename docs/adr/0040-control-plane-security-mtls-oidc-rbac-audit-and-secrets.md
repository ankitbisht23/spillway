# 0040. Control-plane security: mTLS, OIDC, RBAC, audit and secrets

- **Status:** Accepted
- **Date:** 2026-11-13
- **Requirement IDs:** SEC-02, SEC-04, SEC-08, SEC-09, CP-11 – CP-13, CP-22, UI-04, UI-08

## Context

The control plane is the one place that can change every gateway's behaviour, so its surfaces must be closed by default. The design asks for an mTLS gRPC stream with dev certs from `make certs` and certs from a Secret in Helm (SEC-02), Keycloak-only login with confidential clients for service tokens (CP-11), RBAC on every endpoint (CP-12, SEC-08), an append-only audit log with actor, action, entity, diff, time and change note (CP-13, SEC-08), and secrets only from env or mounted files (SEC-09). The user set D4 (five global roles: viewer, editor, publisher, key-admin, admin) and D7 (the client certificate's name is the replica name). CP-12's text still names admin, operator and viewer. Open:

- which CA, which certs, how names are checked, how certs reload, and how D7 works in Kubernetes;
- how the control plane validates human and service tokens and maps roles;
- the role of every endpoint, including those D4 does not name (consumers, overrides, access lists), and how enforcement is guaranteed to cover every operation;
- what an audit record holds, how "append-only" is enforced in Postgres, and who reads it;
- the secret settings and their file convention.

## Decision

### mTLS for the config stream (SEC-02, D7)

- **A dedicated dev CA.** `make certs` runs mkcert with `CAROOT=deploy/certs/stream-ca`, a second mkcert CA used only for the stream and never installed in any trust store (unlike the browser-trusted CA of ADR-0012). Its private key stays in that gitignored directory and is never mounted into a container. `make certs` writes, all gitignored, key files mode 0644 for ADR-0012's reason:
  - `deploy/certs/stream/controlplane/{cert,key}.pem`: server certificate for `controlplane`, `localhost`, `127.0.0.1`;
  - `deploy/certs/stream/gateway-N/{cert,key}.pem` for N = 1 … 5: client certificates (`mkcert -client gateway-N`), whose only DNS name is the replica name;
  - `deploy/certs/stream/<each of the above>/ca.pem`: the stream CA's certificate (public), copied next to each pair.
- **Control plane** (`SPILLWAY_CP_GRPC_TLS_CERT`, `_KEY`, `SPILLWAY_CP_GRPC_CLIENT_CA`): TLS 1.3 only (both ends are Go), `RequireAndVerifyClientCert` against the pinned stream CA only (never the system roots), client certificates must carry the client-auth extended key usage. An interceptor takes the replica name from the certificate: exactly one DNS name, a DNS label (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`), else `PERMISSION_DENIED`. A `replica_id` in a request that differs from it is `PERMISSION_DENIED`, logged with both names. There is no plaintext gRPC listener at all.
- **Gateway** (`SPILLWAY_GATEWAY_CP_CERT`, `_KEY`, `SPILLWAY_GATEWAY_CP_CA`, `SPILLWAY_GATEWAY_CP_SERVER_NAME`): verifies the control plane against the pinned CA only, for the server name (default: the host part of `SPILLWAY_GATEWAY_CONTROL_PLANE`), and presents its client certificate. Its replica name is the DNS name in that certificate; it sends it as `replica_id` and uses it in logs.
- **Reload.** Both ends poll their certificate, key and CA files every second and compare SHA-256 of the content, as ADR-0012 does for the traffic certificate. New handshakes use the new files (`GetCertificate` and `GetConfigForClient` on the server, `GetClientCertificate` and a verify callback with the current CA pool on the gateway); open streams keep theirs until they reconnect. A set that fails to load keeps the current one and is logged once.
- **No revocation list in M5.** A compromised gateway certificate is cut off by replacing the stream CA (`make certs` after deleting `deploy/certs/stream-ca`), which re-issues every certificate.
- **Compose.** Each gateway mounts its own `deploy/certs/stream/gateway-N` at `/etc/spillway-stream` (read-only); the control plane mounts `deploy/certs/stream/controlplane`. The control plane publishes `127.0.0.1:8080` (REST), `127.0.0.1:8081` (admin) and `127.0.0.1:9090` (gRPC), as docs/ports.md lists; inside the network it listens on all interfaces so gateways and Prometheus reach it (Prometheus gets a `controlplane:8081` scrape job). REST is plain HTTP in compose, on loopback only.
- **Helm contract (M7).** The control plane and each gateway mount a Secret with `cert.pem`, `key.pem` and `ca.pem` at `/etc/spillway-stream`. D7 makes the certificate's name the replica name, so **each gateway replica needs its own certificate**; a Deployment whose pods share one Secret would give every pod the same name (each new stream replacing the last). M7 must choose between a StatefulSet with one Secret per ordinal and a different identity rule; ADR-0035 raises this with the user now.

### Human and service tokens (CP-11)

- Humans log in through Keycloak only; the control plane stores no passwords. Service tokens are Keycloak confidential clients with the client-credentials grant.
- Every `/api/v1` request needs `Authorization: Bearer <access token>`, verified with ADR-0039's rules and library, against `SPILLWAY_CP_OIDC_ISSUER` (the browser-facing issuer), `SPILLWAY_CP_OIDC_JWKS_URL` (in-network in dev) and audience `spillway-controlplane`, with the same JWKS cache, 30 s unknown-`kid` refresh and `SPILLWAY_CP_DEV_MODE` rule for `http://` URLs. Failure is **401** `unauthenticated` with `WWW-Authenticate: Bearer realm="spillway"` (and `error="invalid_token"` for a bad token). JWKS never fetched: **503** `unavailable`.
- **Roles** come from Keycloak's `realm_access.roles` claim; the names `viewer`, `editor`, `publisher`, `key-admin` and `admin` are Spillway's, other roles are ignored, and a token with none of them gets 403 on every operation. A user may hold several roles; the permissions add up.
- **Actor**: `sub`, plus `preferred_username` as the display name (Keycloak sets `service-account-<client>` for service accounts) and the roles, stored in audit records only. Process logs carry `sub` and never the username or email (no personal data).
- **Dev realm** (`deploy/compose/keycloak/realm.json`), all values dev-only:
  - realm roles `viewer`, `editor`, `publisher`, `key-admin`, `admin` (D4); `operator` removed;
  - users `test-viewer`, `test-editor`, `test-publisher`, `test-key-admin`, `test-admin`, one role each, password `dev-only-not-secret`, fixed `id`s; `test-operator` removed;
  - `admin-ui` (public, PKCE; M6) gets an audience mapper adding `spillway-controlplane`;
  - new confidential client `spillway-ci` (client credentials only, secret `dev-only-not-secret`, audience `spillway-controlplane`) whose service account holds `editor`, `publisher` and `key-admin`; `make verify-m5` and the integration tests use it;
  - `gateway-tests` (confidential, existing) gets an audience mapper adding `spillway-gateway`, and its service-account user a fixed `id`, so its `sub` is stable for a consumer's `jwt_subject` (ADR-0039);
  - tokens must carry `sub` and `realm_access.roles` (Keycloak's `basic` and `roles` client scopes). The rbac-audit task asserts both on a minted token and declares the scopes in the realm file if the import does not add them.
  
  `.gitleaks.toml` is unchanged: its allowlist already covers `dev-only-not-secret`.

### RBAC (CP-12, SEC-08, D4)

**Roles** (global in v1; per-tenant scoping deferred). Every role can read everything except the audit log; on top of that:

| Role | May also |
| --- | --- |
| viewer | nothing more |
| editor | edit the config working set (upstreams, routes, policies, plans, endpoint drain), import |
| publisher | publish, roll back |
| key-admin | manage consumers, issue, rotate and revoke keys, manage overrides and access lists |
| admin | everything, including tenants, the identity export and the audit log |

D4 names keys for key-admin and does not place consumers, overrides and access lists. They go to key-admin: they are the per-consumer identity entities that apply immediately (CP-20), and blocking a key (demo scenario D7, an access-list entry) is the same operator action as revoking it. Tenants, which own plans and every consumer, stay admin-only as D4 says. ADR-0035 lists this for the user.

**Matrix** (operation IDs as in `api/openapi.yaml`; `all` = any of the five roles):

| Operations | Roles |
| --- | --- |
| `listUpstreams`, `getUpstream`, `listRoutes`, `getRoute`, `listPolicies`, `getPolicy`, `listPlans`, `getPlan` | all |
| `createUpstream`, `updateUpstream`, `deleteUpstream`, `drainEndpoint`, `createRoute`, `updateRoute`, `deleteRoute`, `createPolicy`, `updatePolicy`, `deletePolicy`, `createPlan`, `updatePlan`, `deletePlan`, `importConfig` | editor, admin |
| `exportConfig`, `validateConfig`, `listConfigVersions`, `getConfigVersion`, `diffConfigVersion` | all |
| `publishConfig`, `rollbackConfig` | publisher, admin |
| `listTenants`, `getTenant` | all |
| `createTenant`, `updateTenant`, `deleteTenant` | admin |
| `listConsumers`, `getConsumer`, `listConsumerKeys`, `listOverrides`, `getOverride`, `listAccessLists`, `getAccessList` | all |
| `createConsumer`, `updateConsumer`, `deleteConsumer`, `issueKey`, `rotateKey`, `revokeKey`, `createOverride`, `updateOverride`, `deleteOverride`, `createAccessList`, `updateAccessList`, `deleteAccessList` | key-admin, admin |
| `exportIdentity` | admin |
| `listReplicas`, `listHeavyHitters` | all |
| `listAudit` | admin |

**Enforcement.** Each operation in `api/openapi.yaml` carries `x-spillway-roles: [...]`, the one source the server and (in M6) the UI read (UI-04). One strict-server middleware, which receives the operation ID, verifies the token and allows the call only if the token holds a listed role; everything else is denied: an operation without the extension, an empty list, or an unknown role name answers 403 to everyone. Tests: every operation in the spec has a non-empty list of known roles that includes `admin`; a table-driven test calls every operation with a token of each role and with none, and expects 401 without a token, 403 for roles not listed, and anything but 401/403 for roles listed (handlers backed by a stub store). Requests denied with 401 or 403 are logged (operation, `sub`, request ID) and are not audit records.

### Audit log (CP-13, SEC-08)

- **One record per successful mutation**, written in the same transaction as the change (so a rolled-back change leaves none): `at`, actor (`sub`, display name, roles), `action` (`<entity>.<verb>`: `route.create`, `endpoint.drain`, `config.import`, `config.publish`, `config.rollback`, `key.issue`, `key.rotate`, `key.revoke`, `access_list.delete`, …), `entity`, `entity_id`, `diff`, `note`, `request_id`, and `config_version` or `identity_version` when the change made one.
- **`diff`** is `{"before": …, "after": …}` with the entity's REST representation (`before` null on create, `after` null on delete; cascaded deletes list the removed key, override and access-list IDs). Publish, rollback and import store the changes list and versions instead of whole snapshots (`config_versions` has those). API keys appear only as `id`, `consumer_id`, `prefix`, `expires_at`, `revoked_at`, `rotated_from`: never the key, its secret or its hash. No record holds a token, a password or a client secret.
- **`note`**: the required note of publish and rollback (CP-02, UI-02), the import's optional note, an override's or access-list entry's own `note`, and the optional `{"note": …}` body of rotate and revoke. Working-set edits carry none; the publish note covers them.
- **Append-only**, enforced by Postgres. Migrations grant the app role `spillway_app` only `SELECT, INSERT` on `audit_log` (no `UPDATE`, `DELETE` or `TRUNCATE`) and add triggers `BEFORE UPDATE OR DELETE` (per row) and `BEFORE TRUNCATE` (per statement) that raise `audit_log is append-only`, which also stops the owner role unless a reviewed migration drops them. Integration test: as the app role, `UPDATE`, `DELETE` and `TRUNCATE` fail with a permission error; as the owner, they fail with the trigger's error.
- **Read**: `GET /api/v1/audit`, admin only, newest first, filters per ADR-0036.

### Postgres roles

- `spillway`, the owner (compose's `POSTGRES_USER`), runs `controlplane migrate` and owns every object.
- `spillway_app`, created by the first migration as `NOLOGIN` with the grants it needs (`SELECT, INSERT, UPDATE, DELETE` on the other tables, `USAGE` on sequences, `SELECT` on goose's version table, `SELECT, INSERT` on `audit_log`; default privileges for later tables). `controlplane serve` connects as it.
- `controlplane migrate` then sets `ALTER ROLE spillway_app WITH LOGIN PASSWORD '<verifier>'`, where the verifier is a SCRAM-SHA-256 verifier computed in the process (4,096 iterations, 16-byte random salt) from `SPILLWAY_CP_DATABASE_PASSWORD`, so the plaintext never reaches the server or its statement log. A Helm deployment on a managed Postgres whose owner lacks `CREATEROLE` is an M7 concern.

### Secrets (SEC-09)

- **Convention.** Every secret setting `X` can instead be given as `X_FILE`, a path to a file holding the value (one trailing newline trimmed). Both set: the process refuses to start. Neither set for a required secret: it refuses to start. An unreadable or empty file: it refuses to start. Files are read once at start (changing a password means a restart); TLS files are the exception and reload (above, ADR-0012).
- **The secrets** M5 reads:

  | Setting | Who | Dev value (`.env.example`) |
  | --- | --- | --- |
  | `SPILLWAY_CP_DATABASE_PASSWORD` | control plane (`serve`; `migrate` sets the role's password from it) | `dev-only-not-secret` |
  | `SPILLWAY_CP_MIGRATE_PASSWORD` | `controlplane migrate` (owner) | compose passes `${POSTGRES_PASSWORD}` |
  | `SPILLWAY_GATEWAY_REDIS_PASSWORD` (with the non-secret `SPILLWAY_GATEWAY_REDIS_USERNAME`) | gateway, Redis `AUTH` | unset: the compose cluster has no password |
  | `SPILLWAY_CI_CLIENT_SECRET` | `make verify-m5` and integration tests, for the `spillway-ci` client | `dev-only-not-secret` |
  | TLS and mTLS private keys | gateway, control plane | files from `make certs` |

  Spillway's processes hold no Keycloak secret: tokens are verified with public JWKS. `KEYCLOAK_ADMIN_PASSWORD` stays compose-only.
- **The compose Redis Cluster stays without a password.** Adding `requirepass` and `masterauth` to six nodes would change `redis-init`, every healthcheck and the M3 chaos scripts for a dev-only stack. The gateway's password path is tested instead by an integration test against a single-node Redis testcontainer with `requirepass`; M7's Helm values set it. ADR-0035 lists this for the user.
- **Never logged or stored.** Secrets are held in a type whose `String`, `LogValue` and `MarshalJSON` print `[redacted]`; settings are logged as set or unset only. Secrets never enter a snapshot, Postgres (beyond the role's verifier), an audit record, an error body or a span. The mtls-secrets task adds the test that loads each secret from env and from a file and asserts its value appears in no log line, error or settings dump.

## Consequences

- Only the stream CA can mint a gateway identity, and the browser-trusted mkcert CA cannot. Rotating the stream CA is the revocation story until a requirement asks for more.
- RBAC lives in the spec next to each operation, so a new endpoint without roles is unreachable and fails a unit test, rather than open.
- Editors cannot ship and publishers cannot edit, unless one person holds both roles; separation of duties is a role assignment, not code.
- The audit table survives application bugs and SQL injection through the app role; only a migration can change it.
- M7 must solve per-replica gateway certificates (D7) and the migrate Job's role privileges.

## Alternatives considered

- **The browser-trusted mkcert CA for the stream** — every local mkcert certificate, the gateway's own server certificate included, would be one key-usage check away from a gateway identity.
- **Bearer tokens on the gRPC stream instead of client certificates** — SEC-02 asks for mTLS, and D7 for the certificate name.
- **Roles hard-coded in Go next to handlers** — easy to miss an operation; the spec extension is checked in one place and feeds the UI.
- **One Postgres role for migrate and serve** — the app could then drop the audit triggers.
- **Sending the app role's password in plaintext `ALTER ROLE`** — it would reach the server's statement log when DDL logging is on.
- **Docker or Kubernetes secrets only (files only)** — compose's dev flow uses env; `_FILE` keeps both.
- **Password on the compose Redis Cluster now** — churn in M3's dev stack for no requirement in compose.
