// Pieces the live screens share (UI-03, UI-06).
import type { ReactNode } from "react";

import { describeError, type ApiError } from "../../api/errors";
import { operationRoles } from "../../api/roles.gen";
import { ErrorState, NotAllowed } from "../../components/states";
import { RANGES, RANGE_LABELS, type Range } from "./search";

/** A titled panel of a live screen. */
export function Card({ title, wide, children }: { title: string; wide?: boolean; children: ReactNode }) {
  return (
    <section className={wide ? "card card-wide" : "card"} aria-label={title}>
      <h2>{title}</h2>
      {children}
    </section>
  );
}

/** The stream stopped for good: a 403, a 400 (a filter the server refused) or the session's end. */
export function StreamFailed({ error, reset }: { error: ApiError; reset: ReactNode }) {
  if (error.status === 403) return <NotAllowed message={describeError(error, operationRoles.openStream).message} />;
  return (
    <div className="stream-stopped">
      <ErrorState view={describeError(error)} />
      {error.status === 400 && reset}
    </div>
  );
}

export function RangeSelect({ value, onChange }: { value: Range; onChange: (r: Range) => void }) {
  return (
    <label>
      Range{" "}
      <select value={value} onChange={(e) => onChange(e.target.value as Range)}>
        {RANGES.map((r) => <option key={r} value={r}>{RANGE_LABELS[r]}</option>)}
      </select>
    </label>
  );
}
