import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { setSession } from "../../auth/session";
import { startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import type { Route } from "./fields";

const route = (over: Partial<Route> = {}): Route => ({
  id: 3, name: "orders", upstream: "orders-up", version: 5, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z",
  match: { path_prefix: "/v1/orders", headers: [{ name: "X-Tier", exact: "gold" }] }, max_body_bytes: 1024,
  ...over,
});

interface Sent {
  method: string;
  ifMatch: string | null;
  body: unknown;
}

/** Serves route 3 at ETag "<version>"; a PUT answers with answer, else the body at the next version. */
function mockRoute(current: Route, answer?: () => Response) {
  const sent: Sent[] = [];
  let now = current;
  server.use(
    http.get("/api/v1/routes/3", () => HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } })),
    http.put("/api/v1/routes/3", async ({ request }) => {
      const body = (await request.json()) as Route;
      sent.push({ method: "PUT", ifMatch: request.headers.get("If-Match"), body });
      if (answer) return answer();
      now = { ...body, id: 3, version: now.version + 1, created_at: now.created_at, updated_at: now.updated_at };
      return HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } });
    }),
    http.delete("/api/v1/routes/3", ({ request }) => {
      sent.push({ method: "DELETE", ifMatch: request.headers.get("If-Match"), body: null });
      return new HttpResponse(null, { status: 204 });
    }),
    http.get("/api/v1/routes", () => HttpResponse.json({ items: [], next_cursor: null })),
    http.get("/api/v1/upstreams", () => HttpResponse.json({ items: [{ id: 8, name: "orders-up" }], next_cursor: null })),
    http.get("/api/v1/stream", () => new HttpResponse(new ReadableStream(), { headers: { "Content-Type": "text/event-stream" } })),
  );
  return {
    sent,
    /** Someone else's edit: the next GET answers it. */
    change: (r: Route) => {
      now = r;
    },
  };
}

beforeEach(() => startSession());
afterEach(() => setSession(null));

const field = (name: string) => screen.getByRole("textbox", { name }) as HTMLInputElement;

describe("a route (UI-06, CP-14)", () => {
  it("saves an edit with If-Match, keeps what the form does not change, and points to the publish page", async () => {
    const api = mockRoute(route());
    renderApp("/routes/3", ["editor"]);
    await waitFor(() => expect(field("Response headers to set")).toBeInTheDocument());
    expect(field("Header predicates")).toHaveValue("X-Tier: gold");
    expect(await screen.findByRole("link", { name: "orders-up" })).toHaveAttribute("href", "/upstreams/8");
    await userEvent.type(field("Response headers to set"), "X-Verify: run-1");
    expect(screen.queryByText(/Unpublished changes/)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("status")).toHaveTextContent("Saved to the working set. Unpublished changes: review and publish.");
    expect(screen.getByRole("link", { name: "review and publish" })).toHaveAttribute("href", "/publish");
    expect(api.sent).toEqual([{
      method: "PUT", ifMatch: '"5"',
      body: { name: "orders", upstream: "orders-up", match: { path_prefix: "/v1/orders", headers: [{ name: "X-Tier", exact: "gold" }] }, max_body_bytes: 1024, response_headers: { set: [{ name: "X-Verify", value: "run-1" }] } },
    }]);
    // The form now holds the saved version.
    expect(field("Response headers to set")).toHaveValue("X-Verify: run-1");
  });

  it("shows the server's 422 on its field and the rest under the form", async () => {
    mockRoute(route(), () => HttpResponse.json({
      code: "validation_failed", error: "invalid route",
      details: [{ entity: "route", message: 'auth_mode "basic": want none, api_key, jwt or either' }, { entity: "route", message: "the config is too large" }],
    }, { status: 422 }));
    renderApp("/routes/3", ["admin"]);
    await userEvent.type(await screen.findByRole("textbox", { name: "Path" }), "/v1/x");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("list", { name: "Other errors" })).toHaveTextContent("the config is too large");
    expect(screen.getByRole("combobox", { name: "Auth mode" })).toHaveAccessibleDescription('Unset means none (GW-20). auth_mode "basic": want none, api_key, jwt or either');
  });

  it("with every match field cleared, still sends match and shows the server's 422 on Path", async () => {
    const api = mockRoute(route(), () => HttpResponse.json({
      code: "validation_failed", error: "invalid route",
      details: [{ entity: "route", message: "set exactly one of match.path and match.path_prefix" }],
    }, { status: 422 }));
    renderApp("/routes/3", ["editor"]);
    await userEvent.clear(await screen.findByRole("textbox", { name: "Path prefix" }));
    await userEvent.clear(field("Header predicates"));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(field("Path")).toHaveAccessibleDescription(/set exactly one of match\.path and match\.path_prefix$/));
    expect(screen.queryByRole("list", { name: "Other errors" })).not.toBeInTheDocument();
    expect(api.sent).toEqual([{
      method: "PUT", ifMatch: '"5"', body: { name: "orders", upstream: "orders-up", match: {}, max_body_bytes: 1024 },
    }]);
  });

  it("checks fields in the browser before sending anything", async () => {
    const api = mockRoute(route());
    renderApp("/routes/3", ["editor"]);
    const priority = await screen.findByRole("textbox", { name: "Priority" });
    await userEvent.type(priority, "high");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(priority).toHaveAccessibleDescription(/Must be a whole number$/);
    expect(screen.getByText("Fix the fields marked above.")).toBeInTheDocument();
    await userEvent.clear(priority);
    expect(priority).not.toHaveAttribute("aria-invalid");
    expect(api.sent).toEqual([]);
  });

  it("answers a 412 with Reload, which loads the current version into the form", async () => {
    const api = mockRoute(route(), () => HttpResponse.json({ code: "precondition_failed", error: "changed" }, { status: 412 }));
    renderApp("/routes/3", ["editor"]);
    await userEvent.type(await screen.findByRole("textbox", { name: "Priority" }), "7");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("This route changed since you opened it.");
    api.change(route({ version: 6, priority: 2 }));
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(field("Priority")).toHaveValue("2"));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("disables Save and Delete with their reason for a viewer", async () => {
    mockRoute(route());
    renderApp("/routes/3", ["viewer", "publisher"]);
    const save = await screen.findByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    expect(save).toHaveAccessibleDescription("Needs the editor or admin role");
    expect(screen.getByRole("button", { name: "Delete" })).toHaveAccessibleDescription("Needs the editor or admin role");
    expect(field("Name (required)")).toBeDisabled();
  });

  it("deletes after confirming, with If-Match", async () => {
    const api = mockRoute(route());
    const router = renderApp("/routes/3", ["admin"]);
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    expect(screen.getByRole("dialog", { name: "Delete route orders?" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Delete route" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/routes"));
    expect(api.sent).toEqual([{ method: "DELETE", ifMatch: '"5"', body: null }]);
  });

  it("shows Not found for an ID that is no number", async () => {
    renderApp("/routes/abc");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("shows Not found for a route that is gone", async () => {
    server.use(
      http.get("/api/v1/routes/4", () => HttpResponse.json({ code: "not_found", error: "no route 4" }, { status: 404 })),
      http.get("/api/v1/upstreams", () => HttpResponse.json({ items: [], next_cursor: null })),
    );
    renderApp("/routes/4");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});

describe("a new route (UI-06)", () => {
  it("creates it and opens its page", async () => {
    let sent: unknown;
    server.use(
      http.get("/api/v1/upstreams", () => HttpResponse.json({ items: [{ id: 8, name: "orders-up" }], next_cursor: null })),
      http.post("/api/v1/routes", async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ ...(sent as object), id: 3, version: 1, created_at: "", updated_at: "" }, { status: 201, headers: { ETag: '"1"' } });
      }),
      http.get("/api/v1/stream", () => new HttpResponse(new ReadableStream(), { headers: { "Content-Type": "text/event-stream" } })),
    );
    const router = renderApp("/routes/new", ["editor"]);
    await userEvent.type(await screen.findByRole("textbox", { name: "Name (required)" }), "orders");
    // The upstream field suggests the working set's upstreams (a datalist).
    await userEvent.type(screen.getByRole("combobox", { name: "Upstream (required)" }), "orders-up");
    await userEvent.type(field("Path prefix"), "/v1/orders");
    await userEvent.type(field("Methods"), "GET, POST");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Auth mode" }), "api_key");
    await userEvent.click(screen.getByRole("button", { name: "Create route" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/routes/3"));
    expect(sent).toEqual({ name: "orders", upstream: "orders-up", auth_mode: "api_key", match: { path_prefix: "/v1/orders", methods: ["GET", "POST"] } });
    expect(await screen.findByRole("status")).toHaveTextContent("Saved to the working set.");
  });
});
