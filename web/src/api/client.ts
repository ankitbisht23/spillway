// The typed API client (UI-05, ADR-0057): openapi-fetch over the generated
// paths of api/openapi.yaml, same origin, so no CORS and no credentials.
import createClient from "openapi-fetch";

import { authFetch } from "../auth/session";
import { apiErrorFrom, networkError } from "./errors";
import type { paths } from "./schema.gen";

export type Fetch = (input: Request) => Promise<Response>;

/** A client for /api/v1 whose requests go through fetchFn. */
export function createApi(fetchFn: Fetch = (req) => fetch(req)) {
  return createClient<paths>({ baseUrl: new URL("/api/v1", location.origin).href, fetch: fetchFn });
}

export type Api = ReturnType<typeof createApi>;

/** The app's client: bearer token from the session, one retry on a 401 (ADR-0058). */
export const api = createApi(authFetch);

interface Result {
  data?: unknown;
  error?: unknown;
  response: Response;
}

/**
 * The answer of an API call, with its ETag (an entity's GET or PUT), or an
 * ApiError: any non-2xx answer, or none. An abort is passed on as it is.
 */
export async function call<R extends Result>(request: Promise<R>): Promise<{ data: Exclude<R["data"], undefined>; etag: string | null }> {
  let r: R;
  try {
    r = await request;
  } catch (err) {
    if ((err as { name?: unknown } | null)?.name === "AbortError") throw err;
    throw networkError();
  }
  if (!r.response.ok || r.error !== undefined) throw apiErrorFrom(r.response, r.error);
  return { data: r.data as Exclude<R["data"], undefined>, etag: r.response.headers.get("ETag") };
}
