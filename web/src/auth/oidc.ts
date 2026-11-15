// The OIDC code flow with PKCE against Keycloak (UI-08, ADR-0058), with
// oauth4webapi, which stores nothing: this module holds no token either, it
// hands token responses to the session (session.ts).
import * as oauth from "oauth4webapi";

/** GET /api/ui-config (ADR-0057). */
export interface UiConfig {
  issuer: string;
  client_id: string;
}

/** The authorization server as discovered, and this app as its client. */
export interface Provider {
  as: oauth.AuthorizationServer;
  client: oauth.Client;
  /** Options for every request to it: plain HTTP only for a loopback issuer (compose). */
  options: { [oauth.allowInsecureRequests]?: boolean };
  redirectUri: string;
  postLogoutRedirectUri: string;
}

/** The tokens of one token-endpoint answer. */
export interface Tokens {
  access: string;
  refresh: string | undefined;
  idToken: string | undefined;
  /** Seconds the access token is valid from its receipt. */
  expiresIn: number;
}

export const CALLBACK_PATH = "/oidc/callback";
export const SIGNED_OUT_PATH = "/signed-out";
export const HOME_PATH = "/overview";

/** The control plane's runtime config; it holds no secret. */
export async function loadUiConfig(): Promise<UiConfig> {
  const r = await fetch("/api/ui-config", { headers: { Accept: "application/json" } });
  if (!r.ok) throw new Error(`/api/ui-config: ${r.status}`);
  const c = (await r.json()) as Partial<UiConfig>;
  if (typeof c.issuer !== "string" || typeof c.client_id !== "string") throw new Error("/api/ui-config: not a config");
  return { issuer: c.issuer, client_id: c.client_id };
}

function loopback(u: URL): boolean {
  return u.hostname === "localhost" || u.hostname === "127.0.0.1" || u.hostname === "[::1]";
}

/** Discovery from <issuer>/.well-known/openid-configuration. */
export async function discover(cfg: UiConfig, origin: string = location.origin): Promise<Provider> {
  const issuer = new URL(cfg.issuer);
  const options = issuer.protocol === "http:" && loopback(issuer) ? { [oauth.allowInsecureRequests]: true } : {};
  const as = await oauth.processDiscoveryResponse(issuer, await oauth.discoveryRequest(issuer, { ...options, algorithm: "oidc" }));
  return {
    as,
    client: { client_id: cfg.client_id },
    options,
    redirectUri: origin + CALLBACK_PATH,
    postLogoutRedirectUri: origin + SIGNED_OUT_PATH,
  };
}

/**
 * What must survive the round trip to Keycloak: no token, a single-use
 * verifier bound to one request (ADR-0058). In sessionStorage for at most
 * PENDING_TTL_MS, deleted at the start of the callback.
 */
interface Pending {
  state: string;
  nonce: string;
  code_verifier: string;
  return_to: string;
  created_at: number;
}

export const PENDING_KEY = "spillway.oidc.pending";
export const PENDING_TTL_MS = 10 * 60 * 1000;

/**
 * Starts a login: writes the pending record and returns the authorization
 * URL to navigate to. returnTo is where to go after the callback.
 */
export async function beginLogin(p: Provider, returnTo: string, storage: Storage = sessionStorage, now: () => number = Date.now): Promise<URL> {
  if (!p.as.authorization_endpoint) throw new Error("the issuer has no authorization endpoint");
  // 32 random bytes each, base64url (oauth4webapi's generators).
  const pending: Pending = {
    state: oauth.generateRandomState(),
    nonce: oauth.generateRandomNonce(),
    code_verifier: oauth.generateRandomCodeVerifier(),
    return_to: returnTo,
    created_at: now(),
  };
  const url = new URL(p.as.authorization_endpoint);
  url.search = new URLSearchParams({
    response_type: "code",
    client_id: p.client.client_id,
    redirect_uri: p.redirectUri,
    scope: "openid",
    state: pending.state,
    nonce: pending.nonce,
    code_challenge: await oauth.calculatePKCECodeChallenge(pending.code_verifier),
    code_challenge_method: "S256",
  }).toString();
  storage.setItem(PENDING_KEY, JSON.stringify(pending));
  return url;
}

/** Reads and deletes the pending record; null when missing, broken or expired. */
function takePending(storage: Storage, now: number): Pending | null {
  const raw = storage.getItem(PENDING_KEY);
  storage.removeItem(PENDING_KEY);
  if (raw === null) return null;
  try {
    const p = JSON.parse(raw) as Partial<Pending>;
    if (typeof p.state !== "string" || typeof p.nonce !== "string" || typeof p.code_verifier !== "string" ||
        typeof p.return_to !== "string" || typeof p.created_at !== "number") return null;
    if (now - p.created_at > PENDING_TTL_MS || now < p.created_at) return null;
    return p as Pending;
  } catch {
    return null;
  }
}

export type CallbackResult = { ok: true; tokens: Tokens; returnTo: string } | { ok: false; reason: string };

/**
 * Completes a login at /oidc/callback. First of all the code and state
 * leave the address bar and the history entry; then the pending record is
 * taken, the answer checked (state, iss, error) and the code exchanged
 * (the ID token's iss, aud, exp and nonce checked). Nothing is retried.
 */
export async function completeLogin(
  p: Provider, url: URL, storage: Storage = sessionStorage, hist: Pick<History, "replaceState"> = history, now: () => number = Date.now,
): Promise<CallbackResult> {
  const params = new URLSearchParams(url.search);
  hist.replaceState(null, "", CALLBACK_PATH);
  const pending = takePending(storage, now());
  if (!pending) return { ok: false, reason: "the sign-in took too long or was started in another tab" };
  if (params.has("error")) return { ok: false, reason: `the sign-in service answered ${params.get("error") ?? ""}` };
  if (params.get("state") !== pending.state) return { ok: false, reason: "the answer does not match this sign-in" };
  const iss = params.get("iss");
  if (iss !== null && iss !== p.as.issuer) return { ok: false, reason: "the answer came from another issuer" };
  try {
    const checked = oauth.validateAuthResponse(p.as, p.client, params, pending.state);
    const resp = await oauth.authorizationCodeGrantRequest(p.as, p.client, oauth.None(), checked, p.redirectUri, pending.code_verifier, p.options);
    const tokens = await oauth.processAuthorizationCodeResponse(p.as, p.client, resp, { expectedNonce: pending.nonce, requireIdToken: true });
    return { ok: true, tokens: tokensOf(tokens), returnTo: safeReturnPath(pending.return_to, url.origin) };
  } catch {
    return { ok: false, reason: "the sign-in service did not accept the answer" };
  }
}

/**
 * returnTo when it is a same-origin path (one leading "/", not "//" or
 * "/\") other than the callback and Signed out, else the overview. It never
 * comes from a URL parameter, so there is no open redirect.
 */
export function safeReturnPath(returnTo: string, origin: string): string {
  if (!returnTo.startsWith("/") || returnTo.startsWith("//") || returnTo.startsWith("/\\")) return HOME_PATH;
  let u: URL;
  try {
    u = new URL(returnTo, origin);
  } catch {
    return HOME_PATH;
  }
  if (u.origin !== origin || u.pathname === CALLBACK_PATH || u.pathname === SIGNED_OUT_PATH) return HOME_PATH;
  return u.pathname + u.search;
}

/** A refresh grant: the rotated refresh token replaces the old one. */
export async function refreshTokens(p: Provider, refreshToken: string): Promise<Tokens> {
  const resp = await oauth.refreshTokenGrantRequest(p.as, p.client, oauth.None(), refreshToken, p.options);
  return tokensOf(await oauth.processRefreshTokenResponse(p.as, p.client, resp));
}

/**
 * Whether a failed refresh may succeed later: no answer, or a 5xx. Anything
 * else (invalid_grant: the session ended, idled out, or the token is stale)
 * ends the session.
 */
export function isTransientRefreshError(err: unknown): boolean {
  if (err instanceof TypeError) return true; // fetch: no answer
  if (err instanceof oauth.ResponseBodyError) return err.status >= 500;
  if (err instanceof oauth.OperationProcessingError && err.cause instanceof Response) return err.cause.status >= 500;
  return false;
}

function tokensOf(t: oauth.TokenEndpointResponse): Tokens {
  return { access: t.access_token, refresh: t.refresh_token, idToken: t.id_token, expiresIn: t.expires_in ?? 300 };
}

/**
 * The RP-initiated logout as a POST form (built with DOM APIs), so the ID
 * token is in no URL, history entry or log line. Without an end-session
 * endpoint, null: the caller goes to Signed out itself.
 */
export function logoutForm(p: Provider, idToken: string | undefined, doc: Document = document): HTMLFormElement | null {
  if (!p.as.end_session_endpoint) return null;
  const form = doc.createElement("form");
  form.method = "post";
  form.action = p.as.end_session_endpoint;
  const fields: [string, string | undefined][] = [
    ["id_token_hint", idToken],
    ["client_id", p.client.client_id],
    ["post_logout_redirect_uri", p.postLogoutRedirectUri],
  ];
  for (const [name, value] of fields) {
    if (value === undefined) continue;
    const input = doc.createElement("input");
    input.type = "hidden";
    input.name = name;
    input.value = value;
    form.append(input);
  }
  form.hidden = true;
  return form;
}
