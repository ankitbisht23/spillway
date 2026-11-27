import { screen, waitFor, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { components } from "../../api/schema.gen";
import { setSession } from "../../auth/session";
import { ACCESS_TOKEN, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";

const demo: components["schemas"]["Demo"] = {
  tenants: [
    { name: "free-co", plan: "free", limit: { rate: 10, burst: 20, period: "1s" } },
    { name: "pro-co", plan: "pro", limit: { rate: 100, burst: 200, period: "1s" } },
  ],
  routes: [
    { name: "orders", policy: "consumer-plan", limit: null },
    { name: "payments", policy: "payments-limit", limit: { rate: 100, burst: 200, period: "1s" } },
  ],
  scenarios: ["d1", "d2", "d3"],
};

/** Serves GET /demo with answer and counts the requests and their tokens. */
function serveDemo(answer: () => Response) {
  const seen: (string | null)[] = [];
  server.use(http.get("/api/v1/demo", ({ request }) => {
    seen.push(request.headers.get("Authorization"));
    return answer();
  }));
  return seen;
}

async function navLinks() {
  const nav = await screen.findByRole("navigation", { name: "Screens" });
  return within(nav).getAllByRole("link").map((l) => l.textContent);
}

let log: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  startSession();
  log = vi.spyOn(console, "log");
});
afterEach(() => {
  setSession(null);
  // The screen never writes to the console (ADR-0096), let alone a token.
  expect(log).not.toHaveBeenCalled();
});

describe("demo mode in the UI (SC-08, ADR-0095)", () => {
  it("hides the Playground and shows Not found at its URL when GET /demo is 404, asking once", async () => {
    const seen = serveDemo(() => HttpResponse.json({ error: "no such path", code: "not_found", request_id: "r" }, { status: 404 }));
    renderApp("/playground", ["admin"]);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(await navLinks()).not.toContain("Playground");
    await new Promise((r) => setTimeout(r, 50));
    expect(seen).toEqual([`Bearer ${ACCESS_TOKEN}`]);
  });

  it.each(["viewer", "editor", "publisher", "key-admin", "admin"] as const)("shows the Playground after History to a %s when GET /demo answers", async (role) => {
    serveDemo(() => HttpResponse.json(demo));
    renderApp("/playground", [role]);
    expect(await screen.findByRole("heading", { name: "Playground" })).toBeInTheDocument();
    await waitFor(async () => expect(await navLinks()).toContain("Playground"));
    const links = await navLinks();
    expect(links.indexOf("Playground")).toBe(links.indexOf("History") + 1);
    expect(screen.getByRole("link", { name: "Playground" })).toHaveAttribute("aria-current", "page");
  });

  it("lists the demo's tenants and routes with their limits", async () => {
    serveDemo(() => HttpResponse.json(demo));
    renderApp("/playground", ["viewer"]);
    expect(await screen.findByRole("row", { name: "free-co free 10 per 1s, burst 20" })).toBeInTheDocument();
    expect(screen.getByRole("row", { name: "pro-co pro 100 per 1s, burst 200" })).toBeInTheDocument();
    expect(screen.getByRole("row", { name: "orders consumer-plan each tenant's plan" })).toBeInTheDocument();
    expect(screen.getByRole("row", { name: "payments payments-limit 100 per 1s, burst 200" })).toBeInTheDocument();
  });

  it("hides the Playground on any other error, showing nothing else", async () => {
    serveDemo(() => HttpResponse.json({ error: "internal error", code: "internal", request_id: "r" }, { status: 500 }));
    renderApp("/playground", ["admin"]);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(await navLinks()).not.toContain("Playground");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
