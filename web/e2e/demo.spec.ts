// Demo mode off (SC-08, ADR-0095 §2), as on the compose stack: no
// Playground in the navigation, Not found at its URL, and the one demo
// probe per page load answered 404 before any other demo call.
import { DEMO_PROBE, expect, login, test } from "./fixtures";

for (const user of ["test-viewer", "test-admin"]) {
  test(`${user}: no Playground with demo mode off`, async ({ page }) => {
    const demoCalls: { url: string; status: number }[] = [];
    page.on("response", (r) => {
      if (new URL(r.url()).pathname.startsWith("/api/v1/demo")) demoCalls.push({ url: r.url(), status: r.status() });
    });
    await login(page, user);
    const nav = page.getByRole("navigation", { name: "Screens" });
    await expect(nav.getByRole("link", { name: "History" })).toBeVisible();
    await expect(nav.getByRole("link", { name: "Playground" })).toHaveCount(0);
    await page.goto("/playground");
    await expect(page.getByRole("heading", { name: "Not found" })).toBeVisible();
    await expect(nav.getByRole("link", { name: "Playground" })).toHaveCount(0);
    // One probe per page load (the login's and /playground's), each 404.
    await expect.poll(() => demoCalls.length).toBe(2);
    expect(demoCalls).toEqual([{ url: DEMO_PROBE, status: 404 }, { url: DEMO_PROBE, status: 404 }]);
  });
}
