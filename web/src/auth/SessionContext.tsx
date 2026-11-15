// What components know of the session (ADR-0058): the user's display data,
// roles and signOut, never a token.
import { createContext, useContext } from "react";

import type { OperationId } from "../api/roles.gen";
import { can, needsReason, type Me } from "./permissions";

export interface SessionView {
  user: Me;
  signOut: () => void;
}

export const SessionContext = createContext<SessionView | null>(null);

export function useSession(): SessionView {
  const s = useContext(SessionContext);
  if (!s) throw new Error("useSession outside a session");
  return s;
}

/** Whether the user may call op, and if not, why (UI-04). */
export function useCan(op: OperationId): { allowed: boolean; reason: string | undefined } {
  const { user } = useSession();
  return can(user.roles, op) ? { allowed: true, reason: undefined } : { allowed: false, reason: needsReason(op) };
}
