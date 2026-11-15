// @vitest-environment node
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { ISSUER, MemoryStorage, provider, TOKEN_ENDPOINT, tokenAnswer } from "../test/keycloak";
import { server } from "../test/server";
import { boot, type BootDeps } from "./boot";
import { PENDING_KEY } from "./oidc";

function deps(href: string, over: Partial<BootDeps> = {}) {
  const url = new URL(href);
  const navigate = vi.fn<(u: string) => void>();
  const replaceState = vi.fn();
  const d: BootDeps = {
    loadConfig: () => Promise.resolve({ issuer: ISSUER, client_id: "admin-ui" }),
    discover: () => Promise.resolve(provider),
    location: { href, pathname: url.pathname, search: url.search },
    history: { replaceState },
    storage: new MemoryStorage(),
    navigate,
    sessionDeps: () => ({
      now: () => 0, setTimer: () => 0, clearTimer: () => {}, refresh: () => Promise.reject(new Error("no")),
      fetch: (r) => fetch(r), channel: null, doc: new EventTarget() as unknown as Document, submit: () => {},
    }),
    ...over,
  };
  return { d, navigate, replaceState };
}

describe("boot", () => {
  it("sends a page without a session to Keycloak, to come back to the same path", async () => {
    const { d, navigate } = deps("http://localhost:8080/policies?cursor=x");
    expect(await boot(d)).toEqual({ kind: "redirecting" });
    expect(new URL(navigate.mock.calls[0]?.[0] ?? "").pathname).toBe("/auth/realms/spillway/protocol/openid-connect/auth");
    expect(JSON.parse(d.storage.getItem(PENDING_KEY) ?? "{}")).toMatchObject({ return_to: "/policies?cursor=x" });
  });

  it("never starts a login at Signed out, and drops an abandoned login's record", async () => {
    const { d, navigate } = deps("http://localhost:8080/signed-out");
    d.storage.setItem(PENDING_KEY, "{}");
    expect(await boot(d)).toMatchObject({ kind: "signed-out" });
    expect(navigate).not.toHaveBeenCalled();
    expect(d.storage.length).toBe(0);
  });

  it("completes a callback into a session at the return path", async () => {
    const start = deps("http://localhost:8080/history");
    await boot(start.d);
    const pending = JSON.parse(start.d.storage.getItem(PENDING_KEY) ?? "{}") as { state: string; nonce: string };
    server.use(http.post(TOKEN_ENDPOINT, () => HttpResponse.json(tokenAnswer(1, pending.nonce))));
    const href = `http://localhost:8080/oidc/callback?code=c&state=${pending.state}&iss=${encodeURIComponent(ISSUER)}`;
    const { d, replaceState } = deps(href, { storage: start.d.storage });
    const b = await boot(d);
    expect(b.kind).toBe("session");
    if (b.kind === "session") expect(b.session.accessToken()).toBe("access-1");
    expect(replaceState.mock.calls.map((c) => c[2])).toEqual(["/oidc/callback", "/history"]);
  });

  it("reports a failed callback without retrying", async () => {
    const { d, navigate } = deps("http://localhost:8080/oidc/callback?code=c&state=s");
    expect(await boot(d)).toMatchObject({ kind: "sign-in-failed" });
    expect(navigate).not.toHaveBeenCalled();
  });

  it("reports an unreachable control plane or Keycloak", async () => {
    expect(await boot(deps("http://localhost:8080/", { loadConfig: () => Promise.reject(new Error("down")) }).d)).toEqual({ kind: "unavailable" });
    expect(await boot(deps("http://localhost:8080/", { discover: () => Promise.reject(new Error("down")) }).d)).toEqual({ kind: "unavailable" });
  });
});
