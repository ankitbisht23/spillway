// @vitest-environment node
import { describe, expect, it } from "vitest";

import { formatUnit } from "./LineChart";
import { align, byLabel, formatCount, formatMs, formatNumber, formatShare, isEmptyAligned, latest } from "./series";

describe("align", () => {
  it("puts every series on the union of the timestamps, null where one has no point", () => {
    const a = align({
      kind: "matrix", step_s: 15, series: [
        { labels: { c: "b" }, points: [[30, 2], [15, 1]] },
        { labels: { c: "a" }, points: [[0, 0.5], [30, null]] },
      ],
    }, (s) => s.labels.c ?? "", 1000);
    expect(a).toEqual({ times: [0, 15, 30], lines: [{ label: "a", values: [500, null, null] }, { label: "b", values: [null, 1000, 2000] }] });
    expect(isEmptyAligned(a)).toBe(false);
    expect(isEmptyAligned(align({ kind: "matrix", step_s: 15, series: [{ labels: {}, points: [[0, null]] }] }, () => "x"))).toBe(true);
  });

  it("orders lines by the order given", () => {
    const a = align({ kind: "matrix", step_s: 15, series: [{ labels: { c: "x" }, points: [] }, { labels: { c: "y" }, points: [] }] }, (s) => s.labels.c ?? "", 1, (p, q) => (p < q ? 1 : -1));
    expect(a.lines.map((l) => l.label)).toEqual(["y", "x"]);
  });
});

describe("latest and byLabel", () => {
  it("finds the last value that is not null", () => {
    expect(latest([1, 2, null])).toBe(2);
    expect(latest([null])).toBeNull();
    expect(latest([])).toBeNull();
  });

  it("groups a vector's stats by a label, skipping samples without it", () => {
    expect(byLabel({
      kind: "vector", series: [
        { labels: { route: "a", stat: "rps" }, value: 1 },
        { labels: { route: "a", stat: "p99_s" }, value: null },
        { labels: { stat: "rps" }, value: 9 },
        { labels: { route: "b" }, value: 9 },
      ],
    }, "route")).toEqual([{ name: "a", stats: { rps: 1, p99_s: null } }]);
  });
});

describe("formats", () => {
  it.each([[null, "–"], [0, "0"], [0.004, "<0.01"], [0.5, "0.50"], [12.34, "12.3"], [123.4, "123"], [12345.6, "12,346"], [-2, "-2.00"]])("number %s is %s", (v, want) => {
    expect(formatNumber(v)).toBe(want);
  });

  it.each([[undefined, "–"], [0, "0 %"], [0.0004, "<0.1 %"], [0.0123, "1.2 %"], [0.5, "50 %"]])("share %s is %s", (v, want) => {
    expect(formatShare(v)).toBe(want);
  });

  it("formats milliseconds, counts and chart units", () => {
    expect(formatMs(0.0042)).toBe("4.2 ms");
    expect(formatMs(0.25)).toBe("250 ms");
    expect(formatMs(null)).toBe("–");
    expect(formatCount(1234567)).toBe("1,234,567");
    expect(formatUnit("rps", 2)).toBe("2.00/s");
    expect(formatUnit("rps", null)).toBe("–");
    expect(formatUnit("ms", 31)).toBe("31 ms");
    expect(formatUnit("ms", undefined)).toBe("–");
    expect(formatUnit("share", 0.25)).toBe("25 %");
  });
});
