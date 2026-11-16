// Routes & upstreams' server state (UI-05, UI-06, CP-14): the working
// set's routes and upstreams with their ETags, drain, and undrain, which
// is a PUT of the upstream as loaded with the endpoint's draining unset.
// Every write changes the working set only; publishing is the publish
// page's (ADR-0060).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, call } from "../../api/client";
import { writable, type Route, type Upstream } from "./fields";

/** An entity of the working set and its ETag. */
export interface Loaded<T> {
  data: T;
  etag: string | null;
}

export const routeKey = (id: number) => ["routes", id] as const;
export const upstreamKey = (id: number) => ["upstreams", id] as const;

/** A list's page: 50 rows from cursor (D9), optionally one exact name (ADR-0036). */
export interface ListSearch {
  cursor?: string;
  name?: string;
}

export function useRouteList({ cursor, name }: ListSearch) {
  return useQuery({
    queryKey: ["routes", "list", cursor ?? "", name ?? ""],
    queryFn: async () => (await call(api.GET("/routes", { params: { query: { limit: 50, cursor, name } } }))).data,
  });
}

export function useUpstreamList({ cursor, name }: ListSearch) {
  return useQuery({
    queryKey: ["upstreams", "list", cursor ?? "", name ?? ""],
    queryFn: async () => (await call(api.GET("/upstreams", { params: { query: { limit: 50, cursor, name } } }))).data,
  });
}

/** Up to 500 upstream names with their IDs, for the route form's suggestions and links. */
export function useUpstreamNames() {
  return useQuery({
    queryKey: ["upstreams", "names"],
    queryFn: async () => (await call(api.GET("/upstreams", { params: { query: { limit: 500 } } }))).data.items.map((u) => ({ id: u.id, name: u.name })),
  });
}

export function useRoute(id: number) {
  return useQuery({
    queryKey: routeKey(id),
    queryFn: (): Promise<Loaded<Route>> => call(api.GET("/routes/{id}", { params: { path: { id } } })),
  });
}

export function useUpstream(id: number) {
  return useQuery({
    queryKey: upstreamKey(id),
    queryFn: (): Promise<Loaded<Upstream>> => call(api.GET("/upstreams/{id}", { params: { path: { id } } })),
  });
}

/** What drain and undrain act on: the upstream as loaded and one endpoint's ID. */
export interface EndpointChange {
  loaded: Loaded<Upstream>;
  endpointId: number;
}

/**
 * Drain (drainEndpoint) or undrain (updateUpstream, with the endpoint's
 * draining unset) one endpoint under the upstream's ETag, so a change made
 * since it was loaded is a 412, never overwritten (CP-14, ADR-0060).
 */
export function useEndpointChange(id: number, action: "drain" | "undrain", onDone: (address: string) => void) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ loaded, endpointId }: EndpointChange) => {
      const header = { "If-Match": loaded.etag ?? "" };
      if (action === "drain") return call(api.POST("/upstreams/{id}/endpoints/{eid}/drain", { params: { path: { id, eid: endpointId }, header } }));
      const body = writable(loaded.data) as unknown as Upstream;
      body.endpoints = loaded.data.endpoints.map((e) => (e.id === endpointId || !e.draining ? { address: e.address } : { address: e.address, draining: true }));
      return call(api.PUT("/upstreams/{id}", { params: { path: { id }, header }, body }));
    },
    onSuccess: (r, { loaded, endpointId }) => {
      qc.setQueryData(upstreamKey(id), r);
      void qc.invalidateQueries({ queryKey: ["upstreams", "list"] });
      onDone(loaded.data.endpoints.find((e) => e.id === endpointId)?.address ?? "");
    },
  });
}
