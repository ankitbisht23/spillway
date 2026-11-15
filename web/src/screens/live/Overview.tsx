// Overview (UI-03, OB-01, ADR-0060): request rate and status mix, the
// rate-limited (429) share, p50 and p99 latency, and the top routes and
// tenants, live from the stream's named queries every 5 s, filtered by
// route, tenant and range.
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";

import { useLatestConfig, useTenantNames } from "../../api/latestConfig";
import { LineChart } from "../../charts/LineChart";
import { align, byLabel, formatMs, formatNumber, formatShare, isEmptyAligned, latest, type Aligned } from "../../charts/series";
import { LiveState, newestAt, StreamStatusLine } from "../../components/LiveState";
import type { StreamQueryName } from "../../stream/client";
import { asMatrix, asVector } from "../../stream/data";
import { useSubscription } from "../../stream/useSubscription";
import { Card, RangeSelect, StreamFailed } from "./common";
import { overviewParams, type OverviewSearch } from "./search";

const SUBS: readonly StreamQueryName[] = ["overview.requests", "overview.ratelimited", "overview.latency", "overview.routes", "overview.tenants"];

const route = getRouteApi("/overview");

const CODE_CLASSES = ["2xx", "3xx", "4xx", "5xx"];
/** Palette indexes: 2xx green, 3xx blue, 4xx amber, 5xx red, anything else grey. */
const CODE_TONES: Record<string, number> = { "2xx": 1, "3xx": 0, "4xx": 2, "5xx": 3 };

function byCodeClass(a: string, b: string): number {
  const ia = CODE_CLASSES.indexOf(a);
  const ib = CODE_CLASSES.indexOf(b);
  return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib) || (a < b ? -1 : a > b ? 1 : 0);
}

const requestsOf = (d: unknown): Aligned | null => {
  const m = asMatrix(d);
  return m && align(m, (s) => s.labels.code_class ?? "other", 1, byCodeClass);
};
const ratelimitedOf = (d: unknown): Aligned | null => {
  const m = asMatrix(d);
  return m && align(m, () => "429 share");
};
const latencyOf = (d: unknown): Aligned | null => {
  const m = asMatrix(d);
  return m && align(m, (s) => (s.labels.quantile === "0.5" ? "p50" : s.labels.quantile === "0.99" ? "p99" : `q${s.labels.quantile ?? ""}`), 1000);
};

export function Overview() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const latestConfig = useLatestConfig();
  const tenants = useTenantNames();
  const live = useSubscription(SUBS, overviewParams(search));
  const set = (patch: Partial<OverviewSearch>) =>
    void navigate({ to: "/overview", search: (prev) => ({ ...prev, ...patch }) });

  const routes = latestConfig.data?.routes ?? [];
  const routeOptions = search.route && !routes.includes(search.route) ? [...routes, search.route] : routes;
  const tenantNames = tenants.data ?? [];
  const tenantOptions = search.tenant && !["none", "other", ...tenantNames].includes(search.tenant) ? [...tenantNames, search.tenant] : tenantNames;
  const stale = live.status.state !== "open";
  const subs = live.subs;

  return (
    <section className="live-screen">
      <h1>Overview</h1>
      <div className="toolbar">
        <form className="filters" aria-label="Filters" onSubmit={(e) => e.preventDefault()}>
          <label>
            Route{" "}
            <select value={search.route ?? "*"} onChange={(e) => set({ route: e.target.value === "*" ? undefined : e.target.value })}>
              <option value="*">All routes</option>
              {routeOptions.map((r) => <option key={r} value={r}>{r}</option>)}
            </select>
          </label>
          <label>
            Tenant{" "}
            <select value={search.tenant ?? "*"} onChange={(e) => set({ tenant: e.target.value === "*" ? undefined : e.target.value })}>
              <option value="*">All tenants</option>
              <option value="none">No tenant</option>
              <option value="other">Other tenants (outside the top 50)</option>
              {tenantOptions.map((t) => <option key={t} value={t}>{t}</option>)}
            </select>
          </label>
          <RangeSelect value={search.range ?? "15m"} onChange={(r) => set({ range: r === "15m" ? undefined : r })} />
        </form>
        <StreamStatusLine status={live.status} at={newestAt(subs)} />
      </div>
      {latestConfig.isError && <p className="muted">The published routes could not be read; the route filter offers only All routes.</p>}
      {live.status.state === "failed" ? (
        <StreamFailed error={live.status.error} reset={<Link to="/overview" search={{}}>Reset the filters</Link>} />
      ) : (
        <div className="cards">
          <Card title="Requests per second">
            <LiveState title="requests" sub={subs["overview.requests"]} stale={stale} select={requestsOf} isEmpty={isEmptyAligned} empty="No requests in this range.">
              {(d) => <LineChart title="Requests per second by status class" data={d} unit="rps" tone={(l) => CODE_TONES[l] ?? 6} />}
            </LiveState>
          </Card>
          <Card title="Status mix">
            <LiveState title="status mix" sub={subs["overview.requests"]} stale={stale} select={requestsOf} isEmpty={isEmptyAligned} empty="No requests in this range.">
              {(d) => <StatusMix data={d} />}
            </LiveState>
          </Card>
          <Card title="Rate-limited (429) share">
            <LiveState title="429 share" sub={subs["overview.ratelimited"]} stale={stale} select={ratelimitedOf} isEmpty={isEmptyAligned} empty="No requests in this range.">
              {(d) => <LineChart title="Share of requests rate-limited" data={d} unit="share" tone={() => 2} />}
            </LiveState>
          </Card>
          <Card title="Latency">
            <LiveState title="latency" sub={subs["overview.latency"]} stale={stale} select={latencyOf} isEmpty={isEmptyAligned} empty="No requests in this range.">
              {(d) => <LineChart title="Request latency p50 and p99" data={d} unit="ms" />}
            </LiveState>
          </Card>
          <Card title="Top routes" wide>
            <LiveState title="routes" sub={subs["overview.routes"]} stale={stale} select={asVector} isEmpty={(v) => v.series.length === 0} empty="No requests in this range.">
              {(v) => <TopTable by="route" label="Route" rows={byLabel(v, "route")} filterable={(n) => routes.includes(n)} />}
            </LiveState>
          </Card>
          <Card title="Top tenants" wide>
            <LiveState title="tenants" sub={subs["overview.tenants"]} stale={stale} select={asVector} isEmpty={(v) => v.series.length === 0} empty="No requests in this range.">
              {(v) => <TopTable by="tenant" label="Tenant" rows={byLabel(v, "tenant")} filterable={(n) => n === "none" || n === "other" || tenantNames.includes(n)} />}
            </LiveState>
          </Card>
        </div>
      )}
    </section>
  );
}

/** The latest request rate per status class and its share of all. */
function StatusMix({ data }: { data: Aligned }) {
  const rows = data.lines.map((l) => ({ label: l.label, rps: latest(l.values) ?? 0 }));
  const total = rows.reduce((s, r) => s + r.rps, 0);
  return (
    <table className="table">
      <thead>
        <tr><th scope="col">Status</th><th scope="col">Requests/s</th><th scope="col">Share</th></tr>
      </thead>
      <tbody>
        {rows.map((r) => {
          const share = total > 0 ? r.rps / total : 0;
          return (
            <tr key={r.label}>
              <th scope="row">{r.label}</th>
              <td>{formatNumber(r.rps)}</td>
              <td><meter min={0} max={1} value={share} aria-label={`${r.label} share`} /> {formatShare(share)}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

interface TopProps {
  by: "route" | "tenant";
  label: string;
  rows: { name: string; stats: Record<string, number | null> }[];
  /** Whether a row's name is a valid filter value: the name then filters the screen. */
  filterable: (name: string) => boolean;
}

/** overview.routes or .tenants: the top 10 by request rate over the range. */
function TopTable({ by, label, rows, filterable }: TopProps) {
  return (
    <table className="table">
      <thead>
        <tr>
          <th scope="col">{label}</th><th scope="col">Requests/s</th><th scope="col">429/s</th><th scope="col">429 share</th><th scope="col">p99</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(({ name, stats }) => {
          const rps = stats.rps ?? null;
          const limited = stats.ratelimited_rps ?? null;
          return (
            <tr key={name}>
              <th scope="row">
                {filterable(name)
                  ? <Link to="/overview" search={(prev) => ({ ...prev, [by]: name })} title={`Show only ${name}`}>{name}</Link>
                  : name}
              </th>
              <td>{formatNumber(rps)}</td>
              <td>{formatNumber(limited ?? 0)}</td>
              <td>{formatShare(rps ? (limited ?? 0) / rps : null)}</td>
              <td>{formatMs(stats.p99_s)}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}
