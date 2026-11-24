// Plans (UI-06, RL-08): the config working set's plans, 50 a page, each
// with its default limits per policy. Plans are config: they change with
// the working set and reach gateways with a publish, and tenants and
// consumers may name only plans of the latest published config. This
// screen reads them; editing them stays with the API and config import.
import { getRouteApi, useNavigate } from "@tanstack/react-router";

import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { Tabs, when } from "./common";
import { usePlanList, type Plan } from "./queries";
import { quotasText } from "./quota";

const route = getRouteApi("/plans");

/** "per-client: 100/1s; per-day: 10000/24h". */
export function defaultLimitsText(p: Plan): string {
  const entries = p.default_limits ?? [];
  if (entries.length === 0) return "None: the policies' own limits apply";
  return entries
    .map((e) => `${e.policy ?? "?"}: ${(e.limits ?? []).map((l) => `${l.rate ?? "?"}/${l.period ?? "?"}${l.burst ? ` burst ${l.burst}` : ""}`).join(", ")}`)
    .join("; ");
}

export function PlanList() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const list = usePlanList(search);
  return (
    <section>
      <div className="toolbar">
        <h1>Consumers &amp; keys</h1>
      </div>
      <Tabs />
      <p className="muted">
        Plans are config: they change with the working set and reach gateways with a publish. A policy with <code>from_plan</code> takes its
        limits from the consumer's plan (RL-08). Quotas are cost units per UTC day and month, counted per consumer (RL-10).
      </p>
      <QueryState query={list} isEmpty={(d) => d.items.length === 0 && !search.cursor} empty="No plans in the working set.">
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr><th scope="col">Name</th><th scope="col">Default limits by policy</th><th scope="col">Quotas</th><th scope="col">Updated</th></tr>
              </thead>
              <tbody>
                {d.items.map((p) => (
                  <tr key={p.id}>
                    <th scope="row">{p.name}</th>
                    <td>{defaultLimitsText(p)}</td>
                    <td>{quotasText(p.quotas)}</td>
                    <td>{when(p.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={search.cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/plans", search: { cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}
