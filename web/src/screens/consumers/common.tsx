// Pieces the Consumers & keys and Tenants screens share (UI-06).
import { Link } from "@tanstack/react-router";

import { useLatestConfig } from "../../api/latestConfig";
import { planChoices, type Choice } from "./fields";
import { useTenantOptions } from "./queries";

/** The roles every identity write but tenants' needs (key-admin, admin). */
export const KEY_ADMIN = ["key-admin", "admin"] as const;

/** What an identity write does (CP-20), for confirmations and notices. */
export const APPLIES_AT_ONCE = "It applies at once: gateways get it with the next identity version, within seconds (CP-20).";

/** The screen's three lists. */
export function Tabs() {
  const tab = { activeOptions: { includeSearch: false }, activeProps: { "aria-current": "page" as const, className: "active" } };
  return (
    <nav className="tabs" aria-label="Consumers and keys">
      <Link to="/consumers" {...tab}>Consumers</Link>
      <Link to="/access-lists" {...tab}>Access lists</Link>
      <Link to="/plans" {...tab}>Plans</Link>
    </nav>
  );
}

/** An instant for people, in the browser's time zone; "–" for none. */
export function when(t: string | null | undefined): string {
  if (!t) return "–";
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString("en-GB");
}

/** Tenants and published plans as form choices, and their names by ID. */
export function useIdentityChoices() {
  const tenants = useTenantOptions();
  const latest = useLatestConfig();
  const plans = latest.data?.plans ?? [];
  const tenantChoices: Choice[] = (tenants.data ?? []).map((t) => ({ value: String(t.id), label: t.name }));
  return {
    tenants: tenantChoices,
    plans: planChoices(plans),
    policies: latest.data?.policies ?? [],
    tenantName: (id: number) => tenants.data?.find((t) => t.id === id)?.name ?? `Tenant ${id}`,
    planName: (id: number) => plans.find((p) => p.id === id)?.name ?? `Plan ${id}`,
  };
}

/** Why a select of published things may be short, when the latest config could not be read. */
export function LatestConfigNote() {
  const latest = useLatestConfig();
  if (latest.isError) return <p className="muted">The latest published config could not be read: plan and policy names show as IDs.</p>;
  if (latest.data === null) return <p className="muted">No config version is published yet: publish plans and policies before naming them here.</p>;
  return null;
}
