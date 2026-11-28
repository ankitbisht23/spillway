import { describe, expect, it } from "vitest";

import { limitOf, periodSeconds, perSecond, remaining } from "./ratelimit";

describe("the Playground's RateLimit reading (ADR-0018)", () => {
  it("finds a policy's remaining tokens in a RateLimit header", () => {
    const h = '"consumer-plan";r=7;t=1, "tenant-ceiling";r=1490;t=1';
    expect(remaining(h, "consumer-plan")).toBe(7);
    expect(remaining(h, "tenant-ceiling")).toBe(1490);
    expect(remaining(h, "catalog-limit")).toBeUndefined();
    expect(remaining(null, "consumer-plan")).toBeUndefined();
    expect(remaining('"consumer-plan";t=1', "consumer-plan")).toBeUndefined();
    expect(remaining("garbage", "consumer-plan")).toBeUndefined();
  });

  it("reads periods and per-second rates", () => {
    expect(periodSeconds("1s")).toBe(1);
    expect(periodSeconds("1m")).toBe(60);
    expect(periodSeconds("500ms")).toBe(0.5);
    expect(periodSeconds("2h")).toBe(7200);
    expect(periodSeconds("weird")).toBe(1);
    expect(perSecond({ rate: 120, burst: 120, period: "1m" })).toBe(2);
    expect(perSecond(null)).toBeNull();
  });

  it("takes orders' limit from the tenant's plan and the other routes' from their policy", () => {
    const demo = {
      tenants: [{ name: "free-co", plan: "free", limit: { rate: 10, burst: 20, period: "1s" } }],
      routes: [
        { name: "orders", policy: "consumer-plan", limit: null },
        { name: "catalog", policy: "catalog-limit", limit: { rate: 200, burst: 200, period: "1s" } },
      ],
      scenarios: [], faults: [], playground: { max_rate: 200, max_duration_s: 60, active_runs: 0, runs_left_this_hour: 30 },
      client_ip: "198.51.100.23",
    };
    expect(limitOf(demo, "free-co", "orders")).toEqual({ policy: "consumer-plan", limit: { rate: 10, burst: 20, period: "1s" } });
    expect(limitOf(demo, "free-co", "catalog")).toEqual({ policy: "catalog-limit", limit: { rate: 200, burst: 200, period: "1s" } });
    expect(limitOf(demo, "free-co", "payments")).toEqual({ policy: undefined, limit: { rate: 10, burst: 20, period: "1s" } });
  });
});
