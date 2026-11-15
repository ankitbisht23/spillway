import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { setSession } from "../../auth/session";
import { startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";

const item = (id: number, over: Record<string, unknown> = {}) => ({
  id, name: `p-${id}`, level: "route", match: { routes: ["orders"], methods: ["GET"] }, key_template: "{ip}", algorithm: "gcra",
  limits: [{ rate: 100, period: "1s", burst: 200 }], version: 1, created_at: "x", updated_at: "x", ...over,
});

/** Serves two pages: cursor "Mg" is the second. */
function mockList() {
  const asked: (string | null)[] = [];
  server.use(http.get("/api/v1/policies", ({ request }) => {
    const u = new URL(request.url);
    asked.push(u.searchParams.get("cursor"));
    expect(u.searchParams.get("limit")).toBe("50");
    return u.searchParams.get("cursor") === "Mg"
      ? HttpResponse.json({ items: [item(3, { from_plan: true, limits: undefined, level: "consumer", key_template: "{consumer}" })], next_cursor: null })
      : HttpResponse.json({ items: [item(1, { mode: "shadow" }), item(2, { algorithm: "concurrency", limits: [{ rate: 20 }] })], next_cursor: "Mg" });
  }));
  return asked;
}

beforeEach(() => startSession());
afterEach(() => setSession(null));

describe("the policy list (UI-01, UI-06, D9)", () => {
  it("lists 50 a page with level, mode, match and limits, and pages by cursor", async () => {
    const asked = mockList();
    const router = renderApp("/policies", ["viewer"]);
    expect(await screen.findByRole("row", { name: /^p-1/ })).toHaveTextContent("p-1routeshadoworders · GET{ip}gcra100/1s burst 200");
    expect(screen.getByRole("row", { name: /^p-2/ })).toHaveTextContent("p-2routeenforceorders · GET{ip}concurrency (local)20 in flight");
    expect(screen.getByRole("link", { name: "p-1" })).toHaveAttribute("href", "/policies/1");
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(await screen.findByRole("row", { name: /^p-3/ })).toHaveTextContent("from plan");
    expect(router.state.location.search).toEqual({ cursor: "Mg" });
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
    expect(asked).toEqual([null, "Mg"]);
  });

  it.each([["viewer", "Needs the editor or admin role"], ["editor", null]] as const)("for %s: New policy", async (role, reason) => {
    mockList();
    const router = renderApp("/policies", [role]);
    const b = await screen.findByRole("button", { name: "New policy" });
    if (reason) {
      expect(b).toBeDisabled();
      expect(b).toHaveAccessibleDescription(reason);
    } else {
      await userEvent.click(b);
      await waitFor(() => expect(router.state.location.pathname).toBe("/policies/new"));
    }
  });

  it("shows the empty state, with why the role cannot create", async () => {
    server.use(http.get("/api/v1/policies", () => HttpResponse.json({ items: [], next_cursor: null })));
    renderApp("/policies", ["publisher"]);
    expect(await screen.findByText("No policies yet. Needs the editor or admin role to create one.")).toBeInTheDocument();
  });

  it("shows the error state", async () => {
    server.use(http.get("/api/v1/policies", () => HttpResponse.json({ error: "x", code: "internal", request_id: "r" }, { status: 500 })));
    renderApp("/policies", ["viewer"]);
    expect(await screen.findByRole("alert")).toHaveTextContent("Something went wrong");
  });
});
