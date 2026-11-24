import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { createQueryClient } from "../../api/queryClient";
import type { Role } from "../../auth/permissions";
import { setSession } from "../../auth/session";
import { matrix, mockConfig, mockStream, result, startSession } from "../../test/live";
import { renderApp } from "../../test/render";
import { server } from "../../test/server";
import { lookupBlocker, placeholders, subjectOf } from "./actions";

const T = 1_789_000_000;

const policies = matrix(
  { labels: { policy: "per-ip", mode: "enforce" }, points: [[T, 3], [T + 15, 4]] },
  { labels: { policy: "per-key", mode: "shadow" }, points: [[T, 1], [T + 15, 0.5]] },
  { labels: { policy: "gone", mode: "enforce" }, points: [[T, 1]] },
);
const hitters = {
  items: [
    { policy_id: 7, key_digest: "AAAAAAAAAAAAAAAAAAAAAA", consumer_id: 12, api_key_id: 34, denied: 120, shadow_denied: 0 },
    { policy_id: 8, key_digest: "BBBBBBBBBBBBBBBBBBBBBB", consumer_id: null, api_key_id: null, denied: 0, shadow_denied: 40 },
  ],
};
const snapshot = {
  policies: [
    { id: 7, name: "per-ip", key_template: "{ip}:{header:X-Client}" },
    { id: 8, name: "per-key", key_template: "{api_key}", mode: "shadow" },
  ],
};

beforeEach(() => {
  startSession();
  mockConfig(snapshot);
});
afterEach(() => setSession(null));

async function open(roles: Role[], path = "/throttling", queryClient = createQueryClient()) {
  const stream = mockStream();
  const router = renderApp(path, roles, undefined, queryClient);
  await waitFor(() => expect(stream.opened).toHaveLength(1));
  act(() => {
    stream.last.send("result", result("throttling.policies", policies));
    stream.last.send("result", result("throttling.heavy_hitters", hitters));
  });
  await screen.findByRole("row", { name: /AAAAAAAAAAAAAAAAAAAAAA/ });
  return { stream, router };
}

const card = (name: string) => screen.getByRole("region", { name });

async function select(digest: string) {
  await userEvent.click(screen.getByRole("button", { name: `Actions for ${digest}` }));
  return within(screen.getByRole("complementary", { name: "Selected key" }));
}

describe("Throttling (UI-03, RL-21)", () => {
  it("opens one stream with its two queries, and filters by range and policy", async () => {
    const { stream, router } = await open(["viewer"]);
    expect(stream.last.url.searchParams.getAll("sub")).toEqual(["throttling.policies", "throttling.heavy_hitters"]);
    const policy = screen.getByRole("combobox", { name: "Keys of" });
    await waitFor(() => expect(within(policy).getAllByRole("option").map((o) => o.textContent)).toEqual(["Every policy", "per-ip", "per-key"]));
    await userEvent.selectOptions(policy, "per-key");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Range" }), "Last hour");
    await waitFor(() => expect(stream.last.url.search).toBe("?sub=throttling.policies&sub=throttling.heavy_hitters&range=1h&policy_id=8"));
    expect(router.state.location.search).toEqual({ policy: 8, range: "1h" });
  });

  it("shows the deny rate per policy, each published one with a link to it", async () => {
    await open(["viewer"]);
    const deny = within(card("Deny rate per policy"));
    expect(deny.getByRole("list", { name: "Denies per second by policy: latest values" })).toHaveTextContent("gone 1.00/s");
    expect(deny.getByRole("row", { name: /^per-ip/ })).toHaveTextContent("per-ip4.00Open policy");
    expect(within(deny.getByRole("row", { name: /^per-ip/ })).getByRole("link", { name: "Open policy" })).toHaveAttribute("href", "/policies/7");
    expect(within(deny.getByRole("row", { name: /^per-key \(shadow\)/ })).getByRole("link", { name: "Open policy" })).toHaveAttribute("href", "/policies/8");
    expect(deny.getByRole("row", { name: /^gone/ })).toHaveTextContent("Not in the latest published config");
  });

  it("lists the top throttled keys by digest with their policy's name", async () => {
    await open(["viewer"]);
    const keys = within(card("Top throttled keys"));
    expect(keys.getByRole("row", { name: /AAAA/ })).toHaveTextContent("AAAAAAAAAAAAAAAAAAAAAAper-ip12341200Actions");
    expect(keys.getByRole("row", { name: /BBBB/ })).toHaveTextContent("BBBBBBBBBBBBBBBBBBBBBBper-key––040Actions");
  });

  it("shows the empty and error states", async () => {
    const stream = mockStream();
    renderApp("/throttling", ["viewer"]);
    await waitFor(() => expect(stream.opened).toHaveLength(1));
    act(() => {
      stream.last.send("result", result("throttling.heavy_hitters", { items: [] }));
      stream.last.send("error", { sub: "throttling.policies", at: "2026-11-16T10:00:05Z", code: "prometheus_not_configured", message: "x" });
    });
    expect(await within(card("Top throttled keys")).findByText("No key was throttled in this range.")).toBeInTheDocument();
    expect(within(card("Deny rate per policy")).getByRole("alert")).toHaveTextContent("Prometheus is not configured on the control plane.");
    act(() => stream.last.send("error", { sub: "throttling.heavy_hitters", at: "2026-11-16T10:00:10Z", code: "unavailable", message: "x" }));
    expect(await within(card("Top throttled keys")).findByRole("alert")).toHaveTextContent("The control plane's database is unavailable. Showing data from");
  });

  describe("a key's actions (UI-04)", () => {
    it.each([
      ["viewer", "Needs the key-admin or admin role", "Needs the key-admin or admin role", "Needs the admin role"],
      ["editor", "Needs the key-admin or admin role", "Needs the key-admin or admin role", "Needs the admin role"],
      ["publisher", "Needs the key-admin or admin role", "Needs the key-admin or admin role", "Needs the admin role"],
      ["key-admin", null, null, "Needs the admin role"],
      ["admin", null, null, null],
    ] as const)("for %s: disabled with the reason where the role cannot", async (role, override, block, lookup) => {
      await open([role]);
      const actions = await select("AAAAAAAAAAAAAAAAAAAAAA");
      expect(actions.getByRole("link", { name: "Open policy" })).toHaveAttribute("href", "/policies/7");
      for (const [name, reason] of [["Add override", override], ["Block key", block], ["Who is this?", lookup]] as const) {
        const b = actions.getByRole("button", { name });
        if (reason) {
          expect(b, name).toBeDisabled();
          expect(b, name).toHaveAccessibleDescription(reason);
        } else {
          expect(b, name).toBeEnabled();
        }
      }
    });

    it("are disabled with the key's reason when it has no consumer, no API key, or an {api_key} template", async () => {
      await open(["admin"]);
      const actions = await select("BBBBBBBBBBBBBBBBBBBBBB");
      expect(actions.getByRole("button", { name: "Add override" })).toHaveAccessibleDescription("No consumer made these requests");
      expect(actions.getByRole("button", { name: "Block key" })).toHaveAccessibleDescription("No API key made these requests");
      expect(actions.getByRole("button", { name: "Who is this?" })).toHaveAccessibleDescription("Its key template uses {api_key}: the API key column identifies it");
    });

    it("add override opens the consumer with the policy to prefill", async () => {
      const t = "2026-11-16T10:00:00Z";
      server.use(
        http.get("/api/v1/consumers/12", () => HttpResponse.json({ id: 12, tenant_id: 1, name: "c12", status: "active", version: 1, created_at: t, updated_at: t }, { headers: { ETag: '"1"' } })),
        http.get("/api/v1/consumers/12/keys", () => HttpResponse.json({ items: [], next_cursor: null })),
        http.get("/api/v1/overrides", () => HttpResponse.json({ items: [], next_cursor: null })),
        http.get("/api/v1/quota-overrides", () => HttpResponse.json({ items: [], next_cursor: null })),
        http.get("/api/v1/quota-usage", () => HttpResponse.json({ items: [] })),
      );
      const { router } = await open(["key-admin"]);
      const actions = await select("AAAAAAAAAAAAAAAAAAAAAA");
      await userEvent.click(actions.getByRole("button", { name: "Add override" }));
      await waitFor(() => expect(router.state.location.pathname).toBe("/consumers/12"));
      expect(router.state.location.search).toEqual({ override_policy: 7 });
      const dialog = await screen.findByRole("dialog", { name: "Add an override" });
      expect(within(dialog).getByRole("combobox", { name: "Policy (required)" })).toHaveValue("7");
    });

    it("block key confirms, then creates a deny entry on the key", async () => {
      let body: unknown;
      server.use(http.post("/api/v1/access-lists", async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ id: 99, action: "deny", subject_type: "key", api_key_id: 34, version: 1, created_at: "t", updated_at: "t" }, { status: 201 });
      }));
      await open(["key-admin"]);
      const actions = await select("AAAAAAAAAAAAAAAAAAAAAA");
      await userEvent.click(actions.getByRole("button", { name: "Block key" }));
      const dialog = within(screen.getByRole("dialog", { name: "Block API key 34" }));
      expect(body).toBeUndefined();
      await userEvent.type(dialog.getByRole("textbox", { name: "Note (optional)" }), "abuse");
      await userEvent.click(dialog.getByRole("button", { name: "Block key" }));
      expect(await dialog.findByRole("status")).toHaveTextContent("Blocked: access-list entry 99 denies API key 34.");
      expect(body).toEqual({ action: "deny", subject_type: "key", api_key_id: 34, note: "abuse" });
      await userEvent.click(dialog.getByRole("button", { name: "Close" }));
      expect(screen.queryByRole("dialog")).toBeNull();
    });

    it("block key shows the server's refusal and can be cancelled", async () => {
      server.use(http.post("/api/v1/access-lists", () =>
        HttpResponse.json({ error: "an entry for this key exists", code: "conflict", request_id: "req-4" }, { status: 409 })));
      await open(["admin"]);
      const actions = await select("AAAAAAAAAAAAAAAAAAAAAA");
      await userEvent.click(actions.getByRole("button", { name: "Block key" }));
      const dialog = within(screen.getByRole("dialog"));
      await userEvent.click(dialog.getByRole("button", { name: "Block key" }));
      expect(await dialog.findByRole("alert")).toHaveTextContent("Conflict: an entry for this key exists (request ID req-4)");
      await userEvent.click(dialog.getByRole("button", { name: "Cancel" }));
      expect(screen.queryByRole("dialog")).toBeNull();
    });

    it("who is this? sends the subject in a POST body only and says whether it is the key", async () => {
      const bodies: unknown[] = [];
      server.use(http.post("/api/v1/heavy-hitters/lookup", async ({ request }) => {
        const b = (await request.json()) as { subject: { ip?: string } };
        bodies.push(b);
        return HttpResponse.json(b.subject.ip === "198.51.100.7"
          ? { key_digest: "AAAAAAAAAAAAAAAAAAAAAA", items: [{ window_start: "t", policy_id: 7, key_digest: "AAAAAAAAAAAAAAAAAAAAAA", consumer_id: 12, api_key_id: 34, denied: 100, shadow_denied: 0 }] }
          : { key_digest: "CCCCCCCCCCCCCCCCCCCCCC", items: [] });
      }));
      const queryClient = createQueryClient();
      const { router } = await open(["admin"], "/throttling", queryClient);
      const actions = await select("AAAAAAAAAAAAAAAAAAAAAA");
      await userEvent.click(actions.getByRole("button", { name: "Who is this?" }));
      const dialog = within(screen.getByRole("dialog", { name: "Who is this?" }));
      await userEvent.type(dialog.getByRole("textbox", { name: "Client IP" }), "198.51.100.8");
      await userEvent.type(dialog.getByRole("textbox", { name: "Header X-Client" }), "mobile");
      await userEvent.click(dialog.getByRole("button", { name: "Look up" }));
      expect(await dialog.findByRole("status")).toHaveTextContent("Not this key: the subject's digest is CCCCCCCCCCCCCCCCCCCCCC.");
      await userEvent.clear(dialog.getByRole("textbox", { name: "Client IP" }));
      await userEvent.type(dialog.getByRole("textbox", { name: "Client IP" }), "198.51.100.7");
      await userEvent.click(dialog.getByRole("button", { name: "Look up" }));
      await waitFor(() => expect(dialog.getByRole("status")).toHaveTextContent("This subject is key AAAAAAAAAAAAAAAAAAAAAA: 100 denied and 0 would-deny in 1 windows of the range."));
      expect(bodies[1]).toMatchObject({ policy_id: 7, subject: { ip: "198.51.100.7", headers: { "X-Client": "mobile" } } });
      expect(router.state.location.href).not.toContain("198.51.100");
      await userEvent.click(dialog.getByRole("button", { name: "Close" }));
      expect(screen.queryByRole("dialog")).toBeNull();
      // The lookups' mutations, whose functions hold the subject, leave the cache with the dialog.
      await waitFor(() => expect(queryClient.getMutationCache().getAll()).toHaveLength(0));
      expect(JSON.stringify(queryClient.getQueryCache().getAll().map((q) => q.state.data))).not.toContain("198.51.100");
    });
  });
});

describe("the lookup's form", () => {
  it("reads a template's placeholders and builds the subject from them", () => {
    expect(placeholders("{tenant}:{route}:{header:X-A}:{tenant}")).toEqual(["tenant", "route", "header:X-A"]);
    expect(subjectOf({ ip: " 10.0.0.1 ", consumer: "5", tenant: "", route: "r", method: "get", "header:X-A": "v" }))
      .toEqual({ ip: "10.0.0.1", consumer_id: 5, route: "r", method: "GET", headers: { "X-A": "v" } });
    expect(lookupBlocker(undefined)).toBe("Its policy is not in the latest published config");
    expect(lookupBlocker({ id: 1, name: "p", mode: "enforce", keyTemplate: "{ip}", level: "route" })).toBeUndefined();
  });
});
