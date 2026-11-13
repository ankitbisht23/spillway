# 0043. RBAC and audit: where tokens are checked, test logins and audit details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** CP-10, CP-11, CP-12, CP-13, SEC-08, SC-03

## Context

The rbac-audit task builds ADR-0040's bearer tokens, RBAC and audit log on schema-api's 42 operations, and adds `GET /api/v1/audit`. Building it showed one sentence of ADR-0040 that cannot hold as written, and left details open:

- ADR-0040 puts the token and role check in "one strict-server middleware, which receives the operation ID". The code oapi-codegen generates binds path, query and header parameters and decodes the JSON body *before* the strict middlewares run: a request without a token but with a bad ID, a missing `If-Match`, a non-JSON body or a body over 1 MiB is answered 400, 428, 415 or 413 and never reaches the middleware. ADR-0040's own test ("401 without a token" on every operation) would fail, and anonymous callers would get answers about their requests.
- The RBAC tests need a token for each role from Keycloak, but ADR-0040's dev realm has no client that can log a user in without a browser: `admin-ui` is a public PKCE client, and the confidential clients use client credentials.
- What an audit record holds for a drain, a cascading delete and a note on delete; how the audit filters combine; which index serves each.
- `oasdiff breaking` rightly reports that every operation now refuses anonymous callers (42 errors of `api-security-anonymous-access-removed`), which `make lint-api` fails on.
- SC-03 (M8) says the fault switches are for the "Operator role only"; the CP-12 wording the user approved (P1) has no operator role.

## Decision

### Tokens and roles are checked before the request is read

- The control plane's handler is a `ServeMux` built from the embedded spec: one pattern per operation (`METHOD /api/v1/<path>`), each wrapped by one guard that authenticates the bearer token, checks the operation's `x-spillway-roles`, and only then hands the request to the generated server (body cap, strict decoding, parameter binding, handlers). The roles still come only from the spec, and the guard is still the one place they are enforced; this changes only *where* in the chain it sits, and supersedes ADR-0040's "one strict-server middleware" sentence. Paths the spec does not have reach only the 404, without a token check (the spec is public).
- Answers: no `Authorization` header is 401 `a bearer token is required` with `WWW-Authenticate: Bearer realm="spillway"`; a token that fails a check, a scheme other than `Bearer` (any case), an empty token or two `Authorization` headers are 401 `invalid token` with `error="invalid_token"`; a token without a listed role is 403 `forbidden: this operation needs one of the roles …`; no key set ever fetched is 503 `token keys unavailable: try again` with `Retry-After: 1`. A token without `sub` is invalid (the audit record needs an actor).
- The actor is `sub`, `preferred_username` as the display name (`sub` when absent), and the Spillway roles among `realm_access.roles`, in the order viewer, editor, publisher, key-admin, admin; other roles (`offline_access`, `default-roles-spillway`) are dropped.
- 401 and 403 answers are logged at WARN with the operation ID, the status, the `sub` (403 only) and the request ID; never the token or the display name.
- `internal/auth` parses compact JWS with go-jose's `jose.ParseSignedCompact` and checks the registered claims itself, not with go-jose's `jwt` package: `jwt.Claims.ValidateWithLeeway` refuses an `iat` in the future, which ADR-0039 says is not checked, and accepts a token without `exp`, which ADR-0039 refuses. go-jose was already in the module graph (v4.1.4, indirect); it is now a direct dependency at ADR-0039's v4.1.7.

### Dev realm

- A new confidential client **`rbac-tests`** (secret `dev-only-not-secret`, password grant only, audience `spillway-controlplane`) logs the test users in for the RBAC integration test. It exists only in the compose realm and is never part of a deployment realm; a password grant on the public `admin-ui` client was the alternative.
- Users have fixed IDs: `test-viewer` … `test-admin` are `0b5e7c1a-5f1d-4c3e-9a00-00000000000N` (N = 1 … 5 in that order); the service accounts of `spillway-ci` and `gateway-tests` end in `…c1` and `…c2`. A token minted from the imported realm carries `sub` and `realm_access.roles` with Keycloak's default client scopes, so the realm declares none (ADR-0040 asked to check this).
- Keycloak imports a realm only when it does not have it; after a change to `realm.json`, `docker compose up -d --force-recreate keycloak` (docs/setup.md). CI starts from a clean stack.
- Compose starts the control plane after Keycloak is healthy, so its first key fetch succeeds instead of being retried 30 s later; readiness still does not wait for the keys.

### Audit records

- Entities and actions: `upstream`, `route`, `policy`, `plan`, `tenant`, `consumer`, `access_list`, `override`, with `<entity>.create`, `.update` and `.delete`. A drain is action `endpoint.drain` on entity `upstream` with the upstream's ID, so an upstream's history (`?entity=upstream&entity_id=…`) includes its drains; its diff is the upstream before and after. `entity_id` is the decimal ID.
- The diff's `before` and `after` are built by the same conversion as the REST answers, so a create's `after` equals the `POST` answer's body. A consumer's delete adds `removed: {"api_key_ids", "override_ids", "access_list_ids"}`; the access-list IDs include entries on the consumer's keys.
- `note`: an access-list entry's or override's own `note` on create and update; none on delete (the deleted row, note included, is in `before`).
- `identity_version` is set on identity changes; working-set edits set neither version (publish sets `config_version`).
- A refused change (409, 412, 422, a failed constraint) leaves no record: the record is written last in the change's transaction.
- Filters (ADR-0036): `entity_id` needs `entity` (400 otherwise); `since` is inclusive, `until` exclusive, and `until` not after `since` is 400. The store picks one of four queries: a backward scan of the primary key; `audit_log_entity` with `entity` and `entity_id`; `audit_log_entity`'s leading column with `entity` only, which sorts that entity's rows (ADR-0036 lists no `(entity, id)` index; fine at control-plane sizes); `audit_log_actor` with `actor_sub` only.
- Append-only (ADR-0040): migration 00002 takes back the `UPDATE` and `DELETE` that 00001's default privileges give the app role on new tables, grants `SELECT, INSERT`, and adds the triggers, which raise `audit_log is append-only` (SQLSTATE `P0001`), distinct from the app role's permission error (`42501`).

### oasdiff

`api/oasdiff-ignore.txt` lists exactly the 42 `api-security-anonymous-access-removed` changes, with the reason (ADR-0035's T2 interim, accepted by the user at the M5 checkpoint), and `make lint-api` passes it to `oasdiff breaking --err-ignore`. Each line names one method and path; the lines match only against a base whose operations allow anonymous access, so they match nothing once this change is on main, and a later task may delete them. Any other breaking change still fails.

### Proposed design-text change (for the user)

- **(P12) SC-03** (M8): "Operator role only" names a role CP-12 no longer has. Proposed: "Admin role only". Until the user decides, M8 builds nothing for it; nothing in M5 depends on it.

## Consequences

- An anonymous caller learns nothing about its request but 401; a caller with the wrong role nothing but 403. Validation answers go only to callers allowed to make the change.
- Every operation added later is guarded by its spec entry with no code change, and the unit test pins every operation's roles to ADR-0040's matrix.
- The dev realm has one more client than ADR-0040 lists; it can log in only the realm's own test users.

## Alternatives considered

- **The strict-server middleware, as ADR-0040 says** — runs after binding and decoding, so anonymous requests get 400/413/415/428.
- **Authentication in front, roles in the strict middleware** — still answers a viewer's `DELETE` without `If-Match` 428 instead of 403.
- **Dropping `security` from the spec to keep oasdiff quiet** — hides a real contract change from the spec and the UI client.
- **A password grant on `admin-ui`** — a public SPA client that accepts passwords.
- **Per-role tokens from a test issuer only** — would not show that the realm's role claims reach the control plane; the testcontainer tests use a test issuer, the compose test uses Keycloak.
