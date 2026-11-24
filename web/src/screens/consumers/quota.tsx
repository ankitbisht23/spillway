// Quotas on Consumers & keys (RL-10, ADR-0070): a period's flushed usage
// against its effective quota, as the consumer list and a consumer show it.
import type { QuotaUsage } from "./queries";

export type QuotaPeriod = QuotaUsage["daily"];

const number = (n: number) => n.toLocaleString("en-GB");

/** "1,200 / 5,000", or "—" for no quota. */
export function quotaText(p: QuotaPeriod): string {
  return p.quota > 0 ? `${number(p.used)} / ${number(p.quota)}` : "—";
}

/** Whether a period's usage is at or over its quota. */
export const atQuota = (p: QuotaPeriod) => p.quota > 0 && p.used >= p.quota;

/** One period's usage, red at or over the quota. */
export function QuotaValue({ period, label }: { period: QuotaPeriod; label: string }) {
  const full = atQuota(period);
  return (
    <span className={full ? "quota-full" : undefined} aria-label={`${label}: ${quotaText(period)}${full ? ", at the quota" : ""}`}>
      {quotaText(period)}
    </span>
  );
}

/** "100,000 · 2,000,000" for a plan's or an override's quotas; "None" for none. */
export function quotasText(q: { daily?: number; monthly?: number } | undefined): string {
  const d = q?.daily ?? 0;
  const m = q?.monthly ?? 0;
  if (d <= 0 && m <= 0) return "None";
  return `${d > 0 ? number(d) : "—"} a day · ${m > 0 ? number(m) : "—"} a month`;
}

/** Usage is what the control plane's flusher last wrote (ADR-0070). */
export const QUOTA_NOTE = "Quota usage is what the control plane last flushed, up to 10 s behind the gateways, for the UTC day and month (RL-10).";
