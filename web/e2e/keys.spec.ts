// Issue, rotate and revoke a key (UI-06, CP-15 – CP-17, ADR-0061): a
// key-admin creates a consumer, issues a key (shown once, copied, gone
// from the page on close), rotates it with a grace period, and revokes
// both keys after confirming; the list shows the rotation and the
// revocations. Each key is exercised through a gateway: it works once
// issued, and is refused with 401 once revoked. Then the Replicas screen
// shows the managed gateway live.
//
// make dev's gateway-1 runs static config and keys, so this spec makes it
// managed for its run (as make verify-m5 does) and puts it back after.
// The leak guard collects every key the API returns and fails the test if
// one appears in storage, a cookie or a URL.
//
// Waits: an identity change reaches a managed gateway within 5 s (CP-17,
// NFR-09); a stall of the host lasts at most 0.82 s (ADR-0034), so 15 s
// cannot be missed by a stall. The first key's wait also covers the
// recreated gateway's first connection and snapshot, as verify-m5's 60 s.
import type { APIRequestContext, Locator } from "@playwright/test";

import { APP, GATEWAY } from "./env";
import { expect, login, test } from "./fixtures";
import { gatewayMode, seeded } from "./seed";

const ROUTE = `${GATEWAY}/anything/e2e-keys`;
const FIRST = { timeout: 60_000, intervals: [250] };
const CHANGE = { timeout: 15_000, intervals: [250] };

/** The gateway's status for a request with key (none: no X-API-Key). */
async function status(request: APIRequestContext, key?: string): Promise<number> {
  const r = await request.get(ROUTE, { headers: key ? { "X-API-Key": key } : {}, failOnStatusCode: false });
  return r.status();
}

/** spw_<prefix>_… as the list shows a key. */
const shown = (key: string) => `spw_${key.split("_")[1] ?? ""}_…`;

/**
 * The key a dialog shows. Checked without printing it: a failed matcher
 * would put the key in the report.
 */
async function keyIn(dialog: Locator): Promise<string> {
  const shownKey = dialog.getByLabel("The new API key");
  await expect(shownKey).toBeVisible();
  const key = (await shownKey.textContent()) ?? "";
  expect(/^spw_[0-9A-Za-z]{8}_[0-9A-Za-z]{43}$/.test(key), "the dialog shows a key in the spw_ format").toBe(true);
  return key;
}

// A trace or a screenshot of a failure could hold a key the page showed:
// none is kept for this spec (ADR-0061 keeps them for the others).
test.use({ trace: "off", screenshot: "off" });

test.beforeAll(() => {
  test.setTimeout(180_000);
  gatewayMode("managed");
});

test.afterAll(() => {
  test.setTimeout(180_000);
  gatewayMode("static");
});

test("issue, rotate and revoke a key, each exercised through a gateway", async ({ page, context, request }) => {
  test.setTimeout(240_000);
  const s = seeded();
  const name = `${s.run}-keys`;
  await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin: APP });
  await login(page, "test-key-admin", "/consumers");

  // A new consumer of the run's tenant.
  await page.getByRole("button", { name: "New consumer" }).click();
  await page.getByRole("textbox", { name: "Name (required)" }).fill(name);
  await page.getByRole("combobox", { name: "Tenant (required)" }).selectOption({ label: s.run });
  await page.getByRole("button", { name: "Create consumer" }).click();
  await expect(page.getByRole("heading", { name: `Consumer ${name}` })).toBeVisible();
  const keys = page.getByRole("region", { name: "API keys" });
  await expect(keys.getByText("No API keys yet.", { exact: false })).toBeVisible();

  // Issue: shown once, copied, gone from the page when the dialog closes.
  await keys.getByRole("button", { name: "Issue key" }).click();
  let dialog = page.getByRole("dialog", { name: `Issue an API key for ${name}` });
  await dialog.getByRole("button", { name: "Issue key" }).click();
  const first = await keyIn(dialog);
  await dialog.getByRole("button", { name: "Copy key" }).click();
  await expect(dialog.getByText("Copied.")).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText()) === first, "the clipboard holds the key").toBe(true);
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();
  expect((await page.content()).includes(first), "the key is gone from the page").toBe(false);
  await expect(keys.getByRole("row", { name: new RegExp(shown(first)) })).toContainText("Never");

  expect(await status(request), "no key").toBe(401);
  await expect.poll(() => status(request, first), { ...FIRST, message: "the issued key works through the gateway" }).toBe(200);

  // Rotate with a grace period: the new key is shown once; both work.
  await keys.getByRole("button", { name: `Rotate ${shown(first)}` }).click();
  dialog = page.getByRole("dialog", { name: `Rotate key ${shown(first)}` });
  await dialog.getByRole("textbox", { name: "Grace period" }).fill("1h");
  await dialog.getByRole("textbox", { name: /^Note/ }).fill(`Playwright ${s.run}: rotate`);
  await dialog.getByRole("button", { name: "Rotate key" }).click();
  const second = await keyIn(dialog);
  expect(second === first, "rotation made a new key").toBe(false);
  await expect(dialog.getByText(/keeps working until/)).toBeVisible();
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();
  expect((await page.content()).includes(second), "the new key is gone from the page").toBe(false);
  const oldRow = keys.getByRole("row", { name: new RegExp(shown(first)) });
  const newRow = keys.getByRole("row", { name: new RegExp(shown(second)) });
  await expect(oldRow).not.toContainText("Never"); // it expires with the grace period
  await expect(newRow).toContainText(/Key \d+/); // rotated from the first
  await expect.poll(() => status(request, second), { ...CHANGE, message: "the rotated key works" }).toBe(200);
  expect(await status(request, first), "the old key works in its grace period").toBe(200);

  // Revoke both, each after confirming; each is refused at once.
  for (const [key, row] of [[second, newRow], [first, oldRow]] as const) {
    await keys.getByRole("button", { name: `Revoke ${shown(key)}` }).click();
    dialog = page.getByRole("dialog", { name: `Revoke key ${shown(key)}?` });
    await expect(dialog.getByText(/refuses it with 401 within 5 s/)).toBeVisible();
    await dialog.getByRole("textbox", { name: /^Note/ }).fill(`Playwright ${s.run}: revoke`);
    await dialog.getByRole("button", { name: "Revoke key" }).click();
    await expect(dialog).toBeHidden();
    await expect(row.getByText("Revoked")).toBeVisible();
    await expect.poll(() => status(request, key), { ...CHANGE, message: `revoked key ${shown(key)} is refused` }).toBe(401);
  }
  await expect(keys.getByRole("button", { name: /^(Rotate|Revoke) / })).toHaveCount(0);

  // Replicas (UI-06, CP-08): the managed gateway-1 reports over the
  // stream, live, with its versions and staleness. The stream's first
  // result comes within its 5 s tick; 15 s leaves room for stalls.
  await page.getByRole("navigation", { name: "Screens" }).getByRole("link", { name: "Replicas" }).click();
  const gw = page.getByRole("row").filter({ has: page.getByRole("rowheader", { name: "gateway-1", exact: true }) });
  await expect(gw.getByRole("cell").first(), "gateway-1 is live").toHaveText(/^live/, { timeout: CHANGE.timeout });
  await expect(gw.getByRole("link"), "gateway-1 runs a published config version").toHaveText(/^\d+$/);
  await expect(gw).toContainText(/\d+\.\d s/);
});
