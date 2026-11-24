import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { Role } from "../../auth/permissions";
import { setSession } from "../../auth/session";
import { mockConfig, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import type { Policy } from "./form";

const policy = (over: Partial<Policy> = {}): Policy => ({
  id: 9, name: "per-client", level: "route", match: { routes: ["orders"] }, key_template: "{header:X-Client}", algorithm: "gcra",
  limits: [{ rate: 100, period: "1s" }], local: true, mode: "shadow", version: 4, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z",
  ...over,
});

interface Sent {
  method: string;
  ifMatch: string | null;
  body: unknown;
}

/** Serves policy 9 at ETag "<version>"; PUT answers with answer, else the body at the next version. */
function mockPolicy(current: Policy, answer?: () => Response) {
  const sent: Sent[] = [];
  let now = current;
  server.use(
    http.get("/api/v1/policies/9", () => HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } })),
    http.put("/api/v1/policies/9", async ({ request }) => {
      const body = (await request.json()) as Policy;
      sent.push({ method: "PUT", ifMatch: request.headers.get("If-Match"), body });
      if (answer) return answer();
      now = { ...now, ...body, version: now.version + 1 };
      return HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } });
    }),
    http.delete("/api/v1/policies/9", ({ request }) => {
      sent.push({ method: "DELETE", ifMatch: request.headers.get("If-Match"), body: null });
      return answer ? answer() : new HttpResponse(null, { status: 204 });
    }),
    http.get("/api/v1/policies", () => HttpResponse.json({ items: [], next_cursor: null })),
  );
  return {
    sent,
    /** Someone else's edit: the next GET answers it. */
    change: (p: Policy) => {
      now = p;
    },
  };
}

beforeEach(() => {
  startSession();
  mockConfig({ policies: [{ id: 9, name: "per-client", mode: "shadow", key_template: "{header:X-Client}" }] });
});
afterEach(() => setSession(null));

const field = (name: string) => screen.getByRole("textbox", { name }) as HTMLInputElement;
const limit = (i: number) => within(screen.getByRole("group", { name: `Limit ${i}` }));

async function fillValid() {
  await userEvent.type(field("Name"), "per-client");
  await userEvent.type(field("Routes"), "orders");
  await userEvent.type(field("Key template"), "{{ip}");
  await userEvent.type(limit(1).getByRole("textbox", { name: "Rate" }), "100");
}

describe("a new policy (UI-01)", () => {
  it("checks the form in the browser, in place, and sends nothing until it passes", async () => {
    let posted = 0;
    server.use(http.post("/api/v1/policies", () => {
      posted++;
      return HttpResponse.json({}, { status: 500 });
    }));
    renderApp("/policies/new", ["editor"]);
    await userEvent.click(await screen.findByRole("button", { name: "Create policy" }));
    expect(field("Name")).toHaveAccessibleDescription("1–64 characters from A–Z, a–z, 0–9, '.', '_' and '-'");
    expect(field("Routes")).toHaveAccessibleDescription(/^Route names, comma-separated; \* for every route\. Required/);
    expect(field("Key template")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("group", { name: "Limits" })).toHaveTextContent("Limit 1: rate must be a whole number");
    expect(screen.getByText("Fix the fields marked above.")).toBeInTheDocument();
    // Fixing a field clears its error as the user types.
    await userEvent.type(field("Name"), "per-client");
    expect(field("Name")).not.toHaveAttribute("aria-invalid");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Failure mode" }), "fail_closed");
    await userEvent.click(screen.getByRole("checkbox", { name: "Local (per gateway, no Redis)" }));
    expect(screen.getByText("Only Redis policies have a failure mode; local policies never fail")).toBeInTheDocument();
    expect(posted).toBe(0);
  });

  it("creates it in shadow mode, then opens it with the unpublished-changes banner", async () => {
    let body: unknown;
    server.use(http.post("/api/v1/policies", async ({ request }) => {
      body = await request.json();
      return HttpResponse.json(policy({ id: 9, local: false, key_template: "{ip}", version: 2 }), { status: 201, headers: { ETag: '"2"' } });
    }));
    mockPolicy(policy({ local: false, key_template: "{ip}", version: 2 }));
    const router = renderApp("/policies/new", ["editor"]);
    await screen.findByRole("button", { name: "Create policy" });
    expect(screen.getByRole("combobox", { name: "Mode" })).toHaveValue("shadow");
    await fillValid();
    await userEvent.click(screen.getByRole("button", { name: "Create policy" }));
    await screen.findByRole("heading", { name: "Policy per-client" });
    expect(body).toEqual({
      name: "per-client", level: "route", match: { routes: ["orders"] }, key_template: "{ip}", algorithm: "gcra", mode: "shadow",
      limits: [{ rate: 100, period: "1s" }],
    });
    expect(router.state.location.pathname).toBe("/policies/9");
    const banner = screen.getByRole("status");
    expect(banner).toHaveTextContent("Saved to the working set. Unpublished changes: review and publish.");
    expect(within(banner).getByRole("link", { name: "review and publish" })).toHaveAttribute("href", "/publish");
  });

  it("shows the server's 422 on its field, and the rest below", async () => {
    server.use(http.post("/api/v1/policies", () => HttpResponse.json({
      error: "validation failed", code: "validation_failed", request_id: "r",
      details: [{ entity: "policy", message: 'match.routes: route "orders" is not defined' }, { entity: "policy", message: "something odd" }],
    }, { status: 422 })));
    renderApp("/policies/new", ["admin"]);
    await screen.findByRole("button", { name: "Create policy" });
    await fillValid();
    await userEvent.click(screen.getByRole("button", { name: "Create policy" }));
    await waitFor(() => expect(field("Routes")).toHaveAccessibleDescription(/match\.routes: route "orders" is not defined$/));
    expect(screen.getByRole("list", { name: "Other errors" })).toHaveTextContent("something odd");
  });

  it("for a viewer: the form is read-only and Create says why", async () => {
    renderApp("/policies/new", ["viewer"]);
    const create = await screen.findByRole("button", { name: "Create policy" });
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription("Needs the editor or admin role");
    expect(field("Name")).toBeDisabled();
  });
});

describe("an existing policy (UI-01, CP-14)", () => {
  it("saves with If-Match on the ETag it loaded", async () => {
    const m = mockPolicy(policy());
    renderApp("/policies/9", ["editor"]);
    await screen.findByRole("heading", { name: "Policy per-client" });
    expect(field("Key template")).toHaveValue("{header:X-Client}");
    expect(screen.getByRole("checkbox", { name: "Local (per gateway, no Redis)" })).toBeChecked();
    await userEvent.clear(limit(1).getByRole("textbox", { name: "Rate" }));
    await userEvent.type(limit(1).getByRole("textbox", { name: "Rate" }), "50");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Saved to the working set\./)).toBeInTheDocument();
    expect(m.sent).toEqual([{
      method: "PUT", ifMatch: '"4"',
      body: { name: "per-client", level: "route", match: { routes: ["orders"] }, key_template: "{header:X-Client}", algorithm: "gcra", mode: "shadow", local: true, limits: [{ rate: 50, period: "1s" }] },
    }]);
  });

  it("sets the lease size k of a Redis policy, shown only for Redis gcra and sliding_window policies (RL-18)", async () => {
    const m = mockPolicy(policy({ local: false, failure_mode: "fail_open", lease_k: 10 }));
    renderApp("/policies/9", ["editor"]);
    await screen.findByRole("heading", { name: "Policy per-client" });
    const k = field("Lease size k");
    expect(k).toHaveValue("10");
    expect(k).toHaveAccessibleDescription(/^0 = strict: one Redis call per request\. k > 0 = batched: .* up to replicas × k\. Enforce mode only\.$/);
    await userEvent.clear(k);
    await userEvent.type(k, "101");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(field("Lease size k")).toHaveAccessibleDescription(/At most limit 1's burst of 100/);
    expect(m.sent).toEqual([]);
    await userEvent.clear(field("Lease size k"));
    await userEvent.type(field("Lease size k"), "25");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Saved to the working set\./)).toBeInTheDocument();
    expect(m.sent[0]?.body).toMatchObject({ lease_k: 25, failure_mode: "fail_open" });
    // Local, or a local-only algorithm: no lease.
    await userEvent.click(screen.getByRole("checkbox", { name: "Local (per gateway, no Redis)" }));
    expect(screen.queryByRole("textbox", { name: "Lease size k" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("checkbox", { name: "Local (per gateway, no Redis)" }));
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Algorithm" }), "sliding_window");
    expect(field("Lease size k")).toHaveValue("25");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Algorithm" }), "fixed_window");
    expect(screen.queryByRole("textbox", { name: "Lease size k" })).not.toBeInTheDocument();
  });

  it("on 412 offers Reload, which loads the current version into the form", async () => {
    const m = mockPolicy(policy(), () => HttpResponse.json({ error: "If-Match does not match", code: "precondition_failed", request_id: "r", details: [{ version: 5 }] }, { status: 412 }));
    renderApp("/policies/9", ["editor"]);
    await screen.findByRole("heading", { name: "Policy per-client" });
    await userEvent.type(field("Methods"), "GET");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/This policy changed since you opened it\./)).toBeInTheDocument();
    m.change(policy({ key_template: "{ip}", version: 5 }));
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(field("Key template")).toHaveValue("{ip}"));
    expect(field("Methods")).toHaveValue("");
    expect(screen.queryByText(/This policy changed/)).toBeNull();
  });

  it("promotes shadow to enforce as an edit with If-Match, then points to publish", async () => {
    const m = mockPolicy(policy());
    renderApp("/policies/9", ["editor"]);
    await userEvent.click(await screen.findByRole("button", { name: "Promote to enforce" }));
    expect(await screen.findByText(/Promoted to enforce in the working set\./)).toBeInTheDocument();
    expect(m.sent).toEqual([{
      method: "PUT", ifMatch: '"4"',
      body: { name: "per-client", level: "route", match: { routes: ["orders"] }, key_template: "{header:X-Client}", algorithm: "gcra", limits: [{ rate: 100, period: "1s" }], local: true, mode: "enforce" },
    }]);
    expect(screen.getByRole("link", { name: "review and publish" })).toHaveAttribute("href", "/publish");
    expect(screen.getByRole("combobox", { name: "Mode" })).toHaveValue("enforce");
    expect(screen.queryByRole("button", { name: "Promote to enforce" })).toBeNull();
  });

  it("on a promote 412 offers Reload, which loads the current version without a page reload", async () => {
    let fail = true;
    const m = mockPolicy(policy(), () =>
      fail ? HttpResponse.json({ error: "If-Match does not match", code: "precondition_failed", request_id: "r" }, { status: 412 }) : HttpResponse.json({}),
    );
    renderApp("/policies/9", ["editor"]);
    await userEvent.click(await screen.findByRole("button", { name: "Promote to enforce" }));
    expect(await screen.findByText(/This policy changed since you opened it\./)).toBeInTheDocument();
    fail = false;
    m.change(policy({ key_template: "{ip}", version: 5 }));
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(field("Key template")).toHaveValue("{ip}"));
    expect(screen.queryByText(/This policy changed/)).toBeNull();
    expect(screen.getByRole("button", { name: "Promote to enforce" })).toBeEnabled();
    expect(m.sent).toEqual([expect.objectContaining({ method: "PUT", ifMatch: '"4"' })]);
  });

  it("does not promote over unsaved edits", async () => {
    mockPolicy(policy());
    renderApp("/policies/9", ["editor"]);
    await screen.findByRole("button", { name: "Promote to enforce" });
    await userEvent.type(field("Methods"), "GET");
    expect(screen.getByRole("button", { name: "Promote to enforce" })).toHaveAccessibleDescription("Save or discard your edits first");
  });

  it("links the shadow report of a published policy", async () => {
    mockPolicy(policy());
    renderApp("/policies/9", ["viewer"]);
    expect(await screen.findByRole("link", { name: "Shadow report" })).toHaveAttribute("href", "/policies/9/shadow");
    expect(screen.getByText(/in version 3 as shadow/)).toBeInTheDocument();
  });

  it("deletes after confirming, with If-Match", async () => {
    const m = mockPolicy(policy());
    const router = renderApp("/policies/9", ["admin"]);
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = screen.getByRole("dialog", { name: "Delete policy per-client?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete policy" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/policies"));
    expect(m.sent).toEqual([{ method: "DELETE", ifMatch: '"4"', body: null }]);
  });

  it("shows a delete the server refuses", async () => {
    mockPolicy(policy(), () => HttpResponse.json({ error: "policy per-client is used by override 3", code: "conflict", request_id: "r" }, { status: 409 }));
    renderApp("/policies/9", ["admin"]);
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete policy" }));
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent("Conflict: policy per-client is used by override 3");
  });

  it.each<[Role, string | null, string | null]>([
    ["viewer", "Needs the editor or admin role", "Needs the editor or admin role"],
    ["publisher", "Needs the editor or admin role", "Needs the editor or admin role"],
    ["key-admin", "Needs the editor or admin role", "Needs the editor or admin role"],
    ["editor", null, null],
    ["admin", null, null],
  ])("for %s: save, promote and delete are allowed or say why", async (role, edit, del) => {
    mockPolicy(policy());
    renderApp("/policies/9", [role]);
    await screen.findByRole("heading", { name: "Policy per-client" });
    for (const [name, reason] of [["Save", edit], ["Promote to enforce", edit], ["Delete", del]] as const) {
      const b = screen.getByRole("button", { name });
      if (reason) {
        expect(b, name).toBeDisabled();
        expect(b, name).toHaveAccessibleDescription(reason);
      } else {
        expect(b, name).toBeEnabled();
      }
    }
  });

  it("shows Not found and the error state", async () => {
    server.use(http.get("/api/v1/policies/9", () => HttpResponse.json({ error: "no such entity", code: "not_found", request_id: "r" }, { status: 404 })));
    renderApp("/policies/9", ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("shows Not found for an ID that is not one", async () => {
    renderApp("/policies/abc", ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});
