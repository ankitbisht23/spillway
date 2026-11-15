// Overview and Throttling on live data from the compose stack (UI-03,
// RL-21, ADR-0061): requests sent to gateway-1 reach Prometheus (scraped
// every 5 s), the control plane's named queries (every 5 s) and the
// screens' charts, which keep updating; a filter change reconnects the
// stream with its new parameters; a key-admin blocks the seeded throttled
// key. The leak guard checks every test.
//
// Timeouts, set before any run (ADR-0061's stall rule, no retries): the
// first data needs a scrape of the new series, a second scrape for rate()
// and the next evaluation, at most 5 + 5 + 5 s; FIRST_DATA_MS is twice
// that, so a stall of up to 820 ms (ADR-0034) leaves more than 14 s. An
// update needs one evaluation (5 s): UPDATE_MS is three of them.
import type { APIRequestContext, Page } from "@playwright/test";

import { expect, login, test } from "./fixtures";
import { seeded } from "./seed";

const GATEWAY = "http://localhost:8000"; // gateway-1, static config (docs/ports.md)
const FIRST_DATA_MS = 30_000;
const UPDATE_MS = 15_000;

/**
 * Sends requests to gateway-1 until the returned function is called:
 * /anything/limited answers 200 five times per 10 s per client IP and 429
 * after (the static config's limited-per-ip), /status/200 always 200.
 */
function traffic(request: APIRequestContext): () => Promise<void> {
  let on = true;
  const loop = (async () => {
    while (on) {
      await Promise.all(["/anything/limited", "/anything/limited", "/status/200"].map((p) =>
        request.get(GATEWAY + p, { failOnStatusCode: false, timeout: 10_000 }).catch(() => undefined)));
      await new Promise((r) => setTimeout(r, 250));
    }
  })();
  return async () => {
    on = false;
    await loop;
  };
}

const legend = (page: Page, chart: string) => page.getByRole("list", { name: `${chart}: latest values` });
const status = (page: Page) => page.locator(".toolbar").getByRole("status");

test("Overview shows live traffic, keeps updating, and a filter change reconnects", async ({ page, request }) => {
  const stop = traffic(request);
  try {
    await login(page, "test-viewer", "/overview");
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
    await expect(legend(page, "Requests per second by status class")).toContainText("2xx", { timeout: FIRST_DATA_MS });
    await expect(legend(page, "Requests per second by status class")).toContainText("4xx", { timeout: FIRST_DATA_MS });
    await expect(legend(page, "Share of requests rate-limited")).toContainText(/429 share \d/);
    await expect(legend(page, "Request latency p50 and p99")).toContainText(/p99 \d/);
    await expect(page.getByRole("region", { name: "Top routes" }).getByRole("row", { name: /^limited/ })).toBeVisible();
    await expect(page.locator("canvas").first()).toBeVisible();

    await expect(status(page)).toHaveText(/^Live · updated \d\d:\d\d:\d\d$/);
    const first = await status(page).textContent();
    await expect(status(page)).not.toHaveText(first ?? "", { timeout: UPDATE_MS });

    const reconnect = page.waitForRequest((r) => r.url().startsWith(`${new URL(page.url()).origin}/api/v1/stream?`) && new URL(r.url()).searchParams.get("range") === "1h");
    await page.getByRole("combobox", { name: "Range" }).selectOption({ label: "Last hour" });
    await reconnect;
    await expect(page).toHaveURL(/[?&]range=1h/);
    await expect(legend(page, "Requests per second by status class")).toContainText("2xx", { timeout: UPDATE_MS });
  } finally {
    await stop();
  }
});

test("Throttling shows the deny rate per policy and the top throttled keys", async ({ page, request }) => {
  const s = seeded();
  const stop = traffic(request);
  try {
    await login(page, "test-viewer", "/throttling");
    await page.getByRole("combobox", { name: "Range" }).selectOption({ label: "Last hour" });
    await expect(legend(page, "Denies per second by policy")).toContainText("limited-per-ip", { timeout: FIRST_DATA_MS });
    const row = page.getByRole("region", { name: "Top throttled keys" }).getByRole("row", { name: new RegExp(`^${s.digest}`) });
    await expect(row).toContainText(`e2e-per-client${s.consumerId}${s.keyId}420`);
  } finally {
    await stop();
  }
});

test("a key-admin blocks a throttled key after confirming", async ({ page }) => {
  const s = seeded();
  await login(page, "test-key-admin", "/throttling?range=1h");
  await page.getByRole("button", { name: `Actions for ${s.digest}` }).click();
  await page.getByRole("complementary", { name: "Selected key" }).getByRole("button", { name: "Block key" }).click();
  const dialog = page.getByRole("dialog", { name: `Block API key ${s.keyId}` });
  await dialog.getByRole("textbox", { name: "Note (optional)" }).fill("Playwright: block from Throttling");
  await dialog.getByRole("button", { name: "Block key" }).click();
  await expect(dialog.getByRole("status")).toContainText(`denies API key ${s.keyId}`);
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(dialog).toHaveCount(0);
});
