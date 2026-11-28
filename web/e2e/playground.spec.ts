// The Playground on the demo stack (UI-07, SC-04, SC-03, ADR-0096 §3):
// make test-e2e-demo runs it with playwright.demo.config.ts against make
// demo (demo mode on, three managed gateways, the seed). A viewer fires
// free-co's traffic at 30/s on orders, three times its plan's 10/s, and
// sees allowed and denied requests, the RateLimit headers and the token
// bucket over the run's stream, with no fault switches; an admin switches
// D6's fault on and off. Every request the app makes after the login goes
// to its own origin (the control plane), apart from the token endpoint
// should a refresh fall due; nothing on the page logs to the console, and
// no key, token or JWT is in the DOM. The leak guard checks storage,
// cookies and URLs as for every flow.
//
// Timeouts, set before any run (ADR-0061's stall rule, no retries): a run
// of 10 s ends within 10 s + 5 s (the last requests' timeout) + 1 s;
// RUN_MS is twice that plus the start (a new key waits up to 10 s for
// every gateway), so a stall of 820 ms (ADR-0034) cannot fail it. A fault
// switch is one toxiproxy round trip; the screen reads it back at once.
import type { Page } from "@playwright/test";

import { APP, TOKEN_ENDPOINT } from "./env";
import { expect, login, test } from "./fixtures";

const RUN_MS = 45_000;
const JWT = /eyJ[\w-]+\.[\w-]+\.[\w-]+/;

/** Records every request the page makes from now on, and every console message on the app's pages. */
function record(page: Page) {
  const requests: string[] = [];
  const console: string[] = [];
  page.on("request", (r) => void requests.push(r.url()));
  page.on("console", (m) => {
    if (new URL(page.url()).origin === new URL(APP).origin) console.push(`${m.type()}: ${m.text()}`);
  });
  return { requests, console };
}

/** The numbers of a card's "Whole run" line. */
async function totals(page: Page, tenant: string) {
  const text = await page.getByRole("article", { name: tenant }).locator(".totals").innerText();
  const m = /Whole run: (\d+) sent, (\d+) allowed, (\d+) denied, (\d+) forbidden, (\d+) failed, (\d+) other/.exec(text);
  expect(m, text).not.toBeNull();
  const n = (i: number) => Number(m?.[i] ?? NaN);
  return { sent: n(1), allowed: n(2), denied: n(3), forbidden: n(4), failed: n(5), other: n(6) };
}

async function noSecretsOnPage(page: Page) {
  const html = await page.content();
  expect(html).not.toMatch(/spw_/);
  expect(html).not.toMatch(JWT);
}

test("test-viewer fires traffic and sees allowed vs denied, same origin only, with no fault switches", async ({ page }) => {
  await login(page, "test-viewer", "/playground");
  const seen = record(page);
  await expect(page.getByRole("heading", { name: "Playground" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Faults" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /Switch (on|off)/ })).toHaveCount(0);

  await page.getByLabel("Tenant").selectOption("free-co");
  await page.getByLabel("Route").selectOption("orders");
  await page.getByLabel(/^Rate:/).fill("30");
  await page.getByLabel("Duration (seconds)").fill("10");
  await page.getByRole("button", { name: "Fire" }).click();

  const card = page.getByRole("article", { name: "free-co" });
  await expect(card.getByText(/"consumer-plan";r=\d+;t=\d+/).first()).toBeVisible({ timeout: RUN_MS });
  await expect(card.getByRole("meter", { name: "free-co token bucket" })).toBeVisible();
  await expect(page.getByText("Ended (done).")).toBeVisible({ timeout: RUN_MS });
  const t = await totals(page, "free-co");
  // 300 requests at 30/s against 10/s with a burst of 20: about 120 allowed.
  // Each GCRA burst holds two seconds of the rate, so a stall does not
  // change the counts by itself (ADR-0094 §3).
  expect(t.sent).toBe(300);
  expect(t.allowed).toBeGreaterThanOrEqual(80);
  expect(t.allowed).toBeLessThanOrEqual(160);
  expect(t.denied).toBeGreaterThan(100);
  expect(t.allowed + t.denied).toBe(300);
  expect(t.forbidden + t.failed + t.other).toBe(0);
  await expect(page.getByText(/Requests per replica: gateway-1 \d+, gateway-2 \d+, gateway-3 \d+/)).toBeVisible();

  const offOrigin = seen.requests.filter((u) => new URL(u).origin !== new URL(APP).origin && u !== TOKEN_ENDPOINT);
  expect(offOrigin, "requests to another origin").toEqual([]);
  expect(seen.requests.some((u) => u.startsWith(`${APP}/api/v1/demo/playground/runs/`) && u.endsWith("/stream"))).toBe(true);
  expect(seen.console, "console messages on the Playground").toEqual([]);
  await noSecretsOnPage(page);
});

test("test-admin switches D6's fault on and off from the Playground", async ({ page }) => {
  await login(page, "test-admin", "/playground");
  const seen = record(page);
  const faults = page.locator(".card").filter({ has: page.getByRole("heading", { name: "Faults" }) });
  await expect(faults).toBeVisible();
  const slow = faults.getByRole("listitem").filter({ hasText: "Upstream slow (D6)" });
  // The state alone: the switch's own label also says "on" or "off".
  const state = slow.locator("strong");
  await expect(state).toHaveText("off");

  await slow.getByRole("button", { name: "Switch on" }).click();
  const on = page.getByRole("dialog", { name: "Switch Upstream slow (D6) on" });
  await on.getByLabel("Note (optional)").fill("playwright: D6 on");
  await on.getByRole("button", { name: "Switch on" }).click();
  await expect(on).toHaveCount(0);
  await expect(state).toHaveText("on");

  await slow.getByRole("button", { name: "Switch off" }).click();
  const off = page.getByRole("dialog", { name: "Switch Upstream slow (D6) off" });
  await off.getByRole("button", { name: "Switch off" }).click();
  await expect(off).toHaveCount(0);
  await expect(state).toHaveText("off");

  const offOrigin = seen.requests.filter((u) => new URL(u).origin !== new URL(APP).origin && u !== TOKEN_ENDPOINT);
  expect(offOrigin, "requests to another origin").toEqual([]);
  expect(seen.console, "console messages on the Playground").toEqual([]);
  await noSecretsOnPage(page);
});
