import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { delay, http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { components } from "../../api/schema.gen";
import { setSession } from "../../auth/session";
import { ACCESS_TOKEN, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";

/** xs[i], which must exist. */
function nth<T>(xs: readonly T[], i: number): T {
  const x = xs[i];
  if (x === undefined) throw new Error(`no item ${i}`);
  return x;
}


type Demo = components["schemas"]["Demo"];

const demo: Demo = {
  tenants: [
    { name: "free-co", plan: "free", limit: { rate: 10, burst: 20, period: "1s" } },
    { name: "pro-co", plan: "pro", limit: { rate: 100, burst: 200, period: "1s" } },
    { name: "enterprise-co", plan: "enterprise", limit: { rate: 1000, burst: 1000, period: "1s" } },
  ],
  routes: [
    { name: "orders", policy: "consumer-plan", limit: null },
    { name: "catalog", policy: "catalog-limit", limit: { rate: 200, burst: 200, period: "1s" } },
    { name: "payments", policy: "payments-limit", limit: { rate: 100, burst: 200, period: "1s" } },
  ],
  scenarios: ["d1", "d2", "d3"],
  faults: [
    { name: "redis-cut", active: false },
    { name: "upstream-slow", active: true },
  ],
  playground: { max_rate: 200, max_duration_s: 60, active_runs: 0, runs_left_this_hour: 30 },
};

const RUN_ID = "5b0f3c2a-9d4e-4f6a-8b1c-2d3e4f5a6b7c";
/** A key the API never sends the browser: it must never show up. */
const KEY = "spw_neverSentToTheBrowser";

/** Serves GET /demo with answer and counts the requests and their tokens. */
function serveDemo(answer: () => Response | Promise<Response>) {
  const seen: (string | null)[] = [];
  server.use(http.get("/api/v1/demo", async ({ request }) => {
    seen.push(request.headers.get("Authorization"));
    return answer();
  }));
  return seen;
}

async function navLinks() {
  const nav = await screen.findByRole("navigation", { name: "Screens" });
  return within(nav).getAllByRole("link").map((l) => l.textContent);
}

interface RunStream {
  url: URL;
  authorization: string | null;
  send: (event: string, data: unknown) => void;
  close: () => void;
}

/** Serves the run endpoints: POST answers start(), each stream opened is pushed for the test to write to. */
function serveRuns(start: (body: unknown) => Response) {
  const bodies: unknown[] = [];
  const streams: RunStream[] = [];
  const stops: string[] = [];
  const enc = new TextEncoder();
  server.use(
    http.post("/api/v1/demo/playground/runs", async ({ request }) => {
      const body = await request.json();
      bodies.push(body);
      return start(body);
    }),
    http.delete("/api/v1/demo/playground/runs/:id", ({ params }) => {
      stops.push(String(params.id));
      return new HttpResponse(null, { status: 204 });
    }),
    http.get("/api/v1/demo/playground/runs/:id/stream", ({ request }) => {
      let ctl!: ReadableStreamDefaultController<Uint8Array>;
      const body = new ReadableStream<Uint8Array>({ start: (c) => void (ctl = c) });
      streams.push({
        url: new URL(request.url),
        authorization: request.headers.get("Authorization"),
        send: (event, data) => ctl.enqueue(enc.encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)),
        close: () => ctl.close(),
      });
      return new HttpResponse(body, { headers: { "Content-Type": "text/event-stream" } });
    }),
  );
  return { bodies, streams, stops };
}

function runOf(over: Partial<components["schemas"]["PlaygroundRun"]> = {}): components["schemas"]["PlaygroundRun"] {
  return {
    id: RUN_ID, kind: "custom", tenant: "free-co", route: "orders", rate: 30, duration_s: 30,
    started_at: "2026-11-28T10:00:00Z", ends_at: "2026-11-28T10:00:30Z", state: "running", ...over,
  };
}

function counts(ok: number, limited: number): components["schemas"]["PlaygroundCounts"] {
  return { sent: ok + limited, ok, limited, forbidden: 0, failed: 0, other: 0, dropped: 0, p50_ms: 1.2, p99_ms: 3.4 };
}

let consoleSpies: ReturnType<typeof vi.spyOn>[] = [];
beforeEach(() => {
  startSession();
  consoleSpies = (["log", "info", "debug", "warn", "error"] as const).map((m) => vi.spyOn(console, m));
});
afterEach(() => {
  setSession(null);
  // The screen never writes to the console (ADR-0096 §3), let alone a token or a key.
  for (const spy of consoleSpies) expect(spy).not.toHaveBeenCalled();
  expect(document.body.innerHTML).not.toContain(ACCESS_TOKEN);
  expect(document.body.innerHTML).not.toMatch(/spw_/);
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

  it("hides the Playground on any other error, showing nothing else", async () => {
    serveDemo(() => HttpResponse.json({ error: "internal error", code: "internal", request_id: "r" }, { status: 500 }));
    renderApp("/playground", ["admin"]);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(await navLinks()).not.toContain("Playground");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("the Playground screen (UI-07, SC-04, ADR-0096 §3)", () => {
  it("shows loading until GET /demo answers, then the empty state with the form, the scenarios and the runs left", async () => {
    serveDemo(async () => {
      await delay(400);
      return HttpResponse.json(demo);
    });
    renderApp("/playground", ["viewer"]);
    expect(await screen.findByRole("status", { name: "Loading" })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Playground" })).toBeInTheDocument();
    expect(screen.getByText(/Fire some traffic/)).toBeInTheDocument();
    expect(screen.getByText(/start 30 more runs this hour/)).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "free-co (free, 10 per 1s)" })).toBeInTheDocument();
    for (const name of ["D1 Fair limits by plan", "D2 Noisy neighbour", "D3 Distributed accuracy"]) {
      expect(screen.getByRole("button", { name })).toBeEnabled();
    }
    expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled();
  });

  it("fires traffic and shows allowed vs denied, the RateLimit headers and the token bucket from the run's stream, same origin, never a key", async () => {
    serveDemo(() => HttpResponse.json(demo));
    const runs = serveRuns(() => HttpResponse.json(runOf(), { status: 201 }));
    const user = userEvent.setup();
    renderApp("/playground", ["viewer"]);
    await screen.findByRole("heading", { name: "Playground" });
    await user.click(screen.getByRole("button", { name: "Fire" }));
    await waitFor(() => expect(runs.streams).toHaveLength(1));
    expect(runs.bodies).toEqual([{ kind: "custom", tenant: "free-co", route: "orders", rate: 30, duration_s: 30 }]);
    const stream = nth(runs.streams, 0);
    expect(stream.url.origin).toBe(location.origin);
    expect(stream.url.pathname).toBe(`/api/v1/demo/playground/runs/${RUN_ID}/stream`);
    expect(stream.url.search).toBe("");
    expect(stream.authorization).toBe(`Bearer ${ACCESS_TOKEN}`);

    stream.send("ready", runOf());
    stream.send("tick", {
      t: "2026-11-28T10:00:01Z", elapsed_s: 1, tenants: { "free-co": counts(20, 10) }, replicas: { "gateway-1": 10, "gateway-2": 10, "gateway-3": 10 },
      last: { "free-co": { status: 429, replica: "gateway-2", ratelimit: '"consumer-plan";r=0;t=1', ratelimit_policy: '"consumer-plan";q=10;w=1', retry_after: "1" } },
    });
    stream.send("tick", {
      t: "2026-11-28T10:00:02Z", elapsed_s: 2, tenants: { "free-co": counts(10, 20) }, replicas: { "gateway-1": 10, "gateway-2": 10, "gateway-3": 10 },
      last: { "free-co": { status: 200, replica: "gateway-3", ratelimit: '"consumer-plan";r=7;t=1', ratelimit_policy: '"consumer-plan";q=10;w=1', retry_after: null } },
    });
    const card = await screen.findByRole("article", { name: "free-co" });
    await waitFor(() => expect(within(card).getByText('"consumer-plan";r=7;t=1')).toBeInTheDocument());
    const legend = within(card).getByRole("list", { name: "free-co: allowed and denied per second: latest values" });
    expect(within(legend).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["allowed 10.0/s", "denied (429) 20.0/s", "limit 10.0/s"]);
    expect(within(card).getByText("200 from gateway-3")).toBeInTheDocument();
    expect(within(card).getByText('"consumer-plan";q=10;w=1')).toBeInTheDocument();
    expect(within(card).getByRole("meter", { name: "free-co token bucket" })).toHaveAttribute("value", "7");
    expect(within(card).getByText(/7 of 20 tokens left in/)).toBeInTheDocument();
    expect(screen.getByText("Requests per replica: gateway-1 20, gateway-2 20, gateway-3 20")).toBeInTheDocument();
    expect(screen.getByRole("status", { name: "" })).toHaveTextContent("Running");

    // Stop: DELETE the own run; the stream's end shows the totals.
    await user.click(screen.getByRole("button", { name: "Stop" }));
    await waitFor(() => expect(runs.stops).toEqual([RUN_ID]));
    stream.send("end", { reason: "stopped", totals: { "free-co": counts(30, 30) } });
    expect(await within(card).findByText(/Whole run: 60 sent, 30 allowed, 30 denied/)).toBeInTheDocument();
    expect(screen.getByText("Ended (stopped).")).toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(KEY);
  });

  it("runs a scenario and shows every tenant it sends, closing the previous run's stream", async () => {
    serveDemo(() => HttpResponse.json(demo));
    let n = 0;
    const runs = serveRuns(() => HttpResponse.json(n++ === 0 ? runOf() : runOf({ id: "second", kind: "scenario", scenario: "d2", tenant: undefined, route: undefined, rate: undefined, duration_s: 60 }), { status: 201 }));
    const user = userEvent.setup();
    renderApp("/playground", ["viewer"]);
    await screen.findByRole("heading", { name: "Playground" });
    await user.click(screen.getByRole("button", { name: "Fire" }));
    await waitFor(() => expect(runs.streams).toHaveLength(1));
    await user.click(screen.getByRole("button", { name: "D2 Noisy neighbour" }));
    await waitFor(() => expect(runs.streams).toHaveLength(2));
    expect(runs.bodies[1]).toEqual({ kind: "scenario", scenario: "d2" });
    nth(runs.streams, 1).send("tick", {
      t: "2026-11-28T10:01:01Z", elapsed_s: 1, replicas: { "gateway-1": 217 },
      tenants: { "free-co": counts(10, 490), "pro-co": counts(50, 0), "enterprise-co": counts(100, 0) }, last: {},
    });
    expect(await screen.findByRole("heading", { name: "D2 Noisy neighbour" })).toBeInTheDocument();
    for (const t of ["free-co", "pro-co", "enterprise-co"]) expect(await screen.findByRole("article", { name: t })).toBeInTheDocument();
    // A tick on the first run's stream changes nothing any more.
    expect(() => nth(runs.streams, 0).send("tick", { t: "2026-11-28T10:00:01Z", elapsed_s: 1, replicas: {}, tenants: { "free-co": counts(999, 0) }, last: {} })).toThrow();
  });

  it("shows a cap's 429 with its Retry-After", async () => {
    serveDemo(() => HttpResponse.json(demo));
    serveRuns(() => HttpResponse.json({ error: "a run from your address is still running (at most 1 at a time): stop it or wait", code: "unavailable", request_id: "req-9" }, { status: 429, headers: { "Retry-After": "12" } }));
    const user = userEvent.setup();
    renderApp("/playground", ["viewer"]);
    await screen.findByRole("heading", { name: "Playground" });
    await user.click(screen.getByRole("button", { name: "D1 Fair limits by plan" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Too many runs: a run from your address is still running (at most 1 at a time): stop it or wait Try again in 12 s.");
  });

  it("shows a refused stream's error", async () => {
    serveDemo(() => HttpResponse.json(demo));
    serveRuns(() => HttpResponse.json(runOf(), { status: 201 }));
    server.use(http.get("/api/v1/demo/playground/runs/:id/stream", () => HttpResponse.json({ error: "no such run", code: "not_found", request_id: "req-4" }, { status: 404 })));
    const user = userEvent.setup();
    renderApp("/playground", ["viewer"]);
    await screen.findByRole("heading", { name: "Playground" });
    await user.click(screen.getByRole("button", { name: "Fire" }));
    expect(await screen.findByText("The stream stopped.")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("Not found");
  });

  it.each(["viewer", "editor", "publisher", "key-admin"] as const)("shows a %s no fault switches", async (role) => {
    serveDemo(() => HttpResponse.json(demo));
    renderApp("/playground", [role]);
    await screen.findByRole("heading", { name: "Playground" });
    expect(screen.queryByRole("heading", { name: "Faults" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Switch/ })).toBeNull();
  });

  it("gives an admin the D5 and D6 switches, each behind a confirmation with an optional note", async () => {
    let state = { ...demo };
    serveDemo(() => HttpResponse.json(state));
    const puts: { name: string; body: unknown }[] = [];
    server.use(http.put("/api/v1/demo/faults/:name", async ({ params, request }) => {
      const body = (await request.json()) as { active: boolean };
      puts.push({ name: String(params.name), body });
      state = { ...state, faults: state.faults.map((f) => (f.name === params.name ? { ...f, active: body.active } : f)) };
      return HttpResponse.json({ name: params.name, active: body.active });
    }));
    const user = userEvent.setup();
    renderApp("/playground", ["admin"]);
    expect(await screen.findByRole("heading", { name: "Faults" })).toBeInTheDocument();
    expect(screen.getByText("Redis cut (D5):")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Switch on" }));
    const dialog = screen.getByRole("dialog", { name: "Switch Redis cut (D5) on" });
    await user.type(within(dialog).getByRole("textbox", { name: "Note (optional)" }), "demo D5");
    await user.click(within(dialog).getByRole("button", { name: "Switch on" }));
    await waitFor(() => expect(puts).toEqual([{ name: "redis-cut", body: { active: true, note: "demo D5" } }]));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(screen.getAllByRole("button", { name: "Switch off" })).toHaveLength(2));
    await user.click(nth(screen.getAllByRole("button", { name: "Switch off" }), 1));
    await user.click(within(screen.getByRole("dialog", { name: "Switch Upstream slow (D6) off" })).getByRole("button", { name: "Switch off" }));
    await waitFor(() => expect(puts[1]).toEqual({ name: "upstream-slow", body: { active: false } }));
  });
});
