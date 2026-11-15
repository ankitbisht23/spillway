# 0058. Admin UI: session and roles

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** UI-04, UI-08, CP-11, CP-12, SEC-08, SC-09

## Context

UI-08 asks for the OIDC code flow with PKCE, the access token in memory (never localStorage) with silent refresh, and bearer tokens on the API so there is no cookie CSRF surface. UI-04 asks for actions hidden or disabled per RBAC role, enforced again on the server. The user set D3 (a public client, code flow with PKCE S256, tokens in memory only, no localStorage, sessionStorage or cookies of our own, silent refresh with the refresh token, a reload means logging in again, which Keycloak's SSO session turns into a redirect) and D4 (roles from a server endpoint, not by decoding the JWT; actions a role cannot do are disabled with a reason; screens a role cannot open at all are hidden; the server is the only enforcement). D3 names the client `spillway-ui` and asks this ADR to decide between it and the realm's existing `admin-ui`.

What exists (ADR-0040, ADR-0043): the dev realm's `admin-ui` client is public, standard flow only (no implicit, no direct grants), PKCE `S256`, with redirect URIs `http://localhost:5173/*` and `http://localhost:8080/*`, the same web origins, post-logout redirect URIs `…/*`, and an audience mapper adding `spillway-controlplane`. The realm sets no lifetimes, so Keycloak's defaults apply. The control plane verifies bearer tokens itself (JWKS, `iss`, `aud`), takes roles from `realm_access.roles`, and enforces `x-spillway-roles` per operation before reading a request.

Keycloak 26.8.1 was checked on 2026-11-15 against a throwaway realm on the compose Keycloak (created and deleted through the admin API; the `spillway` realm was not touched): with `revokeRefreshToken: true` and `refreshTokenMaxReuse: 0`, two code-flow logins through one SSO session (the second answered by the SSO cookie without a form, as a second tab is) each got their own refresh token; each chain refreshed twice independently (four 200s), and reusing the first chain's old token answered 400 `invalid_grant` "Stale token". Tokens had `expires_in` 300 and `refresh_expires_in` 1800. Keycloak answers CORS for `http://localhost:8080` on discovery and on the token endpoint's preflight.

## Decision

### The client: keep `admin-ui`

- The UI uses the existing **`admin-ui`** client; there is no `spillway-ui`. It is already the public PKCE client D3 describes, and the design names it: SC-09 (`make demo-share` adds the public URL to "the `admin-ui` client's redirect URIs and web origins"), ADR-0004, ADR-0040 and docs/setup.md. A rename would change requirement text and three documents to gain a name; a second client would leave the first one unused but valid. Recorded as a decision taken for the user (ADR-0056).
- **Realm changes** (`deploy/compose/keycloak/realm.json`, shell task; dev values only):
  - `admin-ui`: `redirectUris` exactly `http://localhost:8080/oidc/callback` and `http://localhost:5173/oidc/callback` (no wildcards: an exact match is what the OAuth security BCP asks for, and nothing else on these origins should ever receive a code); `webOrigins` `http://localhost:8080` and `http://localhost:5173` (unchanged); `attributes."post.logout.redirect.uris"` `http://localhost:8080/signed-out##http://localhost:5173/signed-out`; `pkce.code.challenge.method` `S256` (unchanged, which makes PKCE required); `standardFlowEnabled: true`, `implicitFlowEnabled: false`, `directAccessGrantsEnabled: false`, `serviceAccountsEnabled: false`, `consentRequired: false`, stated explicitly.
  - Realm: `accessTokenLifespan: 300` (5 min), `ssoSessionIdleTimeout: 1800` (30 min), `ssoSessionMaxLifespan: 36000` (10 h), `revokeRefreshToken: true`, `refreshTokenMaxReuse: 0`. The first three are Keycloak's defaults, now written down; rotation is new. Only `admin-ui` gets refresh tokens in practice: `spillway-ci` and `gateway-tests` use client credentials (no refresh token), and the `rbac-tests` password grant never refreshes.
  - **Dev users**: the existing `test-viewer`, `test-editor`, `test-publisher`, `test-key-admin`, `test-admin` (one role each, password `dev-only-not-secret`, email, first and last name set, so Keycloak asks for no profile update at login). No new users; flows that need editing and publishing use `test-admin`.
  - After the change, `docker compose up -d --force-recreate keycloak` (docs/setup.md, ADR-0043); CI starts clean.
- SC-09's wording follows the exact URIs: M8 adds `<url>/oidc/callback`, `<url>/signed-out` and the origin `<url>` instead of `<url>/*`. The change is proposed in ADR-0056 (P14).

### Login: code flow with PKCE (UI-08)

Library: **oauth4webapi 3.8.8** (no dependencies; discovery, PKCE, state and nonce checks, ID token claim validation; it stores nothing itself). oidc-client-ts keeps its state in Web Storage and renews through iframes; keycloak-js couples the UI to Keycloak's adapter and its iframe session checks; a hand-written client would re-implement the checks this library already gets right.

1. **Boot.** `main.tsx` fetches `/api/ui-config` (ADR-0057), then discovery from `<issuer>/.well-known/openid-configuration`. Nothing renders but a "Signing in…" splash until the session is ready.
2. **Start.** With no session in memory, and not on `/oidc/callback` or `/signed-out`, the app creates `state` and `nonce` (32 random bytes each from `crypto.getRandomValues`, base64url) and a PKCE `code_verifier` (32 random bytes, base64url, so 43 characters) with its S256 `code_challenge`, writes the **pending record** (below), and navigates with `location.assign` to the authorization endpoint: `response_type=code`, `client_id=admin-ui`, `redirect_uri=<origin>/oidc/callback`, `scope=openid`, `state`, `nonce`, `code_challenge`, `code_challenge_method=S256`.
3. **Callback** (`/oidc/callback`). First, before anything else, the app reads `code`, `state`, `iss` and `error` from the URL into memory and calls `history.replaceState(null, "", "/oidc/callback")`, so the code and state leave the address bar and the history entry at once. It then reads **and deletes** the pending record; a missing or expired record, a different `state`, an `iss` other than the configured issuer, or an `error` parameter end on an error screen ("Sign-in could not be completed") with a "Sign in again" button and nothing retried automatically. Otherwise it exchanges the code (oauth4webapi checks the ID token's `iss`, `aud`, `exp` and `nonce`), keeps the tokens in memory, and replaces the history entry with the return path (`router.navigate({ to, replace: true })`).
4. **Return path.** The pending record's `return_to` is `location.pathname + location.search` at step 2. It is used only if `new URL(return_to, location.origin)` has the same origin, starts with a single `/` (not `//` or `/\`), and is not `/oidc/callback` or `/signed-out`; otherwise `/overview`. It is never taken from a URL parameter, so there is no open redirect.

**The pending record** is the one thing that must survive the navigation to Keycloak and back, and memory does not. It lives in **sessionStorage** under `spillway.oidc.pending` as `{state, nonce, code_verifier, return_to, created_at}`, written immediately before the redirect, deleted at the start of the callback, and ignored (and deleted) when older than 10 minutes. This is acceptable because it holds no token: the verifier is single-use, bound to one authorization request, and worthless without the code that only the exact redirect URI receives; sessionStorage is per tab and never sent to a server; the record lives for the seconds of a login; and script that could read it could equally drive the in-memory session, so it adds no exposure. The Playwright leak checks (ADR-0061) assert that sessionStorage is empty once the app has loaded, and hold any token to the "nowhere in storage" rule. A popup login, the only way to keep the verifier in memory, is blocked by popup blockers and breaks the reload story; putting the verifier in `state` would defeat PKCE.

### Tokens in memory, refresh and expiry

- The access, refresh and ID tokens and the access token's expiry live in one module-level object in `src/auth/session.ts`: no Web Storage, no cookies of our own, no IndexedDB, not in React state or context (components get `useSession()` with the user's display data and a `signOut()`, never a token), not in the TanStack Query cache, never in a URL, never logged. Only `authFetch` (API calls) and the stream client (ADR-0059) read the access token.
- **Expiry** is tracked from the token response's `expires_in` against `performance.now()` at receipt: the browser's clock is used only for durations, never compared with the token's `exp` (the UI does not decode the access token, D4).
- **Silent refresh**: a timer at receipt + max(`expires_in` − 60 s, `expires_in` / 2), so 240 s for a 300 s token, posts `grant_type=refresh_token` to the token endpoint (CORS allowed by the client's web origins). Refresh-token rotation is on (realm above): every refresh returns a new refresh token, which replaces the old one, and the old one is dead. One refresh at a time per tab (single-flight); `authFetch` also refreshes first when fewer than 30 s remain (a tab whose timers were throttled in the background), and on `visibilitychange` to visible.
- **A failed refresh.** A network error or a 5xx is retried after 5, 10, 20, … s until the access token expires. `invalid_grant` (session ended or idle too long, refresh token stale or reused) ends the session at once. An ended session shows a modal, "Your session ended", with "Sign in again", which runs the login above with the current path as the return path; Keycloak's SSO cookie, if still valid, makes that a redirect without a form. Unsaved edits are lost; dirty forms ask through `beforeunload` before any navigation away.
- **Each refresh reconnects the stream** with the new token (D5, ADR-0059).

### Reload, tabs and logout

- **A full reload** starts at step 1 with empty memory: the app redirects to Keycloak, which answers from the SSO session without a form and returns to the same path. In-memory state (forms, scroll) is lost; URLs carry filters and cursors, so lists come back as they were.
- **Several tabs** each run their own login and hold their own tokens and refresh chain; Keycloak 26.8.1 keeps the chains apart under rotation (measured above), so tabs never invalidate each other. Tabs share no tokens. They share one message: on sign-out a tab posts `"signed-out"` (no token, no user data) on a `BroadcastChannel("spillway-session")`, and every other tab drops its tokens and shows `/signed-out`. A tab that misses the message finds out at its next refresh (`invalid_grant`).
- **Logout** (`signOut()`): the tab drops its tokens, broadcasts, and **POSTs** a form (built with DOM APIs, no inline script) to the end-session endpoint with `id_token_hint`, `client_id=admin-ui` and `post_logout_redirect_uri=<origin>/signed-out`. A POST keeps the ID token out of every URL, history entry and log line that a GET's query string would reach; Keycloak supports RP-initiated logout by POST, ends the SSO session (which kills every tab's refresh token) and redirects to `/signed-out` without a confirmation page because the hint is valid. The CSP's `form-action` allows exactly this (ADR-0057).
- `/signed-out` never starts a login by itself: it shows "Signed out" and a "Sign in" button.
- Access tokens already issued stay valid until they expire (at most 5 minutes): the control plane verifies JWTs locally (ADR-0040), as every bearer-token API does. A tab still holding one after a logout elsewhere is told by the broadcast; a stolen one is bounded by the 5 minutes.

### 401 and 403

- **401** from any API call: `authFetch` makes one refresh (shared with any refresh in flight) and retries the request once with the new token; it keeps the request's body for that (it clones the `Request` before the first send). A second 401, or a refresh that fails, ends the session as above. Never more than one retry per request.
- **403** is never retried and never refreshes: the screen shows "Not allowed" with the operation's needed roles (ADR-0060). A token without any Spillway role gets 403 from `GET /me`; the app then shows "Your account has no Spillway role; ask an admin" and a Sign out button.
- No API call carries cookies (same origin, but the API reads only `Authorization`), so there is no CSRF surface (UI-08).

### Roles (UI-04, D4)

- **`GET /api/v1/me`** (`getMe`, OpenAPI-first, `x-spillway-roles` all five roles, a read, so no audit record) answers the caller as the guard already resolved it (ADR-0043):
  ```json
  {"subject": "0b5e7c1a-…-000000000001", "name": "test-viewer", "roles": ["viewer"]}
  ```
  `subject` is `sub`; `name` is `preferred_username`, or `sub` when absent; `roles` are the Spillway roles in ADR-0043's order. It never echoes the token or any claim beyond these. Schema `Me` with `roles` items enumerated as the five role names; additive for oasdiff.
- **The capability table is generated, not written.** `web/scripts/gen-roles.mjs` reads `api/openapi.yaml` (with `yaml`) and writes `web/src/api/roles.gen.ts`:
  ```ts
  export const operationRoles = { listUpstreams: ["viewer", "editor", "publisher", "key-admin", "admin"], createUpstream: ["editor", "admin"], … } as const;
  export type OperationId = keyof typeof operationRoles;
  ```
  from each operation's `operationId` and `x-spillway-roles`, the same source the server's guard reads (ADR-0040). `make lint-web` fails on drift (ADR-0057), so the UI's table cannot differ from the server's. `can(op)` is true when one of `/me`'s roles is in `operationRoles[op]`. Screens refer to operation IDs, never to role names: when the matrix changes, the UI follows by regeneration.
- **Hide or disable.** A screen or tab is **hidden** (not in the navigation; its URL shows "Not allowed" without calling the API) when the role cannot call its main read, or when the screen exists only to change things the role cannot change:

  | Hidden screen | Shown to | Why |
  | --- | --- | --- |
  | Audit log (`/audit`) | admin | `listAudit` is admin only |
  | Tenants (`/tenants`) | admin | managing tenants (`createTenant`, `updateTenant`, `deleteTenant`) is admin only; every role still sees tenant names on consumers and in filters (`listTenants` is all roles) |

  Every other screen is visible to every role (every role reads everything except the audit log, CP-12). Every **action** whose operation the role cannot call is rendered **disabled with its reason**, visible text next to it and in its accessible description: "Needs the editor or admin role". Examples: Publish (`publishConfig`) for a viewer or editor; Drain (`drainEndpoint`) for a publisher; Issue key (`issueKey`) for an editor; Roll back (`rollbackConfig`) for an editor; the admin-only "Who is this?" heavy-hitter lookup (`lookupHeavyHitters`) for everyone but admin. An action made of two operations is split into two buttons, each with its own check: Promote to enforce is an edit (`updatePolicy`), then Publish is its own step (ADR-0060).
- **The server stays the only enforcement.** The UI's checks only spare the user a 403. Tests: the Go RBAC table test (ADR-0040) covers every operation, `getMe` and the M6 additions included; the Playwright role spec (ADR-0061) also calls `publishConfig`, `drainEndpoint`, `issueKey` and `rollbackConfig` directly with a `test-viewer` token from the `rbac-tests` client and expects 403 for each.

### No credentials anywhere else

- The browser never logs a token, an API key or a key's secret: no `console.*` of request or response objects (lint: `no-console` as an error outside tests).
- Key secrets (ADR-0039: shown once) never enter the TanStack Query cache: the issue and rotate mutations hand the `key` to the dialog's component state through a callback and return the answer without it; the dialog clears it on close. Vitest asserts that neither the query cache nor the mutation cache, serialised, contains `spw_` after an issue and a rotate (ADR-0061).
- Server side: `/me` and the stream log only `sub` (ADR-0043); the existing no-secret tests extend to them.

## Consequences

- No new Keycloak client; the realm gets exact redirect URIs, rotation and written-down lifetimes. M8's `make demo-share` adds exact URIs (P14).
- A reload costs a round trip to Keycloak; the URL keeps filters and pages, memory does not.
- A tab's token never leaves its memory; the only Web Storage use is a login's pending record for seconds, with no token in it.
- Roles in the UI can only drift from the server's if someone skips `make web-gen`, which CI catches.

## Alternatives considered

- **A new `spillway-ui` client** (D3's name) — same settings under a new name; SC-09, ADR-0004, ADR-0040 and docs/setup.md would all change, or two valid clients would exist.
- **Wildcard redirect URIs (`…/*`)** — any path on the origin could receive a code.
- **Tokens or the verifier in sessionStorage across reloads** — a reload would keep the session, but D3 rules out storing tokens; the verifier alone is stored, for seconds.
- **Hidden-iframe silent renew (`prompt=none`)** — third-party cookie rules break it across origins in the demo, and the refresh token already does the job.
- **No refresh-token rotation** — a leaked refresh token would be usable until the SSO session ends; rotation costs nothing now that tabs are measured not to clash.
- **A GET logout with `id_token_hint` in the query string** — puts the ID token in a URL.
- **Roles decoded from the access token** — D4 rules it out; it would also tie the UI to Keycloak's claim layout.
- **Allowed operations listed by `/me`** — a second copy of the matrix on the wire; the generated table is checked at build time and needs no server change when roles change.
- **Hiding every action a role cannot take** — D4 asks for disabled with a reason, which also tells the user which role to ask for.
