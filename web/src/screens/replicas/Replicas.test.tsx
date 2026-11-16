import { act, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { components } from "../../api/schema.gen";
import { setSession } from "../../auth/session";
import { mockConfig, mockStream, result, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { staleness } from "./Replicas";

type Replica = components["schemas"]["Replica"];

function replica(id: string, over: Partial<Replica> = {}): Replica {
  return {
    id, config_version: 3, identity_version: 5, schema_version: { major: 1, minor: 0 }, watch_connected: true,
    last_heartbeat_at: "2026-11-16T10:00:00Z", last_synced_at: "2026-11-16T10:00:00Z", live: true,
    rejected_config_version: null, rejected_reason: null, ...over,
  };
}

const replicas = {
  items: [
    { replica: replica("gateway-1"), staleness_s: 2.04 },
    { replica: replica("gateway-2", { config_version: 2, live: false, watch_connected: false }), staleness_s: 3725 },
    { replica: replica("gateway-3", { config_version: 0, identity_version: 0, last_synced_at: null, rejected_config_version: 3, rejected_reason: "schema 2.0 is newer" }), staleness_s: null },
  ],
};

beforeEach(() => {
  startSession();
  mockConfig();
});
afterEach(() => setSession(null));

async function open() {
  const stream = mockStream();
  renderApp("/replicas", ["viewer"]);
  await waitFor(() => expect(stream.opened).toHaveLength(1));
  return stream;
}

describe("Replicas (UI-06, CP-08)", () => {
  it("subscribes to replicas and shows each one's versions, staleness, last heartbeat and refusal, live", async () => {
    const stream = await open();
    expect(stream.last.url.search).toBe("?sub=replicas");
    expect(screen.getByLabelText("Loading replicas")).toBeInTheDocument();
    act(() => stream.last.send("result", result("replicas", replicas)));

    const one = await screen.findByRole("row", { name: /^gateway-1/ });
    expect(one).toHaveTextContent("gateway-1live352.0 s");
    expect(within(one).getByRole("link", { name: "3" })).toHaveAttribute("href", "/history/3");
    const two = screen.getByRole("row", { name: /^gateway-2/ });
    expect(two).toHaveTextContent("down");
    expect(two).toHaveTextContent("2 · behind 3");
    expect(two).toHaveTextContent("1 h 2 min");
    const three = screen.getByRole("row", { name: /^gateway-3/ });
    expect(three).toHaveTextContent("none");
    expect(three).toHaveTextContent("never synced");
    expect(three).toHaveTextContent("version 3: schema 2.0 is newer");
    expect(screen.getByRole("status")).toHaveTextContent(/^Live · updated/);

    // The next tick replaces the rows.
    act(() => stream.last.send("result", result("replicas", { items: [{ replica: replica("gateway-1"), staleness_s: 7.5 }] }, "2026-11-16T10:00:10Z")));
    await waitFor(() => expect(screen.queryByRole("row", { name: /^gateway-2/ })).toBeNull());
    expect(screen.getByRole("row", { name: /^gateway-1/ })).toHaveTextContent("7.5 s");
  });

  it("says when no replica reports, as with make dev's static gateways", async () => {
    const stream = await open();
    act(() => stream.last.send("result", result("replicas", { items: [] })));
    expect(await screen.findByText(/No gateway replica has sent a heartbeat in the last 7 days/)).toBeInTheDocument();
  });

  it("shows the database's error, and keeps the last rows greyed under it", async () => {
    const stream = await open();
    const failed = { sub: "replicas", at: "2026-11-16T10:00:10Z", code: "unavailable", message: "postgres down" };
    act(() => stream.last.send("error", failed));
    expect(await screen.findByRole("alert")).toHaveTextContent("The control plane's database is unavailable.");
    act(() => stream.last.send("result", result("replicas", replicas)));
    await screen.findByRole("row", { name: /^gateway-1/ });
    act(() => stream.last.send("error", failed));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/The control plane's database is unavailable\. Showing data from/);
    expect(alert.parentElement).toHaveClass("live-stale");
  });

  it("shows Not allowed when the stream is refused", async () => {
    mockStream({ status: 403, body: { error: "forbidden", code: "forbidden", request_id: "req-1" } });
    renderApp("/replicas", ["viewer"]);
    expect(await screen.findByRole("heading", { name: "Not allowed" })).toBeInTheDocument();
  });
});

describe("staleness", () => {
  it("reads as seconds, minutes or hours", () => {
    expect(staleness(null)).toBe("never synced");
    expect(staleness(0)).toBe("0.0 s");
    expect(staleness(59.94)).toBe("59.9 s");
    expect(staleness(65)).toBe("1 min 5 s");
    expect(staleness(7260)).toBe("2 h 1 min");
  });
});
