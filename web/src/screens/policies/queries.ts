// The Policies screens' server state (UI-01, UI-05): the working set's
// policies with their ETags, the shadow report, and promote, which is an
// edit of the working set and nothing more (D8, ADR-0060).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, call } from "../../api/client";
import type { components } from "../../api/schema.gen";
import type { Policy } from "./form";

export type ShadowReport = components["schemas"]["ShadowReport"];
export type Window = ShadowReport["window"];
export const WINDOWS: readonly { value: Window; label: string }[] = [
  { value: "15m", label: "Last 15 minutes" }, { value: "1h", label: "Last hour" },
  { value: "24h", label: "Last 24 hours" }, { value: "7d", label: "Last 7 days" },
];

/** One policy of the working set and its ETag. */
export interface Loaded {
  data: Policy;
  etag: string | null;
}

export const policyKey = (id: number) => ["policies", id] as const;

export function usePolicy(id: number) {
  return useQuery({
    queryKey: policyKey(id),
    queryFn: (): Promise<Loaded> => call(api.GET("/policies/{id}", { params: { path: { id } } })),
  });
}

export function usePolicyList(cursor: string | undefined) {
  return useQuery({
    queryKey: ["policies", "list", cursor ?? ""],
    queryFn: async () => (await call(api.GET("/policies", { params: { query: { limit: 50, cursor } } }))).data,
  });
}

export function useShadowReport(id: number, window: Window) {
  return useQuery({
    queryKey: ["policies", id, "shadow-report", window],
    queryFn: async () => (await call(api.GET("/policies/{id}/shadow-report", { params: { path: { id }, query: { window } } }))).data,
  });
}

/** A policy's writable fields: what the API answered without the read-only ones. */
export function writable(p: Policy): Policy {
  const readOnly = new Set(["id", "version", "created_at", "updated_at"]);
  return Object.fromEntries(Object.entries(p).filter(([k]) => !readOnly.has(k))) as unknown as Policy;
}

/**
 * Promote to enforce: the policy as loaded with mode enforce, under its
 * ETag, so an edit made since it was loaded is a 412, never overwritten.
 * It changes the working set only; publishing is the publish page's.
 */
export function usePromote(id: number, onPromoted: () => void) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ data, etag }: Loaded) =>
      call(api.PUT("/policies/{id}", { params: { path: { id }, header: { "If-Match": etag ?? "" } }, body: { ...writable(data), mode: "enforce" } })),
    onSuccess: (r) => {
      onPromoted();
      qc.setQueryData(policyKey(id), r);
      void qc.invalidateQueries({ queryKey: ["policies", "list"] });
    },
  });
}

/** Lists the policy's routes and methods, for tables. */
export function matchText(p: Policy): string {
  const routes = (p.match?.routes ?? []).join(", ") || "–";
  const methods = p.match?.methods?.length ? ` · ${p.match.methods.join(", ")}` : "";
  return routes + methods;
}

/** "100/1s burst 200; 10000/1h burst 500", or "from plan". */
export function limitsText(p: Policy): string {
  if (p.from_plan) return "from plan";
  return (p.limits ?? []).map((l) => `${l.rate ?? "?"}${l.period ? `/${l.period}` : " in flight"}${l.burst ? ` burst ${l.burst}` : ""}`).join("; ") || "–";
}
