// What a page load does before anything renders (UI-08, ADR-0058): the
// runtime config and discovery, then Signed out, the callback, or a
// redirect to Keycloak. A reload starts here with empty memory; Keycloak's
// SSO session makes the login a redirect without a form.
import {
  beginLogin, CALLBACK_PATH, completeLogin, discover, loadUiConfig, PENDING_KEY, SIGNED_OUT_PATH,
  type Provider, type UiConfig,
} from "./oidc";
import { browserDeps, createSession, type Session, type SessionDeps } from "./session";

export type Boot =
  | { kind: "redirecting" }
  | { kind: "unavailable" }
  | { kind: "signed-out"; provider: Provider }
  | { kind: "sign-in-failed"; provider: Provider; reason: string }
  | { kind: "session"; provider: Provider; session: Session };

export interface BootDeps {
  loadConfig: () => Promise<UiConfig>;
  discover: (cfg: UiConfig) => Promise<Provider>;
  location: Pick<Location, "href" | "pathname" | "search">;
  history: Pick<History, "replaceState">;
  storage: Storage;
  /** location.assign, which jsdom cannot do. */
  navigate: (url: string) => void;
  sessionDeps: () => SessionDeps;
}

export function browserBootDeps(): BootDeps {
  return {
    loadConfig: loadUiConfig,
    discover: (cfg) => discover(cfg),
    location,
    history,
    storage: sessionStorage,
    navigate: (url) => location.assign(url),
    sessionDeps: browserDeps,
  };
}

/** Starts a login that comes back to returnTo. */
export async function signIn(provider: Provider, returnTo: string, deps: Pick<BootDeps, "storage" | "navigate">): Promise<void> {
  deps.navigate((await beginLogin(provider, returnTo, deps.storage)).href);
}

export async function boot(deps: BootDeps = browserBootDeps()): Promise<Boot> {
  const { pathname, search, href } = deps.location;
  // At the callback the code and state leave the address bar before
  // anything else, even when discovery then fails (ADR-0058).
  const callback = pathname === CALLBACK_PATH ? new URL(href) : null;
  if (callback) deps.history.replaceState(null, "", CALLBACK_PATH);
  let provider: Provider;
  try {
    provider = await deps.discover(await deps.loadConfig());
  } catch {
    return { kind: "unavailable" };
  }
  if (callback) {
    const r = await completeLogin(provider, callback, deps.storage, deps.history);
    if (!r.ok) return { kind: "sign-in-failed", provider, reason: r.reason };
    deps.history.replaceState(null, "", r.returnTo);
    return { kind: "session", provider, session: createSession(provider, r.tokens, deps.sessionDeps()) };
  }
  if (pathname === SIGNED_OUT_PATH) {
    deps.storage.removeItem(PENDING_KEY); // an abandoned login's
    return { kind: "signed-out", provider };
  }
  await signIn(provider, pathname + search, deps);
  return { kind: "redirecting" };
}
