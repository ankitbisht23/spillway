import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

/** The banner after a change to the config working set (ADR-0060): it ships only with a publish. */
export function Unpublished({ children }: { children?: ReactNode }) {
  return (
    <p className="banner" role="status">
      {children ?? "Saved to the working set."} Unpublished changes: <Link to="/publish">review and publish</Link>.
    </p>
  );
}
