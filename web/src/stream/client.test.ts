// @vitest-environment node
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it } from "vitest";

import { server } from "../test/server";
import {
  backoffMs, BACKOFF_MS, DEAD_MS, HIDDEN_MS, openStream, STABLE_MS, streamQuery,
  type StreamDeps, type StreamError, type StreamResult, type StreamStatus,
} from "./client";

const ORIGIN = "http://cp.test";
const STREAM = `${ORIGIN}/api/v1/stream`;
const TOKEN = "access-token-dev-only-not-secret";

/**
 * A response body the test writes to, as the control plane would: it
 * starts with the ready event, which the server writes at once (MSW also
 * answers a fetch only once the body has a first chunk).
 */
function sseBody() {
  const enc = new TextEncoder();
  let ctl!: ReadableStreamDefaultController<Uint8Array>;
  const stream = new ReadableStream<Uint8Array>({
    start: (c) => {
      ctl = c;
      c.enqueue(enc.encode('event: ready\ndata: {"subscriptions":["replicas"],"interval_s":5}\n\n'));
    },
  });
  return {
    response: () => new HttpResponse(stream, { headers: { "Content-Type": "text/event-stream; charset=utf-8" } }),
    send: (s: string) => ctl.enqueue(enc.encode(s)),
    event: (name: string, data: unknown) => ctl.enqueue(enc.encode(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`)),
    close: () => ctl.close(),
  };
}

type Body = ReturnType<typeof sseBody>;

/** The test's timers: they run only when the test moves the clock. */
function fakeTimers() {
  let now = 0;
  let seq = 0;
  let timers: { at: number; fn: () => void; id: number }[] = [];
  return {
    setTimer: (fn: () => void, ms: number) => {
      timers.push({ at: now + ms, fn, id: ++seq });
      return seq;
    },
    clearTimer: (id: unknown) => {
      timers = timers.filter((t) => t.id !== id);
    },
    /** Moves the clock by ms, running the timers due on the way. */
    advance: async (ms: number) => {
      const end = now + ms;
      for (;;) {
        timers.sort((a, b) => a.at - b.at);
        const t = timers[0];
        if (!t || t.at > end) break;
        timers.shift();
        now = t.at;
        t.fn();
        await settle();
      }
      now = end;
      await settle();
    },
    pending: () => timers.map((t) => t.at - now),
  };
}

/** Lets the network mock and the client's promises run. */
async function settle() {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 1));
}

/** Waits for cond, on real time, up to 2 s. */
async function until(cond: () => boolean, what: string) {
  for (let i = 0; i < 200 && !cond(); i++) await new Promise((r) => setTimeout(r, 10));
  expect(cond(), what).toBe(true);
}

interface Harness {
  deps: StreamDeps;
  timers: ReturnType<typeof fakeTimers>;
  requests: Request[];
  statuses: StreamStatus[];
  results: StreamResult[];
  errors: StreamError[];
  refresh: () => void;
  setVisibility: (v: DocumentVisibilityState) => void;
  close: () => void;
}

/**
 * Opens a stream whose fetch adds the bearer token, as authFetch does;
 * each request gets the next of answers (a Body, or a function).
 */
function start(answers: (Body | (() => Response))[], query = "sub=replicas"): Harness {
  const timers = fakeTimers();
  const listeners = new Set<() => void>();
  const visibility = { state: "visible" as DocumentVisibilityState, handler: null as (() => void) | null };
  const h: Harness = {
    timers,
    requests: [],
    statuses: [],
    results: [],
    errors: [],
    refresh: () => listeners.forEach((l) => l()),
    setVisibility: (v) => {
      visibility.state = v;
      visibility.handler?.();
    },
    close: () => {},
    deps: {
      fetch: (req) => {
        h.requests.push(req);
        const r = new Request(req);
        r.headers.set("Authorization", `Bearer ${TOKEN}`);
        return fetch(r);
      },
      onSessionChange: (l) => {
        listeners.add(l);
        return () => void listeners.delete(l);
      },
      origin: ORIGIN,
      setTimer: timers.setTimer,
      clearTimer: timers.clearTimer,
      random: () => 0.5,
      doc: {
        get visibilityState() {
          return visibility.state;
        },
        addEventListener: (_: string, fn: unknown) => void (visibility.handler = fn as () => void),
        removeEventListener: () => void (visibility.handler = null),
      } as unknown as StreamDeps["doc"],
    },
  };
  let n = 0;
  server.use(
    http.get(STREAM, ({ request }) => {
      expect(request.headers.get("Authorization")).toBe(`Bearer ${TOKEN}`);
      const a = answers[Math.min(n++, answers.length - 1)];
      if (!a) throw new Error("no answer");
      return typeof a === "function" ? a() : a.response();
    }),
  );
  h.close = openStream(query, {
    onResult: (r) => h.results.push(r),
    onError: (e) => h.errors.push(e),
    onStatus: (s) => h.statuses.push(s),
  }, h.deps);
  return h;
}

const result = (at: string): StreamResult => ({ sub: "replicas", at, data: { items: [] } });

let open: Harness[] = [];
afterEach(() => {
  open.forEach((h) => h.close());
  open = [];
});

function track(h: Harness) {
  open.push(h);
  return h;
}

describe("streamQuery", () => {
  it("repeats sub, sets the given parameters only and never a token", () => {
    expect(streamQuery(["overview.requests", "overview.routes"], { route: "r1", tenant: undefined, range: "1h", policy_id: 7 }))
      .toBe("sub=overview.requests&sub=overview.routes&route=r1&range=1h&policy_id=7");
    expect(streamQuery(["replicas"])).toBe("sub=replicas");
  });
});

describe("backoffMs", () => {
  it("doubles from 1 s to a 30 s cap with ±20 % jitter", () => {
    expect(BACKOFF_MS.map((_, i) => backoffMs(i, () => 0.5))).toEqual([1000, 2000, 4000, 8000, 16000, 30000]);
    expect(backoffMs(9, () => 0.5)).toBe(30000);
    expect(backoffMs(0, () => 0)).toBe(800);
    expect(backoffMs(0, () => 1)).toBe(1200);
    expect(backoffMs(5, () => 0.999999)).toBeLessThanOrEqual(36000);
  });
});

describe("openStream", () => {
  it("streams results and errors over fetch with the token in a header, not the URL", async () => {
    const body = sseBody();
    const h = track(start([body], "sub=replicas&sub=overview.requests&range=1h"));
    await until(() => h.statuses.some((s) => s.state === "open"), "open");
    body.event("result", result("t0"));
    body.send(": keepalive\n\n");
    body.event("error", { sub: "overview.requests", at: "t0", code: "prometheus_unavailable", message: "down" });
    await until(() => h.errors.length === 1, "an error event");
    expect(h.results).toEqual([result("t0")]);
    expect(h.statuses).toEqual([{ state: "connecting" }, { state: "open" }]);
    const req = h.requests[0];
    expect(req?.url).toBe(`${STREAM}?sub=replicas&sub=overview.requests&range=1h`);
    expect(req?.url).not.toContain(TOKEN);
    expect(req?.headers.get("Accept")).toBe("text/event-stream");
    expect(req?.headers.get("Authorization")).toBeNull(); // authFetch adds it
    expect(req?.cache).toBe("no-store");
  });

  it("skips malformed and unknown events", async () => {
    const body = sseBody();
    const h = track(start([body]));
    await until(() => h.statuses.length === 2, "open");
    body.send("event: result\ndata: {not json\n\nevent: result\ndata: 42\n\nevent: other\ndata: {}\n\n");
    body.event("result", result("t1"));
    await until(() => h.results.length === 1, "the good event");
    expect(h.results).toEqual([result("t1")]);
  });

  it("reconnects after 1, 2, 4, 8, 16, 30, 30 s while attempts fail", async () => {
    const h = track(start([() => new HttpResponse(null, { status: 502 })]));
    await until(() => h.statuses.length === 2, "the first failure");
    for (const ms of [1000, 2000, 4000, 8000, 16000, 30000]) {
      await h.timers.advance(ms);
    }
    await until(() => h.requests.length === 7, "seven attempts");
    expect(h.statuses.filter((s) => s.state === "reconnecting")).toEqual(
      [1000, 2000, 4000, 8000, 16000, 30000, 30000].map((retryInMs) => ({ state: "reconnecting", retryInMs })),
    );
  });

  it("resets the backoff once a stream stayed open 30 s", async () => {
    const bodies = [sseBody(), sseBody(), sseBody()];
    const h = track(start(bodies));
    await until(() => h.statuses.length === 2, "open");
    bodies[0]?.close();
    await until(() => h.statuses.length === 3, "dropped");
    await h.timers.advance(1000);
    await until(() => h.requests.length === 2, "second attempt");
    await h.timers.advance(STABLE_MS);
    bodies[1]?.close();
    await until(() => h.statuses.length === 5, "dropped again");
    expect(h.statuses.at(-1)).toEqual({ state: "reconnecting", retryInMs: 1000 });
  });

  it("reconnects at once with the new token after every refresh", async () => {
    const bodies = [sseBody(), sseBody(), sseBody()];
    const h = track(start(bodies));
    await until(() => h.statuses.length === 2, "open");
    h.refresh();
    await until(() => h.requests.length === 2, "a second request");
    await until(() => h.statuses.filter((s) => s.state === "open").length === 2, "open twice");
    h.refresh();
    await until(() => h.requests.length === 3, "a third request");
    await until(() => h.statuses.filter((s) => s.state === "open").length === 3, "open three times");
    expect(h.statuses.some((s) => s.state === "reconnecting" || s.state === "failed")).toBe(false);
    expect(h.requests[0]?.signal.aborted).toBe(true);
    expect(h.requests[1]?.signal.aborted).toBe(true);
  });

  it("reconnects at once after end token_expired, after a backoff after end shutdown", async () => {
    const bodies = [sseBody(), sseBody(), sseBody()];
    const h = track(start(bodies));
    await until(() => h.statuses.length === 2, "open");
    bodies[0]?.event("end", { reason: "token_expired" });
    await until(() => h.requests.length === 2, "a reconnect");
    await until(() => h.statuses.length === 3, "open again");
    expect(h.statuses.at(-1)).toEqual({ state: "open" });
    bodies[1]?.event("end", { reason: "shutdown" });
    await until(() => h.statuses.length === 4, "dropped");
    expect(h.statuses.at(-1)).toEqual({ state: "reconnecting", retryInMs: 1000 });
  });

  it.each([
    [400, "invalid_request", "bad request"],
    [403, "forbidden", "forbidden"],
    [401, "unauthenticated", "your session ended"],
  ])("stops on %d with the API's error", async (status, code, message) => {
    const h = track(start([() => HttpResponse.json({ error: message, code, request_id: "req-9" }, { status })]));
    await until(() => h.statuses.length === 2, "stopped");
    const last = h.statuses.at(-1);
    expect(last?.state).toBe("failed");
    if (last?.state === "failed") {
      expect([last.error.status, last.error.code, last.error.requestId]).toEqual([status, code, "req-9"]);
    }
    await h.timers.advance(60_000);
    expect(h.requests).toHaveLength(1);
  });

  it("waits for Retry-After on a 503, the backoff without one", async () => {
    const busy = () => HttpResponse.json({ error: "too many", code: "unavailable", request_id: "r" }, { status: 503, headers: { "Retry-After": "10" } });
    const h = track(start([busy, () => new HttpResponse("no", { status: 503 })]));
    await until(() => h.statuses.length === 2, "refused");
    expect(h.statuses.at(-1)).toEqual({ state: "reconnecting", retryInMs: 10_000 });
    await h.timers.advance(9_999);
    expect(h.requests).toHaveLength(1);
    await h.timers.advance(1);
    await until(() => h.statuses.length === 3, "refused again");
    expect(h.statuses.at(-1)).toEqual({ state: "reconnecting", retryInMs: 2000 });
  });

  it("reconnects after a network error", async () => {
    const h = track(start([() => HttpResponse.error(), sseBody()]));
    await until(() => h.statuses.length === 2, "failed");
    expect(h.statuses.at(-1)).toEqual({ state: "reconnecting", retryInMs: 1000 });
    await h.timers.advance(1000);
    await until(() => h.statuses.at(-1)?.state === "open", "open");
  });

  it("treats 45 s without a byte as a dead stream; any byte resets the timer", async () => {
    const bodies = [sseBody(), sseBody()];
    const h = track(start(bodies));
    await until(() => h.statuses.length === 2, "open");
    await h.timers.advance(DEAD_MS - 1000);
    bodies[0]?.send(": keepalive\n\n");
    await settle();
    await h.timers.advance(DEAD_MS - 1000);
    expect(h.statuses).toHaveLength(2);
    await h.timers.advance(1000);
    await until(() => h.statuses.length === 3, "dead");
    expect(h.statuses.at(-1)).toEqual({ state: "reconnecting", retryInMs: 1000 });
    expect(h.requests[0]?.signal.aborted).toBe(true);
  });

  it("closes without an error or another request", async () => {
    const body = sseBody();
    const h = track(start([body]));
    await until(() => h.statuses.length === 2, "open");
    h.close();
    h.close();
    await settle();
    expect(h.requests[0]?.signal.aborted).toBe(true);
    expect(h.statuses).toHaveLength(2);
    h.refresh();
    await h.timers.advance(60_000);
    expect(h.requests).toHaveLength(1);
    expect(h.timers.pending()).toEqual([]);
  });

  it("closes the stream of a tab hidden for 60 s and reopens it when visible", async () => {
    const bodies = [sseBody(), sseBody()];
    const h = track(start(bodies));
    await until(() => h.statuses.length === 2, "open");
    // The server's keepalives every 15 s keep the stream alive meanwhile.
    const advance = async (ms: number) => {
      for (let left = ms; left > 0; left -= 15_000) {
        bodies[0]?.send(": keepalive\n\n");
        await settle();
        await h.timers.advance(Math.min(left, 15_000));
      }
    };
    h.setVisibility("hidden");
    await advance(HIDDEN_MS - 1);
    h.setVisibility("visible"); // back in time: nothing happens
    h.setVisibility("hidden");
    await advance(HIDDEN_MS - 1);
    expect(h.statuses).toHaveLength(2);
    await h.timers.advance(1);
    expect(h.statuses.at(-1)).toEqual({ state: "paused" });
    expect(h.requests[0]?.signal.aborted).toBe(true);
    h.refresh(); // no stream while paused
    await settle();
    expect(h.requests).toHaveLength(1);
    h.setVisibility("visible");
    await until(() => h.statuses.at(-1)?.state === "open", "open again");
    expect(h.statuses.slice(-2)).toEqual([{ state: "connecting" }, { state: "open" }]);
    expect(h.requests).toHaveLength(2);
  });
});
