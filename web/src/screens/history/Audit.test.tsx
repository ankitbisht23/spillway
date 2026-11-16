import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { components } from "../../api/schema.gen";
import { setSession } from "../../auth/session";
import { startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import { instant } from "./Audit";

type Record = components["schemas"]["AuditRecord"];

const HOSTILE = `<img src=x onerror="alert(1)">`;

function record(id: number, over: Partial<Record> = {}): Record {
  return {
    id, at: "2026-11-16T10:00:00Z", actor: { sub: "sub-ada", name: "ada", roles: ["publisher"] }, action: "config.publish", entity: "config",
    entity_id: "12", diff: { from_version: 11, to_version: 12 }, note: "ship it", request_id: `req-${id}`, config_version: 12, identity_version: null, ...over,
  };
}

/** Serves the audit log, two pages (cursor "Mw" is the second), recording each request's query. */
function mockAudit(items: Record[] = [record(3), record(2, { action: "route.update", entity: "route", entity_id: "5", note: HOSTILE, config_version: null })]) {
  const asked: URLSearchParams[] = [];
  server.use(http.get("/api/v1/audit", ({ request }) => {
    const q = new URL(request.url).searchParams;
    asked.push(q);
    return q.get("cursor") === "Mw"
      ? HttpResponse.json({ items: [record(1, { action: "key.issue", entity: "api_key", entity_id: "7", config_version: null, identity_version: 4 })], next_cursor: null })
      : HttpResponse.json({ items, next_cursor: "Mw" });
  }));
  return asked;
}

beforeEach(() => startSession());
afterEach(() => setSession(null));

describe("the audit log (CP-13, UI-06)", () => {
  it("lists 50 a page with actor, action, entity, note, versions and before/after, and pages by cursor", async () => {
    const asked = mockAudit();
    const router = renderApp("/audit", ["admin"]);
    const publish = await screen.findByRole("row", { name: /config\.publish/ });
    expect(publish).toHaveTextContent("ada");
    expect(publish).toHaveTextContent("publisher");
    expect(publish).toHaveTextContent("config 12");
    expect(within(publish).getByRole("link", { name: "12" })).toHaveAttribute("href", "/history/12");
    expect(within(publish).getByText(/"to_version": 12/)).toBeInTheDocument();
    expect(within(publish).getByText("req-3")).toBeInTheDocument();
    const update = screen.getByRole("row", { name: /route\.update/ });
    expect(within(update).getByText(HOSTILE)).toBeInTheDocument();
    expect(within(update).queryByRole("link")).toBeNull();
    expect(document.querySelector("img")).toBeNull();
    expect(asked[0]?.get("limit")).toBe("50");

    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(await screen.findByRole("row", { name: /key\.issue/ })).toHaveTextContent("identity 4");
    expect(router.state.location.search).toEqual({ cursor: "Mw" });
  });

  it("filters exactly through the URL, resets the cursor, and clears", async () => {
    const asked = mockAudit();
    const router = renderApp("/audit?cursor=Mw", ["admin"]);
    await screen.findByRole("row", { name: /key\.issue/ });
    const filters = screen.getByRole("form", { name: "Filters" });
    await userEvent.type(within(filters).getByRole("textbox", { name: "Entity" }), "route");
    await userEvent.type(within(filters).getByRole("textbox", { name: "Entity ID" }), "5");
    await userEvent.type(within(filters).getByRole("textbox", { name: "Actor (sub)" }), "sub-ada");
    const since = within(filters).getByLabelText("Since");
    await userEvent.type(since, "2026-11-16T09:30");
    await userEvent.click(within(filters).getByRole("button", { name: "Apply" }));
    const want = { entity: "route", entity_id: "5", actor_sub: "sub-ada", since: new Date("2026-11-16T09:30").toISOString() };
    await waitFor(() => expect(router.state.location.search).toEqual(want));
    await waitFor(() => expect(asked).toHaveLength(2));
    const q = asked[1];
    expect(Object.fromEntries(q?.entries() ?? [])).toEqual({ limit: "50", ...want });

    await userEvent.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
    expect(within(screen.getByRole("form", { name: "Filters" })).getByRole("textbox", { name: "Entity" })).toHaveValue("");
  });

  it("says when nothing matches the filters", async () => {
    server.use(http.get("/api/v1/audit", () => HttpResponse.json({ items: [], next_cursor: null })));
    renderApp("/audit?entity=route", ["admin"]);
    expect(await screen.findByText("No audit record matches these filters.")).toBeInTheDocument();
  });

  it("shows the empty state", async () => {
    server.use(http.get("/api/v1/audit", () => HttpResponse.json({ items: [], next_cursor: null })));
    renderApp("/audit", ["admin"]);
    expect(await screen.findByText("No change has been made yet.")).toBeInTheDocument();
  });

  it("shows a 400 (a filter the server refused) as an error with its message", async () => {
    server.use(http.get("/api/v1/audit", () =>
      HttpResponse.json({ error: "entity_id: needs entity", code: "invalid_request", request_id: "req-9" }, { status: 400 })));
    renderApp("/audit?entity_id=5", ["admin"]);
    expect(await screen.findByRole("alert")).toHaveTextContent("entity_id: needs entity");
  });

  it.each([["viewer"], ["editor"], ["publisher"], ["key-admin"]] as const)("is Not allowed for %s, without calling the API", async (role) => {
    const asked = mockAudit();
    renderApp("/audit", [role]);
    expect(await screen.findByRole("heading", { name: "Not allowed" })).toBeInTheDocument();
    expect(screen.getByText("Needs the admin role.")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Audit" })).toBeNull();
    expect(asked).toEqual([]);
  });
});

describe("instant", () => {
  it("turns a datetime-local value into an RFC 3339 instant", () => {
    expect(instant("")).toBeUndefined();
    expect(instant("not a date")).toBeUndefined();
    expect(instant("2026-11-16T09:30")).toBe(new Date("2026-11-16T09:30").toISOString());
  });
});
