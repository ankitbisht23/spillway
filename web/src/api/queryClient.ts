// TanStack Query defaults (UI-05, ADR-0060): queries retry only transient
// errors, twice; nothing refetches on focus, which would surprise an edit;
// mutations never retry, so a publish or a revoke never runs twice.
import { QueryClient } from "@tanstack/react-query";

import { retryDelay, retryQuery } from "./errors";

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: retryQuery, retryDelay, staleTime: 10_000, refetchOnWindowFocus: false },
      mutations: { retry: false },
    },
  });
}
