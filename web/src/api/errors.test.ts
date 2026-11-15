// @vitest-environment node
import { describe, expect, it } from "vitest";

import { ApiError, apiErrorFrom, describeError, isTransient, networkError, orList, retryDelay, retryQuery } from "./errors";

const answer = (status: number, headers: Record<string, string> = {}) => new Response(null, { status, headers });

describe("apiErrorFrom", () => {
  it("reads the API's error body", () => {
    const e = apiErrorFrom(answer(422, { "X-Request-Id": "hdr" }), {
      error: "validation failed: rate: must be > 0", code: "validation_failed", request_id: "req-1",
      details: [{ entity: "policy", path: "limits[0].rate", message: "must be > 0" }],
    });
    expect(e).toBeInstanceOf(ApiError);
    expect(e).toMatchObject({ status: 422, code: "validation_failed", message: "validation failed: rate: must be > 0", requestId: "req-1" });
    expect(e.details).toEqual([{ entity: "policy", path: "limits[0].rate", message: "must be > 0" }]);
  });

  it("takes Retry-After and falls back to the header's request ID", () => {
    const e = apiErrorFrom(answer(503, { "Retry-After": "3", "X-Request-Id": "hdr" }), { error: "the database is unavailable", code: "unavailable", request_id: "" });
    expect(e).toMatchObject({ status: 503, code: "unavailable", retryAfter: 3, requestId: "hdr" });
  });

  it.each([502, 503, 504])("makes a non-API %i body unavailable", (status) => {
    const e = apiErrorFrom(answer(status, { "X-Request-Id": "proxy" }), "<html>Bad Gateway</html>");
    expect(e).toMatchObject({ status, code: "unavailable", requestId: "proxy", details: [] });
  });

  it.each([
    ["HTML", 500, "<html>oops</html>"],
    ["an unknown code", 400, { error: "x", code: "teapot", request_id: "r" }],
    ["no message", 404, { code: "not_found" }],
    ["details not a list", 422, { error: "x", code: "validation_failed", request_id: "r", details: "x" }],
    ["nothing", 500, undefined],
  ])("makes %s internal", (_, status, body) => {
    expect(apiErrorFrom(answer(status), body)).toMatchObject({ status, code: "internal", retryAfter: undefined });
  });

  it("ignores a Retry-After that is not seconds", () => {
    expect(apiErrorFrom(answer(503, { "Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT" }), null).retryAfter).toBeUndefined();
  });
});

describe("retries", () => {
  const status = (s: number) => new ApiError({ status: s, code: "internal", message: "" });

  it("retries network errors and 502–504 twice, nothing else", () => {
    for (const e of [networkError(), status(502), status(503), status(504)]) {
      expect(isTransient(e)).toBe(true);
      expect([0, 1, 2].map((n) => retryQuery(n, e))).toEqual([true, true, false]);
    }
    for (const e of [status(400), status(401), status(403), status(404), status(409), status(412), status(500), new Error("x")]) {
      expect(retryQuery(0, e)).toBe(false);
    }
  });

  it("waits 1 s, then 2 s, or what Retry-After says, at most 30 s", () => {
    expect(retryDelay(0, networkError())).toBe(1000);
    expect(retryDelay(1, networkError())).toBe(2000);
    expect(retryDelay(0, new ApiError({ status: 503, code: "unavailable", message: "", retryAfter: 5 }))).toBe(5000);
    expect(retryDelay(0, new ApiError({ status: 503, code: "unavailable", message: "", retryAfter: 600 }))).toBe(30000);
  });
});

describe("describeError", () => {
  const err = (status: number, message = "server says", retryAfter?: number) =>
    new ApiError({ status, code: "internal", message, requestId: "req-9", retryAfter });

  it.each([
    [0, "Can't reach the control plane", "Check that it is running, then retry.", true],
    [502, "Can't reach the control plane", "Check that it is running, then retry.", true],
    [504, "Can't reach the control plane", "Check that it is running, then retry.", true],
    [503, "Can't reach the control plane", "It is unavailable: try again shortly.", true],
    [401, "Your session ended", "Sign in again to continue.", false],
    [403, "Not allowed", "server says", false],
    [404, "Not found", "It may have been deleted.", false],
    [409, "Conflict", "server says", false],
    [412, "Changed since you opened it", "Reload to see the current version, then make your change again.", false],
    [422, "Validation failed", "server says", false],
    [400, "The request was rejected", "server says", false],
    [413, "The request was rejected", "server says", false],
    [415, "The request was rejected", "server says", false],
    [428, "The request was rejected", "server says", false],
    [500, "Something went wrong", "server says", true],
  ])("maps %i", (status, title, message, retry) => {
    expect(describeError(err(status))).toEqual({ title, message, retry, requestId: "req-9" });
  });

  it("names Retry-After and the roles a 403 needs", () => {
    expect(describeError(err(503, "", 4)).message).toBe("It is unavailable: try again in 4 s.");
    expect(describeError(err(403), ["editor", "admin"]).message).toBe("This needs the editor or admin role.");
  });

  it("shows other errors as they are", () => {
    expect(describeError(new TypeError("boom"))).toEqual({ title: "Something went wrong", message: "boom", requestId: undefined, retry: true });
    expect(describeError("odd")).toMatchObject({ message: "odd" });
  });

  it("joins role names", () => {
    expect([[], ["admin"], ["editor", "admin"], ["a", "b", "c"]].map(orList)).toEqual(["", "admin", "editor or admin", "a, b or c"]);
  });
});
