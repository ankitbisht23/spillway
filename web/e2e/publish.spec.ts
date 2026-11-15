// Publish with diff (UI-02, CP-02, ADR-0061): an edit in the policy editor
// shows in the publish page's diff, publishes with a note, and the version
// keeps the note; an edit through the API between the preview and the
// click is a 412 that shows the newer diff and needs another click. The
// Routes screen arrives with m6-routes, so the edit is a policy's; History
// arrives with m6-replicas-history, so the version is checked through the
// API.
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
    await login(page, "test-admin", `/policies/${s.policyId}`);
    const rate = page.getByRole("group", { name: "Limit 1" }).getByRole("textbox", { name: "Rate" });
    await expect(rate).toHaveValue("100");
    await rate.fill("150");
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("status")).toContainText("Saved to the working set.");
    await page.getByRole("link", { name: "review and publish" }).click();

    const diff = page.getByLabel("Diff against the published version");
    await expect(diff.locator(".diff-add", { hasText: "rate: 150" })).toBeVisible();
    await expect(diff.locator(".diff-del", { hasText: "rate: 100" })).toBeVisible();
    await expect(page.getByRole("list", { name: "Changes" })).toContainText("changed policy e2e-per-client");
    const first = await page.getByRole("button", { name: /^Publish revision \d+$/ }).textContent();

    // Someone edits a route after the preview: the revision moves.
    const routes = (await (await api.get("routes?name=e2e-orders")).json()) as { items: (Versioned & Record<string, unknown>)[] };
    const route = routes.items[0];
    if (!route) throw new Error("route e2e-orders is not in the working set");
    const put = await api.put(`routes/${route.id}`, { headers: { "If-Match": `"${route.version}"` }, data: { ...route, priority: 7 } });
    expect(put.status(), "the API edit").toBe(200);

    const note = `Playwright ${s.run}: e2e-per-client to 150/s`;
    await page.getByRole("textbox", { name: "Change note (required)" }).fill(note);
    leakGuard.expectFailedLoad(412, "/api/v1/config/publish");
    await page.getByRole("button", { name: /^Publish revision \d+$/ }).click();
    await expect(page.getByRole("alert").filter({ hasText: "The working set changed since your preview" })).toBeVisible();
    await expect(page.getByRole("list", { name: "Changes" })).toContainText("changed route e2e-orders");
    await expect(diff.locator(".diff-add", { hasText: "priority: 7" })).toBeVisible();
    await expect(page.getByRole("textbox", { name: "Change note (required)" })).toHaveValue(note);
    const second = page.getByRole("button", { name: /^Publish revision \d+$/ });
    await expect(second).not.toHaveText(first ?? "");
    await second.click();

    const done = page.getByRole("status").filter({ hasText: "Published version" });
    await expect(done).toBeVisible();
    const version = Number(/Published version (\d+)/.exec((await done.textContent()) ?? "")?.[1]);
    const latest = (await (await api.get("config/versions?limit=1")).json()) as { items: { version: number; note: string }[] };
    expect(latest.items[0]).toMatchObject({ version, note });
    await expect(page.getByText(`Nothing to publish: the working set is version ${version}`)).toBeVisible();
  } finally {
    await api.dispose();
  }
});
