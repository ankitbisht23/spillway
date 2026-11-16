// Consumers & keys' server state (UI-05, UI-06, CP-15 – CP-17, RL-08,
// RL-09): tenants, consumers, plans, a consumer's keys and overrides, and
// access-list entries, each list a cursor page of 50 (D9). Every write is
// an identity change that applies at once (CP-20); nothing here touches
// the config working set.
//
// Key secrets (CP-15, ADR-0058): issue and rotate hand the key to the
// dialog's state through onSecret and resolve without it, so it never
// reaches the mutation cache, and gcTime 0 drops the mutation (variables
// and answer) as soon as its dialog unmounts. No key is ever in a query.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, call } from "../../api/client";
import type { components } from "../../api/schema.gen";

export type Tenant = components["schemas"]["Tenant"];
export type Consumer = components["schemas"]["Consumer"];
export type Plan = components["schemas"]["Plan"];
export type ApiKey = components["schemas"]["ApiKey"];
export type Override = components["schemas"]["Override"];
export type AccessListEntry = components["schemas"]["AccessListEntry"];

/** A new key as the dialogs see it: everything but the key itself. */
export type NewKey = Omit<components["schemas"]["IssuedApiKey"], "key">;

export const consumerKey = (id: number) => ["consumers", id] as const;
export const keysKey = (consumerId: number) => ["consumers", consumerId, "keys"] as const;
export const tenantKey = (id: number) => ["tenants", id] as const;
export const overrideKey = (id: number) => ["overrides", id] as const;
export const accessListKey = (id: number) => ["access-lists", id] as const;

export interface Page {
  cursor?: string;
}

const LIMIT = 50;

export function useConsumerList({ cursor, name, tenant_id }: Page & { name?: string; tenant_id?: number }) {
  return useQuery({
    queryKey: ["consumers", "list", cursor ?? "", name ?? "", tenant_id ?? 0],
    queryFn: async () => (await call(api.GET("/consumers", { params: { query: { limit: LIMIT, cursor, name, tenant_id } } }))).data,
  });
}

export function useConsumer(id: number) {
  return useQuery({ queryKey: consumerKey(id), queryFn: () => call(api.GET("/consumers/{id}", { params: { path: { id } } })) });
}

export function useTenantList({ cursor, name }: Page & { name?: string }) {
  return useQuery({
    queryKey: ["tenants", "list", cursor ?? "", name ?? ""],
    queryFn: async () => (await call(api.GET("/tenants", { params: { query: { limit: LIMIT, cursor, name } } }))).data,
  });
}

export function useTenant(id: number) {
  return useQuery({ queryKey: tenantKey(id), queryFn: () => call(api.GET("/tenants/{id}", { params: { path: { id } } })) });
}

/** Up to 500 tenants' IDs and names, for choices, filters and labels. */
export function useTenantOptions() {
  return useQuery({
    queryKey: ["tenants", "options"],
    queryFn: async () => (await call(api.GET("/tenants", { params: { query: { limit: 500 } } }))).data.items.map((t) => ({ id: t.id, name: t.name })),
  });
}

export function usePlanList({ cursor }: Page) {
  return useQuery({
    queryKey: ["plans", "list", cursor ?? ""],
    queryFn: async () => (await call(api.GET("/plans", { params: { query: { limit: LIMIT, cursor } } }))).data,
  });
}

export function useKeys(consumerId: number, cursor?: string) {
  return useQuery({
    queryKey: [...keysKey(consumerId), cursor ?? ""],
    queryFn: async () => (await call(api.GET("/consumers/{id}/keys", { params: { path: { id: consumerId }, query: { limit: LIMIT, cursor } } }))).data,
  });
}

export function useOverrides(consumerId: number, cursor?: string) {
  return useQuery({
    queryKey: ["overrides", "list", consumerId, cursor ?? ""],
    queryFn: async () => (await call(api.GET("/overrides", { params: { query: { limit: LIMIT, cursor, consumer_id: consumerId } } }))).data,
  });
}

export function useOverride(id: number) {
  return useQuery({ queryKey: overrideKey(id), queryFn: () => call(api.GET("/overrides/{id}", { params: { path: { id } } })) });
}

export type AccessAction = AccessListEntry["action"];
export type SubjectType = AccessListEntry["subject_type"];

export function useAccessLists({ cursor, action, subject_type }: Page & { action?: AccessAction; subject_type?: SubjectType }) {
  return useQuery({
    queryKey: ["access-lists", "list", action ?? "", subject_type ?? "", cursor ?? ""],
    queryFn: async () => (await call(api.GET("/access-lists", { params: { query: { limit: LIMIT, cursor, action, subject_type } } }))).data,
  });
}

export function useAccessList(id: number) {
  return useQuery({ queryKey: accessListKey(id), queryFn: () => call(api.GET("/access-lists/{id}", { params: { path: { id } } })) });
}

/** Issues a key for consumerId; the key goes to onSecret only (CP-15). */
export function useIssueKey(consumerId: number, onSecret: (key: string) => void) {
  const qc = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: async (body: components["schemas"]["KeyIssue"]): Promise<NewKey> => {
      const { key, ...issued } = (await call(api.POST("/consumers/{id}/keys", { params: { path: { id: consumerId } }, body }))).data;
      onSecret(key);
      return issued;
    },
    onSuccess: () => void qc.invalidateQueries({ queryKey: keysKey(consumerId) }),
  });
}

/** Rotates key (CP-16): the old key keeps working for the grace period; the new key goes to onSecret only. */
export function useRotateKey(key: ApiKey, onSecret: (key: string) => void) {
  const qc = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: async (body: components["schemas"]["KeyRotate"]): Promise<{ old: ApiKey; new: NewKey }> => {
      const r = (await call(api.POST("/keys/{id}/rotate", { params: { path: { id: key.id } }, body }))).data;
      const { key: secret, ...created } = r.new;
      onSecret(secret);
      return { old: r.old, new: created };
    },
    onSuccess: () => void qc.invalidateQueries({ queryKey: keysKey(key.consumer_id) }),
  });
}

/** Revokes key at once (CP-17). */
export function useRevokeKey(key: ApiKey) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: components["schemas"]["KeyRevoke"]) => (await call(api.POST("/keys/{id}/revoke", { params: { path: { id: key.id } }, body }))).data,
    onSuccess: () => void qc.invalidateQueries({ queryKey: keysKey(key.consumer_id) }),
  });
}
