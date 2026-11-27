// The leak guard (ADR-0061), used by every test automatically, and the
// login helper. The guard collects every token Keycloak hands the page and
// every API key the API returns, watches every URL the browser requests,
// navigates to or puts in history, and the console; after the test it
// asserts that no token, JWT or key is in Web Storage, a cookie or a URL,
// that the app set no cookie and left sessionStorage empty, and that the
// control plane's pages logged no error but the demo probe's 404 (below).
// With the option leakReport set
// to a path, it also writes what it checked and found there (make
// verify-m6 prints it, ADR-0061), before it asserts.
import { writeFileSync } from "node:fs";

import { test as base, expect, type Page } from "@playwright/test";

import { APP, DEV_PASSWORD, TOKEN_ENDPOINT } from "./env";

const JWT = /eyJ[\w-]+\.[\w-]+\.[\w-]+/;
const API_KEY = /spw_[\w-]+/;
const JWT_ALL = new RegExp(JWT.source, "g");
const API_KEY_ALL = new RegExp(API_KEY.source, "g");
const KEYCLOAK_COOKIE_PATH = "/auth/realms/spillway/";
const appOrigin = new URL(APP).origin;
/**
 * The demo probe (SC-08, ADR-0095 §2): each session start asks GET
 * /api/v1/demo, which answers 404 by design while demo mode is off, as on
 * the compose stack; Chromium logs that load as a console error. Only that
 * status at that URL is excused; demo.spec.ts checks the probe itself.
 */
export const DEMO_PROBE = new URL("/api/v1/demo", APP).href;

function onApp(url: string): boolean {
  try {
    return new URL(url).origin === appOrigin;
  } catch {
    return false;
  }
}

export interface LeakGuard {
  /** Failed loads a test provokes on purpose: Chromium logs each as a console error. */
  expectFailedLoad(status: number, path: string): void;
}

export const test = base.extend<{ leakGuard: LeakGuard; leakReport: string }>({
  /** A path for the guard's report (JSON); "" for none. */
  leakReport: ["", { option: true }],
  leakGuard: [async ({ context, leakReport }, use) => {
    const secrets = new Set<string>();
    const urls: { where: "request" | "navigation" | "history"; url: string }[] = [];
    const errors: string[] = [];
    const excused: { status: number; url: string }[] = [{ status: 404, url: DEMO_PROBE }];
    const reading: Promise<void>[] = [];

    await context.exposeBinding("__spillwayHistoryUrl", (_src, url: string) => void urls.push({ where: "history", url }));
    await context.addInitScript(() => {
      for (const m of ["pushState", "replaceState"] as const) {
        const orig = history[m];
        history[m] = function (this: History, data: unknown, unused: string, url?: string | URL | null) {
          if (url !== undefined && url !== null) {
            (window as unknown as { __spillwayHistoryUrl: (u: string) => void }).__spillwayHistoryUrl(new URL(url, location.href).href);
          }
          return orig.call(this, data, unused, url);
        };
      }
    });

    /** s with every token and key replaced, so a failure message never prints one. */
    const redact = (s: string) => [...secrets].reduce((acc, t) => acc.split(t).join("<token>"), s).replace(JWT_ALL, "<jwt>").replace(API_KEY_ALL, "<key>");

    const watch = (page: Page) => {
      page.on("request", (r) => void urls.push({ where: "request", url: r.url() }));
      page.on("framenavigated", (f) => {
        if (f === page.mainFrame()) urls.push({ where: "navigation", url: f.url() });
      });
      page.on("response", (r) => {
        const url = r.url();
        if (url !== TOKEN_ENDPOINT && !(onApp(url) && new URL(url).pathname.startsWith("/api/v1/"))) return;
        // The live stream's body never ends while the page is open, and it
        // carries no token or key (ADR-0059): its URL is still watched.
        if ((r.headers()["content-type"] ?? "").startsWith("text/event-stream")) return;
        reading.push(r.text().then((body) => {
          for (const m of body.matchAll(/"(access_token|refresh_token|id_token|key)"\s*:\s*"([^"]+)"/g)) {
            if (m[1] !== "key" || API_KEY.test(m[2] ?? "")) secrets.add(m[2] ?? "");
          }
        }, () => {}));
      });
      page.on("console", (m) => {
        if (m.type() !== "error") return;
        const line = `${m.text()} (${m.location().url})`;
        if (onApp(page.url())) errors.push(line);
        else console.log(`[not judged: ${new URL(page.url()).origin}] console error: ${redact(line)}`);
      });
      page.on("pageerror", (e) => {
        if (onApp(page.url())) errors.push(`page error: ${e.message}`);
        else console.log(`[not judged] page error: ${redact(e.message)}`);
      });
    };
    context.pages().forEach(watch);
    context.on("page", watch);

    await use({
      expectFailedLoad(status, path) {
        excused.push({ status, url: new URL(path, APP).href });
      },
    });

    await Promise.all(reading);
    const leaks = (s: string) => [...secrets].some((t) => s.includes(t)) || JWT.test(s) || API_KEY.test(s);
    const findings: string[] = [];
    const found = (bad: boolean, what: string) => void (bad && findings.push(what));
    const checked = { request_urls: 0, navigations: 0, history_entries: 0, pages_storage: 0, cookies: 0 };
    const kind = { request: "request_urls", navigation: "navigations", history: "history_entries" } as const;

    for (const u of urls) {
      checked[kind[u.where]]++;
      found(leaks(u.url), `a token or key in a ${u.where} URL: ${redact(u.url)}`);
    }
    for (const u of urls.filter((x) => x.where === "history")) found(new URL(u.url).searchParams.has("code"), `code= in a history entry: ${redact(u.url)}`);

    for (const page of context.pages()) {
      found(new URL(page.url()).searchParams.has("code"), `code= still in the address bar: ${redact(page.url())}`);
      if (!onApp(page.url())) continue;
      checked.pages_storage++;
      const storage = await page.evaluate(() => ({ local: JSON.stringify({ ...localStorage }), session: sessionStorage.length }));
      found(leaks(storage.local), "a token or key in localStorage");
      found(storage.session !== 0, `sessionStorage once the app has loaded: ${storage.session} items`);
    }

    for (const c of await context.cookies()) {
      checked.cookies++;
      found(!c.path.startsWith(KEYCLOAK_COOKIE_PATH), `a cookie that is not Keycloak's: ${c.name} on ${c.path}`);
      found([...secrets].some((t) => c.value.includes(t)) || API_KEY.test(c.value), `a token or key in cookie ${c.name}`);
    }

    const judged = errors.filter((e) => !excused.some((x) =>
      e.startsWith(`Failed to load resource: the server responded with a status of ${x.status}`) && e.includes(`(${x.url})`))).map(redact);
    if (leakReport) {
      writeFileSync(leakReport, JSON.stringify({ checked, secrets_seen: secrets.size, findings, console_errors: judged }, null, 2));
    }
    expect(secrets.size, "the guard saw the session's tokens").toBeGreaterThan(0);
    expect(findings, "tokens, keys or the code in storage, cookies or URLs").toEqual([]);
    expect(judged, "console errors on the control plane's pages").toEqual([]);
  }, { auto: true }],
});

export { expect };

/** Logs user in through Keycloak's form, starting at path (with its query, if any). */
export async function login(page: Page, user: string, path = "/overview") {
  await page.goto(path);
  await page.locator("#username").fill(user);
  await page.locator("#password").fill(DEV_PASSWORD);
  await page.locator("#kc-login").click();
  await page.waitForURL((u) => onApp(u.href) && u.pathname + u.search === path);
  await expect(page.getByRole("navigation", { name: "Screens" })).toBeVisible();
}

/**
 * Clicks Sign out and waits for Keycloak's redirect to load /signed-out:
 * the tab shows Signed out at once, before its logout POST navigates.
 */
export async function signOut(page: Page) {
  const back = page.waitForResponse((r) => r.url() === `${APP}/signed-out` && r.request().isNavigationRequest());
  await page.getByRole("button", { name: "Sign out" }).click();
  expect((await back).status()).toBe(200);
  await page.waitForLoadState();
  await expect(page.getByRole("heading", { name: "Signed out" })).toBeVisible();
}
