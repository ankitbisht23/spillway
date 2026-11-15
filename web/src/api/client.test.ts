import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "../test/server";
import { call, createApi } from "./client";
import { ApiError } from "./errors";

const policy = {
  id: 7, name: "per-tenant", level: "tenant", mode: "shadow", algorithm: "gcra", key: "{tenant}",
  limits: [{ rate: 10, period: "1s", burst: 10 }], version: 3, created_at: "2026-11-15T00:00:00Z", updated_at: "2026-11-15T00:00:00Z",
};

describe("the typed client", () => {
  it("reads an entity's ETag and sends it back as If-Match", async () => {
    let ifMatch: string | null = null;
    server.use(
      http.get("/api/v1/policies/7", () => HttpResponse.json(policy, { headers: { ETag: '"3"' } })),
      http.put("/api/v1/policies/7", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return HttpResponse.json({ ...(await request.json() as object), version: 4 }, { headers: { ETag: '"4"' } });
      }),
    );
    const api = createApi();
    const got = await call(api.GET("/policies/{id}", { params: { path: { id: 7 } } }));
    expect(got.etag).toBe('"3"');
    expect(got.data.name).toBe("per-tenant");
    const put = await call(api.PUT("/policies/{id}", {
      params: { path: { id: 7 }, header: { "If-Match": got.etag ?? "" } },
      body: { ...got.data, mode: "enforce" },
    }));
    expect(ifMatch).toBe('"3"');
    expect(put.etag).toBe('"4"');
    expect(put.data.mode).toBe("enforce");
  });

  it("throws the API's error as ApiError", async () => {
    server.use(http.get("/api/v1/policies/8", () =>
      HttpResponse.json({ error: "no such entity", code: "not_found", request_id: "req-404" }, { status: 404 })));
    const err = await call(createApi().GET("/policies/{id}", { params: { path: { id: 8 } } })).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 404, code: "not_found", requestId: "req-404", message: "no such entity" });
  });

  it("makes a proxy's 502 page unavailable", async () => {
    server.use(http.get("/api/v1/replicas", () => new HttpResponse("<html>Bad Gateway</html>", { status: 502, headers: { "Content-Type": "text/html" } })));
    await expect(call(createApi().GET("/replicas"))).rejects.toMatchObject({ status: 502, code: "unavailable" });
  });

  it("makes no answer a network error", async () => {
    server.use(http.get("/api/v1/replicas", () => HttpResponse.error()));
    await expect(call(createApi().GET("/replicas"))).rejects.toMatchObject({ status: 0, code: "unavailable" });
  });

  it("passes an abort on as it is", async () => {
    server.use(http.get("/api/v1/replicas", () => new Promise(() => {})));
    const ctl = new AbortController();
    const p = call(createApi().GET("/replicas", { signal: ctl.signal }));
    ctl.abort();
    await expect(p).rejects.toMatchObject({ name: "AbortError" });
  });

  it("sends requests through the given fetch", async () => {
    const seen: string[] = [];
    server.use(http.get("/api/v1/replicas", () => HttpResponse.json({ items: [], next_cursor: null })));
    const api = createApi((req) => {
      seen.push(`${req.method} ${new URL(req.url).pathname}`);
      return fetch(req);
    });
    expect((await call(api.GET("/replicas"))).data).toEqual({ items: [], next_cursor: null });
    expect(seen).toEqual(["GET /api/v1/replicas"]);
  });
});
