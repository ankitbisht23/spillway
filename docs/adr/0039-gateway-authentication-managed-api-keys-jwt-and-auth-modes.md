# 0039. Gateway authentication: managed API keys, JWT and auth modes

- **Status:** Accepted
- **Date:** 2026-11-13
- **Requirement IDs:** GW-17 – GW-20, CP-15 – CP-17, CP-20, SEC-03, SEC-04, OB-01, OB-03, NFR-09

## Context

M2 gave the gateway static API keys from a YAML file (ADR-0017): a 12-byte prefix, a SHA-256 of the whole key, `X-API-Key` on every route, 401 `{"error":"invalid API key"}`. M5 moves keys to the control plane (CP-15 – CP-17), adds JWT validation (GW-18, SEC-04), identity resolution to tenant and plan (GW-19) and a per-route auth mode (GW-20). The user fixed D1 (format `spw_<8-char prefix>_<32-byte secret, base62>`, prefix plus SHA-256 of the secret stored, shown once), D2 (rotation grace 24 h by default, set per rotation, at most 7 days; revocation immediate through identity) and D8 (RS256 and ES256 only; JWKS cached 5 min; an unknown `kid` refreshes at most once per 30 s). Open:

- the exact key grammar, generation and lookup, and the issue, rotate and revoke endpoints;
- how a rotation's expiry reaches gateways, and how managed keys replace the static file;
- where JWT settings live, every check a token must pass, and what happens when JWKS is unreachable;
- how a token's claims find a consumer, and what each auth mode does with each credential;
- the exact 401 and 403 answers.

## Decision

### Managed API keys (CP-15, D1)

- **Grammar**: `spw_` + 8 prefix characters + `_` + 43 secret characters, 56 characters in all, from the base62 alphabet `0-9A-Za-z`. Regular expression: `^spw_[0-9A-Za-z]{8}_[0-9A-Za-z]{43}$`.
- **Generation** (`crypto/rand` only): the secret is 32 random bytes (256 bits, CP-15) written as one big-endian number in base62, left-padded with `0` to 43 characters (62^43 > 2^256). The prefix is a uniformly random number below 62^8 (`rand.Int`), written in base62 and padded to 8 characters (about 47.6 bits, public). The prefix must be unique (`UNIQUE (prefix)`); on a collision the control plane draws a new one, up to 3 times, then answers 500.
- **Stored**: the prefix and `hash` = SHA-256 of the secret's 43 ASCII characters (32 bytes, `bytea`). The secret has 256 bits of entropy, so a fast hash is safe (GW-17) and no salt or KDF is needed. The full key exists only in the issue or rotate response.
- **Endpoints** (key-admin or admin, ADR-0040):
  - `POST /api/v1/consumers/{id}/keys`, body `{"expires_at": "<RFC 3339>"}` (optional, in the future): **201** with the key's listing fields plus `"key": "spw_…"`.
  - `GET /api/v1/consumers/{id}/keys`: listing fields only — `id`, `consumer_id`, `prefix`, `created_at`, `expires_at`, `revoked_at`, `rotated_from`; never the key or its hash (CP-15). Revoked and expired keys stay listed.
  - `POST /api/v1/keys/{id}/rotate`, body `{"grace": "24h"}` (optional; Go duration from `0s` to `168h`, default `24h`, D2): one identity change that issues a new key for the same consumer (`rotated_from` = the old key, no expiry) and sets the old key's `expires_at` to the earlier of its current value and now + grace. **200** `{"old": <listing>, "new": <listing + key>}`. Rotating a revoked or expired key is **409**.
  - `POST /api/v1/keys/{id}/revoke`: sets `revoked_at` = now, one identity change (CP-17). Revoking a revoked key is **200** with no change, no new identity version and no audit record. No `If-Match` on rotate or revoke (ADR-0036).
- **Never exposed**: the issue and rotate responses carry `Cache-Control: no-store`; the key is never logged, traced, audited or stored. Audit records name keys by `id` and `prefix`. A gitleaks rule for the key grammar is added to `.gitleaks.toml` as a detection rule (it adds findings; the allowlist is untouched), and tests make their keys at run time.

### How keys reach and work in the gateway (GW-17, SEC-03, D2, NFR-09)

- **Identity snapshot.** Each non-revoked, non-expired key travels in the managed `Identity` (ADR-0038) with `key_format: "spw1"`, its `id`, `consumer_id`, the prefix, the hash as 64 lower-case hex characters, and `expires_at`. Revocation removes the key from the next identity snapshot, which every replica applies within NFR-09's 5 s (ADR-0037). Expiry needs no change: the gateway compares `expires_at` with its own clock at each lookup, so a rotated key stops working when its grace ends.
- **Lookup** in managed mode: the single `X-API-Key` value must match the grammar (else 401); its 8 prefix characters find the entry in a map; SHA-256 of its 43 secret characters is compared with the stored hash using `crypto/subtle.ConstantTimeCompare` (SEC-03); an `expires_at` at or before now fails. Any failure is the same 401 `{"error":"invalid API key"}`, so an answer reveals nothing about which check failed.
- **Managed replaces static.** In managed mode the identity comes only from the control plane, and `SPILLWAY_GATEWAY_IDENTITY` must be unset (ADR-0038). Static mode keeps ADR-0017's file and key rules unchanged (`key_format: ""`). The two never mix in one gateway.
- **Logs** keep ADR-0017's `key_hash` (the first 12 hex characters of SHA-256 of the presented key, valid or not). The prefix, though public, is not logged.

### JWT (GW-18, SEC-04, D8)

- **Library**: `github.com/go-jose/go-jose/v4` **v4.1.7** (exact; released 2026-10-30), in `internal/auth`, shared with the control plane's bearer-token checks (ADR-0040). It parses compact JWS with an explicit algorithm list, so `none` and HS256 can never be accepted.
- **Settings**, from the environment (ADR-0008: a trust anchor in the snapshot would let the admin API change it):

  | Variable | Meaning | Default |
  | --- | --- | --- |
  | `SPILLWAY_GATEWAY_JWT_ISSUER` | the browser-facing issuer `iss` must equal; dev `http://localhost:8180/auth/realms/spillway` | none: JWT off |
  | `SPILLWAY_GATEWAY_JWT_JWKS_URL` | where keys are fetched; dev `http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/certs` (ADR-0004) | required with the issuer |
  | `SPILLWAY_GATEWAY_JWT_AUDIENCE` | a value `aud` must contain | required with the issuer; dev `spillway-gateway` |
  | `SPILLWAY_GATEWAY_JWT_CA` | optional PEM bundle trusted for the JWKS URL, added to the system roots (M7's Keycloak cert from a Secret) | none |

  An `http://` issuer or JWKS URL stops the gateway at start unless `SPILLWAY_GATEWAY_DEV_MODE` is `true` (SEC-04). A snapshot with a route whose `auth_mode` is `jwt` or `either` fails to apply when the issuer is unset: `route "<name>" uses auth_mode jwt, but SPILLWAY_GATEWAY_JWT_ISSUER is not set` (the last good snapshot stays; ADR-0038 reports it).
- **A token is accepted only if all hold**:
  1. `Authorization` has exactly one value, scheme `Bearer` (case-insensitive), and a compact JWS.
  2. The header's `alg` is `RS256` or `ES256`; `kid` is present; a `crit` header is rejected; `jku`, `x5u` and embedded `jwk` headers are ignored — keys only ever come from the configured URL.
  3. The JWKS has a key with that `kid` whose type fits the algorithm (RSA of at least 2,048 bits for RS256, EC P-256 for ES256), with `use` absent or `sig`, and `alg` absent or equal; the signature verifies with it.
  4. `iss` equals the configured issuer exactly.
  5. `aud` (a string or an array) contains the configured audience.
  6. `exp` is present and now < `exp` + 60 s; `nbf`, if present, ≤ now + 60 s (60 s leeway). `iat` is not checked.
  7. `sub` is present and not empty.
- **JWKS cache** (D8). Fetched at start in the background (readiness never waits for it) and every 5 min after: `GET` with a 5 s timeout, at most 1 MiB, status 200, JSON. Unusable keys (wrong type, `use: enc`) are dropped; a response with no usable key counts as a failed fetch. A failed fetch keeps the last good key set, is logged (at most once a minute) and is retried after 30 s. **Unknown `kid`**: if the last fetch started more than 30 s ago, one fetch runs (single-flight: concurrent requests share it) and the request waits for it up to 2 s; within 30 s of the last fetch, the request is answered 401 at once. So the gateway fetches at most twice a minute however many bad tokens arrive.
- **JWKS unreachable.** With a last good key set, tokens keep verifying against it, with no age limit. With no key set ever fetched (cold start while Keycloak is down), a request that needs a token is answered **503** `{"error":"token keys unavailable"}` with `Retry-After: 1`: an honest "try again", not a 401 that tells a client its valid token is bad. It counts as `origin="gateway"`, `reason="none"`, so NFR-11 counts it as a gateway failure.
- **Claims → identity** (GW-19). `sub` is matched against consumers' `jwt_subject` (unique, identity data set through the consumer API). The consumer gives tenant and plan exactly as a key does. A valid token whose subject has no consumer is 401 `{"error":"invalid token"}`. Keycloak's `sub` for a client-credentials token is its service-account user's ID; the dev realm pins those IDs so they survive re-imports (ADR-0040).

### Identity resolution (GW-19)

A key or token resolves to a consumer, its tenant (`consumers.tenant_id`) and its plan (`consumers.plan_id`, or the tenant's when null). In managed mode the key template placeholders `{consumer}` and `{tenant}` are filled with their decimal IDs (stable across renames and unique across tenants; consumer names are unique only within a tenant) and RL-13's hash tag is `{t:<tenant id>}`. Static mode keeps names. The `tenant` metric label (OB-01) stays the tenant's name. A suspended consumer or tenant, and the access lists, are ADR-0041's.

### Auth mode per route (GW-20)

`Route.auth_mode`: `none`, `api_key`, `jwt` or `either`; unset means `none`.

| `auth_mode` | `X-API-Key` sent | `Authorization: Bearer` sent | neither |
| --- | --- | --- | --- |
| `none` | resolved; invalid → 401 | not read | anonymous |
| `api_key` | resolved; invalid → 401 | not read | 401 `API key required` |
| `jwt` | not read | verified; invalid → 401 | 401 `bearer token required` |
| `either` | resolved (the bearer is then not read); invalid → 401 | verified when no key was sent | 401 `API key or bearer token required` |

- **`none` still resolves a key that is sent.** That is what every route does today (ADR-0017), so configs without `auth_mode` keep their meaning and M2–M4's checks are unchanged: anonymous traffic passes, keyed traffic is limited per consumer, and a bad key is still 401. A route that must ignore keys entirely has no requirement yet. ADR-0035 lists this reading of GW-20 for the user.
- **`either` prefers the key**: one credential decides, with no "both must agree" rule; the bearer header then passes upstream untouched, like any header the gateway does not read.
- Headers the gateway did not read pass to the upstream unchanged, and so do the ones it did (as today for `X-API-Key`).

### Answers

| Status | Body (`Content-Type: application/json`) | When |
| --- | --- | --- |
| 401 | `{"error":"API key required"}` | `api_key` route, no key |
| 401 | `{"error":"invalid API key"}` | a key that is malformed, unknown, revoked or expired, or has a wrong secret; two `X-API-Key` values |
| 401 | `{"error":"bearer token required"}`, `WWW-Authenticate: Bearer realm="spillway"` | `jwt` route, no bearer token |
| 401 | `{"error":"invalid token"}`, `WWW-Authenticate: Bearer realm="spillway", error="invalid_token"` | any failed check above, or a subject without a consumer |
| 401 | `{"error":"API key or bearer token required"}`, `WWW-Authenticate: Bearer realm="spillway"` | `either` route, neither |
| 403 | `{"error":"forbidden"}` | deny list, suspended consumer or tenant (ADR-0041) |
| 503 | `{"error":"token keys unavailable"}`, `Retry-After: 1` | a token is needed and no JWKS was ever fetched |

All are gateway answers (`origin="gateway"`, `reason="none"`), always access-logged (ADR-0013), and carry no detail about the credential. Authentication keeps its place in the pipeline (after phase-1 policies, ADR-0017) and its `authn` span (OB-02); a span records the outcome (`ok`, `missing`, `invalid`, `unavailable`), never a key, token, claim or subject.

### No credentials in logs (SEC-03)

Keys, secrets, tokens, token claims and `Authorization` values never appear in the access log, `slog` output, spans, audit records or error bodies of the gateway or the control plane. Each task that handles them adds a test that sends a known key or token through the path it built and asserts that neither the value nor its secret part appears in any of those outputs (the captured log handler, an in-memory span exporter, the audit rows, the response body).

## Consequences

- A leaked key is identifiable by its prefix in the key listing; a pasted key is caught by the gitleaks rule.
- Rotation needs no timer anywhere: the old key's `expires_at` does the work in every gateway, on gateway clocks (seconds of skew are irrelevant against a 24 h grace).
- A JWKS outage leaves verification working with the last keys for as long as it lasts, including keys the issuer may have retired; the 5 min refresh resumes when it is back.
- Verifying RS256 or ES256 costs tens of microseconds per request; a verified-token cache is a candidate for M7's pprof pass, not M5.
- In managed mode, Redis keys and local limiter keys use IDs, so counters reset once when a deployment moves from static to managed config (as the design already says for policies).

## Alternatives considered

- **Hash of the whole key** (ADR-0017's static rule) — D1 says the secret; the prefix is public and adds nothing to the hash.
- **bcrypt or Argon2** — slow by design and pointless for a 256-bit secret; GW-17 asks for SHA-256 and O(1) lookups.
- **JWT settings in the config snapshot** — the admin API could swap the trust anchor.
- **A token's own tenant and plan claims** — identity would then live in Keycloak too, and blocking or re-planning a consumer would need an IdP change; `sub` → consumer keeps the control plane authoritative.
- **401 when JWKS was never reachable** — tells clients with valid tokens their tokens are bad.
- **Answering an unknown `kid` at once and refreshing in the background** — the first requests after an issuer key rotation would all fail.
- **`none` ignoring keys** — would change every existing config's meaning.
- **`either` requiring both to agree, or rejecting both** — more rules for a case clients can avoid by sending one.
