// The Playground's reading of the gateways' RateLimit headers (ADR-0018):
// `"<policy>";r=<remaining>;t=<reset>` items, comma separated, copied
// verbatim by the control plane. Only policy names and numbers.
import type { components } from "../../api/schema.gen";

type Demo = components["schemas"]["Demo"];
type DemoLimit = components["schemas"]["DemoLimit"];

/** The r of policy's item in a RateLimit header, if there is one. */
export function remaining(header: string | null | undefined, policy: string): number | undefined {
  if (!header) return undefined;
  for (const item of header.split(",")) {
    const m = /^\s*"([^"]*)"((?:\s*;\s*[a-z]+=[^;,]*)*)\s*$/.exec(item);
    if (!m || m[1] !== policy) continue;
    const r = /;\s*r=(\d+)/.exec(m[2] ?? "");
    if (r) return Number(r[1]);
  }
  return undefined;
}

/** A period such as "1s" or "1m" in seconds; 1 when unreadable. */
export function periodSeconds(period: string): number {
  const m = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(period);
  if (!m) return 1;
  const n = Number(m[1]);
  return n * ({ ms: 0.001, s: 1, m: 60, h: 3600 } as const)[m[2] as "ms" | "s" | "m" | "h"];
}

/**
 * The limit a tenant's Playground traffic on route meets, and the policy
 * whose RateLimit item shows its bucket: the route's policy, with the
 * tenant's plan limit when the policy takes its limits from plans (orders).
 */
export function limitOf(demo: Demo, tenant: string, route: string): { policy: string | undefined; limit: DemoLimit } {
  const r = demo.routes.find((x) => x.name === route);
  const limit = r?.limit ?? demo.tenants.find((t) => t.name === tenant)?.limit ?? null;
  return { policy: r?.policy ?? undefined, limit };
}

/** A limit's rate per second, if any. */
export function perSecond(limit: DemoLimit): number | null {
  return limit ? limit.rate / periodSeconds(limit.period) : null;
}
