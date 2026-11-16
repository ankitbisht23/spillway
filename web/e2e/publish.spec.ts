// Publish with diff (UI-02, CP-02, ADR-0061): a route's response header
// edited in the Routes screen shows in the publish page's diff, publishes
// with a note, and the version keeps the note; an edit of another route
// through the API between the preview and the click is a 412 that shows
// the newer diff and needs another click. History then lists the version
// with its note, and shows its diff.
import { expect, login, test } from "./fixtures";
import { ciApi, seeded } from "./seed";

interface Versioned {
  id: number;
  version: number;
}

test("an edit shows in the diff and publishes with its note; a 412 shows the newer diff", async ({ page, leakGuard }) => {
  const s = seeded();
  const api = await ciApi();
  try {
    await login(page, "test-admin", "/routes");
    await page.getByRole("link", { name: "e2e-orders", exact: true }).click();
    const header = page.getByRole("textbox", { name: "Response headers to set" });
    await expect(header).toHaveValue("");
    await header.fill(`X-E2E-Run: ${s.run}`);
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("status")).toContainText("Saved to the working set.");
    await page.getByRole("link", { name: "review and publish" }).click();

    const diff = page.getByLabel("Diff against the published version");
    await expect(diff.locator(".diff-add", { hasText: `value: ${s.run}` })).toBeVisible();
    await expect(diff.locator(".diff-add", { hasText: "name: X-E2E-Run" })).toBeVisible();
    await expect(page.getByRole("list", { name: "Changes" })).toContainText("changed route e2e-orders");
    const first = await page.getByRole("button", { name: /^Publish revision \d+$/ }).textContent();

    // Someone edits another route after the preview: the revision moves.
    const routes = (await (await api.get("routes?name=e2e-other")).json()) as { items: (Versioned & Record<string, unknown>)[] };
    const route = routes.items[0];
    if (!route) throw new Error("route e2e-other is not in the working set");
    const put = await api.put(`routes/${route.id}`, { headers: { "If-Match": `"${route.version}"` }, data: { ...route, priority: 7 } });
    expect(put.status(), "the API edit").toBe(200);

    const note = `Playwright ${s.run}: e2e-orders answers X-E2E-Run`;
    await page.getByRole("textbox", { name: "Change note (required)" }).fill(note);
    leakGuard.expectFailedLoad(412, "/api/v1/config/publish");
    await page.getByRole("button", { name: /^Publish revision \d+$/ }).click();
    await expect(page.getByRole("alert").filter({ hasText: "The working set changed since your preview" })).toBeVisible();
    await expect(page.getByRole("list", { name: "Changes" })).toContainText("changed route e2e-other");
    await expect(diff.locator(".diff-add", { hasText: "priority: 7" })).toBeVisible();
    await expect(page.getByRole("textbox", { name: "Change note (required)" })).toHaveValue(note);
    const second = page.getByRole("button", { name: /^Publish revision \d+$/ });
    await expect(second).not.toHaveText(first ?? "");
    await second.click();

    const done = page.getByRole("status").filter({ hasText: "Published version" });
    await expect(done).toBeVisible();
    const version = Number(/Published version (\d+)/.exec((await done.textContent()) ?? "")?.[1]);
    await expect(page.getByText(`Nothing to publish: the working set is version ${version}`)).toBeVisible();

    // History has it, newest, with its note and its diff against the version before.
    await done.getByRole("link", { name: "see it in History" }).click();
    await expect(page.getByRole("heading", { name: `Version ${version}` })).toBeVisible();
    await expect(page.getByRole("definition").filter({ hasText: note })).toHaveText(note);
    const versionDiff = page.getByLabel(`Diff from version ${version - 1} to version ${version}`);
    await expect(versionDiff.locator(".diff-add", { hasText: `value: ${s.run}` })).toBeVisible();
    await expect(versionDiff.locator(".diff-add", { hasText: "priority: 7" })).toBeVisible();
    await page.getByRole("link", { name: "All versions" }).click();
    const newest = page.getByRole("row").nth(1);
    await expect(newest).toContainText(`Version ${version}`);
    await expect(newest).toContainText(note);
  } finally {
    await api.dispose();
  }
});
