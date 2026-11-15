// Role-aware navigation (UI-04, ADR-0058) for each of the dev realm's five
// users, and the server refusing a viewer directly. Grows with each screen
// task: its actions, disabled with their reasons.
import { APP, DEV_PASSWORD, TOKEN_ENDPOINT } from "./env";
import { expect, login, test } from "./fixtures";

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
  });
}

test("the server refuses a viewer's publish, drain, key issue and rollback", async ({ page, request }) => {
  await login(page, "test-viewer"); // the leak guard wants a session to watch
  const form = { grant_type: "password", client_id: "rbac-tests", client_secret: DEV_PASSWORD, username: "test-viewer", password: DEV_PASSWORD };
  const tok = await request.post(TOKEN_ENDPOINT, { form });
  expect(tok.status()).toBe(200);
  const { access_token: token } = await tok.json() as { access_token: string };
  for (const path of ["/api/v1/config/publish", "/api/v1/upstreams/1/endpoints/1/drain", "/api/v1/consumers/1/keys", "/api/v1/config/versions/1/rollback"]) {
    const r = await request.post(APP + path, { headers: { Authorization: `Bearer ${token}`, "If-Match": '"1"' }, data: {} });
    expect(r.status(), path).toBe(403);
    expect((await r.json() as { code: string }).code, path).toBe("forbidden");
  }
});
