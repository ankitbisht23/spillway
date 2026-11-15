import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { setSession } from "../../auth/session";
import { mockStream, result, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import type { Upstream } from "./fields";

const upstream = (over: Partial<Upstream> = {}): Upstream => ({
  id: 2, name: "orders-up", version: 9, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z",
  endpoints: [{ id: 11, address: "orders-1:8080" }, { id: 12, address: "orders-2:8080", draining: true }, { id: 13, address: "orders-3:8080" }],
  outlier_detection: { consecutive_failures: 5 },
  ...over,
});

interface Sent {
  method: string;
  path: string;
  ifMatch: string | null;
  body: unknown;
}

/** Serves upstream 2 at ETag "<version>"; drain and PUT answer with answer, else the change at the next version. */
function mockUpstream(current: Upstream, answer?: () => Response) {
  const sent: Sent[] = [];
  let now = current;
  const next = (u: Upstream) => {
    now = { ...u, version: now.version + 1 };
    return HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } });
  };
  server.use(
    http.get("/api/v1/upstreams/2", () => HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } })),
    http.post("/api/v1/upstreams/2/endpoints/:eid/drain", ({ request, params }) => {
      sent.push({ method: "POST", path: new URL(request.url).pathname, ifMatch: request.headers.get("If-Match"), body: null });
      if (answer) return answer();
      return next({ ...now, endpoints: now.endpoints.map((e) => (e.id === Number(params.eid) ? { ...e, draining: true } : e)) });
    }),
    http.put("/api/v1/upstreams/2", async ({ request }) => {
      const body = (await request.json()) as Upstream;
      sent.push({ method: "PUT", path: "/api/v1/upstreams/2", ifMatch: request.headers.get("If-Match"), body });
      if (answer) return answer();
      return next({ ...now, ...body, endpoints: body.endpoints.map((e) => ({ ...e, id: now.endpoints.find((o) => o.address === e.address)?.id })) });
    }),
    http.get("/api/v1/upstreams", () => HttpResponse.json({ items: [], next_cursor: null })),
  );
  return {
    sent,
    change: (u: Upstream) => {
      now = u;
    },
  };
}

beforeEach(() => startSession());
afterEach(() => setSession(null));

const endpoints = () => within(screen.getByRole("region", { name: "Endpoints" }));
const row = (address: string) => within(endpoints().getByRole("row", { name: new RegExp(address) }));

async function open(roles = ["editor"] as Parameters<typeof renderApp>[1]) {
  const stream = mockStream();
  renderApp("/upstreams/2", roles);
  await screen.findByRole("heading", { name: "Upstream orders-up" });
  await waitFor(() => expect(stream.opened).toHaveLength(1));
  return stream;
}

describe("an upstream's endpoints (UI-06, CP-14)", () => {
  it("drains an endpoint with the upstream's ETag and says it ships with the next publish", async () => {
    const api = mockUpstream(upstream());
    await open();
    expect(row("orders-2:8080").getByRole("cell", { name: "Draining" })).toBeInTheDocument();
    await userEvent.click(row("orders-1:8080").getByRole("button", { name: "Drain orders-1:8080" }));
    expect(await screen.findByText(/^Endpoint orders-1:8080 draining in the working set\./)).toHaveTextContent("Endpoint orders-1:8080 draining in the working set. Unpublished changes: review and publish.");
    expect(api.sent).toEqual([{ method: "POST", path: "/api/v1/upstreams/2/endpoints/11/drain", ifMatch: '"9"', body: null }]);
    expect(row("orders-1:8080").getByRole("button", { name: "Undrain orders-1:8080" })).toBeEnabled();
  });

  it("undrains with a PUT of the upstream as loaded, the endpoint's draining unset, the others kept", async () => {
    const api = mockUpstream(upstream({ endpoints: [{ id: 11, address: "orders-1:8080", draining: true }, { id: 12, address: "orders-2:8080", draining: true }] }));
    await open(["admin"]);
    await userEvent.click(row("orders-2:8080").getByRole("button", { name: "Undrain orders-2:8080" }));
    expect(await screen.findByText(/Endpoint orders-2:8080 taking traffic again in the working set\./)).toBeInTheDocument();
    expect(api.sent).toEqual([{
      method: "PUT", path: "/api/v1/upstreams/2", ifMatch: '"9"',
      body: { name: "orders-up", endpoints: [{ address: "orders-1:8080", draining: true }, { address: "orders-2:8080" }], outlier_detection: { consecutive_failures: 5 } },
    }]);
  });

  it("answers a 412 with Reload, which loads the current version", async () => {
    const api = mockUpstream(upstream(), () => HttpResponse.json({ code: "precondition_failed", error: "changed" }, { status: 412 }));
    await open();
    await userEvent.click(row("orders-3:8080").getByRole("button", { name: "Drain orders-3:8080" }));
    expect(await endpoints().findByRole("alert")).toHaveTextContent("This upstream changed since you opened it.");
    api.change(upstream({ version: 10, endpoints: [{ id: 13, address: "orders-3:8080", draining: true }] }));
    await userEvent.click(endpoints().getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(row("orders-3:8080").getByRole("button", { name: "Undrain orders-3:8080" })).toBeInTheDocument());
    expect(endpoints().queryByRole("alert")).not.toBeInTheDocument();
  });

  it("disables drain while the form has unsaved edits", async () => {
    mockUpstream(upstream());
    await open();
    await userEvent.type(screen.getByRole("textbox", { name: "Hash on" }), "{{ip}");
    const drain = row("orders-1:8080").getByRole("button", { name: "Drain orders-1:8080" });
    expect(drain).toBeDisabled();
    expect(drain).toHaveAccessibleDescription("Save or discard your edits first");
  });

  it("shows a viewer every action disabled with its reason", async () => {
    mockUpstream(upstream());
    await open(["viewer", "key-admin"]);
    for (const name of ["Drain orders-1:8080", "Undrain orders-2:8080", "Save", "Delete"]) {
      const b = screen.getByRole("button", { name });
      expect(b, name).toBeDisabled();
      expect(b, name).toHaveAccessibleDescription("Needs the editor or admin role");
    }
  });

  it("saves the settings with If-Match, endpoints as edited", async () => {
    const api = mockUpstream(upstream());
    await open();
    const eps = screen.getByRole("textbox", { name: "Endpoints (required)" });
    await userEvent.clear(eps);
    await userEvent.type(eps, "orders-2:8080{Enter}orders-4:8080");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Saved to the working set\./)).toBeInTheDocument();
    expect(api.sent[0]).toMatchObject({ method: "PUT", ifMatch: '"9"', body: { endpoints: [{ address: "orders-2:8080", draining: true }, { address: "orders-4:8080" }] } });
  });
});

describe("an upstream's live state (UI-03, ADR-0059)", () => {
  it("shows each replica's endpoints and circuit for this upstream only", async () => {
    mockUpstream(upstream());
    const stream = await open(["viewer"]);
    expect(stream.last.url.searchParams.getAll("sub")).toEqual(["routes.endpoints", "routes.circuits"]);
    const s = (replica: string, up: string, endpoint: string, stat: string, value: number) => ({ labels: { replica, upstream: up, endpoint, stat }, value });
    act(() => {
      stream.last.send("result", result("routes.endpoints", {
        kind: "vector",
        series: [
          s("gateway-1", "orders-up", "10.0.0.1:8080", "healthy", 1), s("gateway-1", "orders-up", "10.0.0.1:8080", "ejected", 0),
          s("gateway-1", "orders-up", "10.0.0.3:8080", "healthy", 0), s("gateway-1", "orders-up", "10.0.0.3:8080", "ejected", 1),
          s("gateway-2", "orders-up", "10.0.0.3:8080", "healthy", 0), s("gateway-2", "orders-up", "10.0.0.3:8080", "ejected", 0),
          s("gateway-1", "other", "10.0.0.9:8080", "healthy", 1),
        ],
      }));
      stream.last.send("result", result("routes.circuits", {
        kind: "vector",
        series: [{ labels: { replica: "gateway-1", upstream: "orders-up" }, value: 2 }, { labels: { replica: "gateway-2", upstream: "other" }, value: 1 }],
      }));
    });
    const states = await screen.findByRole("table", { name: "Endpoint states" });
    expect(within(states).getAllByRole("row").slice(1).map((r) => r.textContent)).toEqual([
      "gateway-110.0.0.1:8080Taking traffic",
      "gateway-110.0.0.3:8080Ejected (outlier detection)",
      "gateway-210.0.0.3:8080Failing health checks",
    ]);
    expect(within(screen.getByRole("table", { name: "Circuit states" })).getAllByRole("row").slice(1).map((r) => r.textContent)).toEqual(["gateway-1half-open"]);

    // Prometheus goes down: the last data stays, greyed, with the reason.
    act(() => stream.last.send("error", { sub: "routes.endpoints", at: "2026-11-16T10:00:10Z", code: "prometheus_unavailable", message: "down" }));
    expect(await screen.findByText(/Prometheus is unavailable\. Showing data from/)).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Endpoint states" })).toBeInTheDocument();
  });

  it("says when no gateway reports the upstream, and when Prometheus is not configured", async () => {
    mockUpstream(upstream());
    const stream = await open(["viewer"]);
    act(() => {
      stream.last.send("result", result("routes.endpoints", { kind: "vector", series: [] }));
      stream.last.send("error", { sub: "routes.circuits", at: "2026-11-16T10:00:05Z", code: "prometheus_not_configured", message: "no URL" });
    });
    expect(await screen.findByText("No gateway reports this upstream: it is not in the published config, or no gateway is up.")).toBeInTheDocument();
    expect(screen.getByText("Prometheus is not configured on the control plane.")).toBeInTheDocument();
  });
});
