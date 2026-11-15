// The live-data stream's client (UI-03, D5, ADR-0059): fetch with a
// streaming body through the session's authFetch, so the token goes in
// Authorization (never in a URL) and a 401 gets one refresh and one retry
// (ADR-0058). It reconnects with backoff on a network error, an ended body
// or 45 s of silence; at once after every token refresh and after the
// server ends a stream at the token's expiry; after Retry-After on a 503.
// A 400, a 403 or a session that ended stops it with an error. A tab
// hidden for 60 s closes the stream until it is visible again.
import { apiErrorFrom, type ApiError } from "../api/errors";
import type { components, operations } from "../api/schema.gen";
import { authFetch, onSessionChange } from "../auth/session";
import { SseParser, type SseEvent } from "./sse";

export type StreamQueryName = components["schemas"]["StreamQueryName"];
export type StreamParams = Omit<operations["openStream"]["parameters"]["query"], "sub">;
export type StreamResult = components["schemas"]["StreamResult"];
export type StreamError = components["schemas"]["StreamError"];
type StreamEnd = components["schemas"]["StreamEnd"];

export type StreamStatus =
  | { state: "connecting" }
  | { state: "open" }
  /** Dropped: the next attempt is in retryInMs; the last data stays shown. */
  | { state: "reconnecting"; retryInMs: number }
  /** The tab was hidden for a minute: no stream until it is visible. */
  | { state: "paused" }
  /** Stopped for good: a 400 (a bug), a 403, or the session ended. */
  | { state: "failed"; error: ApiError };

export interface StreamHandlers {
  onResult: (r: StreamResult) => void;
  onError: (e: StreamError) => void;
  onStatus: (s: StreamStatus) => void;
}

/** What the client needs from the browser and the session; tests pass fakes. */
export interface StreamDeps {
  /** authFetch: bearer token, refresh before expiry, one refresh and retry on a 401. */
  fetch: (req: Request) => Promise<Response>;
  /** Calls listener on every token refresh and session end. */
  onSessionChange: (listener: () => void) => () => void;
  /** The control plane's origin. */
  origin: string;
  setTimer: (fn: () => void, ms: number) => unknown;
  clearTimer: (t: unknown) => void;
  random: () => number;
  doc: Pick<Document, "visibilityState" | "addEventListener" | "removeEventListener">;
}

export function browserStreamDeps(): StreamDeps {
  return {
    fetch: authFetch,
    onSessionChange,
    origin: location.origin,
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (t) => clearTimeout(t as ReturnType<typeof setTimeout>),
    random: Math.random,
    doc: document,
  };
}

/** Backoff steps after consecutive failures, in ms, the last repeated. */
export const BACKOFF_MS = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000] as const;
/** ±20 % jitter on each backoff step. */
export const JITTER = 0.2;
/** No bytes for this long (three keepalives) is a dead stream. */
export const DEAD_MS = 45_000;
/** A stream open this long resets the backoff. */
export const STABLE_MS = 30_000;
/** A tab hidden this long closes its stream. */
export const HIDDEN_MS = 60_000;

/** The backoff before attempt number attempt (0-based) of a failure run. */
export function backoffMs(attempt: number, random: () => number): number {
  const base = BACKOFF_MS[Math.min(attempt, BACKOFF_MS.length - 1)] ?? 30_000;
  return Math.round(base * (1 - JITTER + 2 * JITTER * random()));
}

/** The query string of a stream: sub repeated, then the set parameters. No token, ever. */
export function streamQuery(subs: readonly StreamQueryName[], params: StreamParams = {}): string {
  const q = new URLSearchParams();
  for (const s of subs) q.append("sub", s);
  for (const [k, v] of Object.entries(params) as [string, string | number | undefined][]) {
    if (v !== undefined) q.set(k, String(v));
  }
  return q.toString();
}

type Outcome =
  | { kind: "backoff" }
  | { kind: "now" }
  | { kind: "after"; ms: number }
  | { kind: "fail"; error: ApiError }
  | { kind: "aborted" };

/**
 * Opens the stream of query (streamQuery's) and keeps it open until the
 * returned function is called, which aborts it without an error.
 */
export function openStream(query: string, h: StreamHandlers, deps: StreamDeps = browserStreamDeps()): () => void {
  const url = `${deps.origin}/api/v1/stream?${query}`;
  let closed = false;
  let paused = false;
  let controller: AbortController | null = null;
  let attempt = 0; // failures since the last stable stream
  let retryTimer: unknown;
  let hiddenTimer: unknown;

  const clear = (t: unknown) => {
    if (t !== undefined) deps.clearTimer(t);
  };

  function connect(first = false) {
    clear(retryTimer);
    retryTimer = undefined;
    if (closed || paused) return;
    controller?.abort();
    const ac = new AbortController();
    controller = ac;
    if (first) h.onStatus({ state: "connecting" });
    void run(ac).then((o) => {
      if (closed || controller !== ac) return;
      settle(o);
    });
  }

  function settle(o: Outcome) {
    switch (o.kind) {
      case "now":
        connect();
        return;
      case "fail":
        h.onStatus({ state: "failed", error: o.error });
        return;
      case "aborted":
        return;
    }
    const ms = o.kind === "after" ? o.ms : backoffMs(attempt, deps.random);
    attempt++;
    h.onStatus({ state: "reconnecting", retryInMs: ms });
    retryTimer = deps.setTimer(() => connect(), ms);
  }

  async function run(ac: AbortController): Promise<Outcome> {
    let deadHit = false;
    let deadTimer: unknown;
    let stableTimer: unknown;
    const armDead = () => {
      clear(deadTimer);
      deadTimer = deps.setTimer(() => {
        deadHit = true;
        ac.abort();
      }, DEAD_MS);
    };
    try {
      const res = await deps.fetch(new Request(url, { headers: { Accept: "text/event-stream" }, signal: ac.signal, cache: "no-store" }));
      if (!res.ok) return await refused(res);
      if (!res.body) return { kind: "backoff" };
      h.onStatus({ state: "open" });
      stableTimer = deps.setTimer(() => {
        attempt = 0;
      }, STABLE_MS);
      armDead();
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      // An abort ends a pending read even where it does not error the body.
      ac.signal.addEventListener("abort", () => void reader.cancel().catch(() => {}), { once: true });
      const parser = new SseParser();
      for (;;) {
        const { value, done } = await reader.read();
        if (ac.signal.aborted) return deadHit ? { kind: "backoff" } : { kind: "aborted" };
        if (done) return { kind: "backoff" };
        armDead();
        for (const e of parser.push(value)) {
          const o = dispatch(e);
          if (o) {
            ac.abort();
            return o;
          }
        }
      }
    } catch {
      return ac.signal.aborted && !deadHit ? { kind: "aborted" } : { kind: "backoff" };
    } finally {
      clear(deadTimer);
      clear(stableTimer);
    }
  }

  async function refused(res: Response): Promise<Outcome> {
    let body: unknown;
    try {
      body = await res.json();
    } catch {
      body = undefined;
    }
    const error = apiErrorFrom(res, body);
    if (res.status === 400 || res.status === 401 || res.status === 403) return { kind: "fail", error };
    if (res.status === 503 && error.retryAfter !== undefined) return { kind: "after", ms: error.retryAfter * 1000 };
    return { kind: "backoff" };
  }

  // An event: results and errors go to the handlers; end decides the
  // reconnect. Malformed data is skipped.
  function dispatch(e: SseEvent): Outcome | null {
    let data: unknown;
    try {
      data = JSON.parse(e.data);
    } catch {
      return null;
    }
    if (typeof data !== "object" || data === null) return null;
    switch (e.event) {
      case "result":
        h.onResult(data as StreamResult);
        return null;
      case "error":
        h.onError(data as StreamError);
        return null;
      case "end":
        return (data as StreamEnd).reason === "token_expired" ? { kind: "now" } : { kind: "backoff" };
    }
    return null;
  }

  function onVisibility() {
    if (deps.doc.visibilityState === "hidden") {
      clear(hiddenTimer);
      hiddenTimer = deps.setTimer(() => {
        hiddenTimer = undefined;
        paused = true;
        clear(retryTimer);
        retryTimer = undefined;
        controller?.abort();
        controller = null;
        h.onStatus({ state: "paused" });
      }, HIDDEN_MS);
      return;
    }
    clear(hiddenTimer);
    hiddenTimer = undefined;
    if (paused) {
      paused = false;
      attempt = 0;
      connect(true);
    }
  }

  // Each refresh brings a new token: reconnect with it at once (D5). A
  // session that ended makes authFetch answer 401 without a request.
  const unsubscribe = deps.onSessionChange(() => {
    if (closed || paused) return;
    attempt = 0;
    connect();
  });
  deps.doc.addEventListener("visibilitychange", onVisibility);
  connect(true);

  return () => {
    if (closed) return;
    closed = true;
    unsubscribe();
    deps.doc.removeEventListener("visibilitychange", onVisibility);
    clear(retryTimer);
    clear(hiddenTimer);
    controller?.abort();
    controller = null;
  };
}
