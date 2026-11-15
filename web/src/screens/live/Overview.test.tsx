import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { setSession } from "../../auth/session";
import { ACCESS_TOKEN, matrix, mockConfig, mockStream, result, startSession } from "../../test/live";
import { renderApp } from "../../test/render";

const T = 1_789_000_000;

const requests = matrix(
  { labels: { code_class: "4xx" }, points: [[T, 1], [T + 15, 2]] },
  { labels: { code_class: "2xx" }, points: [[T, 8], [T + 15, 6]] },
);
const ratelimited = matrix({ labels: {}, points: [[T, 0.1], [T + 15, 0.25]] });
const latency = matrix(
  { labels: { quantile: "0.5" }, points: [[T, 0.004], [T + 15, 0.005]] },
  { labels: { quantile: "0.99" }, points: [[T, 0.02], [T + 15, 0.031]] },
);
const routes = {
  kind: "vector",
  series: [
    { labels: { route: "orders", stat: "rps" }, value: 6 },
    { labels: { route: "orders", stat: "ratelimited_rps" }, value: 1.5 },
    { labels: { route: "orders", stat: "p99_s" }, value: 0.031 },
    { labels: { route: "static-only", stat: "rps" }, value: 2 },
    { labels: { route: "static-only", stat: "ratelimited_rps" }, value: null },
    { labels: { route: "static-only", stat: "p99_s" }, value: null },
  ],
};
const tenants = { kind: "vector", series: [{ labels: { tenant: "acme", stat: "rps" }, value: 8 }] };

function sendAll(send: (event: string, data: unknown) => void) {
  send("result", result("overview.requests", requests));
  send("result", result("overview.ratelimited", ratelimited));
  send("result", result("overview.latency", latency));
  send("result", result("overview.routes", routes));
  send("result", result("overview.tenants", tenants));
}

const card = (name: string) => screen.getByRole("region", { name });

beforeEach(() => {
  startSession();
  mockConfig({ routes: [{ name: "orders" }, { name: "billing" }] }, ["acme"]);
});
afterEach(() => setSession(null));

describe("Overview (UI-03)", () => {
  it("opens one stream with its five queries, the token in Authorization and never in the URL", async () => {
    const stream = mockStream();
    renderApp("/overview", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    expect(stream.last.url.searchParams.getAll("sub")).toEqual(["overview.requests", "overview.ratelimited", "overview.latency", "overview.routes", "overview.tenants"]);
    expect(stream.last.authorization).toBe(`Bearer ${ACCESS_TOKEN}`);
    expect(stream.last.url.href).not.toContain(ACCESS_TOKEN);
    expect(location.href).not.toContain(ACCESS_TOKEN);
  });

  it("shows loading until results arrive, then the charts, status mix and top tables", async () => {
    const stream = mockStream();
    renderApp("/overview", ["viewer"]);
    expect(await screen.findByLabelText("Loading requests")).toBeInTheDocument();
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    await screen.findByText(/^Live/);
    act(() => sendAll(stream.last.send));

    const rps = within(await screen.findByRole("list", { name: "Requests per second by status class: latest values" }));
    expect(rps.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["2xx 6.00/s", "4xx 2.00/s"]);
    expect(within(card("Status mix")).getByRole("row", { name: /2xx/ })).toHaveTextContent("2xx6.00 75 %");
    expect(screen.getByRole("list", { name: "Share of requests rate-limited: latest values" })).toHaveTextContent("429 share 25 %");
    expect(screen.getByRole("list", { name: "Request latency p50 and p99: latest values" })).toHaveTextContent("p50 5.0 msp99 31 ms");

    const top = within(card("Top routes"));
    expect(top.getByRole("row", { name: /orders/ })).toHaveTextContent("orders6.001.5025 %31 ms");
    expect(top.getByRole("link", { name: "orders" })).toHaveAttribute("href", "/overview?route=orders");
    // Not in the latest published config, so not a filter value.
    expect(top.getByRole("row", { name: /static-only/ })).toHaveTextContent("static-only2.0000 %–");
    expect(top.queryByRole("link", { name: "static-only" })).toBeNull();
    expect(within(card("Top tenants")).getByRole("link", { name: "acme" })).toHaveAttribute("href", "/overview?tenant=acme");
    expect(screen.getByRole("status", { name: "" })).toHaveTextContent(/Live · updated \d\d:\d\d:\d\d/);
  });

  it("shows the empty state when there is no traffic", async () => {
    const stream = mockStream();
    renderApp("/overview", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    act(() => {
      stream.last.send("result", result("overview.requests", matrix()));
      stream.last.send("result", result("overview.ratelimited", matrix({ labels: {}, points: [[T, null]] })));
      stream.last.send("result", result("overview.routes", { kind: "vector", series: [] }));
    });
    expect(await within(card("Requests per second")).findByText("No requests in this range.")).toBeInTheDocument();
    expect(within(card("Rate-limited (429) share")).getByText("No requests in this range.")).toBeInTheDocument();
    expect(within(card("Top routes")).getByText("No requests in this range.")).toBeInTheDocument();
    expect(within(card("Latency")).getByLabelText("Loading latency")).toBeInTheDocument();
  });

  it("shows a subscription's error, alone or over its last data, and recovers", async () => {
    const stream = mockStream();
    renderApp("/overview", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    const failed = { sub: "overview.latency", at: "2026-11-16T10:00:10Z", code: "prometheus_unavailable", message: "Prometheus is unavailable" };
    act(() => stream.last.send("error", failed));
    expect(await within(card("Latency")).findByRole("alert")).toHaveTextContent("Prometheus is unavailable.");

    act(() => stream.last.send("result", result("overview.requests", requests)));
    act(() => stream.last.send("error", { ...failed, sub: "overview.requests" }));
    const live = await within(card("Requests per second")).findByRole("alert");
    expect(live).toHaveTextContent(/Prometheus is unavailable\. Showing data from \d\d:\d\d:\d\d\./);
    expect(live.parentElement).toHaveClass("live-stale");
    expect(within(card("Requests per second")).getByRole("list")).toHaveTextContent("2xx 6.00/s");

    act(() => stream.last.send("result", result("overview.requests", requests)));
    await waitFor(() => expect(within(card("Requests per second")).queryByRole("alert")).toBeNull());
  });

  it("keeps the last data, greyed, while it reconnects", async () => {
    const stream = mockStream();
    renderApp("/overview", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    act(() => sendAll(stream.last.send));
    await within(card("Requests per second")).findByRole("list");
    act(() => stream.last.close());
    expect(await screen.findByText("Reconnecting…")).toBeInTheDocument();
    expect(within(card("Requests per second")).getByRole("list")).toHaveTextContent("2xx 6.00/s");
    expect(within(card("Requests per second")).getByRole("figure").parentElement).toHaveClass("live-stale");
  });

  it("filters by route, tenant and range: the URL changes and a new stream opens", async () => {
    const stream = mockStream();
    const router = renderApp("/overview", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    const route = await screen.findByRole("combobox", { name: "Route" });
    await waitFor(() => expect(within(route).getAllByRole("option").map((o) => o.textContent)).toEqual(["All routes", "billing", "orders"]));
    await userEvent.selectOptions(route, "orders");
    await waitFor(() => expect(stream.opened).toHaveLength(2));
    expect(stream.opened[0]?.signal.aborted).toBe(true);
    expect(stream.last.url.searchParams.get("route")).toBe("orders");
    expect(router.state.location.search).toEqual({ route: "orders" });

    const tenant = screen.getByRole("combobox", { name: "Tenant" });
    await waitFor(() => expect(within(tenant).getAllByRole("option").map((o) => o.textContent)).toContain("acme"));
    await userEvent.selectOptions(tenant, "acme");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Range" }), "Last 24 hours");
    await waitFor(() => expect(stream.last.url.search).toBe("?sub=overview.requests&sub=overview.ratelimited&sub=overview.latency&sub=overview.routes&sub=overview.tenants&route=orders&tenant=acme&range=24h"));
    expect(router.state.location.search).toEqual({ route: "orders", tenant: "acme", range: "24h" });

    await userEvent.selectOptions(route, "All routes");
    await waitFor(() => expect(stream.last.url.searchParams.has("route")).toBe(false));
  });

  it("drops filter values the stream would refuse", async () => {
    const stream = mockStream();
    renderApp("/overview?route=bad%20name&range=2d&tenant=acme", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    expect(stream.last.url.searchParams.get("tenant")).toBe("acme");
    expect(stream.last.url.searchParams.has("route")).toBe(false);
    expect(stream.last.url.searchParams.has("range")).toBe(false);
  });

  it("shows a refused filter's error with a way back", async () => {
    mockStream({ status: 400, body: { error: "route: no route of that name in the latest published config version", code: "invalid_request", request_id: "req-9" } });
    const router = renderApp("/overview?route=gone", ["viewer"]);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("The request was rejected");
    expect(alert).toHaveTextContent("route: no route of that name");
    expect(alert).toHaveTextContent("req-9");
    await userEvent.click(screen.getByRole("link", { name: "Reset the filters" }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });

  it("shows Not allowed on a 403", async () => {
    mockStream({ status: 403, body: { error: "forbidden", code: "forbidden", request_id: "req-1" } });
    renderApp("/overview", ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Not allowed" })).toBeInTheDocument();
  });

  it("closes its stream when the user leaves the screen", async () => {
    const stream = mockStream();
    renderApp("/overview", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    await userEvent.click(screen.getByRole("link", { name: "Replicas" }));
    await waitFor(() => expect(stream.last.signal.aborted).toBe(true));
  });
});
