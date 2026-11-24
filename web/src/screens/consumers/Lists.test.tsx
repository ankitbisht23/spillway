// Consumers, access lists, plans and tenants (UI-04, UI-06, D9, RL-09).
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { setSession } from "../../auth/session";
import { mockConfig, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import { consumer, entry, mockUsage, SNAPSHOT, T0, type Sent, usage } from "./testing";

beforeEach(() => startSession());
afterEach(() => setSession(null));

/** Serves a list at path, recording each request's query; page answers each. */
function mockList(path: string, page: (q: URLSearchParams) => { items: unknown[]; next_cursor: string | null }) {
  const queries: URLSearchParams[] = [];
  server.use(http.get(path, ({ request }) => {
    const q = new URL(request.url).searchParams;
    queries.push(q);
    return HttpResponse.json(page(q));
  }));
  return queries;
}

describe("consumers (UI-06, D9)", () => {
  it("lists 50 a page with tenant and plan names, pages by cursor and filters by tenant", async () => {
    mockConfig(SNAPSHOT, ["acme", "globex"]);
    mockUsage([usage(7, [0, 0], [0, 0]), usage(9, [0, 0], [0, 0])]);
    const queries = mockList("/api/v1/consumers", (q) => (q.get("cursor") === "c2"
      ? { items: [consumer({ id: 9, name: "billing", tenant_id: 2 })], next_cursor: null }
      : { items: [consumer({ plan_id: 1, jwt_subject: "svc-checkout" })], next_cursor: "c2" }));
    const user = userEvent.setup();
    const router = renderApp("/consumers", ["viewer"]);
    const row = within(await screen.findByRole("row", { name: /checkout/ }));
    await waitFor(() => expect(row.getByText("acme")).toBeInTheDocument());
    expect(row.getByText("free")).toBeInTheDocument();
    expect(row.getByText("svc-checkout")).toBeInTheDocument();
    expect(screen.getByText(/Quota usage is what the control plane last flushed/)).toBeInTheDocument();
    expect(queries[0]?.get("limit")).toBe("50");

    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(await screen.findByRole("row", { name: /billing/ })).toBeInTheDocument();
    expect(router.state.location.search).toEqual({ cursor: "c2" });

    await user.selectOptions(screen.getByRole("combobox", { name: "Tenant" }), "globex");
    await waitFor(() => expect(router.state.location.search).toEqual({ tenant_id: 2 }));
    await waitFor(() => expect(queries.at(-1)?.get("tenant_id")).toBe("2"));
    expect(queries.at(-1)?.has("cursor")).toBe(false);
  });

  it("shows each consumer's quota usage today and this month from one call per page, red at the quota (RL-10)", async () => {
    mockConfig(SNAPSHOT, ["acme"]);
    const asked = mockUsage([usage(7, [1200, 5000], [3000, 100000]), usage(8, [5000, 5000], [0, 0]), usage(9, [0, 0], [0, 0])]);
    mockList("/api/v1/consumers", () => ({
      items: [consumer(), consumer({ id: 8, name: "billing" }), consumer({ id: 9, name: "reports" })],
      next_cursor: null,
    }));
    renderApp("/consumers", ["viewer"]);
    expect(await screen.findByRole("columnheader", { name: "Quota (today · month)" })).toBeInTheDocument();
    const row = (name: string) => within(screen.getByRole("row", { name: new RegExp(`^${name}`) }));
    await waitFor(() => expect(row("checkout").getByLabelText("Today: 1,200 / 5,000")).toBeInTheDocument());
    expect(row("checkout").getByLabelText("This month: 3,000 / 100,000")).toBeInTheDocument();
    const full = row("billing").getByLabelText("Today: 5,000 / 5,000, at the quota");
    expect(full).toHaveClass("quota-full");
    expect(row("billing").getByLabelText("This month: —")).not.toHaveClass("quota-full");
    expect(row("reports").getByLabelText("Today: —")).toBeInTheDocument();
    expect(asked).toEqual([["7", "8", "9"]]);
  });

  it("says Unavailable in the quota column when the usage cannot be read", async () => {
    mockConfig(SNAPSHOT, ["acme"]);
    server.use(http.get("/api/v1/quota-usage", () => HttpResponse.json({ code: "internal", error: "boom" }, { status: 500 })));
    mockList("/api/v1/consumers", () => ({ items: [consumer()], next_cursor: null }));
    renderApp("/consumers", ["viewer"]);
    const row = within(await screen.findByRole("row", { name: /^checkout/ }));
    expect(await row.findByText("Unavailable")).toBeInTheDocument();
  });

  it("says why a viewer cannot create one, in the empty state and on the button", async () => {
    mockConfig(SNAPSHOT, []);
    mockList("/api/v1/consumers", () => ({ items: [], next_cursor: null }));
    renderApp("/consumers", ["viewer"]);
    expect(await screen.findByText("No consumers yet. Needs the key-admin or admin role to create one.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New consumer" })).toHaveAccessibleDescription("Needs the key-admin or admin role");
  });

  it("shows a list error with its request ID and Retry", async () => {
    mockConfig(SNAPSHOT, []);
    server.use(http.get("/api/v1/consumers", () => HttpResponse.json({ code: "internal", error: "boom" }, { status: 500, headers: { "X-Request-Id": "req-4" } })));
    renderApp("/consumers", ["admin"]);
    expect(await screen.findByRole("heading", { name: "Something went wrong" })).toBeInTheDocument();
    expect(screen.getByText("req-4")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });
});

describe("access lists (RL-09)", () => {
  it("lists entries, filters by action and subject type, and creates a CIDR deny", async () => {
    const queries = mockList("/api/v1/access-lists", () => ({
      items: [entry(), entry({ id: 41, action: "allow", subject_type: "consumer", consumer_id: 7, cidr: null }), entry({ id: 42, subject_type: "key", api_key_id: 20, cidr: null, note: "leaked" })],
      next_cursor: null,
    }));
    const sent: Sent[] = [];
    server.use(http.post("/api/v1/access-lists", async ({ request }) => {
      const body = await request.json();
      sent.push({ method: "POST", path: "/api/v1/access-lists", ifMatch: null, body });
      return HttpResponse.json({ ...(body as object), id: 50, version: 1, created_at: T0, updated_at: T0 }, { status: 201 });
    }));
    const user = userEvent.setup();
    const router = renderApp("/access-lists", ["key-admin"]);
    expect(await screen.findByRole("row", { name: /^40 deny CIDR 203\.0\.113\.0\/24/ })).toBeInTheDocument();
    expect(screen.getByRole("row", { name: /^41 allow Consumer 7/ })).toBeInTheDocument();
    expect(screen.getByRole("row", { name: /^42 deny API key 20 leaked/ })).toBeInTheDocument();

    await user.selectOptions(screen.getByRole("combobox", { name: "Action" }), "deny");
    await user.selectOptions(screen.getByRole("combobox", { name: "Subject" }), "cidr");
    await waitFor(() => expect(router.state.location.search).toEqual({ action: "deny", subject_type: "cidr" }));
    await waitFor(() => expect(queries.at(-1)?.toString()).toBe("limit=50&action=deny&subject_type=cidr"));

    await user.click(screen.getByRole("button", { name: "New entry" }));
    const dialog = within(await screen.findByRole("dialog", { name: "New access-list entry" }));
    await user.selectOptions(dialog.getByRole("combobox", { name: "Action (required)" }), "deny");
    await user.selectOptions(dialog.getByRole("combobox", { name: "Subject type (required)" }), "cidr");
    await user.type(dialog.getByRole("textbox", { name: "CIDR" }), "198.51.100.0/24");
    await user.click(dialog.getByRole("button", { name: "Create entry" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(sent).toEqual([{ method: "POST", path: "/api/v1/access-lists", ifMatch: null, body: { action: "deny", subject_type: "cidr", cidr: "198.51.100.0/24" } }]);
  });

  it("puts the server's 422 on the CIDR field", async () => {
    mockList("/api/v1/access-lists", () => ({ items: [], next_cursor: null }));
    server.use(http.post("/api/v1/access-lists", () => HttpResponse.json({
      code: "validation_failed", error: "invalid", details: [{ entity: "access_list", path: "cidr", message: 'cidr "10.0.0.1/8" has bits set past its prefix; want 10.0.0.0/8' }],
    }, { status: 422 })));
    const user = userEvent.setup();
    renderApp("/access-lists", ["admin"]);
    await user.click(await screen.findByRole("button", { name: "New entry" }));
    const dialog = within(await screen.findByRole("dialog"));
    await user.selectOptions(dialog.getByRole("combobox", { name: "Action (required)" }), "allow");
    await user.selectOptions(dialog.getByRole("combobox", { name: "Subject type (required)" }), "cidr");
    const cidr = dialog.getByRole("textbox", { name: "CIDR" });
    await user.type(cidr, "10.0.0.1/8");
    await user.click(dialog.getByRole("button", { name: "Create entry" }));
    await waitFor(() => expect(cidr).toHaveAccessibleDescription(/has bits set past its prefix/));
  });

  it("deletes an entry after confirming, with its version as If-Match", async () => {
    mockList("/api/v1/access-lists", () => ({ items: [entry()], next_cursor: null }));
    const sent: Sent[] = [];
    server.use(http.delete("/api/v1/access-lists/40", ({ request }) => {
      sent.push({ method: "DELETE", path: "/api/v1/access-lists/40", ifMatch: request.headers.get("If-Match"), body: null });
      return new HttpResponse(null, { status: 204 });
    }));
    const user = userEvent.setup();
    renderApp("/access-lists", ["key-admin"]);
    await user.click(await screen.findByRole("button", { name: "Delete entry 40" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Delete access-list entry 40?" }));
    expect(dialog.getByText(/It stops refusing its subject\./)).toBeInTheDocument();
    expect(sent).toEqual([]);
    await user.click(dialog.getByRole("button", { name: "Delete access-list entry" }));
    await waitFor(() => expect(sent).toEqual([{ method: "DELETE", path: "/api/v1/access-lists/40", ifMatch: '"3"', body: null }]));
  });

  it("shows a publisher every write disabled with the reason", async () => {
    mockList("/api/v1/access-lists", () => ({ items: [entry()], next_cursor: null }));
    renderApp("/access-lists", ["publisher"]);
    for (const name of ["New entry", "Edit entry 40", "Delete entry 40"]) {
      const b = await screen.findByRole("button", { name });
      expect(b, name).toBeDisabled();
      expect(b, name).toHaveAccessibleDescription("Needs the key-admin or admin role");
    }
  });
});

describe("plans (RL-08)", () => {
  it("lists each plan's default limits, read-only", async () => {
    mockList("/api/v1/plans", () => ({
      items: [
        { id: 1, name: "free", default_limits: [{ policy: "per-consumer", limits: [{ rate: 10, period: "1s" }, { rate: 1000, period: "1h" }] }], quotas: { daily: 100000, monthly: 2000000 }, version: 1, created_at: T0, updated_at: T0 },
        { id: 2, name: "pro", version: 1, created_at: T0, updated_at: T0 },
      ],
      next_cursor: null,
    }));
    renderApp("/plans", ["viewer"]);
    expect(await screen.findByRole("row", { name: /^free per-consumer: 10\/1s, 1000\/1h 100,000 a day · 2,000,000 a month/ })).toBeInTheDocument();
    expect(screen.getByRole("row", { name: /^pro None: the policies' own limits apply None/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /New|Edit|Delete/ })).not.toBeInTheDocument();
  });
});

describe("tenants (admin only, ADR-0058)", () => {
  it.each(["viewer", "editor", "publisher", "key-admin"] as const)("%s: hidden from the navigation, Not allowed at its URL, no API call", async (role) => {
    mockConfig(SNAPSHOT, []);
    renderApp("/tenants/new", [role]);
    expect(await screen.findByRole("heading", { name: "Not allowed" })).toBeInTheDocument();
    expect(screen.getByText("Needs the admin role.")).toBeInTheDocument();
    expect(within(screen.getByRole("navigation", { name: "Screens" })).queryByRole("link", { name: "Tenants" })).not.toBeInTheDocument();
  });

  it("an admin lists tenants and creates one on a published plan", async () => {
    mockConfig(SNAPSHOT, ["acme"]);
    const tenant = { id: 3, name: "initech", plan_id: 2, status: "active", version: 1, created_at: T0, updated_at: T0 };
    const sent: Sent[] = [];
    server.use(
      http.post("/api/v1/tenants", async ({ request }) => {
        const body = await request.json();
        sent.push({ method: "POST", path: "/api/v1/tenants", ifMatch: null, body });
        return HttpResponse.json(tenant, { status: 201, headers: { ETag: '"1"' } });
      }),
      http.get("/api/v1/tenants/3", () => HttpResponse.json(tenant, { headers: { ETag: '"1"' } })),
    );
    const user = userEvent.setup();
    const router = renderApp("/tenants", ["admin"]);
    expect(await screen.findByRole("row", { name: /^acme/ })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "New tenant" }));
    await user.type(await screen.findByRole("textbox", { name: "Name (required)" }), "initech");
    const plan = screen.getByRole("combobox", { name: "Plan (required)" });
    await waitFor(() => expect(within(plan).getByRole("option", { name: "pro" })).toBeInTheDocument());
    await user.selectOptions(plan, "pro");
    await user.click(screen.getByRole("button", { name: "Create tenant" }));
    expect(await screen.findByRole("heading", { name: "Tenant initech" })).toBeInTheDocument();
    expect(sent).toEqual([{ method: "POST", path: "/api/v1/tenants", ifMatch: null, body: { name: "initech", plan_id: 2 } }]);
    expect(router.state.location.pathname).toBe("/tenants/3");
  });
});
