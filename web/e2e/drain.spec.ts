// Drain an endpoint (UI-06, CP-14, ADR-0061): an editor-publisher drains
// one of upstream e2e's endpoints on its page, the banner points to the
// publish page, whose diff shows the endpoint draining; it publishes with
// a note, and the version's diff says the same. Then undrain, the same
// way, so the run ends where it started. In make dev the gateways are
// static, so the live view says no gateway reports the upstream: the
// stream answers, from Prometheus, for an upstream no gateway runs.
// History arrives with m6-replicas-history, so the version is checked
// through the API.
import type { Page } from "@playwright/test";

import { expect, login, test } from "./fixtures";
import { ciApi, seeded } from "./seed";

const ENDPOINT = "upstream-2:8080";

/** Publishes the preview with note; returns the version. */
async function publish(page: Page, note: string): Promise<number> {
  await page.getByRole("textbox", { name: "Change note (required)" }).fill(note);
  await page.getByRole("button", { name: /^Publish revision \d+$/ }).click();
  const done = page.getByRole("status").filter({ hasText: "Published version" });
  await expect(done).toBeVisible();
  return Number(/Published version (\d+)/.exec((await done.textContent()) ?? "")?.[1]);
}

/** Opens upstream e2e from the navigation, as a user would. */
async function openUpstream(page: Page) {
  await page.getByRole("navigation", { name: "Screens" }).getByRole("link", { name: "Routes & upstreams" }).click();
  await page.getByRole("navigation", { name: "Routes and upstreams" }).getByRole("link", { name: "Upstreams" }).click();
  await page.getByRole("row", { name: /^e2e\b/ }).getByRole("link", { name: "e2e", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Upstream e2e" })).toBeVisible();
}

test("drain an endpoint, publish it with its diff, then undrain", async ({ page }) => {
  const s = seeded();
  const api = await ciApi();
  try {
    await login(page, "test-admin");
    await openUpstream(page);
    const endpoints = page.getByRole("region", { name: "Endpoints" });
    const row = endpoints.getByRole("row", { name: new RegExp(ENDPOINT) });
    await expect(row).toContainText("Taking traffic");
    await expect(page.getByText("No gateway reports this upstream", { exact: false }).first()).toBeVisible();

    await row.getByRole("button", { name: `Drain ${ENDPOINT}` }).click();
    await expect(page.getByText(`Endpoint ${ENDPOINT} draining in the working set.`)).toBeVisible();
    await expect(row).toContainText("Draining");
    await page.getByRole("link", { name: "review and publish" }).click();

    const diff = page.getByLabel("Diff against the published version");
    await expect(diff.locator(".diff-add", { hasText: "draining: true" })).toBeVisible();
    await expect(page.getByRole("list", { name: "Changes" })).toContainText("changed upstream e2e");
    const drained = await publish(page, `Playwright ${s.run}: drain ${ENDPOINT}`);
    const d = (await (await api.get(`config/versions/${drained}/diff`)).json()) as { diff: string };
    expect(d.diff).toMatch(/^\+\s+draining: true$/m);

    await openUpstream(page);
    await row.getByRole("button", { name: `Undrain ${ENDPOINT}` }).click();
    await expect(page.getByText(`Endpoint ${ENDPOINT} taking traffic again in the working set.`)).toBeVisible();
    await page.getByRole("link", { name: "review and publish" }).click();
    await expect(diff.locator(".diff-del", { hasText: "draining: true" })).toBeVisible();
    const undrained = await publish(page, `Playwright ${s.run}: undrain ${ENDPOINT}`);
    expect(undrained).toBe(drained + 1);
    const u = (await (await api.get("upstreams?name=e2e")).json()) as { items: { endpoints: { address: string; draining?: boolean }[] }[] };
    expect(u.items[0]?.endpoints.find((e) => e.address === ENDPOINT)?.draining ?? false).toBe(false);
  } finally {
    await api.dispose();
  }
});
