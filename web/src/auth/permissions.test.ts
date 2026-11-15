import { describe, expect, it } from "vitest";

import { operationRoles } from "../api/roles.gen";
import { can, needsReason, type Role } from "./permissions";

const roles: Role[] = ["viewer", "editor", "publisher", "key-admin", "admin"];

describe("the generated role table", () => {
  it.each([
    ["listRoutes", roles],
    ["getMe", roles],
    ["updatePolicy", ["editor", "admin"]],
    ["drainEndpoint", ["editor", "admin"]],
    ["publishConfig", ["publisher", "admin"]],
    ["rollbackConfig", ["publisher", "admin"]],
    ["issueKey", ["key-admin", "admin"]],
    ["createTenant", ["admin"]],
    ["listAudit", ["admin"]],
    ["lookupHeavyHitters", ["admin"]],
  ] as const)("%s is allowed to %j", (op, allowed) => {
    expect(roles.filter((r) => can([r], op))).toEqual(allowed);
  });

  it("gives admin every operation", () => {
    for (const op of Object.keys(operationRoles) as (keyof typeof operationRoles)[]) expect(can(["admin"], op)).toBe(true);
  });

  it("allows the union of several roles, and nothing to none", () => {
    expect(can(["viewer", "publisher"], "publishConfig")).toBe(true);
    expect(can([], "listRoutes")).toBe(false);
  });

  it("names the roles an action needs", () => {
    expect(needsReason("publishConfig")).toBe("Needs the publisher or admin role");
    expect(needsReason("listAudit")).toBe("Needs the admin role");
    expect(needsReason("issueKey")).toBe("Needs the key-admin or admin role");
  });
});
