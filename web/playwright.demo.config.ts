// make test-e2e-demo's Playground flows (UI-07, ADR-0096 §3):
// web/e2e/playground.spec.ts alone, against make demo (demo mode on, the
// seed, three managed gateways), with playwright.config.ts's browser,
// timeouts (no retries) and output folders, but without its global setup
// and teardown, which would replace the demo's working set with the e2e
// fixture.
import { defineConfig } from "@playwright/test";

import base from "./playwright.config";

export default defineConfig({
  ...base,
  testMatch: "playground.spec.ts",
  testIgnore: [],
  globalSetup: undefined,
  globalTeardown: undefined,
});
