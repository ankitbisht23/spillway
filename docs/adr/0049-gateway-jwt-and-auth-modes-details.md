# 0049. Gateway JWT and auth modes: details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** GW-17, GW-18, GW-19, GW-20, SEC-03, SEC-04, OB-02

## Context

M5's jwt task builds ADR-0039's bearer tokens in the gateway, the four auth modes and identity resolution for tokens, reusing `internal/auth`, which the rbac-audit task built for the control plane (ADR-0043). ADR-0039 fixes the settings, every check a token must pass, the JWKS cache, the outage rules, the auth-mode table and the answers. Building it left these open:

- what a gateway does with a `jwt` or `either` route in static mode, whose identity file may not give consumers JWT subjects (ADR-0045);
- which answer an `Authorization` header gets when it is not one `Bearer` token: a missing credential or an invalid one;
- what the `authn` span reports now that a credential can also be missing or uncheckable (ADR-0030 lists `anonymous`, `consumer`, `invalid`; ADR-0039 names the outcomes `ok`, `missing`, `invalid`, `unavailable`);
- whether the key fetch follows HTTP redirects, given SEC-04's "JWKS fetched … from the configured JWKS URL only";
- what a token fills in the key templates, and where the plan of GW-19 goes before the plans task uses it.

## Decision

- **Token routes need managed mode.** A config with a `jwt` or `either` route fails to apply on a gateway without `SPILLWAY_GATEWAY_JWT_ISSUER` (ADR-0039's message) and on a static gateway: `route "<name>" uses auth_mode jwt, which needs managed mode: only the control plane gives consumers JWT subjects`. A static gateway would otherwise answer every valid token 401, since its identity file cannot name subjects (ADR-0045). Static mode keeps `none` and `api_key`. As with Redis policies (ADR-0021), a static gateway refuses the file at start and keeps its last good config on a reload; a managed one reports the rejected version (ADR-0038).
- **`Authorization` is a token attempt.** On a route that reads it, any `Authorization` header counts as a credential: a scheme other than `Bearer` (any case), an empty token or two headers are 401 `invalid token`, exactly as the control plane answers (ADR-0043). Only its absence is "bearer token required" (`jwt`) or "API key or bearer token required" (`either`).
- **`spillway.authn.result`** keeps ADR-0030's three values and adds two: `consumer` (ADR-0039's `ok`), `anonymous` (a `none` route with no key), `invalid`, `missing` (a required credential not sent) and `unavailable` (no key set ever fetched, the 503). Dashboards and traces from M4 keep their meaning.
- **No redirects.** `internal/auth` fetches keys with `auth.NewClient`: the 5 s timeout and no redirect following, so a 3xx is a failed fetch and the last good keys stay. The control plane's verifier uses the same client. `SPILLWAY_GATEWAY_JWT_CA` (PEM, read once at start; a file without a certificate stops the start) is added to the system roots for that client only.
- **Settings** are all or none: `SPILLWAY_GATEWAY_JWT_JWKS_URL`, `_AUDIENCE` and `_CA` without the issuer stop the start, as do the issuer without the URL or the audience, and an `http://` URL without dev mode (SEC-04). Readiness never waits for the first fetch. Compose sets the issuer `http://localhost:8180/auth/realms/spillway`, the JWKS URL `http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/certs` and the audience `spillway-gateway` on every gateway; static gateways fetch the keys and use them for nothing.
- **Identity from a token** (GW-19): the subject's consumer fills `{consumer}` and `{tenant}` (decimal IDs, ADR-0048) and the tenant metric label (its name), exactly as its key would. `{api_key}` stays unfilled for a token, so a key-keyed policy does not apply to token traffic, and a key that is sent on a `jwt` route, not read, never fills it either. Each resolved consumer carries its plan ID (its own `plan_id`, else its tenant's), which the plans task uses for limits (ADR-0041); nothing reads it before then.
- **Subjects are unique in a compiled identity**: two consumers with one `jwt_subject` fail to compile, named by consumer ID (the database's `UNIQUE (jwt_subject)` already prevents it). Errors and logs never name a subject.

## Consequences

- A static compose gateway cannot try a token route; the compose integration test runs gateway-1 in managed mode, as `make verify-m5` will.
- A misconfigured `Authorization` header (`Basic …`) on a token route is told its token is invalid, not that one is required.
- The control plane no longer follows a JWKS redirect; the compose Keycloak serves its keys without one.

## Alternatives considered

- **Accepting token routes in static mode** — every token would be 401 with no hint why; or `jwt_subject` in identity files, which ADR-0045 refuses.
- **A non-`Bearer` header as "required"** — the control plane already answers `invalid token`; two answers for one header would be a needless difference.
- **ADR-0039's four outcome names in place of ADR-0030's** — would rename values M4's traces and dashboards use.
- **Following redirects** — keys could then come from a URL nobody configured.
