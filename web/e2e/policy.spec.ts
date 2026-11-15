// Shadow → promote → rollback (UI-01, UI-02, D8, ADR-0061): a new policy
// is created in shadow mode, published with its diff and note, its shadow
// report opens (in make dev the gateways are static, so it shows its
// empty states), it is promoted with an edit and published again, and the
// config is rolled back to the version before it, after which it is gone
// from the working set. Rollback is History's (m6-replicas-history), which
// will replace the API call below with the UI.
import type { Page } from "@playwright/test";

import { expect, login, signOut, test } from "./fixtures";
import { ciApi, seeded } from "./seed";

async function publish(page: Page, note: string): Promise<number> {
  await page.getByRole("textbox", { name: "Change note (required)" }).fill(note);
  await page.getByRole("button", { name: /^Publish revision \d+$/ }).click();
  const done = page.getByRole("status").filter({ hasText: "Published version" });
  await expect(done).toBeVisible();
  return Number(/Published version (\d+)/.exec((await done.textContent()) ?? "")?.[1]);
}

test("a new policy goes shadow → published → reviewed → promoted → published → rolled back", async ({ page }) => {
  const s = seeded();
  const name = `${s.run}-shadow`;
  const api = await ciApi();
  try {
    const before = (await (await api.get("config/versions?limit=1")).json()) as { items: { version: number }[] };
    const base = before.items[0]?.version;
    if (base === undefined) throw new Error("no published version");

    // Shadow: create it in the editor.
    await login(page, "test-editor", "/policies/new");
    await page.getByRole("textbox", { name: "Name" }).fill(name);
    await page.getByRole("textbox", { name: "Routes" }).fill("e2e-orders");
    await page.getByRole("textbox", { name: "Key template" }).fill("{ip}");
    await page.getByRole("checkbox", { name: "Local (per gateway, no Redis)" }).check();
    await expect(page.getByRole("combobox", { name: "Mode", exact: true })).toHaveValue("shadow");
    await page.getByRole("group", { name: "Limit 1" }).getByRole("textbox", { name: "Rate" }).fill("50");
    await page.getByRole("button", { name: "Create policy" }).click();
    await expect(page.getByRole("heading", { name: `Policy ${name}` })).toBeVisible();
    const id = Number(/\/policies\/(\d+)/.exec(page.url())?.[1]);
    // An editor may not publish: the page says so.
    await page.getByRole("link", { name: "review and publish" }).click();
    await expect(page.getByRole("button", { name: /^Publish revision/ })).toHaveAccessibleDescription("Needs the publisher or admin role");

    // Published by a publisher, after the diff.
    await signOut(page);
    await login(page, "test-publisher", "/publish");
    const diff = page.getByLabel("Diff against the published version");
    await expect(diff.locator(".diff-add", { hasText: `name: ${name}` })).toBeVisible();
    await expect(diff.locator(".diff-add", { hasText: "mode: shadow" })).toBeVisible();
    await expect(page.getByRole("list", { name: "Changes" })).toContainText(`added policy ${name}`);
    const shadowVersion = await publish(page, `Playwright ${s.run}: ${name} in shadow`);

    // Reviewed: its shadow report.
    await page.goto(`/policies/${id}`);
    await expect(page.getByText(`in version ${shadowVersion} as shadow`)).toBeVisible();
    await page.getByRole("link", { name: "Shadow report" }).click();
    await expect(page.getByRole("heading", { name: `Shadow report: ${name}` })).toBeVisible();
    await expect(page.getByText(`Version ${shadowVersion} runs this policy in shadow mode.`)).toBeVisible();
    await expect(page.getByLabel("Counts")).toContainText("Would deny (shadow)");
    await expect(page.getByText("No key was throttled or would have been in this window.")).toBeVisible();
    await page.getByRole("combobox", { name: "Window" }).selectOption("15m");
    await expect(page).toHaveURL(/window=15m/);
    // A publisher may not edit, so may not promote.
    await expect(page.getByRole("button", { name: "Promote to enforce" })).toHaveAccessibleDescription("Needs the editor or admin role");

    // Enforced: promote (an edit), then publish with its diff, as an admin.
    await signOut(page);
    await login(page, "test-admin", `/policies/${id}/shadow`);
    await page.getByRole("button", { name: "Promote to enforce" }).click();
    await expect(page.getByRole("status")).toContainText("Promoted to enforce in the working set.");
    await page.getByRole("link", { name: "review and publish" }).click();
    await expect(diff.locator(".diff-del", { hasText: "mode: shadow" })).toBeVisible();
    await expect(diff.locator(".diff-add", { hasText: "mode: enforce" })).toBeVisible();
    const enforceVersion = await publish(page, `Playwright ${s.run}: promote ${name} to enforce`);
    expect(enforceVersion).toBe(shadowVersion + 1);
    await page.goto(`/policies/${id}/shadow`);
    await expect(page.getByText(`Version ${enforceVersion} runs this policy in enforce mode.`)).toBeVisible();

    // Rolled back to the version before it (History's, through the API until m6-replicas-history).
    const validated = await api.post("config/validate");
    const rollback = await api.post(`config/versions/${base}/rollback`, {
      headers: { "If-Match": validated.headers().etag ?? "" }, data: { note: `Playwright ${s.run}: roll back ${name}` },
    });
    expect(rollback.status(), "rollback").toBe(200);
    await page.goto("/policies");
    await expect(page.getByRole("link", { name: "e2e-per-client" })).toBeVisible();
    await expect(page.getByRole("link", { name })).toHaveCount(0);
  } finally {
    await api.dispose();
  }
});
