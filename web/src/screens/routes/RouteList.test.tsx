import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { setSession } from "../../auth/session";
import { mockStream, result, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";

const route = (id: number, name: string, upstream = "orders-up") => ({
  id, name, upstream, version: 1, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z",
  match: { path_prefix: "/v1/orders", methods: ["GET"] }, auth_mode: "api_key",
});

const endpoints = {
  kind: "vector",
  series: [
    { labels: { replica: "gateway-1", upstream: "orders-up", endpoint: "10.0.0.1:80", stat: "healthy" }, value: 1 },
    { labels: { replica: "gateway-1", upstream: "orders-up", endpoint: "10.0.0.1:80", stat: "ejected" }, value: 0 },
    { labels: { replica: "gateway-1", upstream: "orders-up", endpoint: "10.0.0.2:80", stat: "healthy" }, value: 0 },
    { labels: { replica: "gateway-1", upstream: "orders-up", endpoint: "10.0.0.2:80", stat: "ejected" }, value: 1 },
  ],
};
const circuits = { kind: "vector", series: [{ labels: { replica: "gateway-1", upstream: "orders-up" }, value: 1 }] };

beforeEach(() => startSession());
afterEach(() => setSession(null));

describe("Routes (UI-06, D9)", () => {
  it("lists routes with their upstream's live health and circuit, and pages by cursor", async () => {
    const asked: (string | null)[] = [];
    server.use(http.get("/api/v1/routes", ({ request }) => {
      const cursor = new URL(request.url).searchParams.get("cursor");
      asked.push(new URL(request.url).search);
      return HttpResponse.json(cursor ? { items: [route(2, "second")], next_cursor: null } : { items: [route(1, "orders")], next_cursor: "c2" });
    }));
    const stream = mockStream();
    const router = renderApp("/routes", ["viewer"]);
    const row = await screen.findByRole("row", { name: /orders/ });
    expect(row).toHaveTextContent("GET /v1/orders*");
    expect(row).toHaveTextContent("api_key");
    expect(within(row).getAllByText("…")).toHaveLength(2);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    expect(stream.last.url.searchParams.getAll("sub")).toEqual(["routes.endpoints", "routes.circuits"]);
    act(() => {
      stream.last.send("result", result("routes.endpoints", endpoints));
      stream.last.send("result", result("routes.circuits", circuits));
    });
    await waitFor(() => expect(row).toHaveTextContent("1 of 2 taking traffic on 1 replica"));
    expect(row).toHaveTextContent("open on gateway-1");
    expect(within(row).getByRole("link", { name: "orders-up" })).toHaveAttribute("href", "/upstreams?name=orders-up");
    // A viewer may not create a route: the action stays, disabled with its reason.
    expect(screen.getByRole("button", { name: "New route" })).toHaveAccessibleDescription("Needs the editor or admin role");

    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await screen.findByRole("row", { name: /second/ });
    expect(router.state.location.search).toEqual({ cursor: "c2" });
    expect(asked).toEqual(["?limit=50", "?limit=50&cursor=c2"]);
  });

  it("shows the empty state with the reason a viewer cannot create one, and live data that fails", async () => {
    server.use(http.get("/api/v1/routes", () => HttpResponse.json({ items: [], next_cursor: null })));
    mockStream();
    renderApp("/routes", ["viewer"]);
    expect(await screen.findByText("No routes yet. Needs the editor or admin role to create one.")).toBeInTheDocument();
  });

  it("filters by an exact name", async () => {
    let asked = "";
    server.use(http.get("/api/v1/routes", ({ request }) => {
      asked = new URL(request.url).search;
      return HttpResponse.json({ items: [], next_cursor: null });
    }));
    mockStream();
    renderApp("/routes?name=orders", ["editor"]);
    expect(await screen.findByText("No route named orders.")).toBeInTheDocument();
    expect(asked).toBe("?limit=50&name=orders");
    expect(screen.getByRole("link", { name: "All routes" })).toHaveAttribute("href", "/routes");
  });

  it("shows the error state with a Retry that asks again", async () => {
    let calls = 0;
    server.use(http.get("/api/v1/routes", () => {
      calls++;
      return calls === 1 ? HttpResponse.json({ code: "internal", error: "boom", request_id: "req-1" }, { status: 500 }) : HttpResponse.json({ items: [route(1, "orders")], next_cursor: null });
    }));
    mockStream();
    renderApp("/routes");
    expect(await screen.findByText(/req-1/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("row", { name: /orders/ })).toBeInTheDocument();
  });
});

describe("Upstreams (UI-06, D9)", () => {
  it("lists upstreams with draining endpoints counted and their live state", async () => {
    server.use(http.get("/api/v1/upstreams", () => HttpResponse.json({
      items: [{ id: 2, name: "orders-up", version: 1, created_at: "", updated_at: "", endpoints: [{ id: 1, address: "a:80" }, { id: 2, address: "b:80", draining: true }] }],
      next_cursor: null,
    })));
    const stream = mockStream();
    renderApp("/upstreams", ["editor"]);
    const row = await screen.findByRole("row", { name: /orders-up/ });
    expect(row).toHaveTextContent("2, 1 draining");
    expect(row).toHaveTextContent("p2c");
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    act(() => {
      stream.last.send("result", result("routes.endpoints", { kind: "vector", series: [] }));
      stream.last.send("error", { sub: "routes.circuits", at: "2026-11-16T10:00:05Z", code: "prometheus_unavailable", message: "down" });
    });
    await waitFor(() => expect(within(row).getAllByText("Unavailable")).toHaveLength(2));
    expect(screen.getByRole("button", { name: "New upstream" })).toBeEnabled();
    expect(screen.getByRole("link", { name: "Upstreams" })).toHaveAttribute("aria-current", "page");
    // The navigation's Routes & upstreams is current on the upstream pages too.
    expect(within(screen.getByRole("navigation", { name: "Screens" })).getByRole("link", { name: "Routes & upstreams" })).toHaveAttribute("aria-current", "page");
  });

  it("shows the empty state with the reason a viewer cannot create one", async () => {
    server.use(http.get("/api/v1/upstreams", () => HttpResponse.json({ items: [], next_cursor: null })));
    mockStream();
    renderApp("/upstreams", ["viewer"]);
    expect(await screen.findByText("No upstreams yet. Needs the editor or admin role to create one.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New upstream" })).toBeDisabled();
  });

  it("shows the empty state of a name filter", async () => {
    server.use(http.get("/api/v1/upstreams", () => HttpResponse.json({ items: [], next_cursor: null })));
    mockStream();
    renderApp("/upstreams?name=orders-up", ["editor"]);
    expect(await screen.findByText("No upstream named orders-up.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "All upstreams" })).toHaveAttribute("href", "/upstreams");
  });

  it("shows the error state with its request ID and a Retry that asks again", async () => {
    let calls = 0;
    server.use(http.get("/api/v1/upstreams", () => {
      calls++;
      return calls === 1
        ? HttpResponse.json({ code: "internal", error: "boom", request_id: "req-up-1" }, { status: 500 })
        : HttpResponse.json({ items: [{ id: 2, name: "orders-up", version: 1, created_at: "", updated_at: "", endpoints: [] }], next_cursor: null });
    }));
    mockStream();
    renderApp("/upstreams");
    expect(await screen.findByText(/req-up-1/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("row", { name: /orders-up/ })).toBeInTheDocument();
  });
});
