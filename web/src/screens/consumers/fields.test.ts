import { describe, expect, it } from "vitest";

import { parseSnapshot } from "../../api/latestConfig";
import { bodyOf, localInput, valuesOf, type FieldSpec } from "../routes/fields";
import { overrideFields, takesOverrides } from "./fields";

const LIMITS: FieldSpec = { path: "limits", label: "Limits", kind: "limits", required: true };
const ENDS: FieldSpec = { path: "ends_at", label: "Ends", kind: "datetime" };
const PLAN: FieldSpec = { path: "plan_id", label: "Plan", kind: "int", choices: [{ value: "2", label: "pro" }] };

describe("limit lines (RL-08)", () => {
  it("round-trips rate/period and an optional burst", () => {
    const limits = [{ rate: 100, period: "1s", burst: 200 }, { rate: 5000, period: "1h" }];
    const values = valuesOf([LIMITS], { limits });
    expect(values.limits).toBe("100/1s burst 200\n5000/1h");
    expect(bodyOf([LIMITS], values).body).toEqual({ limits });
    expect(bodyOf([LIMITS], { limits: " 10 / 1m \n\n" }).body).toEqual({ limits: [{ rate: 10, period: "1m" }] });
  });

  it.each(["100", "100/", "fast/1s", "100/1s burst", "100/1s bursty 3"])("rejects %j", (line) => {
    expect(bodyOf([LIMITS], { limits: line }).errors.limits).toMatch(/want rate\/period/);
  });

  it("is required when the field is", () => {
    expect(bodyOf([LIMITS], { limits: "" }).errors.limits).toBe("Required");
  });
});

describe("date and time fields", () => {
  it("send an instant, keep an unchanged one exactly, and unset an empty one", () => {
    const at = "2026-11-16T10:00:00.123Z";
    expect(bodyOf([ENDS], valuesOf([ENDS], { ends_at: at }), { ends_at: at }).body).toEqual({ ends_at: at });
    expect(bodyOf([ENDS], { ends_at: "2026-12-01T18:30" }).body).toEqual({ ends_at: new Date("2026-12-01T18:30").toISOString() });
    expect(bodyOf([ENDS], { ends_at: "" }, { ends_at: at }).body).toEqual({});
    expect(bodyOf([ENDS], { ends_at: "soon" }).errors.ends_at).toBe("Must be a date and time");
    expect(localInput(null)).toBe("");
    expect(localInput("not a time")).toBe("");
  });
});

describe("choices", () => {
  it("send the chosen ID as a number, and none for the empty choice", () => {
    expect(bodyOf([PLAN], { plan_id: "2" }).body).toEqual({ plan_id: 2 });
    expect(bodyOf([PLAN], { plan_id: "" }, { plan_id: 2 }).body).toEqual({});
  });
});

describe("which policies take overrides (RL-08, ADR-0041)", () => {
  it("consumer or route level, keyed per consumer", () => {
    const p = (level: string, keyTemplate: string) => ({ id: 1, name: "p", mode: "enforce", level, keyTemplate });
    expect(takesOverrides(p("consumer", "{consumer}"))).toBe(true);
    expect(takesOverrides(p("route", "{api_key}:{route}"))).toBe(true);
    expect(takesOverrides(p("route", "{ip}"))).toBe(false);
    expect(takesOverrides(p("tenant", "{consumer}"))).toBe(false);
    expect(takesOverrides(p("global", ""))).toBe(false);
    expect(overrideFields([p("consumer", "{consumer}"), p("global", "")])[0]?.choices).toEqual([{ value: "1", label: "p" }]);
  });
});

describe("the latest published config", () => {
  it("reads policies with their level and plans, sorted by name, skipping malformed entries", () => {
    const c = parseSnapshot(3, {
      routes: [{ name: "b" }, { name: "a" }, { nope: 1 }],
      policies: [{ id: 2, name: "z", level: "route", key_template: "{ip}" }, { id: 1, name: "a" }, { name: "no-id" }],
      plans: [{ id: 2, name: "pro" }, { id: 1, name: "free" }, { id: "3", name: "bad" }],
    });
    expect(c).toEqual({
      version: 3, routes: ["a", "b"],
      policies: [{ id: 1, name: "a", mode: "enforce", keyTemplate: "", level: "" }, { id: 2, name: "z", mode: "enforce", keyTemplate: "{ip}", level: "route" }],
      plans: [{ id: 1, name: "free" }, { id: 2, name: "pro" }],
    });
    expect(parseSnapshot(1, undefined)).toEqual({ version: 1, routes: [], policies: [], plans: [] });
  });
});
