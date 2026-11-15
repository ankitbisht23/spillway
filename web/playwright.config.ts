// Playwright against the compose stack (D10, ADR-0061): the control plane's
// embedded production build on 8080 (CSP and headers live) and the real
// Keycloak on 8180. No retries: a flow passes or fails on its own. The
// timeouts are at least 12× the 820 ms worst host stall for actions that
// take well under a second (ADR-0061).
import { defineConfig, devices } from "@playwright/test";

import { APP } from "./e2e/env";

export default defineConfig({
  testDir: "e2e",
  testMatch: "*.spec.ts",
  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",
  retries: 0,
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  timeout: 120_000,
  expect: { timeout: 10_000 },
  outputDir: "test-results",
  reporter: [["list"], ["html", { outputFolder: "playwright-report", open: "never" }]],
  use: {
    baseURL: APP,
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "off",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
