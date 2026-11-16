// make verify-m6's exit flow (ADR-0061): web/e2e/m6-exit.spec.ts alone,
// with playwright.config.ts's browser, timeouts (no retries) and output
// folders, but without its global setup and teardown, which would replace
// the working set with the e2e fixture: test/verify/m6.sh imports and
// publishes test/verify/m6.yaml instead, and hands the spec its run ID and
// base version. The flow runs about 2 minutes; the test's limit is 15.
import { defineConfig } from "@playwright/test";

import base from "./playwright.config";

export default defineConfig({
  ...base,
  testMatch: "m6-exit.spec.ts",
  testIgnore: [],
  globalSetup: undefined,
  globalTeardown: undefined,
  timeout: 15 * 60_000,
});
