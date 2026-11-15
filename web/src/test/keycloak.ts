// A fake Keycloak for MSW (ADR-0061): discovery, the token endpoint and
// tokens shaped like Keycloak's. ID tokens are unsigned: oauth4webapi
// checks their claims, not their signature, on a token-endpoint answer.
import { http, HttpResponse } from "msw";
import { allowInsecureRequests } from "oauth4webapi";

import type { Provider } from "../auth/oidc";

export const ISSUER = "http://localhost:8180/auth/realms/spillway";
export const TOKEN_ENDPOINT = `${ISSUER}/protocol/openid-connect/token`;
export const END_SESSION_ENDPOINT = `${ISSUER}/protocol/openid-connect/logout`;

export const discoveryDocument = {
  issuer: ISSUER,
  authorization_endpoint: `${ISSUER}/protocol/openid-connect/auth`,
  token_endpoint: TOKEN_ENDPOINT,
  end_session_endpoint: END_SESSION_ENDPOINT,
  jwks_uri: `${ISSUER}/protocol/openid-connect/certs`,
  response_types_supported: ["code"],
  id_token_signing_alg_values_supported: ["RS256"],
  code_challenge_methods_supported: ["S256"],
  authorization_response_iss_parameter_supported: true,
};

export const discoveryHandler = http.get(`${ISSUER}/.well-known/openid-configuration`, () => HttpResponse.json(discoveryDocument));

function b64url(s: string): string {
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** An unsigned JWT-shaped ID token for admin-ui. */
export function idToken(claims: Record<string, unknown> = {}): string {
  const now = Math.floor(Date.now() / 1000);
  const body = { iss: ISSUER, aud: "admin-ui", sub: "sub-1", iat: now, exp: now + 300, ...claims };
  return `${b64url(JSON.stringify({ alg: "RS256", typ: "JWT" }))}.${b64url(JSON.stringify(body))}.c2ln`;
}

/** A token-endpoint answer; access and refresh are tagged with n. */
export function tokenAnswer(n: number, nonce?: string) {
  return {
    access_token: `access-${n}`, refresh_token: `refresh-${n}`, id_token: idToken(nonce ? { nonce } : {}),
    token_type: "Bearer", expires_in: 300, refresh_expires_in: 1800, scope: "openid",
  };
}

/** The provider discovery would give for an app at http://localhost:8080. */
export const provider: Provider = {
  as: discoveryDocument,
  client: { client_id: "admin-ui" },
  options: { [allowInsecureRequests]: true },
  redirectUri: "http://localhost:8080/oidc/callback",
  postLogoutRedirectUri: "http://localhost:8080/signed-out",
};

/** Storage in memory, recording every write. */
export class MemoryStorage implements Storage {
  readonly writes: string[] = [];
  private m = new Map<string, string>();
  get length() { return this.m.size; }
  clear() { this.m.clear(); }
  getItem(k: string) { return this.m.get(k) ?? null; }
  key(i: number) { return [...this.m.keys()][i] ?? null; }
  removeItem(k: string) { this.m.delete(k); }
  setItem(k: string, v: string) { this.writes.push(k); this.m.set(k, v); }
}
