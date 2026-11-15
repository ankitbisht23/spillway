import type { UseQueryResult } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { ApiError, describeError } from "../api/errors";
import { Empty, ErrorState, Loading, NotAllowed, NotFound } from "./states";

interface Props<T> {
  query: Pick<UseQueryResult<T>, "status" | "data" | "error" | "refetch">;
  /** Whether the answer is empty; empty is then shown instead of children. */
  isEmpty?: (data: T) => boolean;
  empty?: ReactNode;
  /** The roles the query's operation needs, named on a 403. */
  needs?: readonly string[];
  children: (data: T) => ReactNode;
}

/** A query's loading, error and empty states, else its content (UI-06). */
export function QueryState<T>({ query, isEmpty, empty, needs, children }: Props<T>) {
  if (query.status === "pending") return <Loading />;
  if (query.status === "error") {
    const view = describeError(query.error, needs);
    if (query.error instanceof ApiError && query.error.status === 403) return <NotAllowed message={view.message} />;
    if (query.error instanceof ApiError && query.error.status === 404) return <NotFound message={view.message} />;
    return <ErrorState view={view} onRetry={() => void query.refetch()} />;
  }
  const data = query.data as T;
  if (isEmpty?.(data)) return <Empty>{empty ?? "Nothing here yet."}</Empty>;
  return <>{children(data)}</>;
}
