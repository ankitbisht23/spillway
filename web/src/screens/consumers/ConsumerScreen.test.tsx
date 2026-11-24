import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { setSession } from "../../auth/session";
import { startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import { localInput } from "../routes/fields";
import { consumer, mockConsumer, mockUsage, override, quotaOverride, type Sent, usage } from "./testing";

beforeEach(() => startSession());
afterEach(() => setSession(null));

/** Records override and access-list writes. */
function mockWrites(answer?: () => Response) {
  const sent: Sent[] = [];
  const handle = (method: string) => async ({ request }: { request: Request }) => {
    const body = await request.json();
    sent.push({ method, path: new URL(request.url).pathname, ifMatch: request.headers.get("If-Match"), body });
    if (answer) return answer();
    return HttpResponse.json({ ...(body as object), id: 99, version: 1, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z" }, { status: 201 });
  };
  server.use(
    http.post("/api/v1/overrides", handle("POST")),
    http.post("/api/v1/access-lists", handle("POST")),
    http.put("/api/v1/overrides/30", handle("PUT")),
    http.post("/api/v1/quota-overrides", handle("POST")),
    http.put("/api/v1/quota-overrides/50", handle("PUT")),
    http.delete("/api/v1/quota-overrides/50", ({ request }) => {
      sent.push({ method: "DELETE", path: "/api/v1/quota-overrides/50", ifMatch: request.headers.get("If-Match"), body: null });
      return new HttpResponse(null, { status: 204 });
    }),
  );
  return sent;
}

const overrides = async () => within(await screen.findByRole("region", { name: "Overrides" }));

describe("a consumer's settings (UI-06, CP-20)", () => {
  it("shows its tenant and plan, and its quota usage today and this month with when it was flushed (RL-10)", async () => {
    mockConsumer();
    renderApp("/consumers/7", ["viewer"]);
    await screen.findByRole("heading", { name: "Consumer checkout" });
    expect(await screen.findByText("acme", { selector: "dd" })).toBeInTheDocument();
    expect(await screen.findByLabelText("Quota today: 120 / 1,000")).toBeInTheDocument();
    expect(screen.getByText(/\(flushed 16\/11\/2026/)).toBeInTheDocument();
    expect(screen.getByLabelText("Quota this month: —")).toBeInTheDocument();
    expect(screen.getByText(/up to 10 s behind the gateways/)).toBeInTheDocument();
    for (const name of ["Save", "Delete"]) {
      expect(screen.getByRole("button", { name })).toBeDisabled();
      expect(screen.getByRole("button", { name })).toHaveAccessibleDescription("Needs the key-admin or admin role");
    }
  });

  it("saves with If-Match: a published plan from the choices, the rest as loaded", async () => {
    const api = mockConsumer();
    const user = userEvent.setup();
    renderApp("/consumers/7", ["key-admin"]);
    const plan = await screen.findByRole("combobox", { name: "Plan" });
    await waitFor(() => expect(within(plan).getByRole("option", { name: "pro" })).toBeInTheDocument());
    await user.selectOptions(plan, "pro");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/^Saved\. It applies at once/)).toBeInTheDocument();
    expect(api.sent).toEqual([{ method: "PUT", path: "/api/v1/consumers/7", ifMatch: '"4"', body: { tenant_id: 1, name: "checkout", status: "active", plan_id: 2 } }]);
  });

  it("answers a 412 with Reload", async () => {
    mockConsumer({ answer: () => HttpResponse.json({ code: "precondition_failed", error: "changed" }, { status: 412 }) });
    const user = userEvent.setup();
    renderApp("/consumers/7", ["admin"]);
    await user.type(await screen.findByRole("textbox", { name: /^JWT subject/ }), "svc-checkout");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("This consumer changed since you opened it.", { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload" })).toBeEnabled();
  });

  it("shows Not found for a deleted consumer", async () => {
    mockConsumer();
    server.use(http.get("/api/v1/consumers/7", () => HttpResponse.json({ code: "not_found", error: "no consumer 7" }, { status: 404 })));
    renderApp("/consumers/7");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("denies the consumer through a prefilled access-list entry", async () => {
    mockConsumer();
    const sent = mockWrites();
    const user = userEvent.setup();
    renderApp("/consumers/7", ["key-admin"]);
    await user.click(await screen.findByRole("button", { name: "Deny this consumer" }));
    const dialog = within(await screen.findByRole("dialog", { name: "New access-list entry" }));
    await user.click(dialog.getByRole("button", { name: "Create entry" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(sent).toEqual([{ method: "POST", path: "/api/v1/access-lists", ifMatch: null, body: { action: "deny", subject_type: "consumer", consumer_id: 7 } }]);
  });
});

describe("a consumer's overrides (RL-08)", () => {
  it("opens the form prefilled from Throttling's Add override and creates a time-boxed burst grant", async () => {
    mockConsumer();
    const sent = mockWrites();
    const user = userEvent.setup();
    const router = renderApp("/consumers/7?override_policy=4", ["key-admin"]);
    const dialog = within(await screen.findByRole("dialog", { name: "Add an override" }));
    const policy = dialog.getByRole("combobox", { name: "Policy (required)" });
    await waitFor(() => expect(policy).toHaveValue("4"));
    // Only per-consumer policies are offered (RL-08, ADR-0041).
    expect(within(policy).getAllByRole("option").map((o) => o.textContent)).toEqual(["Choose a policy", "per-consumer", "per-key-route"]);

    await user.type(dialog.getByRole("textbox", { name: "Limits (required)" }), "500/1m{Enter}50/1s burst 100");
    const ends = "2026-12-01T18:30";
    await user.type(dialog.getByLabelText("Ends"), ends);
    await user.type(dialog.getByRole("textbox", { name: "Note" }), "launch week");
    await user.click(dialog.getByRole("button", { name: "Create override" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(sent).toEqual([{
      method: "POST", path: "/api/v1/overrides", ifMatch: null,
      body: { consumer_id: 7, policy_id: 4, limits: [{ rate: 500, period: "1m" }, { rate: 50, period: "1s", burst: 100 }], ends_at: new Date(ends).toISOString(), note: "launch week" },
    }]);
    // The prefill is spent: a reload does not open the form again.
    expect(router.state.location.search).toEqual({});
  });

  it("checks limit lines in the browser and puts the server's 422 on its field", async () => {
    mockConsumer();
    const sent = mockWrites(() => HttpResponse.json({
      code: "validation_failed", error: "invalid override",
      details: [{ entity: "override", path: "limits", message: "override for policy 4 of the published config version 3: limit 1m: period not in the policy" }],
    }, { status: 422 }));
    const user = userEvent.setup();
    renderApp("/consumers/7", ["admin"]);
    await user.click(await (await overrides()).findByRole("button", { name: "Add override" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Add an override" }));
    const limits = dialog.getByRole("textbox", { name: "Limits (required)" });
    await user.type(limits, "lots");
    await user.click(dialog.getByRole("button", { name: "Create override" }));
    expect(limits).toHaveAccessibleDescription(/"lots": want rate\/period/);
    expect(dialog.getByText("Required")).toBeInTheDocument(); // the policy
    expect(sent).toEqual([]);

    await user.selectOptions(dialog.getByRole("combobox", { name: "Policy (required)" }), "per-consumer");
    await user.clear(limits);
    await user.type(limits, "500/1m");
    await user.click(dialog.getByRole("button", { name: "Create override" }));
    await waitFor(() => expect(limits).toHaveAccessibleDescription(/period not in the policy/));
  });

  it("lists overrides with their state and edits one with If-Match, its start kept as it was", async () => {
    const start = "2026-01-01T09:15:42.5Z";
    mockConsumer({ overrides: [override({ starts_at: start, ends_at: "2099-01-01T00:00:00Z", note: "promo" })] });
    server.use(http.get("/api/v1/overrides/30", () => HttpResponse.json(override({ starts_at: start, ends_at: "2099-01-01T00:00:00Z", note: "promo" }), { headers: { ETag: '"2"' } })));
    const sent = mockWrites();
    const user = userEvent.setup();
    renderApp("/consumers/7", ["key-admin"]);
    const row = within(await (await overrides()).findByRole("row", { name: /per-consumer/ }));
    expect(row.getByText("500/1m")).toBeInTheDocument();
    expect(row.getByText("Burst grant, active")).toBeInTheDocument();
    await user.click(row.getByRole("button", { name: "Edit the override of per-consumer" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Override of per-consumer" }));
    expect(dialog.getByLabelText("Starts")).toHaveValue(localInput(start));
    const limits = dialog.getByRole("textbox", { name: "Limits (required)" });
    await user.clear(limits);
    await user.type(limits, "900/1m");
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toEqual({
      method: "PUT", path: "/api/v1/overrides/30", ifMatch: '"2"',
      body: { consumer_id: 7, policy_id: 4, limits: [{ rate: 900, period: "1m" }], starts_at: start, ends_at: "2099-01-01T00:00:00Z", note: "promo" },
    });
  });

  it("shows the empty state with the reason an editor cannot add one", async () => {
    mockConsumer();
    renderApp("/consumers/7", ["editor"]);
    expect(await (await overrides()).findByText("No overrides: the plan's or the policy's limits apply. Needs the key-admin or admin role to add one.")).toBeInTheDocument();
    expect((await overrides()).getByRole("button", { name: "Add override" })).toHaveAccessibleDescription("Needs the key-admin or admin role");
  });

  it("shows the overrides' error state with the request ID", async () => {
    mockConsumer();
    server.use(http.get("/api/v1/overrides", () => HttpResponse.json({ code: "internal", error: "boom" }, { status: 500, headers: { "X-Request-Id": "req-9" } })));
    renderApp("/consumers/7", ["viewer"]);
    expect(await (await overrides()).findByRole("alert")).toHaveTextContent("Something went wrong");
    expect((await overrides()).getByText("req-9")).toBeInTheDocument();
  });
});

const quotaOverrides = async () => within(await screen.findByRole("region", { name: "Quota overrides" }));

describe("a consumer's quota overrides (RL-10, ADR-0070)", () => {
  it("creates a standing quota override and shows the quota at its usage in red", async () => {
    mockConsumer();
    const sent = mockWrites();
    const user = userEvent.setup();
    renderApp("/consumers/7", ["key-admin"]);
    expect(await (await quotaOverrides()).findByText(/No quota overrides: the plan's quotas apply\./)).toBeInTheDocument();
    await user.click((await quotaOverrides()).getByRole("button", { name: "Add quota override" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Add a quota override" }));
    await user.type(dialog.getByRole("textbox", { name: "Daily quota" }), "5000");
    await user.type(dialog.getByRole("textbox", { name: "Note" }), "launch");
    await user.click(dialog.getByRole("button", { name: "Create quota override" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(sent).toEqual([{ method: "POST", path: "/api/v1/quota-overrides", ifMatch: null, body: { consumer_id: 7, quotas: { daily: 5000 }, note: "launch" } }]);
  });

  it("lists quota overrides as Quota override rows and edits one with If-Match", async () => {
    mockConsumer({ quotaOverrides: [quotaOverride({ ends_at: "2099-01-01T00:00:00Z", quotas: { daily: 1000, monthly: 20000 } })] });
    const sent = mockWrites();
    const user = userEvent.setup();
    renderApp("/consumers/7", ["admin"]);
    const row = within(await (await quotaOverrides()).findByRole("row", { name: /^Quota override/ }));
    expect(row.getByText("1,000 a day · 20,000 a month")).toBeInTheDocument();
    expect(row.getByText("Burst grant, active")).toBeInTheDocument();
    await user.click(row.getByRole("button", { name: "Edit quota override 50" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Quota override 50" }));
    const monthly = dialog.getByRole("textbox", { name: "Monthly quota" });
    await waitFor(() => expect(monthly).toHaveValue("20000"));
    await user.clear(monthly);
    await user.type(monthly, "30000");
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toEqual({
      method: "PUT", path: "/api/v1/quota-overrides/50", ifMatch: '"3"',
      body: { consumer_id: 7, quotas: { daily: 1000, monthly: 30000 }, ends_at: "2099-01-01T00:00:00Z" },
    });
  });

  it("deletes a quota override after confirming, with its version as If-Match", async () => {
    mockConsumer({ quotaOverrides: [quotaOverride()] });
    const sent = mockWrites();
    const user = userEvent.setup();
    renderApp("/consumers/7", ["key-admin"]);
    await user.click(await (await quotaOverrides()).findByRole("button", { name: "Delete quota override 50" }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByText(/The plan's quotas apply again/)).toBeInTheDocument();
    await user.click(dialog.getByRole("button", { name: /^Delete/ }));
    await waitFor(() => expect(sent).toEqual([{ method: "DELETE", path: "/api/v1/quota-overrides/50", ifMatch: '"3"', body: null }]));
  });

  it("disables every quota override write for a viewer, with the reason", async () => {
    mockConsumer({ quotaOverrides: [quotaOverride()] });
    renderApp("/consumers/7", ["viewer"]);
    for (const name of ["Add quota override", "Edit quota override 50", "Delete quota override 50"]) {
      const b = await (await quotaOverrides()).findByRole("button", { name });
      expect(b, name).toBeDisabled();
      expect(b, name).toHaveAccessibleDescription("Needs the key-admin or admin role");
    }
  });

  it("says when the quota usage cannot be read", async () => {
    mockConsumer();
    server.use(http.get("/api/v1/quota-usage", () => HttpResponse.json({ code: "internal", error: "boom" }, { status: 500 })));
    renderApp("/consumers/7", ["viewer"]);
    expect(await screen.findAllByText("Unavailable")).toHaveLength(2);
  });

  it("marks a period at its quota", async () => {
    mockConsumer();
    mockUsage([usage(7, [1000, 1000], [10, 500])]);
    renderApp("/consumers/7", ["viewer"]);
    expect(await screen.findByLabelText("Quota today: 1,000 / 1,000, at the quota")).toHaveClass("quota-full");
    expect(screen.getByLabelText("Quota this month: 10 / 500")).not.toHaveClass("quota-full");
  });
});

describe("a new consumer", () => {
  it("needs a tenant and creates it under the chosen one", async () => {
    mockConsumer();
    const sent: Sent[] = [];
    server.use(http.post("/api/v1/consumers", async ({ request }) => {
      const body = await request.json();
      sent.push({ method: "POST", path: "/api/v1/consumers", ifMatch: null, body });
      return HttpResponse.json(consumer({ id: 7, name: "checkout" }), { status: 201, headers: { ETag: '"1"' } });
    }));
    const user = userEvent.setup();
    const router = renderApp("/consumers/new", ["key-admin"]);
    await user.type(await screen.findByRole("textbox", { name: "Name (required)" }), "checkout");
    await user.click(screen.getByRole("button", { name: "Create consumer" }));
    expect(screen.getByRole("combobox", { name: "Tenant (required)" })).toHaveAccessibleDescription("Required");
    const tenant = screen.getByRole("combobox", { name: "Tenant (required)" });
    await waitFor(() => expect(within(tenant).getByRole("option", { name: "globex" })).toBeInTheDocument());
    await user.selectOptions(tenant, "globex");
    await user.click(screen.getByRole("button", { name: "Create consumer" }));
    await screen.findByRole("heading", { name: "Consumer checkout" });
    expect(sent).toEqual([{ method: "POST", path: "/api/v1/consumers", ifMatch: null, body: { name: "checkout", tenant_id: 2 } }]);
    expect(router.state.location.pathname).toBe("/consumers/7");
  });
});
