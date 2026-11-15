import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { Role } from "../../auth/permissions";
import { setSession } from "../../auth/session";
import { mockConfig, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import type { ShadowReport } from "./queries";

const SINCE = "2026-11-16T09:00:00Z";

function report(over: Partial<ShadowReport> = {}): ShadowReport {
  return {
    policy_id: 9, policy_name: "per-client", mode: "shadow", config_version: 3, window: "1h", since: SINCE, until: "2026-11-16T10:00:00Z",
    evaluated: 1000, would_deny: 250.4, share: 0.2504, enforce_evaluated: 0, denied: 0, enforce_share: null,
    metrics_since: SINCE, metrics_error: null,
    keys: [
      { key_digest: "AAAAAAAAAAAAAAAAAAAAAA", consumer_id: 12, api_key_id: 34, shadow_denied: 200, denied: 0 },
      { key_digest: "BBBBBBBBBBBBBBBBBBBBBB", consumer_id: null, api_key_id: null, shadow_denied: 30, denied: 0 },
    ],
    keys_shadow_denied_total: 230, keys_since: SINCE,
    ...over,
  };
}

function mockReport(r: ShadowReport | ((window: string | null) => ShadowReport)) {
  const windows: (string | null)[] = [];
  server.use(http.get("/api/v1/policies/9/shadow-report", ({ request }) => {
    const w = new URL(request.url).searchParams.get("window");
    windows.push(w);
    return HttpResponse.json(typeof r === "function" ? r(w) : r);
  }));
  return windows;
}

function mockWorkingSet(mode: string) {
  const puts: { ifMatch: string | null; body: unknown }[] = [];
  const p = { id: 9, name: "per-client", level: "route", match: { routes: ["orders"] }, key_template: "{header:X-Client}", algorithm: "gcra", limits: [{ rate: 10, period: "1s" }], local: true, mode, version: 6, created_at: "x", updated_at: "x" };
  server.use(
    http.get("/api/v1/policies/9", () => HttpResponse.json(p, { headers: { ETag: '"6"' } })),
    http.put("/api/v1/policies/9", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      puts.push({ ifMatch: request.headers.get("If-Match"), body });
      return HttpResponse.json({ ...p, ...body, version: 7 }, { headers: { ETag: '"7"' } });
    }),
  );
  return puts;
}

beforeEach(() => {
  startSession();
  mockConfig({ policies: [{ id: 9, name: "per-client", mode: "shadow", key_template: "{header:X-Client}" }] });
});
afterEach(() => setSession(null));

async function open(roles: Role[] = ["viewer"], path = "/policies/9/shadow") {
  const router = renderApp(path, roles);
  await screen.findByRole("heading", { name: "Shadow report: per-client" });
  return router;
}

describe("the shadow report (UI-01)", () => {
  it("shows would-denies, their share and the top keys", async () => {
    mockReport(report());
    mockWorkingSet("shadow");
    await open();
    expect(screen.getByLabelText("Counts")).toHaveTextContent("Would deny (shadow)250Share of its shadow decisions25.0 %Shadow decisions1,000");
    expect(screen.getByText(/Counts are by its published name/)).toBeInTheDocument();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows[0]).toHaveTextContent("AAAAAAAAAAAAAAAAAAAAAA12342000");
    expect(within(rows[0] as HTMLElement).getByRole("link", { name: "12" })).toHaveAttribute("href", "/consumers/12");
    expect(rows[1]).toHaveTextContent("BBBBBBBBBBBBBBBBBBBBBB––300");
    expect(screen.getByRole("list", { name: "Coverage" })).toHaveTextContent("Top keys only: 20 would-denies are not attributed to a listed key.");
  });

  it("says when Prometheus or the key windows cover less of the window", async () => {
    mockReport(report({ window: "7d", metrics_since: "2026-11-16T09:30:00Z", keys_since: "2026-11-16T09:45:00Z", would_deny: 230 }));
    mockWorkingSet("shadow");
    await open(["viewer"], "/policies/9/shadow?window=7d");
    const notes = screen.getByRole("list", { name: "Coverage" });
    expect(notes).toHaveTextContent(/Prometheus has data from .* only\./);
    expect(notes).toHaveTextContent(/Key data from .* only\./);
    expect(notes).not.toHaveTextContent("not attributed");
  });

  it("shows the keys without Prometheus, with the reason", async () => {
    mockReport(report({ evaluated: null, would_deny: null, share: null, enforce_evaluated: null, denied: null, metrics_since: null, metrics_error: "prometheus_unavailable" }));
    mockWorkingSet("shadow");
    await open();
    expect(screen.getByRole("alert")).toHaveTextContent("Counts unavailable: Prometheus is unavailable. The keys below still answer.");
    expect(screen.getByLabelText("Counts")).toHaveTextContent("Would deny (shadow)–");
    expect(screen.getAllByRole("row")).toHaveLength(3);
  });

  it("shows the empty state", async () => {
    mockReport(report({ evaluated: 0, would_deny: 0, share: null, keys: [], keys_shadow_denied_total: 0, keys_since: null }));
    mockWorkingSet("shadow");
    await open();
    expect(screen.getByText("No key was throttled or would have been in this window.")).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Coverage" })).toHaveTextContent("No heavy-hitter windows of this policy are kept");
  });

  it("changes the window", async () => {
    const windows = mockReport((w) => report({ window: (w ?? "1h") as ShadowReport["window"] }));
    mockWorkingSet("shadow");
    const router = await open();
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Window" }), "Last 24 hours");
    await waitFor(() => expect(windows).toEqual(["1h", "24h"]));
    expect(router.state.location.search).toEqual({ window: "24h" });
  });

  it("promotes as an edit of the working set with If-Match, then points to publish", async () => {
    mockReport(report());
    const puts = mockWorkingSet("shadow");
    await open(["editor"]);
    await userEvent.click(await screen.findByRole("button", { name: "Promote to enforce" }));
    expect(await screen.findByRole("status")).toHaveTextContent("Promoted to enforce in the working set. Unpublished changes: review and publish.");
    expect(puts).toHaveLength(1);
    expect(puts[0]?.ifMatch).toBe('"6"');
    expect(puts[0]?.body).toMatchObject({ name: "per-client", mode: "enforce", local: true });
    expect(puts[0]?.body).not.toHaveProperty("version");
  });

  it("says when the working set already enforces it", async () => {
    mockReport(report());
    mockWorkingSet("enforce");
    await open(["editor"]);
    expect(await screen.findByText(/The working set already has it in enforce mode\./)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Promote to enforce" })).toBeNull();
  });

  it.each<[Role, string | null]>([["viewer", "Needs the editor or admin role"], ["publisher", "Needs the editor or admin role"], ["admin", null]])(
    "for %s: Promote is allowed or says why", async (role, reason) => {
      mockReport(report());
      mockWorkingSet("shadow");
      await open([role]);
      const b = await screen.findByRole("button", { name: "Promote to enforce" });
      if (reason) expect(b).toHaveAccessibleDescription(reason);
      else expect(b).toBeEnabled();
    });

  it("offers admins the audited lookup per key; others see why not", async () => {
    mockReport(report());
    mockWorkingSet("shadow");
    await open(["admin"]);
    const who = () => screen.getByRole("button", { name: "Who is AAAAAAAAAAAAAAAAAAAAAA?" });
    await waitFor(() => expect(who()).toBeEnabled());
    await userEvent.click(who());
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("explains the lookup to other roles", async () => {
    mockReport(report());
    mockWorkingSet("shadow");
    await open(["editor"]);
    expect(screen.queryByRole("button", { name: /Who is/ })).toBeNull();
    expect(screen.getByText("Who is this? (matching a digest to a subject) needs the admin role.")).toBeInTheDocument();
  });

  it("says a policy that is not published has no report yet", async () => {
    server.use(http.get("/api/v1/policies/9/shadow-report", () => HttpResponse.json({ error: "policy 9 is not in the latest published config: publish it first", code: "not_found", request_id: "r" }, { status: 404 })));
    renderApp("/policies/9/shadow", ["viewer"]);
    expect(await screen.findByText("This policy is not in the latest published config: publish it first, then its report shows here.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the policy" })).toHaveAttribute("href", "/policies/9");
  });

  it("shows the error state", async () => {
    server.use(http.get("/api/v1/policies/9/shadow-report", () => HttpResponse.json({ error: "x", code: "unavailable", request_id: "r" }, { status: 400 })));
    renderApp("/policies/9/shadow", ["viewer"]);
    expect(await screen.findByRole("alert")).toHaveTextContent("The request was rejectedx");
  });
});
