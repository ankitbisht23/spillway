import { describe, expect, it, vi } from "vitest";

import { ApiError } from "../../api/errors";
import { openRunStream, RECONNECT_MS, runStreamUrl, type RunStreamDeps } from "./runStream";

/** xs[i], which must exist. */
function nth<T>(xs: readonly T[], i: number): T {
  const x = xs[i];
  if (x === undefined) throw new Error(`no item ${i}`);
  return x;
}


const enc = new TextEncoder();

/** A response whose body sends frames, then ends unless open. */
function sse(frames: string[], open = false): Response {
  return new Response(new ReadableStream<Uint8Array>({
    start: (c) => {
      for (const f of frames) c.enqueue(enc.encode(f));
      if (!open) c.close();
    },
  }), { headers: { "Content-Type": "text/event-stream" } });
}

const ev = (name: string, data: unknown) => `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;

function deps(answers: (() => Response | Promise<Response>)[]) {
  const requests: Request[] = [];
  const timers: { fn: () => void; ms: number }[] = [];
  const d: RunStreamDeps = {
    fetch: async (req) => {
      requests.push(req);
      const a = answers.shift();
      if (!a) throw new TypeError("no answer");
      return a();
    },
    origin: "https://cp.example",
    setTimer: (fn, ms) => timers.push({ fn, ms }),
    clearTimer: () => {},
  };
  return { d, requests, timers };
}

const handlers = () => ({ onReady: vi.fn(), onTick: vi.fn(), onEnd: vi.fn(), onError: vi.fn() });

describe("a Playground run's stream (ADR-0096 §3)", () => {
  it("asks the control plane's own origin for the run, with no token in the URL, and hands over ready, ticks and end", async () => {
    const { d, requests } = deps([() => sse([ev("ready", { id: "r1" }), ": keepalive\n\n", ev("tick", { elapsed_s: 1 }), "event: tick\ndata: not json\n\n", ev("end", { reason: "done", totals: {} })])]);
    const h = handlers();
    openRunStream("r1", h, d);
    await vi.waitFor(() => expect(h.onEnd).toHaveBeenCalledWith({ reason: "done", totals: {} }));
    expect(requests.map((r) => r.url)).toEqual(["https://cp.example/api/v1/demo/playground/runs/r1/stream"]);
    expect(runStreamUrl("https://cp.example", "a/b")).toBe("https://cp.example/api/v1/demo/playground/runs/a%2Fb/stream");
    expect(h.onReady).toHaveBeenCalledWith({ id: "r1" });
    expect(h.onTick).toHaveBeenCalledTimes(1);
    expect(h.onError).not.toHaveBeenCalled();
  });

  it("reconnects at once after token_expired and with backoff after a drop, at most 3 times", async () => {
    const { d, requests, timers } = deps([
      () => sse([ev("end", { reason: "token_expired", totals: {} })]),
      () => sse([ev("tick", { elapsed_s: 2 })]),
    ]);
    const h = handlers();
    openRunStream("r1", h, d);
    await vi.waitFor(() => expect(timers).toHaveLength(1));
    expect(requests).toHaveLength(2);
    expect(nth(timers, 0).ms).toBe(RECONNECT_MS[0]);
    for (let i = 0; i < 3; i++) {
      nth(timers, timers.length - 1).fn();
      await new Promise((r) => setTimeout(r, 10));
    }
    await vi.waitFor(() => expect(h.onError).toHaveBeenCalled());
    expect(timers.map((t) => t.ms)).toEqual([...RECONNECT_MS]);
    expect(h.onEnd).not.toHaveBeenCalled();
  });

  it("stops with the API's error on a refused stream, and stays closed once closed", async () => {
    const { d } = deps([() => Response.json({ error: "no such run", code: "not_found", request_id: "x" }, { status: 404 })]);
    const h = handlers();
    openRunStream("r1", h, d);
    await vi.waitFor(() => expect(h.onError).toHaveBeenCalled());
    const err = nth(h.onError.mock.calls, 0)[0] as ApiError;
    expect(err.status).toBe(404);

    const open = deps([() => sse([ev("ready", { id: "r2" })], true)]);
    const h2 = handlers();
    const close = openRunStream("r2", h2, open.d);
    await vi.waitFor(() => expect(h2.onReady).toHaveBeenCalled());
    close();
    close();
    await new Promise((r) => setTimeout(r, 10));
    expect(open.timers).toHaveLength(0);
    expect(h2.onError).not.toHaveBeenCalled();
  });
});
