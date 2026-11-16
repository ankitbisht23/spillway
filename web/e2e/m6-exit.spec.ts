// The M6 exit flow of make verify-m6 (ADR-0061), run by test/verify/m6.sh
// with playwright.m6.config.ts while gateway-1 … gateway-5 run managed
// and chaos m6 probe drives them. Everything is done by clicking in the
// UI as test-admin; the spillway-ci client's API is used only to keep
// time (each version's apply heartbeats) and to record the review's
// numbers, read-only.
//
//   1. 20 timed publishes: m6-propagation's "Response headers to set"
//      edited in the Routes screen (ADR-0064), then the publish page.
//   2. Shadow: policy m6-limit created in the Policy editor (local GCRA,
//      50/s burst 50, keyed by X-Spillway-Probe-Client), published as S.
//   3. Reviewed: its shadow report shows would-denies and a key row.
//   4. Enforced: promoted from the report, published with the diff as E.
//   5. Rolled back: History, roll back to the base version B, as R.
//
// After each stage's apply on all five the stage is held 25 s, the probe's
// 20 s window plus margin. Each timed action records t_click just before
// the click and t_response when the answer arrives (host clock), and the
// spec writes test-results/m6-exit.json after every step, so a failed
// run still hands the probe what it did. No token or key is in it; the
// digests are of the probe's two fixed labels. The leak guard writes its
// report to test-results/m6-leak-guard.json.
//
// Stall tolerance (ADR-0061): the config's expect (10 s), action (15 s)
// and navigation (30 s) timeouts are at least 12× the 820 ms worst host
// stall; a version's apply on all five is awaited up to 30 s (NFR-08's
// 5 s plus room); the report's data up to 60 s (a 5 s heartbeat and a
// 5 s scrape). No retries.
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import type { APIRequestContext, APIResponse, Page } from "@playwright/test";

import { APP } from "./env";
import { expect, login, test } from "./fixtures";
import { ciApi } from "./seed";

const OUT = fileURLToPath(new URL("../test-results/m6-exit.json", import.meta.url));
const GUARD = fileURLToPath(new URL("../test-results/m6-leak-guard.json", import.meta.url));
const GATEWAYS = ["gateway-1", "gateway-2", "gateway-3", "gateway-4", "gateway-5"];
const POLICY = "m6-limit";
const ROUTE = "m6-propagation";
const PROBE_HEADER = "X-Spillway-Probe-Client";
const ROUTE_PUBLISHES = 20;
const HOLD_MS = 25_000;
const APPLIED = { timeout: 30_000, intervals: [100] };
const REPORT = { timeout: 60_000, intervals: [5_000] };

test.use({ leakReport: GUARD });

interface Timed {
  kind: "route" | "shadow" | "enforce" | "rollback";
  version: number;
  t_click: number;
  t_response: number;
}

interface ShadowReport {
  would_deny: number | null;
  evaluated: number | null;
  share: number | null;
  keys: { key_digest: string; shadow_denied: number; denied: number }[];
}

const run = process.env.SPILLWAY_VERIFY_M6_RUN ?? "";
const base = Number(process.env.SPILLWAY_VERIFY_M6_BASE);

const record = {
  run,
  base_version: base,
  publishes: [] as Timed[],
  review: null as (ShadowReport & { ui_showed_would_deny: boolean; ui_would_deny: string; heavy_digest: string; light_digest: string }) | null,
  steps: [] as string[],
};
const save = () => writeFileSync(OUT, JSON.stringify(record, null, 2));

async function json<T>(what: string, r: Promise<APIResponse>): Promise<T> {
  const res = await r;
  if (res.status() !== 200) throw new Error(`${what}: status ${res.status()}`);
  return (await res.json()) as T;
}

/** Waits until all five gateways reported applying version v. */
async function applied(api: APIRequestContext, v: number) {
  await expect.poll(async () => {
    const got = await json<{ applied: { replica: string }[] }>(`version ${v}`, api.get(`config/versions/${v}`));
    return GATEWAYS.filter((g) => got.applied.some((a) => a.replica === g)).length;
  }, { ...APPLIED, message: `version ${v} applied on all five gateways` }).toBe(GATEWAYS.length);
}

/**
 * Clicks a publish button and times it: t_click just before the click,
 * t_response when the answer to POST path arrives. Returns the version.
 */
async function timed(page: Page, kind: Timed["kind"], path: string, click: () => Promise<void>): Promise<number> {
  const answer = page.waitForResponse((r) => r.url() === `${APP}${path}` && r.request().method() === "POST");
  const tClick = Date.now();
  await click();
  const res = await answer;
  const tResponse = Date.now();
  expect(res.status(), `POST ${path}`).toBe(200);
  const { version } = (await res.json()) as { version: number };
  record.publishes.push({ kind, version, t_click: tClick, t_response: tResponse });
  save();
  return version;
}

/** On the publish page: the note, then a timed Publish. */
async function publish(page: Page, api: APIRequestContext, kind: Timed["kind"], note: string): Promise<number> {
  await page.getByRole("textbox", { name: "Change note (required)" }).fill(note);
  const button = page.getByRole("button", { name: /^Publish revision \d+$/ });
  await expect(button).toBeEnabled();
  const version = await timed(page, kind, "/api/v1/config/publish", () => button.click());
  await expect(page.getByRole("status").filter({ hasText: `Published version ${version}` })).toBeVisible();
  await applied(api, version);
  return version;
}

async function screen(page: Page, name: string) {
  await page.getByRole("navigation", { name: "Screens" }).getByRole("link", { name, exact: true }).click();
}

test("make verify-m6: a new policy goes shadow → reviewed → enforced → rolled back, entirely from the UI", async ({ page }) => {
  test.setTimeout(15 * 60_000);
  expect(run, "SPILLWAY_VERIFY_M6_RUN (set by test/verify/m6.sh)").not.toBe("");
  expect(Number.isSafeInteger(base) && base > 0, "SPILLWAY_VERIFY_M6_BASE (set by test/verify/m6.sh)").toBe(true);
  save();
  const api = await ciApi();
  try {
    const latest = await json<{ items: { version: number }[] }>("latest version", api.get("config/versions?limit=1"));
    expect(latest.items[0]?.version, "the latest version is the base the script published").toBe(base);
    await login(page, "test-admin", "/routes");

    // 1. NFR-08's publishes: a route edit each.
    for (let i = 0; i < ROUTE_PUBLISHES; i++) {
      const value = `${run}-${i}`;
      if (i > 0) await screen(page, "Routes & upstreams");
      await page.getByRole("link", { name: ROUTE, exact: true }).click();
      const header = page.getByRole("textbox", { name: "Response headers to set" });
      await expect(header).toBeVisible();
      await header.fill(`X-Spillway-Verify: ${value}`);
      await page.getByRole("button", { name: "Save" }).click();
      await expect(page.getByRole("status")).toContainText("Saved to the working set.");
      await page.getByRole("link", { name: "review and publish" }).click();
      await expect(page.getByLabel("Diff against the published version").locator(".diff-add", { hasText: `value: ${value}` })).toBeVisible();
      await publish(page, api, "route", `make verify-m6 ${run} ${i}`);
      await page.waitForTimeout(1_000);
    }
    record.steps.push("publishes");
    save();

    // 2. Shadow: the policy, created in the editor and published.
    await screen(page, "Policies");
    await page.getByRole("button", { name: "New policy" }).click();
    await page.getByRole("textbox", { name: "Name" }).fill(POLICY);
    await expect(page.getByRole("combobox", { name: "Level" })).toHaveValue("route");
    await page.getByRole("textbox", { name: "Routes" }).fill("m6-probe");
    await page.getByRole("textbox", { name: "Key template" }).fill(`{header:${PROBE_HEADER}}`);
    await expect(page.getByRole("combobox", { name: "Algorithm" })).toHaveValue("gcra");
    await page.getByRole("checkbox", { name: "Local (per gateway, no Redis)" }).check();
    await expect(page.getByRole("combobox", { name: "Failure mode" })).toHaveValue("");
    await expect(page.getByRole("combobox", { name: "Mode", exact: true })).toHaveValue("shadow");
    const limit = page.getByRole("group", { name: "Limit 1" });
    await limit.getByRole("textbox", { name: "Rate" }).fill("50");
    await limit.getByRole("textbox", { name: "Period" }).fill("1s");
    await limit.getByRole("textbox", { name: "Burst" }).fill("50");
    await page.getByRole("button", { name: "Create policy" }).click();
    await expect(page.getByRole("heading", { name: `Policy ${POLICY}` })).toBeVisible();
    const id = Number(/\/policies\/(\d+)/.exec(page.url())?.[1]);
    expect(Number.isSafeInteger(id) && id > 0, "the new policy's ID in the URL").toBe(true);
    await page.getByRole("link", { name: "review and publish" }).click();
    const diff = page.getByLabel("Diff against the published version");
    await expect(diff.locator(".diff-add", { hasText: `name: ${POLICY}` })).toBeVisible();
    await expect(diff.locator(".diff-add", { hasText: "mode: shadow" })).toBeVisible();
    await expect(page.getByRole("list", { name: "Changes" })).toContainText(`added policy ${POLICY}`);
    const shadow = await publish(page, api, "shadow", `make verify-m6 ${run}: ${POLICY} in shadow`);
    record.steps.push("shadow");
    save();
    await page.waitForTimeout(HOLD_MS);

    // 3. Reviewed: the shadow report shows would-denies and the heavy key.
    await screen(page, "Policies");
    await page.getByRole("link", { name: POLICY, exact: true }).click();
    await expect(page.getByText(`in version ${shadow} as shadow`)).toBeVisible();
    await page.getByRole("link", { name: "Shadow report" }).click();
    await expect(page.getByRole("heading", { name: `Shadow report: ${POLICY}` })).toBeVisible();
    const windowSelect = page.getByRole("combobox", { name: "Window" });
    await windowSelect.selectOption("15m");
    await expect(page).toHaveURL(/window=15m/);
    await expect(page.getByText(`Version ${shadow} runs this policy in shadow mode.`)).toBeVisible();
    const wouldDeny = page.getByLabel("Counts").locator("div").filter({ has: page.getByText("Would deny (shadow)", { exact: true }) }).locator("dd");
    const keyRows = page.locator("table tbody tr");
    let shown = "";
    // The report does not refresh by itself: switching the window away
    // and back fetches it again once it is 10 s old.
    await expect.poll(async () => {
      shown = ((await wouldDeny.textContent()) ?? "").trim();
      const ok = Number(shown.replace(/,/g, "")) > 0 && (await keyRows.count()) > 0;
      if (!ok) {
        await windowSelect.selectOption("1h");
        await expect(page).toHaveURL(/window=1h/);
        await windowSelect.selectOption("15m");
        await expect(page).toHaveURL(/window=15m/);
      }
      return ok;
    }, { ...REPORT, message: "the shadow report shows would-denies and a key" }).toBe(true);
    const report = await json<ShadowReport>("shadow report", api.get(`policies/${id}/shadow-report?window=15m`));
    const digest = async (client: string) => (await json<{ key_digest: string }>(`lookup ${client}`,
      api.post("heavy-hitters/lookup", { data: { policy_id: id, subject: { headers: { [PROBE_HEADER]: client } } } }))).key_digest;
    record.review = {
      ui_showed_would_deny: true, ui_would_deny: shown,
      would_deny: report.would_deny, evaluated: report.evaluated, share: report.share,
      keys: report.keys.map((k) => ({ key_digest: k.key_digest, shadow_denied: k.shadow_denied, denied: k.denied })),
      heavy_digest: await digest("heavy"), light_digest: await digest("light"),
    };
    record.steps.push("reviewed");
    save();

    // 4. Enforced: promoted from the report, published with the diff.
    await page.getByRole("button", { name: "Promote to enforce" }).click();
    await expect(page.getByRole("status")).toContainText("Promoted to enforce in the working set.");
    await page.getByRole("link", { name: "review and publish" }).click();
    await expect(diff.locator(".diff-del", { hasText: "mode: shadow" })).toBeVisible();
    await expect(diff.locator(".diff-add", { hasText: "mode: enforce" })).toBeVisible();
    const enforce = await publish(page, api, "enforce", `make verify-m6 ${run}: promote ${POLICY} to enforce`);
    expect(enforce).toBe(shadow + 1);
    record.steps.push("enforced");
    save();
    await page.waitForTimeout(HOLD_MS);

    // 5. Rolled back to the base version, from History.
    await screen(page, "History");
    await expect(page.getByRole("row").nth(1)).toContainText(`Version ${enforce}`);
    await page.getByRole("link", { name: `Version ${base}`, exact: true }).click();
    await expect(page.getByRole("heading", { name: `Version ${base}` })).toBeVisible();
    await page.getByRole("button", { name: `Roll back to version ${base}` }).click();
    const dialog = page.getByRole("dialog", { name: `Roll back to version ${base}?` });
    await expect(dialog.getByLabel(`Diff from version ${enforce} to version ${base}`).locator(".diff-del", { hasText: `name: ${POLICY}` })).toBeVisible();
    await expect(dialog.getByRole("list", { name: "Changes" })).toContainText(`removed policy ${POLICY}`);
    await dialog.getByRole("textbox", { name: "Change note (required)" }).fill(`make verify-m6 ${run}: roll back to version ${base}`);
    const confirm = dialog.getByRole("button", { name: `Roll back to version ${base}` });
    await expect(confirm).toBeEnabled();
    const rollback = await timed(page, "rollback", `/api/v1/config/versions/${base}/rollback`, () => confirm.click());
    await expect(dialog).toBeHidden();
    await expect(page.getByRole("status").filter({ hasText: "Published version" })).toContainText(`Published version ${rollback}, a copy of version ${base}`);
    await applied(api, rollback);
    record.steps.push("rolled back");
    save();
    await page.waitForTimeout(HOLD_MS);

    // The working set is the base again: the policy is gone.
    await screen(page, "Policies");
    await expect(page.getByRole("heading", { name: "Policies" })).toBeVisible();
    await expect(page.getByRole("link", { name: POLICY, exact: true })).toHaveCount(0);
  } finally {
    await api.dispose();
  }
});
