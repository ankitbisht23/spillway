import { act, renderHook, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "../test/server";
import type { StreamDeps, StreamParams, StreamQueryName } from "./client";
import { useSubscription } from "./useSubscription";

const ORIGIN = "http://cp.test";

/** The streams the test has opened: their URL, abort signal and writer. */
interface Opened {
  url: string;
  signal: AbortSignal;
  send: (name: string, data: unknown) => void;
}

function serve(opened: Opened[]) {
  const enc = new TextEncoder();
  server.use(
    http.get(`${ORIGIN}/api/v1/stream`, ({ request }) => {
      let ctl!: ReadableStreamDefaultController<Uint8Array>;
      const body = new ReadableStream<Uint8Array>({
        start: (c) => {
          ctl = c;
          c.enqueue(enc.encode('event: ready\ndata: {"subscriptions":[],"interval_s":5}\n\n'));
        },
      });
      opened.push({
        url: request.url,
        signal: request.signal,
        send: (name, data) => ctl.enqueue(enc.encode(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`)),
      });
      return new HttpResponse(body, { headers: { "Content-Type": "text/event-stream" } });
    }),
  );
}

const deps = (): StreamDeps => ({
  fetch: (req) => fetch(req),
  onSessionChange: () => () => {},
  origin: ORIGIN,
  setTimer: (fn, ms) => setTimeout(fn, ms),
  clearTimer: (t) => clearTimeout(t as ReturnType<typeof setTimeout>),
  random: () => 0.5,
  doc: document,
});

const matrix = { kind: "matrix" as const, step_s: 15, series: [] };

describe("useSubscription", () => {
  it("keeps the latest result per subscription and an error beside it", async () => {
    const opened: Opened[] = [];
    serve(opened);
    const { result } = renderHook(() => useSubscription(["overview.requests", "replicas"], { range: "1h" }, deps));
    expect(result.current.status).toEqual({ state: "connecting" });
    await waitFor(() => expect(result.current.status).toEqual({ state: "open" }));
    expect(opened[0]?.url).toBe(`${ORIGIN}/api/v1/stream?sub=overview.requests&sub=replicas&range=1h`);
    const r1 = { sub: "overview.requests", at: "t1", data: matrix };
    const e2 = { sub: "overview.requests", at: "t2", code: "prometheus_unavailable", message: "Prometheus is unavailable" };
    const r3 = { sub: "overview.requests", at: "t3", data: matrix };
    act(() => opened[0]?.send("result", r1));
    await waitFor(() => expect(result.current.subs["overview.requests"]).toEqual({ result: r1 }));
    act(() => opened[0]?.send("error", e2));
    await waitFor(() => expect(result.current.subs["overview.requests"]).toEqual({ result: r1, error: e2 }));
    act(() => opened[0]?.send("result", r3));
    await waitFor(() => expect(result.current.subs["overview.requests"]).toEqual({ result: r3 }));
    expect(result.current.subs.replicas).toBeUndefined();
  });

  it("opens a new stream, empty, when the parameters change, and closes the old one", async () => {
    const opened: Opened[] = [];
    serve(opened);
    const { result, rerender } = renderHook(
      ({ subs, params }: { subs: StreamQueryName[]; params: StreamParams }) => useSubscription(subs, params, deps),
      { initialProps: { subs: ["overview.requests"], params: { route: "r1" } } },
    );
    await waitFor(() => expect(result.current.status).toEqual({ state: "open" }));
    act(() => opened[0]?.send("result", { sub: "overview.requests", at: "t1", data: matrix }));
    await waitFor(() => expect(result.current.subs["overview.requests"]).toBeDefined());
    rerender({ subs: ["overview.requests"], params: { route: "r1" } }); // the same query: no new stream
    rerender({ subs: ["overview.requests"], params: { route: "orders" } });
    expect(result.current.subs).toEqual({});
    await waitFor(() => expect(opened).toHaveLength(2));
    expect(opened[0]?.signal.aborted).toBe(true);
    expect(opened[1]?.url).toContain("route=orders");
    await waitFor(() => expect(result.current.status).toEqual({ state: "open" }));
  });

  it("closes the stream on unmount without an error", async () => {
    const opened: Opened[] = [];
    serve(opened);
    const { result, unmount } = renderHook(() => useSubscription(["replicas"], {}, deps));
    await waitFor(() => expect(result.current.status).toEqual({ state: "open" }));
    unmount();
    await waitFor(() => expect(opened[0]?.signal.aborted).toBe(true));
  });
});
