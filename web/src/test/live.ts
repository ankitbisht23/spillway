// Live screens under test: a session whose requests reach MSW, the
// control plane's stream as an MSW handler the test writes events to, and
// the latest published config and tenants the filters read.
import { http, HttpResponse } from "msw";
import { vi } from "vitest";

import { createSession, setSession } from "../auth/session";
import { provider } from "./keycloak";
import { server } from "./server";

/** The fake session's access token, which a test can look for. */
export const ACCESS_TOKEN = "test-access-token-not-secret";

/** Makes a session the tab's, as main.tsx does after a login; requests go through MSW. */
export function startSession() {
  const s = createSession(provider, { access: ACCESS_TOKEN, refresh: "refresh-0", idToken: "id-0", expiresIn: 300 }, {
    now: () => 0, setTimer: () => 0, clearTimer: () => {},
    refresh: () => Promise.reject(new Error("invalid_grant")),
    fetch: (r) => fetch(r), channel: null, doc: document, submit: vi.fn(),
  });
  setSession(s);
  return s;
}

/** One stream the screen opened. */
export interface OpenedStream {
  url: URL;
  authorization: string | null;
  signal: AbortSignal;
  send: (event: string, data: unknown) => void;
  /** Ends the body, as a dropped connection does. */
  close: () => void;
}

/**
 * Serves GET /api/v1/stream: each request opens a stream that starts with
 * ready and then sends what the test writes. refuse answers with an error
 * instead.
 */
export function mockStream(refuse?: { status: number; body: Record<string, string> }) {
  const opened: OpenedStream[] = [];
  const enc = new TextEncoder();
  server.use(
    http.get("/api/v1/stream", ({ request }) => {
      if (refuse) return HttpResponse.json(refuse.body, { status: refuse.status });
      let ctl!: ReadableStreamDefaultController<Uint8Array>;
      const body = new ReadableStream<Uint8Array>({
        start: (c) => {
          ctl = c;
          c.enqueue(enc.encode('event: ready\ndata: {"subscriptions":[],"interval_s":5}\n\n'));
        },
      });
      opened.push({
        url: new URL(request.url),
        authorization: request.headers.get("Authorization"),
        signal: request.signal,
        send: (event, data) => ctl.enqueue(enc.encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)),
        close: () => ctl.close(),
      });
      return new HttpResponse(body, { headers: { "Content-Type": "text/event-stream" } });
    }),
  );
  return {
    opened,
    /** The newest stream. */
    get last(): OpenedStream {
      const s = opened[opened.length - 1];
      if (!s) throw new Error("no stream opened");
      return s;
    },
  };
}

/** A result event's data. */
export function result(sub: string, data: unknown, at = "2026-11-16T10:00:05Z") {
  return { sub, at, data };
}

/** A matrix with one series per entry: labels and [unix s, value] points. */
export function matrix(...series: { labels: Record<string, string>; points: [number, number | null][] }[]) {
  return { kind: "matrix", step_s: 15, series };
}

/** Serves the latest published config (version 3) and the tenants. */
export function mockConfig(snapshot: Record<string, unknown> = {}, tenants: string[] = []) {
  server.use(
    http.get("/api/v1/config/versions", () => HttpResponse.json({ items: [{ version: 3 }], next_cursor: null })),
    http.get("/api/v1/config/versions/3", () => HttpResponse.json({ version: 3, snapshot })),
    http.get("/api/v1/tenants", () => HttpResponse.json({ items: tenants.map((name, i) => ({ id: i + 1, name })), next_cursor: null })),
  );
}
