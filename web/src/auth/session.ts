// The in-memory session (UI-08, ADR-0058): the access, refresh and ID
// tokens live only in a Session object, never in Web Storage, cookies,
// React state, the query cache, a URL or a log. Only authFetch (API calls)
// and the stream client (ADR-0059) read the access token.
import { isTransientRefreshError, logoutForm, refreshTokens, type Provider, type Tokens } from "./oidc";

export type SessionStatus = "active" | "ended" | "signed-out";

/** What a session needs from the browser; tests pass fakes. */
export interface SessionDeps {
  /** Milliseconds for durations only (performance.now), never compared with a token's exp. */
  now: () => number;
  setTimer: (fn: () => void, ms: number) => unknown;
  clearTimer: (t: unknown) => void;
  refresh: (p: Provider, refreshToken: string) => Promise<Tokens>;
  fetch: (req: Request) => Promise<Response>;
  /** The sign-out broadcast between tabs; it carries no token and no user data. */
  channel: Pick<BroadcastChannel, "postMessage" | "close"> & { onmessage: ((e: MessageEvent) => void) | null } | null;
  doc: Document;
  /** Submits the logout form (a navigation away). */
  submit: (form: HTMLFormElement) => void;
}

export function browserDeps(): SessionDeps {
  return {
    now: () => performance.now(),
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (t) => clearTimeout(t as ReturnType<typeof setTimeout>),
    refresh: refreshTokens,
    fetch: (req) => fetch(req),
    channel: typeof BroadcastChannel === "undefined" ? null : new BroadcastChannel(CHANNEL),
    doc: document,
    submit: (form) => form.submit(),
  };
}

export const CHANNEL = "spillway-session";
export const SIGNED_OUT_MESSAGE = "signed-out";
/** authFetch refreshes first when fewer than this many ms remain. */
export const MIN_VALIDITY_MS = 30_000;
/** The first retry of a refresh that got no answer or a 5xx; then doubled. */
export const FIRST_RETRY_MS = 5_000;

/** When to refresh: max(expires_in − 60 s, expires_in / 2), in ms. */
export function refreshDelayMs(expiresIn: number): number {
  return 1000 * Math.max(expiresIn - 60, expiresIn / 2);
}

export interface Session {
  status(): SessionStatus;
  /** Calls listener on every change of status or token. */
  subscribe(listener: () => void): () => void;
  /** The current access token: for authFetch and the stream client only. */
  accessToken(): string | null;
  /** One refresh at a time; true when new tokens arrived. */
  refresh(): Promise<boolean>;
  /** fetch with the bearer token; a 401 refreshes once and retries once. */
  fetch(req: Request): Promise<Response>;
  signOut(): void;
  dispose(): void;
}

/** The answer authFetch gives without a session, so no request is sent. */
export function sessionEndedResponse(): Response {
  return Response.json({ error: "your session ended: sign in again", code: "unauthenticated", request_id: "" }, { status: 401 });
}

export function createSession(p: Provider, first: Tokens, deps: SessionDeps): Session {
  let tokens: Tokens | null = null;
  let status: SessionStatus = "active";
  let expiresAt = 0;
  let refreshAt = 0;
  let timer: unknown = undefined;
  let backoff = 0;
  let inflight: Promise<boolean> | null = null;
  const listeners = new Set<() => void>();
  const notify = () => listeners.forEach((l) => l());

  function schedule(ms: number) {
    if (timer !== undefined) deps.clearTimer(timer);
    refreshAt = deps.now() + ms;
    timer = deps.setTimer(() => {
      timer = undefined;
      void refresh();
    }, ms);
  }

  function accept(t: Tokens) {
    // A refresh answer may leave out what did not change.
    tokens = { ...t, refresh: t.refresh ?? tokens?.refresh, idToken: t.idToken ?? tokens?.idToken };
    expiresAt = deps.now() + t.expiresIn * 1000;
    backoff = 0;
    schedule(refreshDelayMs(t.expiresIn));
    notify();
  }

  function drop() {
    tokens = null;
    if (timer !== undefined) deps.clearTimer(timer);
    timer = undefined;
  }

  function end(next: Exclude<SessionStatus, "active"> = "ended") {
    if (status !== "active") return;
    drop();
    status = next;
    notify();
  }

  async function attempt(): Promise<boolean> {
    if (status !== "active") return false;
    const rt = tokens?.refresh;
    if (!rt) {
      end();
      return false;
    }
    try {
      const t = await deps.refresh(p, rt);
      if (status !== "active") return false;
      accept(t);
      return true;
    } catch (err) {
      if (status !== "active") return false;
      const now = deps.now();
      if (!isTransientRefreshError(err) || now >= expiresAt) {
        end();
        return false;
      }
      // No answer or a 5xx: again after 5, 10, 20, … s, the last at expiry.
      backoff = backoff ? backoff * 2 : FIRST_RETRY_MS;
      schedule(Math.min(backoff, expiresAt - now));
      return false;
    }
  }

  function refresh(): Promise<boolean> {
    inflight ??= attempt().finally(() => {
      inflight = null;
    });
    return inflight;
  }

  function withToken(req: Request, token: string): Request {
    const r = new Request(req);
    r.headers.set("Authorization", `Bearer ${token}`);
    return r;
  }

  async function authFetch(req: Request): Promise<Response> {
    // A tab whose timers were throttled in the background.
    if (status === "active" && expiresAt - deps.now() < MIN_VALIDITY_MS) await refresh();
    if (status !== "active" || !tokens) return sessionEndedResponse();
    const used = tokens.access;
    const copy = req.clone(); // the body, for the one retry
    const first = await deps.fetch(withToken(req, used));
    if (first.status !== 401) return first;
    const fresh = tokens?.access !== used || (await refresh());
    if (!fresh || !tokens) {
      end();
      return first;
    }
    const second = await deps.fetch(withToken(copy, tokens.access));
    if (second.status === 401) end();
    return second;
  }

  function onVisible() {
    if (deps.doc.visibilityState === "visible" && status === "active" && deps.now() >= refreshAt) void refresh();
  }

  function signOut() {
    const idToken = tokens?.idToken;
    end("signed-out");
    deps.channel?.postMessage(SIGNED_OUT_MESSAGE);
    const form = logoutForm(p, idToken, deps.doc);
    if (form) {
      deps.doc.body.append(form);
      deps.submit(form);
    }
  }

  if (deps.channel) {
    deps.channel.onmessage = (e: MessageEvent) => {
      if (e.data === SIGNED_OUT_MESSAGE) end("signed-out");
    };
  }
  deps.doc.addEventListener("visibilitychange", onVisible);
  accept(first);

  return {
    status: () => status,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    accessToken: () => tokens?.access ?? null,
    refresh,
    fetch: authFetch,
    signOut,
    dispose() {
      drop();
      deps.channel?.close();
      deps.doc.removeEventListener("visibilitychange", onVisible);
      listeners.clear();
    },
  };
}

let current: Session | null = null;

/** Makes s the tab's session (main.tsx, after a login). */
export function setSession(s: Session | null) {
  current = s;
}

/** fetch for the API client: the current session's, or a local 401 without one. */
export function authFetch(req: Request): Promise<Response> {
  return current ? current.fetch(req) : Promise.resolve(sessionEndedResponse());
}
