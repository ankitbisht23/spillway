// @vitest-environment node
import { describe, expect, it } from "vitest";

import { bodyOf, ROUTE_FIELDS, serverErrors, UPSTREAM_FIELDS, valuesOf, writable, type Route, type Upstream } from "./fields";

const route: Route = {
  id: 3, name: "orders", upstream: "orders-up", version: 5, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z",
  match: { host: "api.example.com", path_prefix: "/v1/orders", methods: ["GET", "POST"], headers: [{ name: "X-Tier", exact: "gold" }, { name: "X-Id", regex: "^[0-9]+$" }] },
  timeouts: { connect: "200ms", total: "3s" },
  retry: { attempts: 2, non_idempotent: true },
  response_headers: { set: [{ name: "X-Served-By", value: "spillway: edge" }], remove: ["Server"] },
  priority: 4,
};

describe("the route form (UI-06, ADR-0064)", () => {
  it("shows every setting and sends the same body back unchanged", () => {
    const v = valuesOf(ROUTE_FIELDS, route);
    expect(v).toMatchObject({
      name: "orders", "match.methods": "GET, POST", "match.headers": "X-Tier: gold\nX-Id ~ ^[0-9]+$", "timeouts.connect": "200ms",
      "retry.attempts": "2", "retry.non_idempotent": true, "response_headers.set": "X-Served-By: spillway: edge", "response_headers.remove": "Server",
      priority: "4", cost: "", auth_mode: "",
    });
    const { body, errors } = bodyOf(ROUTE_FIELDS, v, writable(route));
    expect(errors).toEqual({});
    expect(body).toEqual(writable(route));
    expect(body).not.toHaveProperty("id");
    expect(body).not.toHaveProperty("version");
  });

  it("unsets an emptied setting, drops objects left empty and keeps what the form does not show", () => {
    const base = { ...writable(route), future_setting: { kept: true } };
    const v = { ...valuesOf(ROUTE_FIELDS, route), "timeouts.connect": "", "timeouts.total": " ", "retry.attempts": "", "retry.non_idempotent": false, cost: "2", "request_headers.add": "X-A: 1\n\n X-B : two " };
    const { body } = bodyOf(ROUTE_FIELDS, v, base);
    expect(body).not.toHaveProperty("timeouts");
    expect(body).not.toHaveProperty("retry");
    expect(body.cost).toBe(2);
    expect(body.request_headers).toEqual({ add: [{ name: "X-A", value: "1" }, { name: "X-B", value: "two" }] });
    expect(body.future_setting).toEqual({ kept: true });
  });

  it("checks names, whole numbers, required fields and line formats", () => {
    const { errors } = bodyOf(ROUTE_FIELDS, {
      ...valuesOf(ROUTE_FIELDS), name: "no spaces", priority: "1.5", cost: "x", "request_headers.set": "no colon", "match.headers": "bad line",
    });
    expect(errors).toEqual({
      name: "1–64 characters from A–Z, a–z, 0–9, '.', '_' and '-'",
      upstream: "Required",
      priority: "Must be a whole number",
      cost: "Must be a whole number",
      "request_headers.set": '"no colon": want Name: value',
      "match.headers": '"bad line": want Name: value or Name ~ regex',
    });
  });

  it("puts a 422's details on the fields they name, by path or by the message's start", () => {
    const r = serverErrors(ROUTE_FIELDS, [
      { entity: "route", path: "upstream", message: 'upstream "x" does not exist' },
      { message: "timeouts.connect: want a duration" },
      { message: 'auth_mode "basic": want none, api_key, jwt or either' },
      { message: "set exactly one of match.path and match.path_prefix" },
      { path: "elsewhere", message: "something" },
    ]);
    expect(r.fields).toEqual({
      upstream: 'upstream "x" does not exist',
      "timeouts.connect": "timeouts.connect: want a duration",
      auth_mode: 'auth_mode "basic": want none, api_key, jwt or either',
    });
    expect(r.rest).toEqual(["set exactly one of match.path and match.path_prefix", "elsewhere: something"]);
  });
});

describe("the upstream form (UI-06, ADR-0064)", () => {
  const upstream: Upstream = {
    id: 2, name: "orders-up", version: 9, created_at: "2026-11-16T10:00:00Z", updated_at: "2026-11-16T10:00:00Z",
    endpoints: [{ id: 11, address: "orders-1:8080" }, { id: 12, address: "orders-2:8080", draining: true }],
    circuit: { error_rate: 0.5, min_requests: 20 },
  };

  it("keeps a kept address's draining state; a new address takes traffic", () => {
    const v = { ...valuesOf(UPSTREAM_FIELDS, upstream), endpoints: "orders-2:8080\norders-3:8080\n", "circuit.error_rate": "0.25" };
    expect(valuesOf(UPSTREAM_FIELDS, upstream).endpoints).toBe("orders-1:8080\norders-2:8080");
    const { body, errors } = bodyOf(UPSTREAM_FIELDS, v, writable(upstream));
    expect(errors).toEqual({});
    expect(body.endpoints).toEqual([{ address: "orders-2:8080", draining: true }, { address: "orders-3:8080" }]);
    expect(body.circuit).toEqual({ error_rate: 0.25, min_requests: 20 });
  });

  it("needs an endpoint and a number", () => {
    const { errors } = bodyOf(UPSTREAM_FIELDS, { ...valuesOf(UPSTREAM_FIELDS, upstream), endpoints: " \n", "retry_budget.ratio": "a fifth" });
    expect(errors).toEqual({ endpoints: "Required", "retry_budget.ratio": "Must be a number" });
  });

  it("maps endpoint and nested messages to their fields", () => {
    const r = serverErrors(UPSTREAM_FIELDS, [
      { message: 'endpoint "bad": want host:port' },
      { message: "health_check.interval: must be positive" },
      { message: "needs at least one endpoint" },
    ]);
    expect(r.fields).toEqual({ endpoints: 'endpoint "bad": want host:port', "health_check.interval": "health_check.interval: must be positive" });
    expect(r.rest).toEqual(["needs at least one endpoint"]);
  });
});
