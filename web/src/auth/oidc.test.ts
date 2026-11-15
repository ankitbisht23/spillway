// @vitest-environment node
// Protocol logic in Node's environment (ADR-0061): jsdom's typed arrays
// come from another realm than the one oauth4webapi's crypto sees.
import { http, HttpResponse } from "msw";
import * as oauth from "oauth4webapi";
import { describe, expect, it, vi } from "vitest";

import { discoveryHandler, idToken, ISSUER, MemoryStorage, provider, TOKEN_ENDPOINT, tokenAnswer } from "../test/keycloak";
import { server } from "../test/server";
import {
  beginLogin, completeLogin, discover, isTransientRefreshError, PENDING_KEY, PENDING_TTL_MS,
  refreshTokens, safeReturnPath,
} from "./oidc";

const origin = "http://localhost:8080";

async function sha256url(s: string): Promise<string> {
  const d = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s)));
  return btoa(String.fromCharCode(...d)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** A login begun for returnTo, and the callback URL Keycloak would send back. */
async function begun(returnTo = "/policies?cursor=abc") {
  const storage = new MemoryStorage();
  const auth = await beginLogin(provider, returnTo, storage);
  const pending = JSON.parse(storage.getItem(PENDING_KEY) ?? "{}") as { state: string; nonce: string; code_verifier: string };
  const callback = new URL(`${origin}/oidc/callback`);
  callback.search = new URLSearchParams({ code: "code-1", state: pending.state, iss: ISSUER, session_state: "s" }).toString();
  return { storage, auth, pending, callback };
}

describe("discovery", () => {
  it("discovers the provider, allowing plain HTTP only for a loopback issuer", async () => {
    server.use(discoveryHandler);
    const p = await discover({ issuer: ISSUER, client_id: "admin-ui" }, origin);
    expect(p.as.token_endpoint).toBe(TOKEN_ENDPOINT);
    expect(p.redirectUri).toBe(`${origin}/oidc/callback`);
    expect(p.postLogoutRedirectUri).toBe(`${origin}/signed-out`);
    expect(p.options[oauth.allowInsecureRequests]).toBe(true);
    await expect(discover({ issuer: "http://id.example.com/realms/x", client_id: "admin-ui" }, origin)).rejects.toThrow();
  });
});

describe("beginLogin", () => {
  it("asks for a code with PKCE S256, state, nonce and the exact redirect URI", async () => {
    const { storage, auth, pending } = await begun();
    expect(auth.origin + auth.pathname).toBe(`${ISSUER}/protocol/openid-connect/auth`);
    const q = auth.searchParams;
    expect(Object.fromEntries(q)).toMatchObject({
      response_type: "code", client_id: "admin-ui", redirect_uri: `${origin}/oidc/callback`, scope: "openid",
      state: pending.state, nonce: pending.nonce, code_challenge_method: "S256",
    });
    expect(q.get("code_challenge")).toBe(await sha256url(pending.code_verifier));
    expect(pending.code_verifier).toMatch(/^[\w-]{43}$/);
    expect(pending.state).toMatch(/^[\w-]{43}$/);
    expect(pending.nonce).toMatch(/^[\w-]{43}$/);
    expect(storage.writes).toEqual([PENDING_KEY]);
  });
});

describe("completeLogin", () => {
  it("removes the code from the URL before the exchange, checks the nonce and returns the tokens", async () => {
    const { storage, pending, callback } = await begun();
    const events: string[] = [];
    let form: URLSearchParams | undefined;
    server.use(http.post(TOKEN_ENDPOINT, async ({ request }) => {
      events.push("exchange");
      form = new URLSearchParams(await request.text());
      return HttpResponse.json(tokenAnswer(1, pending.nonce));
    }));
    const replaceState = vi.fn((_d: unknown, _u: string, url?: string | URL | null) => void events.push(`replace ${String(url)}`));
    const r = await completeLogin(provider, callback, storage, { replaceState });
    expect(events).toEqual(["replace /oidc/callback", "exchange"]);
    expect(r).toEqual({ ok: true, returnTo: "/policies?cursor=abc", tokens: { access: "access-1", refresh: "refresh-1", idToken: expect.any(String), expiresIn: 300 } });
    expect(Object.fromEntries(form ?? [])).toMatchObject({
      grant_type: "authorization_code", code: "code-1", code_verifier: pending.code_verifier, redirect_uri: `${origin}/oidc/callback`, client_id: "admin-ui",
    });
    expect(storage.length).toBe(0);
  });

  it("fails without a pending record, with an expired one, or one from the future", async () => {
    const hist = { replaceState: vi.fn() };
    const { callback } = await begun();
    expect(await completeLogin(provider, callback, new MemoryStorage(), hist)).toMatchObject({ ok: false });
    const late = await begun();
    expect(await completeLogin(provider, late.callback, late.storage, hist, () => Date.now() + PENDING_TTL_MS + 1)).toMatchObject({ ok: false });
    expect(late.storage.length).toBe(0);
    const early = await begun();
    expect(await completeLogin(provider, early.callback, early.storage, hist, () => Date.now() - 1000)).toMatchObject({ ok: false });
    const broken = new MemoryStorage();
    broken.setItem(PENDING_KEY, "{not json");
    expect(await completeLogin(provider, callback, broken, hist)).toMatchObject({ ok: false });
    expect(hist.replaceState).toHaveBeenCalledTimes(4);
  });

  it("fails on another state, another issuer or an error, without exchanging anything", async () => {
    let exchanged = false;
    server.use(http.post(TOKEN_ENDPOINT, () => {
      exchanged = true;
      return HttpResponse.json({});
    }));
    for (const change of [
      (u: URL) => u.searchParams.set("state", "other"),
      (u: URL) => u.searchParams.set("iss", "http://evil.test/realms/spillway"),
      (u: URL) => u.searchParams.set("error", "access_denied"),
      (u: URL) => u.searchParams.delete("iss"),
    ]) {
      const { storage, callback } = await begun();
      change(callback);
      const r = await completeLogin(provider, callback, storage, { replaceState: vi.fn() });
      expect(r.ok).toBe(false);
      expect(storage.length).toBe(0);
    }
    expect(exchanged).toBe(false);
  });

  it("fails when the ID token's nonce, audience or issuer is wrong", async () => {
    for (const claims of [{ nonce: "other" }, { aud: "spillway-ci" }, { iss: "http://evil.test" }]) {
      const { storage, pending, callback } = await begun();
      server.use(http.post(TOKEN_ENDPOINT, () => HttpResponse.json({ ...tokenAnswer(1), id_token: idToken({ nonce: pending.nonce, ...claims }) })));
      expect(await completeLogin(provider, callback, storage, { replaceState: vi.fn() })).toMatchObject({ ok: false });
    }
  });

  it("falls back to the overview for an unsafe return path", async () => {
    const { storage, pending, callback } = await begun("//evil.test/x");
    server.use(http.post(TOKEN_ENDPOINT, () => HttpResponse.json(tokenAnswer(1, pending.nonce))));
    expect(await completeLogin(provider, callback, storage, { replaceState: vi.fn() })).toMatchObject({ ok: true, returnTo: "/overview" });
  });
});

describe("safeReturnPath", () => {
  it.each([
    ["/policies/7?window=1h", "/policies/7?window=1h"],
    ["/", "/"],
    ["//evil.test/x", "/overview"],
    ["/\\evil.test", "/overview"],
    ["https://evil.test/", "/overview"],
    ["evil", "/overview"],
    ["/oidc/callback?code=x", "/overview"],
    ["/signed-out", "/overview"],
  ])("%s → %s", (given, want) => {
    expect(safeReturnPath(given, origin)).toBe(want);
  });
});

describe("refreshTokens", () => {
  it("sends the refresh token and takes the rotated one", async () => {
    let form: URLSearchParams | undefined;
    server.use(http.post(TOKEN_ENDPOINT, async ({ request }) => {
      form = new URLSearchParams(await request.text());
      return HttpResponse.json(tokenAnswer(2));
    }));
    expect(await refreshTokens(provider, "refresh-1")).toMatchObject({ access: "access-2", refresh: "refresh-2" });
    expect(Object.fromEntries(form ?? [])).toMatchObject({ grant_type: "refresh_token", refresh_token: "refresh-1", client_id: "admin-ui" });
  });

  it("tells transient failures from an ended session", async () => {
    const fail = async (resp: () => Response) => {
      server.use(http.post(TOKEN_ENDPOINT, resp));
      return refreshTokens(provider, "refresh-1").catch((e: unknown) => e);
    };
    expect(isTransientRefreshError(await fail(() => HttpResponse.json({ error: "invalid_grant", error_description: "Stale token" }, { status: 400 })))).toBe(false);
    expect(isTransientRefreshError(await fail(() => HttpResponse.json({ error: "server_error" }, { status: 503 })))).toBe(true);
    expect(isTransientRefreshError(await fail(() => new HttpResponse("<html>Bad Gateway</html>", { status: 502 })))).toBe(true);
    expect(isTransientRefreshError(await fail(() => HttpResponse.error()))).toBe(true);
    expect(isTransientRefreshError(new Error("other"))).toBe(false);
  });
});
