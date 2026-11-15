// The session (UI-08, ADR-0058) against the real Keycloak: login, reload
// through SSO, a forced silent refresh, sign-out by POST, and the sign-out
// broadcast to a second tab. The leak guard checks every test.
import type { Page } from "@playwright/test";

import { APP, AUTH_ENDPOINT, ISSUER, TOKEN_ENDPOINT } from "./env";
import { expect, login, test } from "./fixtures";

/**
 * Clicks Sign out and waits for Keycloak's redirect to load /signed-out:
 * the tab shows Signed out at once, before its logout POST navigates.
 */
async function signOut(page: Page) {
  const back = page.waitForResponse((r) => r.url() === `${APP}/signed-out` && r.request().isNavigationRequest());
  await page.getByRole("button", { name: "Sign out" }).click();
  expect((await back).status()).toBe(200);
  await page.waitForLoadState();
  await expect(page.getByRole("heading", { name: "Signed out" })).toBeVisible();
}

test("logs in through Keycloak and lands on the page asked for", async ({ page }) => {
  await login(page, "test-admin", "/history");
  await expect(page.getByRole("heading", { name: "History" })).toBeVisible();
  await expect(page.getByRole("banner")).toContainText("test-admin");
  await expect(page.getByLabel("Roles")).toHaveText("admin");
});

test("a reload returns through the SSO session, without a form, to the same path", async ({ page }) => {
  await login(page, "test-viewer", "/routes");
  const auth = page.waitForResponse((r) => r.url().startsWith(AUTH_ENDPOINT));
  await page.reload();
  expect((await auth).status(), "Keycloak answered from its SSO session").toBe(302);
  await page.waitForURL("/routes");
  await expect(page.getByRole("heading", { name: "Routes" })).toBeVisible();
  await expect(page.locator("#username")).toHaveCount(0);
});

test("a silent refresh at 240 s keeps the session, with a new refresh token", async ({ page }) => {
  await page.clock.install();
  await login(page, "test-editor");
  const refreshed = page.waitForResponse((r) => r.url() === TOKEN_ENDPOINT && (r.request().postData() ?? "").includes("grant_type=refresh_token"));
  await page.clock.fastForward("04:01");
  const r = await refreshed;
  expect(r.status()).toBe(200);
  const sent = new URLSearchParams(r.request().postData() ?? "").get("refresh_token");
  expect((await r.json() as { refresh_token: string }).refresh_token, "rotated").not.toBe(sent);
  await page.getByRole("link", { name: "Replicas" }).click();
  await expect(page.getByRole("heading", { name: "Replicas" })).toBeVisible();
  await expect(page.getByRole("alertdialog")).toHaveCount(0);
});

test("sign-out POSTs to Keycloak, lands on Signed out and ends the SSO session", async ({ page }) => {
  await login(page, "test-publisher");
  const logout = page.waitForRequest((r) => r.url().startsWith(`${ISSUER}/protocol/openid-connect/logout`));
  await signOut(page);
  const req = await logout;
  expect(req.method()).toBe("POST");
  expect(new URL(req.url()).search).toBe("");
  // Signed out never starts a login by itself; Sign in asks for the password again.
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.locator("#username")).toBeVisible();
});

test("signing out in one tab signs out the other", async ({ page, context }) => {
  await login(page, "test-key-admin");
  const other = await context.newPage();
  await other.goto("/replicas");
  await expect(other.getByRole("heading", { name: "Replicas" })).toBeVisible();
  await signOut(page);
  await expect(other.getByRole("heading", { name: "Signed out" })).toBeVisible();
  expect(new URL(other.url()).pathname).toBe("/signed-out");
});
