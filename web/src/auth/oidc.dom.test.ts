import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { END_SESSION_ENDPOINT, ISSUER, provider } from "../test/keycloak";
import { server } from "../test/server";
import { loadUiConfig, logoutForm } from "./oidc";

const origin = "http://localhost:8080";

describe("the runtime config", () => {
  it("reads the issuer and client from /api/ui-config", async () => {
    server.use(http.get("/api/ui-config", () => HttpResponse.json({ issuer: ISSUER, client_id: "admin-ui" })));
    expect(await loadUiConfig()).toEqual({ issuer: ISSUER, client_id: "admin-ui" });
  });

  it("fails on a bad config", async () => {
    server.use(http.get("/api/ui-config", () => HttpResponse.json({ issuer: 1 })));
    await expect(loadUiConfig()).rejects.toThrow("not a config");
    server.use(http.get("/api/ui-config", () => new HttpResponse(null, { status: 502 })));
    await expect(loadUiConfig()).rejects.toThrow("502");
  });
});

describe("logoutForm", () => {
  it("POSTs the ID token as a form field, never in the action URL", () => {
    const form = logoutForm(provider, "id-token-1");
    expect(form?.method).toBe("post");
    expect(form?.action).toBe(END_SESSION_ENDPOINT);
    expect(Object.fromEntries(new FormData(form ?? undefined))).toEqual({
      id_token_hint: "id-token-1", client_id: "admin-ui", post_logout_redirect_uri: `${origin}/signed-out`,
    });
  });

  it("is null without an end-session endpoint", () => {
    expect(logoutForm({ ...provider, as: { issuer: ISSUER } }, "id-token-1")).toBeNull();
  });
});
