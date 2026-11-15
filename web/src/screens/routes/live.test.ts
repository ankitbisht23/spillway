// @vitest-environment node
import { describe, expect, it } from "vitest";

import { circuitText, healthText, liveCircuits, liveEndpoints, summarize } from "./live";

const sample = (labels: Record<string, string>, value: number | null) => ({ labels, value });

const endpoints = {
  kind: "vector",
  series: [
    sample({ replica: "gateway-1", upstream: "u", endpoint: "10.0.0.1:80", stat: "healthy" }, 1),
    sample({ replica: "gateway-1", upstream: "u", endpoint: "10.0.0.1:80", stat: "ejected" }, 0),
    sample({ replica: "gateway-1", upstream: "u", endpoint: "10.0.0.2:80", stat: "healthy" }, 0),
    sample({ replica: "gateway-1", upstream: "u", endpoint: "10.0.0.2:80", stat: "ejected" }, 1),
    sample({ replica: "gateway-2", upstream: "u", endpoint: "10.0.0.2:80", stat: "healthy" }, 0),
    sample({ replica: "gateway-2", upstream: "u", endpoint: "10.0.0.2:80", stat: "ejected" }, 0),
    sample({ replica: "gateway-2", upstream: "v", endpoint: "10.0.0.9:80", stat: "healthy" }, null),
  ],
};

describe("live upstream state (ADR-0059)", () => {
  it("tells serving, ejected and failing health checks apart, per replica", () => {
    expect(liveEndpoints(endpoints)).toEqual([
      { replica: "gateway-1", upstream: "u", endpoint: "10.0.0.1:80", state: "serving" },
      { replica: "gateway-1", upstream: "u", endpoint: "10.0.0.2:80", state: "ejected" },
      { replica: "gateway-2", upstream: "u", endpoint: "10.0.0.2:80", state: "failing" },
      { replica: "gateway-2", upstream: "v", endpoint: "10.0.0.9:80", state: "unknown" },
    ]);
    expect(liveEndpoints({ items: [] })).toBeNull();
  });

  it("reads circuits and summarizes an upstream across replicas", () => {
    const circuits = liveCircuits({ kind: "vector", series: [sample({ replica: "gateway-1", upstream: "u" }, 0), sample({ replica: "gateway-2", upstream: "u" }, 1), sample({ replica: "gateway-2", upstream: "v" }, 7)] });
    expect(circuits?.map((c) => c.state)).toEqual(["closed", "open", "unknown"]);
    const s = summarize("u", liveEndpoints(endpoints) ?? [], circuits ?? []);
    expect(healthText(s)).toBe("1 of 3 taking traffic on 2 replicas");
    expect(circuitText(s)).toBe("open on gateway-2");
    const none = summarize("w", [], []);
    expect([healthText(none), circuitText(none)]).toEqual(["No gateway reports it", "–"]);
    expect(circuitText(summarize("u", [], [{ replica: "gateway-1", upstream: "u", state: "closed" }]))).toBe("closed");
  });
});
