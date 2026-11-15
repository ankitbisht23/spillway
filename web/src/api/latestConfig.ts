// The latest published config version's routes and policies: what the
// gateways run, so what the live filters may name (ADR-0059: a route
// filter must be a route of the latest published config) and how a
// policy ID in a heavy-hitter window gets its name (UI-03, RL-21).
import { useQuery } from "@tanstack/react-query";

import { api, call } from "./client";

export interface PublishedPolicy {
  id: number;
  name: string;
  mode: string;
  keyTemplate: string;
}

export interface LatestConfig {
  version: number;
  routes: string[];
  policies: PublishedPolicy[];
}

/** The snapshot's routes and policies; anything not of the expected shape is skipped. */
export function parseSnapshot(version: number, snapshot: Record<string, unknown> | undefined): LatestConfig {
  const list = (k: string): Record<string, unknown>[] => {
    const v = snapshot?.[k];
    return Array.isArray(v) ? v.filter((x): x is Record<string, unknown> => typeof x === "object" && x !== null) : [];
  };
  const routes = list("routes").map((r) => r.name).filter((n): n is string => typeof n === "string").sort();
  const policies = list("policies").flatMap((p) =>
    typeof p.id === "number" && typeof p.name === "string"
      ? [{ id: p.id, name: p.name, mode: typeof p.mode === "string" && p.mode ? p.mode : "enforce", keyTemplate: typeof p.key_template === "string" ? p.key_template : "" }]
      : [],
  );
  policies.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return { version, routes, policies };
}

/** The latest published config, or null before the first publish. */
export function useLatestConfig() {
  return useQuery({
    queryKey: ["config", "latest"],
    queryFn: async (): Promise<LatestConfig | null> => {
      const page = await call(api.GET("/config/versions", { params: { query: { limit: 1 } } }));
      const newest = page.data.items[0];
      if (!newest) return null;
      const one = await call(api.GET("/config/versions/{v}", { params: { path: { v: newest.version } } }));
      return parseSnapshot(one.data.version, one.data.snapshot);
    },
  });
}

/** Up to 500 tenant names, for the tenant filter. */
export function useTenantNames() {
  return useQuery({
    queryKey: ["tenants", "names"],
    queryFn: async () => (await call(api.GET("/tenants", { params: { query: { limit: 500 } } }))).data.items.map((t) => t.name).sort(),
  });
}
