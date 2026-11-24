// The control plane's identity API under test (MSW at the fetch boundary,
// D10): consumer 7 of tenant "acme", its keys and overrides, and the
// latest published config's plans and policies.
import type { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";

import { mockConfig } from "../../test/live";
import { server } from "../../test/server";
import type { AccessListEntry, ApiKey, Consumer, Override, QuotaOverride, QuotaUsage } from "./queries";

/** A key in the real format, made at run time (.gitleaks.toml): never a real key. */
const fakeKey = (prefix: string, fill: string) => `spw_${prefix}_${fill.repeat(43)}`;

/** The issued and the rotated key. */
export const SECRET = fakeKey("Ab12Cd34", "s");
export const ROTATED = fakeKey("Ef56Gh78", "r");

export const T0 = "2026-11-16T10:00:00Z";

export const consumer = (over: Partial<Consumer> = {}): Consumer => ({
  id: 7, tenant_id: 1, name: "checkout", status: "active", plan_id: null, jwt_subject: null, version: 4, created_at: T0, updated_at: T0, ...over,
});

export const apiKey = (over: Partial<ApiKey> = {}): ApiKey => ({
  id: 20, consumer_id: 7, prefix: "Zz99Yy88", expires_at: null, revoked_at: null, rotated_from: null, version: 1, created_at: T0, updated_at: T0, ...over,
});

export const override = (over: Partial<Override> = {}): Override => ({
  id: 30, consumer_id: 7, policy_id: 4, limits: [{ rate: 500, period: "1m" }], starts_at: null, ends_at: null, note: null, version: 2,
  created_at: T0, updated_at: T0, ...over,
});

export const quotaOverride = (over: Partial<QuotaOverride> = {}): QuotaOverride => ({
  id: 50, consumer_id: 7, quotas: { daily: 1000, monthly: 0 }, starts_at: null, ends_at: null, note: null, version: 3, created_at: T0, updated_at: T0, ...over,
});

/** Consumer id's usage: daily and monthly used of their quotas (0: none), flushed at T0 when used. */
export const usage = (id: number, daily: [number, number], monthly: [number, number]): QuotaUsage => ({
  consumer_id: id,
  daily: { period: "d20261116", used: daily[0], quota: daily[1], flushed_at: daily[0] > 0 ? T0 : null },
  monthly: { period: "m202611", used: monthly[0], quota: monthly[1], flushed_at: monthly[0] > 0 ? T0 : null },
});

/** Serves /quota-usage from items, recording each request's consumer_id values. */
export function mockUsage(items: QuotaUsage[]): string[][] {
  const asked: string[][] = [];
  server.use(http.get("/api/v1/quota-usage", ({ request }) => {
    const ids = new URL(request.url).searchParams.getAll("consumer_id");
    asked.push(ids);
    return HttpResponse.json({ items: items.filter((u) => ids.includes(String(u.consumer_id))) });
  }));
  return asked;
}

export const entry = (over: Partial<AccessListEntry> = {}): AccessListEntry => ({
  id: 40, action: "deny", subject_type: "cidr", cidr: "203.0.113.0/24", api_key_id: null, consumer_id: null, note: null, version: 3,
  created_at: T0, updated_at: T0, ...over,
});

/** The latest published config: plans free and pro, a per-consumer policy, a per-IP one and a global one. */
export const SNAPSHOT = {
  plans: [{ id: 2, name: "pro" }, { id: 1, name: "free" }],
  policies: [
    { id: 4, name: "per-consumer", level: "consumer", key_template: "{consumer}" },
    { id: 5, name: "per-ip", level: "route", key_template: "{ip}" },
    { id: 6, name: "per-key-route", level: "route", key_template: "{api_key}:{route}" },
    { id: 8, name: "everyone", level: "global", key_template: "" },
  ],
};

export interface Sent {
  method: string;
  path: string;
  ifMatch: string | null;
  body: unknown;
}

const json = async (r: Request): Promise<unknown> => {
  const t = await r.text();
  return t ? JSON.parse(t) : null;
};

/**
 * Serves consumer 7 (ETag "<version>"), its keys (issue, rotate and revoke
 * change them), overrides, quota overrides and quota usage (120 of a daily
 * 1,000, no monthly quota), tenant acme and the latest config. answer, if
 * set, answers every write instead.
 */
export function mockConsumer({ keys = [apiKey()], overrides = [] as Override[], quotaOverrides = [] as QuotaOverride[], answer }: {
  keys?: ApiKey[]; overrides?: Override[]; quotaOverrides?: QuotaOverride[]; answer?: () => Response;
} = {}) {
  mockConfig(SNAPSHOT, ["acme", "globex"]);
  mockUsage([usage(7, [120, 1000], [0, 0])]);
  const sent: Sent[] = [];
  let now = consumer();
  let list = keys;
  const record = async (request: Request) => {
    const u = new URL(request.url);
    sent.push({ method: request.method, path: u.pathname, ifMatch: request.headers.get("If-Match"), body: await json(request) });
  };
  server.use(
    http.get("/api/v1/consumers/7", () => HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } })),
    http.put("/api/v1/consumers/7", async ({ request }) => {
      await record(request.clone());
      if (answer) return answer();
      now = { ...now, ...((await request.json()) as Consumer), version: now.version + 1 };
      return HttpResponse.json(now, { headers: { ETag: `"${now.version}"` } });
    }),
    http.get("/api/v1/consumers/7/keys", () => HttpResponse.json({ items: list, next_cursor: null })),
    http.post("/api/v1/consumers/7/keys", async ({ request }) => {
      await record(request);
      if (answer) return answer();
      const k = apiKey({ id: 21, prefix: "Ab12Cd34" });
      list = [...list, k];
      return HttpResponse.json({ ...k, key: SECRET }, { status: 201, headers: { "Cache-Control": "no-store" } });
    }),
    http.post("/api/v1/keys/:id/rotate", async ({ request, params }) => {
      await record(request);
      if (answer) return answer();
      const id = Number(params.id);
      const old = { ...(list.find((k) => k.id === id) ?? apiKey({ id })), expires_at: "2026-11-16T11:00:00Z", version: 2 };
      const k = apiKey({ id: 22, prefix: "Ef56Gh78", rotated_from: id });
      list = [...list.map((x) => (x.id === id ? old : x)), k];
      return HttpResponse.json({ old, new: { ...k, key: ROTATED } }, { headers: { "Cache-Control": "no-store" } });
    }),
    http.post("/api/v1/keys/:id/revoke", async ({ request, params }) => {
      await record(request);
      if (answer) return answer();
      const id = Number(params.id);
      const revoked = { ...(list.find((k) => k.id === id) ?? apiKey({ id })), revoked_at: "2026-11-16T10:30:00Z" };
      list = list.map((x) => (x.id === id ? revoked : x));
      return HttpResponse.json(revoked);
    }),
    http.get("/api/v1/overrides", () => HttpResponse.json({ items: overrides, next_cursor: null })),
    http.get("/api/v1/quota-overrides", () => HttpResponse.json({ items: quotaOverrides, next_cursor: null })),
    http.get("/api/v1/quota-overrides/:id", ({ params }) => {
      const o = quotaOverrides.find((x) => x.id === Number(params.id));
      return o ? HttpResponse.json(o, { headers: { ETag: `"${o.version}"` } }) : HttpResponse.json({ code: "not_found", error: "no such quota override" }, { status: 404 });
    }),
  );
  return { sent };
}

/** Everything the query client holds: every query's data and every mutation's variables, answer and error. */
export function cached(qc: QueryClient): string {
  return JSON.stringify([
    qc.getQueryCache().getAll().map((q) => [q.queryKey, q.state.data, q.state.error ? String(q.state.error) : null]),
    qc.getMutationCache().getAll().map((m) => [m.state.variables, m.state.data, m.state.context, m.state.error ? String(m.state.error) : null]),
  ]);
}
