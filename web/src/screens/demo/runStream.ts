// A Playground run's stream (UI-07, ADR-0096 §3): GET
// /api/v1/demo/playground/runs/{id}/stream with fetch through the
// session's authFetch, so the token goes in Authorization (never in a URL)
// and only to the control plane's own origin. Events: ready (the run),
// tick (one second), end. A dropped connection before end reconnects up
// to 3 times (1 s, 2 s, 4 s); end with token_expired reconnects at once
// (the run goes on). A 403, 404 or 429 stops it with the error.
import { apiErrorFrom, networkError, type ApiError } from "../../api/errors";
import type { components } from "../../api/schema.gen";
import { authFetch } from "../../auth/session";
import { SseParser } from "../../stream/sse";

export type PlaygroundRun = components["schemas"]["PlaygroundRun"];
export type PlaygroundTick = components["schemas"]["PlaygroundTick"];
export type PlaygroundEnd = components["schemas"]["PlaygroundEnd"];

export interface RunHandlers {
  onReady: (run: PlaygroundRun) => void;
  onTick: (tick: PlaygroundTick) => void;
  onEnd: (end: PlaygroundEnd) => void;
  onError: (err: ApiError) => void;
}

export interface RunStreamDeps {
  fetch: (req: Request) => Promise<Response>;
  origin: string;
  setTimer: (fn: () => void, ms: number) => unknown;
  clearTimer: (t: unknown) => void;
}

export function browserRunDeps(): RunStreamDeps {
  return {
    fetch: authFetch,
    origin: location.origin,
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (t) => clearTimeout(t as ReturnType<typeof setTimeout>),
  };
}

/** Reconnect delays after a dropped connection, in ms. */
export const RECONNECT_MS = [1_000, 2_000, 4_000] as const;

/** The stream's URL: same origin, the run's ID only. */
export function runStreamUrl(origin: string, id: string): string {
  return `${origin}/api/v1/demo/playground/runs/${encodeURIComponent(id)}/stream`;
}

/** Opens run id's stream; the returned function closes it without an error. */
export function openRunStream(id: string, h: RunHandlers, deps: RunStreamDeps = browserRunDeps()): () => void {
  let closed = false;
  let controller: AbortController | null = null;
  let timer: unknown;
  let drops = 0;

  function connect() {
    timer = undefined;
    if (closed) return;
    const ac = new AbortController();
    controller = ac;
    void run(ac).then((outcome) => {
      if (closed || controller !== ac) return;
      if (outcome === "done") return;
      if (outcome === "now") {
        connect();
        return;
      }
      const ms = RECONNECT_MS[drops];
      drops++;
      if (ms === undefined) {
        h.onError(networkError());
        return;
      }
      timer = deps.setTimer(connect, ms);
    });
  }

  async function run(ac: AbortController): Promise<"done" | "now" | "drop"> {
    try {
      const res = await deps.fetch(new Request(runStreamUrl(deps.origin, id), { headers: { Accept: "text/event-stream" }, signal: ac.signal, cache: "no-store" }));
      if (!res.ok) {
        let body: unknown;
        try {
          body = await res.json();
        } catch {
          body = undefined;
        }
        h.onError(apiErrorFrom(res, body));
        return "done";
      }
      if (!res.body) return "drop";
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      ac.signal.addEventListener("abort", () => void reader.cancel().catch(() => {}), { once: true });
      const parser = new SseParser();
      for (;;) {
        const { value, done } = await reader.read();
        if (ac.signal.aborted) return "done";
        if (done) return "drop";
        for (const e of parser.push(value)) {
          let data: unknown;
          try {
            data = JSON.parse(e.data);
          } catch {
            continue;
          }
          if (typeof data !== "object" || data === null) continue;
          if (e.event === "ready") h.onReady(data as PlaygroundRun);
          else if (e.event === "tick") h.onTick(data as PlaygroundTick);
          else if (e.event === "end") {
            const end = data as PlaygroundEnd;
            if (end.reason === "token_expired") return "now";
            h.onEnd(end);
            return "done";
          }
        }
      }
    } catch {
      return ac.signal.aborted ? "done" : "drop";
    }
  }

  connect();
  return () => {
    if (closed) return;
    closed = true;
    if (timer !== undefined) deps.clearTimer(timer);
    controller?.abort();
    controller = null;
  };
}
