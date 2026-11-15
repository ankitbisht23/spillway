// Shared screen states (UI-06, ADR-0060): loading, empty, error, Not
// allowed and Not found.
import { Link } from "@tanstack/react-router";
import { useEffect, useState, type ReactNode } from "react";

import type { ErrorView } from "../api/errors";

/** A skeleton, shown only after 200 ms so fast loads do not flicker. */
export function Loading({ label = "Loading" }: { label?: string }) {
  const [shown, setShown] = useState(false);
  useEffect(() => {
    const t = setTimeout(() => setShown(true), 200);
    return () => clearTimeout(t);
  }, []);
  return (
    <div className="state state-loading" role="status" aria-busy="true" aria-label={label}>
      {shown && (
        <>
          <div className="skeleton" />
          <div className="skeleton" />
          <div className="skeleton short" />
        </>
      )}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="state state-empty">{children}</div>;
}

export function ErrorState({ view, onRetry, retryLabel = "Retry" }: { view: ErrorView; onRetry?: () => void; retryLabel?: string }) {
  return (
    <div className="state state-error" role="alert">
      <h2>{view.title}</h2>
      <p>{view.message}</p>
      {view.requestId && <p className="muted">Request ID: <code>{view.requestId}</code></p>}
      {view.retry && onRetry && <button type="button" onClick={onRetry}>{retryLabel}</button>}
    </div>
  );
}

export function NotAllowed({ message = "Your roles do not allow this screen." }: { message?: string }) {
  return (
    <section className="full-state">
      <h1>Not allowed</h1>
      <p>{message}</p>
      <Link to="/">Go to the overview</Link>
    </section>
  );
}

export function NotFound({ message = "There is nothing at this address." }: { message?: string }) {
  return (
    <section className="full-state">
      <h1>Not found</h1>
      <p>{message}</p>
      <Link to="/">Go to the overview</Link>
    </section>
  );
}
