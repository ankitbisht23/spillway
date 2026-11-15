// The live screens' filters as search params (UI-03, ADR-0059): only
// values the stream accepts survive; anything else falls back to the
// default, so a hand-edited URL cannot send a malformed filter.
import type { StreamParams } from "../../stream/client";

export const RANGES = ["15m", "1h", "6h", "24h", "7d"] as const;
export type Range = (typeof RANGES)[number];

export const RANGE_LABELS: Record<Range, string> = {
  "15m": "Last 15 minutes", "1h": "Last hour", "6h": "Last 6 hours", "24h": "Last 24 hours", "7d": "Last 7 days",
};

/** ADR-0036's name rule, which route and tenant values follow. */
const NAME = /^[A-Za-z0-9._-]{1,64}$/;

/**
 * A search value as text: the router parses values that look like JSON, so
 * a route named 123 arrives as a number.
 */
function text(v: unknown): string | undefined {
  return typeof v === "string" || typeof v === "number" || typeof v === "boolean" ? String(v) : undefined;
}

function name(v: unknown): string | undefined {
  const s = text(v);
  return s !== undefined && NAME.test(s) ? s : undefined;
}

function range(v: unknown): Range | undefined {
  const s = text(v);
  return RANGES.find((r) => r === s);
}

export interface OverviewSearch {
  route?: string;
  tenant?: string;
  range?: Range;
}

// Every key is set, undefined when absent or invalid: the router merges a
// route's validated search over the raw one, so an omitted key would keep
// its raw value.
export function overviewSearch(s: Record<string, unknown>): OverviewSearch {
  return { route: name(s.route), tenant: name(s.tenant), range: range(s.range) };
}

export interface ThrottlingSearch {
  range?: Range;
  policy?: number;
}

export function throttlingSearch(s: Record<string, unknown>): ThrottlingSearch {
  const p = Number(text(s.policy));
  return { range: range(s.range), policy: Number.isSafeInteger(p) && p >= 1 ? p : undefined };
}

/** The overview's stream parameters: unset filters are left to the server's defaults. */
export function overviewParams(s: OverviewSearch): StreamParams {
  return { route: s.route, tenant: s.tenant, range: s.range };
}

export function throttlingParams(s: ThrottlingSearch): StreamParams {
  return { range: s.range, policy_id: s.policy };
}
