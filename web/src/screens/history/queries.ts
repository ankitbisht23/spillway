// History's server state (UI-06, CP-02, CP-04, CP-13): published config
// versions, 50 a page by cursor (D9), one version with where it applied,
// the diff between two versions, the audit log, and rollback.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, call } from "../../api/client";
import type { components } from "../../api/schema.gen";

export type ConfigVersion = components["schemas"]["ConfigVersion"];
export type AuditRecord = components["schemas"]["AuditRecord"];

export function useVersionList(cursor: string | undefined) {
  return useQuery({
    queryKey: ["config", "versions", "list", cursor ?? ""],
    queryFn: async () => (await call(api.GET("/config/versions", { params: { query: { limit: 50, cursor } } }))).data,
  });
}

export function useVersion(v: number) {
  return useQuery({
    queryKey: ["config", "versions", v],
    queryFn: async () => (await call(api.GET("/config/versions/{v}", { params: { path: { v } } }))).data,
  });
}

/** The changes from version against (default v-1; 0 is the empty config) to version v. */
export function useVersionDiff(v: number, against: number | undefined, enabled = true) {
  return useQuery({
    queryKey: ["config", "versions", v, "diff", against ?? "previous"],
    queryFn: async () => (await call(api.GET("/config/versions/{v}/diff", { params: { path: { v }, query: { against } } }))).data,
    enabled,
  });
}

/** The audit log's exact-match filters (ADR-0036). */
export interface AuditFilters {
  entity?: string;
  entity_id?: string;
  actor_sub?: string;
  since?: string;
  until?: string;
}

export function useAuditList(cursor: string | undefined, filters: AuditFilters) {
  return useQuery({
    queryKey: ["audit", "list", cursor ?? "", filters],
    queryFn: async () => (await call(api.GET("/audit", { params: { query: { limit: 50, cursor, ...filters } } }))).data,
  });
}

/**
 * Rollback (CP-04): a new version copying version v, published with note
 * if the working set is still at revision, which it then becomes. Every
 * config list and entity is read again after it, since the working set
 * was reset.
 */
export function useRollback(v: number, onDone: (version: ConfigVersion) => void) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ revision, note }: { revision: number; note: string }) =>
      (await call(api.POST("/config/versions/{v}/rollback", { params: { path: { v }, header: { "If-Match": `"${revision}"` } }, body: { note } }))).data,
    onSuccess: (version) => {
      onDone(version);
      for (const key of ["config", "policies", "routes", "upstreams", "plans"]) void qc.invalidateQueries({ queryKey: [key] });
    },
  });
}
