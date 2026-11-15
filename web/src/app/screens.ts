// The navigation (docs/design.md's screen table, ADR-0057's routes).
import type { OperationId } from "../api/roles.gen";

/**
 * The screens in the navigation, in its order. A screen with op is hidden
 * from roles that cannot call it, and its URL shows Not allowed (ADR-0058):
 * the audit log's read is admin only; Tenants exists only to manage tenants,
 * which is admin only (every role still reads tenant names elsewhere).
 */
export const navigation = [
  { to: "/overview", label: "Overview" },
  { to: "/throttling", label: "Throttling" },
  { to: "/policies", label: "Policies" },
  { to: "/publish", label: "Publish" },
  { to: "/routes", label: "Routes & upstreams" },
  { to: "/consumers", label: "Consumers & keys" },
  { to: "/tenants", label: "Tenants", op: "createTenant" },
  { to: "/replicas", label: "Replicas" },
  { to: "/history", label: "History" },
  { to: "/audit", label: "Audit", op: "listAudit" },
] as const satisfies readonly { to: string; label: string; op?: OperationId }[];

/** The operation that decides whether a screen is shown, if any. */
export function screenOp(s: (typeof navigation)[number]): OperationId | undefined {
  return "op" in s ? s.op : undefined;
}
