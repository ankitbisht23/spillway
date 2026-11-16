// The identity forms (UI-06, RL-08, RL-09, CP-20): field lists over the
// API's entities, in the routes task's form pattern (ADR-0064). Choices
// that must name something of the latest published config (a tenant's or
// consumer's plan, an override's policy) come from that config.
import type { PublishedPlan, PublishedPolicy } from "../../api/latestConfig";
import type { FieldSpec } from "../routes/fields";

export interface Choice {
  value: string;
  label: string;
}

const STATUS: FieldSpec = { path: "status", label: "Status", kind: "select", options: ["active", "suspended"], emptyLabel: "Default (active)", hint: "A suspended one's requests are refused with 403 (ADR-0041)." };

export const planChoices = (plans: readonly PublishedPlan[]): Choice[] => plans.map((p) => ({ value: String(p.id), label: p.name }));

export function tenantFields(plans: readonly Choice[]): FieldSpec[] {
  return [
    { path: "name", label: "Name", kind: "text", required: true },
    { path: "plan_id", label: "Plan", kind: "int", required: true, choices: plans, emptyLabel: "Choose a plan", hint: "A plan of the latest published config." },
    STATUS,
  ];
}

export function consumerFields(tenants: readonly Choice[], plans: readonly Choice[]): FieldSpec[] {
  return [
    { path: "name", label: "Name", kind: "text", required: true, hint: "Unique within its tenant." },
    { path: "tenant_id", label: "Tenant", kind: "int", required: true, choices: tenants, emptyLabel: "Choose a tenant" },
    { path: "plan_id", label: "Plan", kind: "int", choices: plans, emptyLabel: "The tenant's plan", hint: "A plan of the latest published config." },
    STATUS,
    { path: "jwt_subject", label: "JWT subject", kind: "text", hint: "The sub of bearer tokens that act as this consumer (GW-19); empty for none." },
  ];
}

/**
 * Whether policy takes overrides (RL-08, ADR-0041): consumer or route
 * level, keyed per consumer ({consumer} or {api_key}).
 */
export function takesOverrides(p: PublishedPolicy): boolean {
  return (p.level === "consumer" || p.level === "route") && /\{(consumer|api_key)\}/.test(p.keyTemplate);
}

export function overrideFields(policies: readonly PublishedPolicy[]): FieldSpec[] {
  return [
    {
      path: "policy_id", label: "Policy", kind: "int", required: true, emptyLabel: "Choose a policy",
      choices: policies.filter(takesOverrides).map((p) => ({ value: String(p.id), label: p.name })),
      hint: "A per-consumer policy of the latest published config: consumer or route level, keyed by {consumer} or {api_key}.",
    },
    { path: "limits", label: "Limits", kind: "limits", required: true, hint: "One a line: rate/period, such as 1000/1m, or 100/1s burst 200 for GCRA." },
    { path: "starts_at", label: "Starts", kind: "datetime", hint: "Your local time; empty: at once." },
    { path: "ends_at", label: "Ends", kind: "datetime", hint: "Set for a time-boxed burst grant (RL-08); empty: no end." },
    { path: "note", label: "Note", kind: "text" },
  ];
}

export const ACCESS_LIST_FIELDS: readonly FieldSpec[] = [
  { path: "action", label: "Action", kind: "select", required: true, options: ["allow", "deny"], emptyLabel: "Choose allow or deny" },
  { path: "subject_type", label: "Subject type", kind: "select", required: true, options: ["key", "consumer", "cidr"], emptyLabel: "Choose key, consumer or CIDR" },
  { path: "api_key_id", label: "API key ID", kind: "int", hint: "For subject type key only." },
  { path: "consumer_id", label: "Consumer ID", kind: "int", hint: "For subject type consumer only." },
  { path: "cidr", label: "CIDR", kind: "text", hint: "For subject type cidr only, such as 203.0.113.0/24; checked before authentication." },
  { path: "note", label: "Note", kind: "text" },
];
