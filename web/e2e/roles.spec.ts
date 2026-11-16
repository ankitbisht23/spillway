// Role-aware navigation (UI-04, ADR-0058) for each of the dev realm's five
// users, and the server refusing a viewer directly. Grows with each screen
// task: its actions, disabled with their reasons.
import type { Locator } from "@playwright/test";

import { APP, DEV_PASSWORD, TOKEN_ENDPOINT } from "./env";
import { expect, login, test } from "./fixtures";
import { seeded } from "./seed";

const everyone = ["Overview", "Throttling", "Policies", "Publish", "Routes & upstreams", "Consumers & keys", "Replicas", "History"];
const all = ["Overview", "Throttling", "Policies", "Publish", "Routes & upstreams", "Consumers & keys", "Tenants", "Replicas", "History", "Audit"];

for (const role of ["viewer", "editor", "publisher", "key-admin", "admin"]) {
  test(`test-${role} sees exactly its screens`, async ({ page }) => {
    await login(page, `test-${role}`);
    await expect(page.getByLabel("Roles")).toHaveText(role);
    await expect(page.getByRole("navigation", { name: "Screens" }).getByRole("link")).toHaveText(role === "admin" ? all : everyone);
    for (const [path, title] of [["/audit", "Audit log"], ["/tenants", "Tenants"]]) {
      await page.goto(path ?? "");
      if (role === "admin") {
        await expect(page.getByRole("heading", { name: title })).toBeVisible();
      } else {
        await expect(page.getByRole("heading", { name: "Not allowed" })).toBeVisible();
        await expect(page.getByText("Needs the admin role.")).toBeVisible();
      }
    }

    // Throttling's actions on the seeded throttled key (UI-04, ADR-0060).
    const s = seeded();
    await page.goto("/throttling?range=1h");
    await page.getByRole("button", { name: `Actions for ${s.digest}` }).click();
    const actions = page.getByRole("complementary", { name: "Selected key" });
    await expect(actions.getByRole("link", { name: "Open policy" })).toHaveAttribute("href", `/policies/${s.policyId}`);
    const keyAdmin = role === "key-admin" || role === "admin";
    for (const [name, allowed, reason] of [
      ["Add override", keyAdmin, "Needs the key-admin or admin role"],
      ["Block key", keyAdmin, "Needs the key-admin or admin role"],
      ["Who is this?", role === "admin", "Needs the admin role"],
    ] as const) {
      const button = actions.getByRole("button", { name });
      if (allowed) {
        await expect(button, name).toBeEnabled();
      } else {
        await expect(button, name).toBeDisabled();
        await expect(button, name).toHaveAccessibleDescription(reason);
      }
    }

    // Policies and Publish (UI-01, UI-02): an editor edits but does not
    // publish; a publisher publishes but does not edit.
    const editor = role === "editor" || role === "admin";
    const publisher = role === "publisher" || role === "admin";
    await page.goto("/policies");
    await expectAllowed(page.getByRole("button", { name: "New policy" }), editor, "Needs the editor or admin role");
    await page.goto(`/policies/${s.policyId}`);
    await expectAllowed(page.getByRole("button", { name: "Save" }), editor, "Needs the editor or admin role");
    await expectAllowed(page.getByRole("button", { name: "Delete" }), editor, "Needs the editor or admin role");

    // Routes & upstreams (UI-06): an editor edits routes and drains
    // endpoints; every other role sees both disabled with the reason.
    await page.goto("/routes");
    await expectAllowed(page.getByRole("button", { name: "New route" }), editor, "Needs the editor or admin role");
    await page.getByRole("link", { name: "e2e-orders", exact: true }).click();
    await expectAllowed(page.getByRole("button", { name: "Save" }), editor, "Needs the editor or admin role");
    await page.getByRole("definition").getByRole("link", { name: "e2e", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Upstream e2e" })).toBeVisible();
    await expectAllowed(page.getByRole("button", { name: "Drain upstream-1:8080" }), editor, "Needs the editor or admin role");
    await expectAllowed(page.getByRole("button", { name: "Save" }), editor, "Needs the editor or admin role");

    // Consumers & keys (UI-06, CP-15 – CP-17, RL-08, RL-09): key-admin and
    // admin manage consumers, keys, overrides and access lists; every
    // other role sees each write disabled with the reason.
    const keyReason = "Needs the key-admin or admin role";
    await page.goto("/consumers");
    await expectAllowed(page.getByRole("button", { name: "New consumer" }), keyAdmin, keyReason);
    await page.goto(`/consumers/${s.consumerId}`);
    await expect(page.getByRole("heading", { name: `Consumer ${s.run}` })).toBeVisible();
    for (const name of ["Save", "Delete", "Issue key", "Add override", "Deny this consumer", "Allow this consumer"]) {
      await expectAllowed(page.getByRole("button", { name, exact: true }), keyAdmin, keyReason);
    }
    const seededKey = page.getByRole("region", { name: "API keys" }).getByRole("row", { name: new RegExp(`\\(ID ${s.keyId}\\)`) });
    for (const name of [/^Rotate /, /^Revoke /, /^Block /]) await expectAllowed(seededKey.getByRole("button", { name }), keyAdmin, keyReason);
    await page.goto("/access-lists");
    await expectAllowed(page.getByRole("button", { name: "New entry" }), keyAdmin, keyReason);

    await page.goto("/publish");
    const publish = page.getByRole("button", { name: /^Publish revision \d+$/ });
    if (publisher) {
      // Allowed, though it may still be disabled for another reason (nothing to publish).
      await expect(publish).not.toHaveAccessibleDescription("Needs the publisher or admin role");
    } else {
      await expect(publish).toBeDisabled();
      await expect(publish).toHaveAccessibleDescription("Needs the publisher or admin role");
    }
  });
}

/** button is enabled when allowed, else disabled with reason. */
async function expectAllowed(button: Locator, allowed: boolean, reason: string) {
  if (allowed) {
    await expect(button).toBeEnabled();
  } else {
    await expect(button).toBeDisabled();
    await expect(button).toHaveAccessibleDescription(reason);
  }
}

test("the server refuses a viewer's publish, drain, key issue, rotate and revoke, rollback, override, block and tenant", async ({ page, request }) => {
  await login(page, "test-viewer"); // the leak guard wants a session to watch
  const form = { grant_type: "password", client_id: "rbac-tests", client_secret: DEV_PASSWORD, username: "test-viewer", password: DEV_PASSWORD };
  const tok = await request.post(TOKEN_ENDPOINT, { form });
  expect(tok.status()).toBe(200);
  const { access_token: token } = await tok.json() as { access_token: string };
  for (const path of ["/api/v1/config/publish", "/api/v1/upstreams/1/endpoints/1/drain", "/api/v1/consumers/1/keys", "/api/v1/keys/1/rotate", "/api/v1/keys/1/revoke",
    "/api/v1/config/versions/1/rollback", "/api/v1/overrides", "/api/v1/access-lists", "/api/v1/tenants"]) {
    const r = await request.post(APP + path, { headers: { Authorization: `Bearer ${token}`, "If-Match": '"1"' }, data: {} });
    expect(r.status(), path).toBe(403);
    expect((await r.json() as { code: string }).code, path).toBe("forbidden");
  }
});
