// Errors of the control plane's API as one type, and how screens show them
// (UI-06, ADR-0057, ADR-0060).
import type { components } from "./schema.gen";

export type ErrorCode = components["schemas"]["Error"]["code"];
export type ErrorDetail = components["schemas"]["ErrorDetail"];

const codes: readonly ErrorCode[] = [
  "invalid_request", "unauthenticated", "forbidden", "not_found", "conflict", "precondition_failed",
  "payload_too_large", "unsupported_media_type", "validation_failed", "precondition_required", "internal", "unavailable",
];

/** Any failed API call: an error answer (status ≥ 400) or no answer (status 0). */
export class ApiError extends Error {
  override readonly name = "ApiError";
  readonly status: number;
  readonly code: ErrorCode;
  readonly requestId: string | undefined;
  readonly details: readonly ErrorDetail[];
  /** Seconds from a Retry-After header, if any. */
  readonly retryAfter: number | undefined;

  constructor(init: { status: number; code: ErrorCode; message: string; requestId?: string; details?: readonly ErrorDetail[]; retryAfter?: number }) {
    super(init.message);
    this.status = init.status;
    this.code = init.code;
    this.requestId = init.requestId;
    this.details = init.details ?? [];
    this.retryAfter = init.retryAfter;
  }
}

/** The ApiError for an error answer; body is the parsed JSON or the text. */
export function apiErrorFrom(response: Response, body: unknown): ApiError {
  const retry = Number.parseInt(response.headers.get("Retry-After") ?? "", 10);
  const retryAfter = Number.isFinite(retry) && retry >= 0 ? retry : undefined;
  const headerID = response.headers.get("X-Request-Id") ?? undefined;
  if (isErrorBody(body)) {
    return new ApiError({
      status: response.status, code: body.code, message: body.error,
      requestId: body.request_id || headerID, details: body.details ?? [], retryAfter,
    });
  }
  // Not the API's error body: a proxy's or the network's answer.
  const unavailable = response.status >= 502 && response.status <= 504;
  return new ApiError({
    status: response.status, code: unavailable ? "unavailable" : "internal",
    message: `${response.status} ${response.statusText}`.trim(), requestId: headerID, retryAfter,
  });
}

/** The ApiError for a request that got no answer. */
export function networkError(): ApiError {
  return new ApiError({ status: 0, code: "unavailable", message: "no answer from the control plane" });
}

function isErrorBody(b: unknown): b is components["schemas"]["Error"] {
  if (typeof b !== "object" || b === null) return false;
  const o = b as Record<string, unknown>;
  return typeof o.error === "string" && codes.includes(o.code as ErrorCode) && (o.details === undefined || Array.isArray(o.details));
}

/** Network errors and 502–504: the control plane may answer a moment later. */
export function isTransient(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 0 || (err.status >= 502 && err.status <= 504));
}

/** Queries retry transient errors twice, after 1 s and 2 s (ADR-0060). */
export function retryQuery(failureCount: number, err: unknown): boolean {
  return failureCount < 2 && isTransient(err);
}

/** The delay before a query's retry: Retry-After when given, else 1 s, 2 s. */
export function retryDelay(attempt: number, err: unknown): number {
  if (err instanceof ApiError && err.retryAfter !== undefined) return Math.min(err.retryAfter, 30) * 1000;
  return 1000 * 2 ** attempt;
}

export interface ErrorView {
  title: string;
  message: string;
  requestId: string | undefined;
  /** Whether a Retry button makes sense. */
  retry: boolean;
}

/**
 * What a screen shows for err (ADR-0060's error mapping). needs is the roles
 * an operation needs, for a 403.
 */
export function describeError(err: unknown, needs?: readonly string[]): ErrorView {
  if (!(err instanceof ApiError)) {
    return { title: "Something went wrong", message: err instanceof Error ? err.message : String(err), requestId: undefined, retry: true };
  }
  const v = (title: string, message: string, retry = false): ErrorView => ({ title, message, requestId: err.requestId, retry });
  switch (err.status) {
    case 0:
    case 502:
    case 504:
      return v("Can't reach the control plane", "Check that it is running, then retry.", true);
    case 503:
      return v("Can't reach the control plane",
        err.retryAfter ? `It is unavailable: try again in ${err.retryAfter} s.` : "It is unavailable: try again shortly.", true);
    case 401:
      return v("Your session ended", "Sign in again to continue.");
    case 403:
      return v("Not allowed", needs?.length ? `This needs the ${orList(needs)} role.` : err.message);
    case 404:
      return v("Not found", "It may have been deleted.");
    case 409:
      return v("Conflict", err.message);
    case 412:
      return v("Changed since you opened it", "Reload to see the current version, then make your change again.");
    case 422:
      return v("Validation failed", err.message);
    case 400:
    case 413:
    case 415:
    case 428:
      return v("The request was rejected", err.message);
    default:
      return v("Something went wrong", err.message, true);
  }
}

/** "a", "a or b", "a, b or c". */
export function orList(items: readonly string[]): string {
  return items.length <= 1 ? (items[0] ?? "") : `${items.slice(0, -1).join(", ")} or ${items[items.length - 1]}`;
}
