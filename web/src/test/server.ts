// The one mock of the network (D10, ADR-0061): MSW at the fetch boundary.
// An unmocked request fails its test.
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";

/**
 * GET /demo as a control plane with demo mode off answers it (SC-08,
 * ADR-0095): every app shell asks once, so this handler is the default;
 * a test of demo mode replaces it with server.use.
 */
export const demoOff = http.get("/api/v1/demo", () =>
  HttpResponse.json({ error: "no such path", code: "not_found", request_id: "req-demo" }, { status: 404 }));

export const server = setupServer(demoOff);
