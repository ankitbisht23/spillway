import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { provider, TOKEN_ENDPOINT, tokenAnswer } from "../test/keycloak";
import { server } from "../test/server";
import { refreshTokens, type Tokens } from "./oidc";
import { authFetch, createSession, refreshDelayMs, sessionEndedResponse, setSession, SIGNED_OUT_MESSAGE, type SessionDeps } from "./session";

const API = "http://localhost:8080/api/v1";
const first: Tokens = { access: "access-0", refresh: "refresh-0", idToken: "id-0", expiresIn: 300 };

/** A clock and timers the test moves by hand. */
function fakeClock() {
  let t = 0;
  let seq = 0;
  let timers: { at: number; fn: () => void; id: number }[] = [];
  return {
    now: () => t,
    setTimer: (fn: () => void, ms: number) => {
      timers.push({ at: t + ms, fn, id: ++seq });
      return seq;
    },
    clearTimer: (id: unknown) => {
      timers = timers.filter((x) => x.id !== id);
    },
    /** Moves the clock without running timers, as a throttled background tab. */
    skip: (ms: number) => {
      t += ms;
    },
    /** Runs the next timer, if any. */
    next: () => {
      timers.sort((a, b) => a.at - b.at);
      const n = timers.shift();
      if (!n) return undefined;
      t = Math.max(t, n.at);
      n.fn();
      return n.at;
    },
    due: () => timers.map((x) => x.at),
  };
}

/** The token endpoint: answers in turn, recording each refresh token sent. */
function tokenEndpoint(...answers: (() => Response)[]) {
  const sent: string[] = [];
  server.use(http.post(TOKEN_ENDPOINT, async ({ request }) => {
    sent.push(new URLSearchParams(await request.text()).get("refresh_token") ?? "");
    const a = answers[Math.min(sent.length - 1, answers.length - 1)];
    return a ? a() : HttpResponse.json({ error: "invalid_grant" }, { status: 400 });
  }));
  return sent;
}

const ok = (n: number) => () => HttpResponse.json(tokenAnswer(n));
const invalidGrant = () => HttpResponse.json({ error: "invalid_grant", error_description: "Session not active" }, { status: 400 });

function setup(overrides: Partial<SessionDeps> = {}) {
  const clock = fakeClock();
  const channel = { postMessage: vi.fn(), close: vi.fn(), onmessage: null as ((e: MessageEvent) => void) | null };
  const submit = vi.fn();
  const deps: SessionDeps = {
    now: clock.now, setTimer: clock.setTimer, clearTimer: clock.clearTimer, refresh: refreshTokens,
    fetch: (req) => fetch(req), channel, doc: document, submit, ...overrides,
  };
  const s = createSession(provider, first, deps);
  const changes = vi.fn();
  s.subscribe(changes);
  return { s, clock, channel, submit, changes };
}

/** An API endpoint that records the bearer token and body of each request. */
function apiEndpoint(path: string, ...statuses: number[]) {
  const seen: { auth: string | null; body: string }[] = [];
  server.use(http.all(`${API}${path}`, async ({ request }) => {
    seen.push({ auth: request.headers.get("Authorization"), body: await request.text() });
    const status = statuses[Math.min(seen.length - 1, statuses.length - 1)] ?? 200;
    return status === 200 ? HttpResponse.json({ ok: true }) : HttpResponse.json({ error: "x", code: "unauthenticated", request_id: "r" }, { status });
  }));
  return seen;
}

describe("refresh", () => {
  it("is due at max(expires_in − 60 s, expires_in / 2)", () => {
    expect(refreshDelayMs(300)).toBe(240_000);
    expect(refreshDelayMs(60)).toBe(30_000);
  });

  it("runs at 240 s for a 300 s token, and the rotated refresh token replaces the old one", async () => {
    const sent = tokenEndpoint(ok(1), ok(2));
    const { s, clock, changes } = setup();
    expect(clock.due()).toEqual([240_000]);
    expect(clock.next()).toBe(240_000);
    await vi.waitFor(() => expect(s.accessToken()).toBe("access-1"));
    expect(changes).toHaveBeenCalledOnce();
    expect(clock.due()).toEqual([480_000]);
    clock.next();
    await vi.waitFor(() => expect(s.accessToken()).toBe("access-2"));
    expect(sent).toEqual(["refresh-0", "refresh-1"]);
    const seen = apiEndpoint("/me");
    await s.fetch(new Request(`${API}/me`));
    expect(seen[0]?.auth).toBe("Bearer access-2");
  });

  it("sends one request for concurrent refreshes", async () => {
    const sent = tokenEndpoint(ok(1));
    const { s } = setup();
    expect(await Promise.all([s.refresh(), s.refresh(), s.refresh()])).toEqual([true, true, true]);
    expect(sent).toEqual(["refresh-0"]);
  });

  it("ends the session at once on invalid_grant, and sends nothing after", async () => {
    tokenEndpoint(invalidGrant);
    const { s, clock, changes } = setup();
    expect(await s.refresh()).toBe(false);
    expect(s.status()).toBe("ended");
    expect(s.accessToken()).toBeNull();
    expect(changes).toHaveBeenCalledOnce();
    expect(clock.due()).toEqual([]);
    const seen = apiEndpoint("/me");
    const r = await s.fetch(new Request(`${API}/me`));
    expect(r.status).toBe(401);
    expect(seen).toEqual([]);
  });

  it("retries no answer or a 5xx after 5, 10, 20 s until the token expires, then ends", async () => {
    const sent = tokenEndpoint(() => HttpResponse.error(), () => HttpResponse.json({ error: "x" }, { status: 503 }));
    const { s, clock } = setup();
    const at: number[] = [];
    for (let i = 0; i < 5 && s.status() === "active"; i++) {
      const n = sent.length;
      at.push(clock.next() ?? -1);
      await vi.waitFor(() => expect(sent.length).toBe(n + 1));
      await vi.waitFor(() => expect(clock.due().length + (s.status() === "active" ? 0 : 1)).toBe(1));
    }
    expect(at).toEqual([240_000, 245_000, 255_000, 275_000, 300_000]);
    expect(s.status()).toBe("ended");
  });

  it("recovers when a retry succeeds", async () => {
    tokenEndpoint(() => HttpResponse.error(), ok(1));
    const { s, clock } = setup();
    clock.next();
    await vi.waitFor(() => expect(clock.due()).toEqual([245_000]));
    clock.next();
    await vi.waitFor(() => expect(s.accessToken()).toBe("access-1"));
    expect(clock.due()).toEqual([245_000 + 240_000]);
  });

  it("refreshes first when fewer than 30 s remain, as in a throttled background tab", async () => {
    const sent = tokenEndpoint(ok(1));
    const { s, clock } = setup();
    clock.skip(275_000);
    const seen = apiEndpoint("/me");
    await s.fetch(new Request(`${API}/me`));
    expect(sent).toEqual(["refresh-0"]);
    expect(seen[0]?.auth).toBe("Bearer access-1");
  });

  it("refreshes when the tab becomes visible after the refresh was due", async () => {
    const sent = tokenEndpoint(ok(1));
    const { clock } = setup();
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    expect(sent).toEqual([]);
    clock.skip(241_000);
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.waitFor(() => expect(sent).toEqual(["refresh-0"]));
    visibility.mockRestore();
  });
});

describe("authFetch", () => {
  it("sends the bearer token", async () => {
    const seen = apiEndpoint("/routes");
    const { s } = setup();
    expect((await s.fetch(new Request(`${API}/routes`))).status).toBe(200);
    expect(seen).toEqual([{ auth: "Bearer access-0", body: "" }]);
  });

  it("on a 401 refreshes once and retries once with the same body", async () => {
    tokenEndpoint(ok(1));
    const seen = apiEndpoint("/routes", 401, 200);
    const { s } = setup();
    const r = await s.fetch(new Request(`${API}/routes`, { method: "POST", body: '{"name":"r1"}', headers: { "Content-Type": "application/json" } }));
    expect(r.status).toBe(200);
    expect(seen).toEqual([{ auth: "Bearer access-0", body: '{"name":"r1"}' }, { auth: "Bearer access-1", body: '{"name":"r1"}' }]);
    expect(s.status()).toBe("active");
  });

  it("ends the session on a second 401", async () => {
    tokenEndpoint(ok(1));
    const seen = apiEndpoint("/routes", 401);
    const { s } = setup();
    expect((await s.fetch(new Request(`${API}/routes`))).status).toBe(401);
    expect(seen).toHaveLength(2);
    expect(s.status()).toBe("ended");
  });

  it("ends the session when the refresh after a 401 fails", async () => {
    tokenEndpoint(invalidGrant);
    const seen = apiEndpoint("/routes", 401);
    const { s } = setup();
    expect((await s.fetch(new Request(`${API}/routes`))).status).toBe(401);
    expect(seen).toHaveLength(1);
    expect(s.status()).toBe("ended");
  });

  it("never retries or refreshes on a 403", async () => {
    const sent = tokenEndpoint(ok(1));
    const seen = apiEndpoint("/config/publish", 403);
    const { s } = setup();
    expect((await s.fetch(new Request(`${API}/config/publish`, { method: "POST", body: "{}" }))).status).toBe(403);
    expect(seen).toHaveLength(1);
    expect(sent).toEqual([]);
    expect(s.status()).toBe("active");
  });

  it("answers a local 401 without a session", async () => {
    const seen = apiEndpoint("/me");
    setSession(null);
    const r = await authFetch(new Request(`${API}/me`));
    expect(r.status).toBe(401);
    expect(await r.json()).toMatchObject({ code: "unauthenticated" });
    expect(seen).toEqual([]);
    const { s } = setup();
    setSession(s);
    expect((await authFetch(new Request(`${API}/me`))).status).toBe(200);
    setSession(null);
  });

  it("gives the local 401 the API's error body", async () => {
    expect(await sessionEndedResponse().json()).toEqual({ error: "your session ended: sign in again", code: "unauthenticated", request_id: "" });
  });
});

describe("sign-out", () => {
  it("drops the tokens, tells other tabs and POSTs the logout form", () => {
    const { s, channel, submit, clock } = setup();
    s.signOut();
    expect(s.status()).toBe("signed-out");
    expect(s.accessToken()).toBeNull();
    expect(clock.due()).toEqual([]);
    expect(channel.postMessage).toHaveBeenCalledWith(SIGNED_OUT_MESSAGE);
    expect(JSON.stringify(channel.postMessage.mock.calls)).not.toMatch(/access|refresh|id-0/);
    const form = submit.mock.calls[0]?.[0] as HTMLFormElement;
    expect(form.method).toBe("post");
    expect(form.action).not.toContain("id-0");
    expect(new FormData(form).get("id_token_hint")).toBe("id-0");
    form.remove();
  });

  it("follows another tab's sign-out", () => {
    const { s, channel, changes, submit } = setup();
    channel.onmessage?.(new MessageEvent("message", { data: "something else" }));
    expect(s.status()).toBe("active");
    channel.onmessage?.(new MessageEvent("message", { data: SIGNED_OUT_MESSAGE }));
    expect(s.status()).toBe("signed-out");
    expect(s.accessToken()).toBeNull();
    expect(changes).toHaveBeenCalledOnce();
    expect(submit).not.toHaveBeenCalled();
  });

  it("disposes of its timer, channel and listeners", () => {
    const { s, channel, clock } = setup();
    s.dispose();
    expect(clock.due()).toEqual([]);
    expect(channel.close).toHaveBeenCalledOnce();
  });
});

describe("storage", () => {
  it("writes nothing to any storage or cookie", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    tokenEndpoint(ok(1));
    apiEndpoint("/me", 401, 200);
    const { s } = setup();
    await s.fetch(new Request(`${API}/me`));
    s.signOut();
    expect(setItem).not.toHaveBeenCalled();
    expect(document.cookie).toBe("");
    document.querySelectorAll("form").forEach((f) => f.remove());
  });
});
