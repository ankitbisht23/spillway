// What the caller's roles allow (UI-04, ADR-0058): operation IDs against
// the generated table of x-spillway-roles. Only spares the user a 403; the
// server is the enforcement.
import { orList } from "../api/errors";
import { operationRoles, type OperationId } from "../api/roles.gen";
import type { components } from "../api/schema.gen";

export type Me = components["schemas"]["Me"];
export type Role = Me["roles"][number];

/** Whether one of roles may call op. */
export function can(roles: readonly Role[], op: OperationId): boolean {
  const allowed: readonly Role[] = operationRoles[op];
  return roles.some((r) => allowed.includes(r));
}

/** Why an action is disabled: "Needs the editor or admin role". */
export function needsReason(op: OperationId): string {
  const allowed: readonly Role[] = operationRoles[op];
  return allowed.length ? `Needs the ${orList(allowed)} role` : "No role may do this";
}
