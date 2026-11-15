// What a preview or a version's diff changes, and what stops a publish
// (UI-02, ADR-0060): config entities by name, linked to their screens.
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import type { ErrorDetail } from "../api/errors";
import type { components } from "../api/schema.gen";

type Change = components["schemas"]["ConfigChange"];

/** A link to a config entity's screen; plans have none. */
function EntityLink({ entity, id, children }: { entity: string | undefined; id: number | undefined; children: ReactNode }) {
  if (id === undefined) return <>{children}</>;
  switch (entity) {
    case "policy":
      return <Link to="/policies/$id" params={{ id: String(id) }}>{children}</Link>;
    case "route":
      return <Link to="/routes/$id" params={{ id: String(id) }}>{children}</Link>;
    case "upstream":
      return <Link to="/upstreams/$id" params={{ id: String(id) }}>{children}</Link>;
    default:
      return <>{children}</>;
  }
}

export function ChangesList({ changes }: { changes: readonly Change[] }) {
  if (changes.length === 0) return <p className="muted">No entity changed.</p>;
  return (
    <ul className="changes" aria-label="Changes">
      {changes.map((c) => (
        <li key={`${c.entity}/${c.id}`}>
          <span className={`change change-${c.change}`}>{c.change}</span> {c.entity}{" "}
          <EntityLink entity={c.entity} id={c.id}>{c.name}</EntityLink>
        </li>
      ))}
    </ul>
  );
}

/** Validation errors, each with the entity at fault when the server names one. */
export function ValidationErrors({ errors }: { errors: readonly ErrorDetail[] }) {
  return (
    <ul className="validation-errors" aria-label="Validation errors">
      {errors.map((e, i) => (
        <li key={i}>
          {e.entity && (
            <>
              {e.entity}{" "}
              {e.name && <EntityLink entity={e.entity} id={e.id}>{e.name}</EntityLink>}
              {e.path && <> <code>{e.path}</code></>}:{" "}
            </>
          )}
          {e.message}
        </li>
      ))}
    </ul>
  );
}
