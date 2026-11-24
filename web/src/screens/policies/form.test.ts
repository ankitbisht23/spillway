import { describe, expect, it } from "vitest";

import { bodyOf, formOf, newForm, parseDuration, parseKeyTemplate, serverErrors, validatePolicy, type Policy, type PolicyForm } from "./form";

const valid = (over: Partial<PolicyForm> = {}): PolicyForm => ({
  ...newForm(), name: "per-client", routes: "orders", keyTemplate: "{header:X-Client}", limits: [{ rate: "100", period: "1s", burst: "" }], ...over,
});

describe("validatePolicy (CP-03's policy rules, in the browser)", () => {
  it("passes a valid policy", () => {
    expect(validatePolicy(valid())).toEqual({});
    expect(validatePolicy(valid({ routes: "*", algorithm: "concurrency", limits: [{ rate: "20", period: "", burst: "" }] }))).toEqual({});
    expect(validatePolicy(valid({ local: true, algorithm: "fixed_window", limits: [{ rate: "10", period: "1m", burst: "" }, { rate: "100", period: "1h", burst: "" }] }))).toEqual({});
    expect(validatePolicy(valid({ level: "consumer", keyTemplate: "{consumer}", fromPlan: true, limits: [] }))).toEqual({});
    // Lease sizes up to every limit's burst (its rate by default) or window rate.
    expect(validatePolicy(valid({ leaseK: "100" }))).toEqual({});
    expect(validatePolicy(valid({ leaseK: "0" }))).toEqual({});
    expect(validatePolicy(valid({ algorithm: "sliding_window", leaseK: "60", limits: [{ rate: "60", period: "1m", burst: "" }] }))).toEqual({});
    expect(validatePolicy(valid({ level: "consumer", keyTemplate: "{consumer}", fromPlan: true, limits: [], leaseK: "500" }))).toEqual({});
    // Hidden, so not checked: a local policy's leftover value.
    expect(validatePolicy(valid({ local: true, leaseK: "x" }))).toEqual({});
  });

  it.each<[string, Partial<PolicyForm>, keyof PolicyForm, string | RegExp]>([
    ["an empty name", { name: "" }, "name", /1–64 characters/],
    ["a name with a space", { name: "per client" }, "name", /1–64 characters/],
    ["a name over 64 characters", { name: "x".repeat(65) }, "name", /1–64/],
    ["no level", { level: "" }, "level", "Choose a level"],
    ["no routes", { routes: " , " }, "routes", /Required/],
    ["* with other routes", { routes: "*, orders" }, "routes", '"*" must be the only entry'],
    ["a route twice", { routes: "orders, orders" }, "routes", '"orders" is listed twice'],
    ["no key template", { keyTemplate: "" }, "keyTemplate", /Required/],
    ["an unknown placeholder", { keyTemplate: "{user}" }, "keyTemplate", /Unknown placeholder \{user\}/],
    ["a bad header name", { keyTemplate: "{header:X Client}" }, "keyTemplate", /not a valid header name/],
    ["an unclosed brace", { keyTemplate: "{ip" }, "keyTemplate", "'{' without '}'"],
    ["a stray brace", { keyTemplate: "ip}" }, "keyTemplate", "'}' without '{'"],
    ["a failure mode on a local policy", { local: true, failureMode: "fail_closed" }, "failureMode", /local policies never fail/],
    ["fixed_window in Redis", { algorithm: "fixed_window" }, "algorithm", /must be local/],
    ["no limits", { limits: [] }, "limits", "At least one limit is required"],
    ["a rate of 0", { limits: [{ rate: "0", period: "1s", burst: "" }] }, "limits", /Limit 1: rate/],
    ["a rate that is not a number", { limits: [{ rate: "ten", period: "1s", burst: "" }] }, "limits", /Limit 1: rate/],
    ["a period under a second", { limits: [{ rate: "1", period: "500ms", burst: "" }] }, "limits", /Limit 1: period/],
    ["a period of fractional seconds", { limits: [{ rate: "1", period: "1.5s", burst: "" }] }, "limits", /Limit 1: period/],
    ["a period over 8760h", { limits: [{ rate: "1", period: "8761h", burst: "" }] }, "limits", /Limit 1: period/],
    ["two limits with one period", { limits: [{ rate: "1", period: "60s", burst: "" }, { rate: "2", period: "1m", burst: "" }] }, "limits", /Limit 2: another limit/],
    ["no burst over a minute", { limits: [{ rate: "1000", period: "1h", burst: "" }] }, "limits", /burst is required/],
    ["a burst of 0", { limits: [{ rate: "1", period: "1s", burst: "0" }] }, "limits", /Limit 1: burst/],
    ["a burst on a sliding window", { algorithm: "sliding_window", limits: [{ rate: "1", period: "1s", burst: "5" }] }, "limits", /only gcra has a burst/],
    ["two concurrency limits", { algorithm: "concurrency", limits: [{ rate: "1", period: "", burst: "" }, { rate: "2", period: "", burst: "" }] }, "limits", /exactly one limit/],
    ["a concurrency period", { algorithm: "concurrency", limits: [{ rate: "1", period: "1s", burst: "" }] }, "limits", /exactly one limit/],
    ["from plan on a global policy", { level: "global", keyTemplate: "{consumer}", fromPlan: true, limits: [] }, "fromPlan", /Plans apply only/],
    ["from plan per IP", { level: "consumer", keyTemplate: "{ip}", fromPlan: true, limits: [] }, "fromPlan", /Plans apply only/],
    ["a lease size that is not a number", { leaseK: "ten" }, "leaseK", "A whole number from 0 to 1000000"],
    ["a negative lease size", { leaseK: "-1" }, "leaseK", /A whole number/],
    ["a lease size over 10^6", { leaseK: "1000001" }, "leaseK", /A whole number/],
    ["a lease size above the default burst", { leaseK: "101" }, "leaseK", "At most limit 1's burst of 100: a lease holds at most what a limit admits at once"],
    ["a lease size above a second limit's burst", { leaseK: "11", limits: [{ rate: "100", period: "1s", burst: "" }, { rate: "1000", period: "1h", burst: "10" }] }, "leaseK", /At most limit 2's burst of 10/],
    ["a lease size above a window's rate", { algorithm: "sliding_window", leaseK: "61", limits: [{ rate: "60", period: "1m", burst: "" }] }, "leaseK", /At most limit 1's rate of 60/],
  ])("flags %s", (_, over, field, want) => {
    const errs = validatePolicy(valid(over));
    expect(errs[field]).toMatch(want);
  });
});

describe("parseDuration (Go's time.ParseDuration)", () => {
  it.each([["1s", 1], ["90m", 5400], ["1h30m", 5400], ["1.5h", 5400], ["500ms", 0.5], ["0", 0]])("%s is %d s", (s, want) => {
    expect(parseDuration(s)).toBe(want);
  });
  it.each(["", "1", "s", "1d", "1s ", "-1s", "1x"])("%j is not a duration", (s) => {
    expect(parseDuration(s)).toBeNull();
  });
});

describe("parseKeyTemplate", () => {
  it("lists the placeholders", () => {
    expect(parseKeyTemplate("t:{tenant}/{header:X-Client}")).toEqual({ fields: ["tenant", "header:X-Client"] });
  });
});

describe("the body", () => {
  it("is the form's fields, without read-only ones, and round-trips", () => {
    const p = {
      id: 7, name: "per-tenant", level: "tenant", match: { routes: ["a", "b"], methods: ["GET"] }, key_template: "{tenant}", algorithm: "gcra",
      limits: [{ rate: 100, period: "1s", burst: 200 }, { rate: 10000, period: "1h", burst: 500 }], mode: "shadow", failure_mode: "degraded",
      version: 4, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z",
    } as Policy;
    const body = bodyOf(formOf(p));
    expect(body).toEqual({
      name: "per-tenant", level: "tenant", match: { routes: ["a", "b"], methods: ["GET"] }, key_template: "{tenant}", algorithm: "gcra",
      limits: [{ rate: 100, period: "1s", burst: 200 }, { rate: 10000, period: "1h", burst: 500 }], mode: "shadow", failure_mode: "degraded",
    });
  });

  it("sends from_plan without limits, local, and a concurrency limit without a period", () => {
    expect(bodyOf(valid({ level: "consumer", keyTemplate: "{consumer}", fromPlan: true }))).toMatchObject({ from_plan: true });
    expect(bodyOf(valid({ level: "consumer", keyTemplate: "{consumer}", fromPlan: true }))).not.toHaveProperty("limits");
    expect(bodyOf(valid({ algorithm: "concurrency", local: true, limits: [{ rate: "5", period: "", burst: "" }] }))).toMatchObject({ local: true, limits: [{ rate: 5 }] });
  });

  it("sends lease_k for a batched Redis policy only", () => {
    expect(bodyOf(valid({ leaseK: "10" }))).toMatchObject({ lease_k: 10 });
    expect(bodyOf(valid({ leaseK: "0" }))).not.toHaveProperty("lease_k");
    expect(bodyOf(valid({ leaseK: "" }))).not.toHaveProperty("lease_k");
    expect(bodyOf(valid({ local: true, leaseK: "10" }))).not.toHaveProperty("lease_k");
    expect(formOf({ id: 1, name: "x", lease_k: 25, version: 1, created_at: "", updated_at: "" }).leaseK).toBe("25");
    expect(formOf({ id: 1, name: "x", version: 1, created_at: "", updated_at: "" }).leaseK).toBe("");
  });

  it("reads an unset mode as enforce", () => {
    expect(formOf({ id: 1, name: "x", version: 1, created_at: "", updated_at: "" }).mode).toBe("enforce");
  });
});

describe("serverErrors (422 in place)", () => {
  it("places details by path, then by the message's field, and keeps the rest", () => {
    const got = serverErrors([
      { entity: "policy", path: "name", message: "name must be 1-64 characters" },
      { entity: "policy", message: 'match.routes: route "nope" is not defined' },
      { entity: "policy", message: 'limits[0].burst: required for periods over 1m0s (RL-03)' },
      { entity: "policy", message: 'route "orders" costs 5, more than the 2 that limits[0] admits' },
      { entity: "policy", message: "something else" },
      { entity: "policy", message: "lease_k 21 is more than limits[0]'s burst of 20" },
    ]);
    expect(got.fields).toEqual({
      name: "name must be 1-64 characters",
      routes: 'match.routes: route "nope" is not defined',
      limits: "limits[0].burst: required for periods over 1m0s (RL-03)",
      leaseK: "lease_k 21 is more than limits[0]'s burst of 20",
    });
    expect(got.rest).toEqual(['route "orders" costs 5, more than the 2 that limits[0] admits', "something else"]);
  });
});
