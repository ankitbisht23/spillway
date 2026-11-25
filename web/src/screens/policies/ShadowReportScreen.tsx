// The shadow report (UI-01, RL-21, D8, ADR-0060): what a published policy
// would have denied, or denied, over a window, its top keys from the
// heavy-hitter windows, and how much of the window each source covers.
// Promote is an edit of the working set; publishing it is the publish
// page's, with its diff and note.
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { ApiError } from "../../api/errors";
import { useLatestConfig } from "../../api/latestConfig";
import { useCan } from "../../auth/SessionContext";
import { formatCount } from "../../charts/series";
import { ActionButton } from "../../components/ActionButton";
import { QueryState } from "../../components/QueryState";
import { Empty, NotFound } from "../../components/states";
import { LookupDialog, lookupBlocker } from "../live/actions";
import { Unpublished } from "../../components/Unpublished";
import { PromoteError } from "./PolicyEditor";
import { usePolicy, usePromote, useShadowReport, WINDOWS, type ShadowReport, type Window } from "./queries";

const route = getRouteApi("/policies/$id/shadow");

export function ShadowReportScreen() {
  const { id: raw } = route.useParams();
  const id = Number(raw);
  if (!Number.isSafeInteger(id) || id < 1) return <NotFound />;
  return <Report id={id} />;
}

function time(t: string): string {
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString("en-GB");
}

function percent(v: number | null): string {
  return v === null ? "–" : `${(v * 100).toFixed(v < 0.001 && v > 0 ? 3 : 1)} %`;
}

function count(v: number | null): string {
  return v === null ? "–" : formatCount(Math.round(v));
}

const METRICS_ERRORS: Record<NonNullable<ShadowReport["metrics_error"]>, string> = {
  prometheus_not_configured: "Prometheus is not configured on the control plane",
  prometheus_unavailable: "Prometheus is unavailable",
  query_failed: "the Prometheus query failed",
};

function Report({ id }: { id: number }) {
  const { window = "1h" } = route.useSearch();
  const navigate = useNavigate();
  const report = useShadowReport(id, window);
  return (
    <section>
      <div className="toolbar">
        <h1>Shadow report{report.data ? `: ${report.data.policy_name}` : ""}</h1>
        <label>
          Window{" "}
          <select value={window} onChange={(e) => void navigate({ to: "/policies/$id/shadow", params: { id: String(id) }, search: { window: e.target.value as Window } })}>
            {WINDOWS.map((w) => <option key={w.value} value={w.value}>{w.label}</option>)}
          </select>
        </label>
      </div>
      <p>
        <Link to="/policies/$id" params={{ id: String(id) }}>Open the policy</Link>
      </p>
      {report.error instanceof ApiError && report.error.status === 404 ? (
        <Empty>This policy is not in the latest published config: publish it first, then its report shows here.</Empty>
      ) : (
        <QueryState query={report}>
          {(r) => (
            <>
              <Counts r={r} />
              <Promote id={id} published={r.mode} />
              <Keys r={r} />
            </>
          )}
        </QueryState>
      )}
    </section>
  );
}

function Counts({ r }: { r: ShadowReport }) {
  const since = new Date(r.since).getTime();
  const unattributed = r.would_deny === null ? 0 : Math.round(r.would_deny) - r.keys_shadow_denied_total;
  return (
    <>
      <p className="muted">
        Version {r.config_version} runs this policy in {r.mode} mode. From {time(r.since)} to {time(r.until)}. Counts are by its published name,{" "}
        <code>{r.policy_name}</code>: requests counted under an earlier name are not included.
      </p>
      {/* O1, ADR-0071: the counts are by name, the keys by ID. */}
      <p className="muted">
        Would-deny counts come from Prometheus by policy name. A policy that reuses the name of a deleted policy shows that
        policy's counts until they leave the window. The top keys come from heavy-hitter windows by policy ID and are this
        policy's only.
      </p>
      {r.metrics_error && (
        <p className="banner banner-warn" role="alert">Counts unavailable: {METRICS_ERRORS[r.metrics_error]}. The keys below still answer.</p>
      )}
      <dl className="report" aria-label="Counts">
        <div><dt>Would deny (shadow)</dt><dd>{count(r.would_deny)}</dd></div>
        <div><dt>Share of its shadow decisions</dt><dd>{percent(r.share)}</dd></div>
        <div><dt>Shadow decisions</dt><dd>{count(r.evaluated)}</dd></div>
        <div><dt>Denied (enforce)</dt><dd>{count(r.denied)}</dd></div>
        <div><dt>Share of its enforce decisions</dt><dd>{percent(r.enforce_share)}</dd></div>
        <div><dt>Enforce decisions</dt><dd>{count(r.enforce_evaluated)}</dd></div>
      </dl>
      <ul className="notes" aria-label="Coverage">
        {r.metrics_since && new Date(r.metrics_since).getTime() > since && <li>Prometheus has data from {time(r.metrics_since)} only.</li>}
        {r.keys_since && new Date(r.keys_since).getTime() > since && <li>Key data from {time(r.keys_since)} only.</li>}
        {r.keys_since === null && <li>No heavy-hitter windows of this policy are kept (the last 7 days).</li>}
        {unattributed > 0 && <li>Top keys only: {formatCount(unattributed)} would-denies are not attributed to a listed key.</li>}
      </ul>
    </>
  );
}

/** Promote from the report: an edit of the working set (D8). */
function Promote({ id, published }: { id: number; published: string }) {
  const policy = usePolicy(id);
  const can = useCan("updatePolicy");
  const [promoted, setPromoted] = useState(false);
  const promote = usePromote(id, () => setPromoted(true));
  if (promoted) return <Unpublished>Promoted to enforce in the working set.</Unpublished>;
  if (published !== "shadow") return null;
  if (policy.isPending) return null;
  if (policy.isError || !policy.data) return <p className="muted">This policy is no longer in the working set, so it cannot be promoted.</p>;
  const ws = policy.data.data.mode || "enforce";
  if (ws !== "shadow") return <Unpublished>The working set already has it in {ws} mode.</Unpublished>;
  return (
    <div className="actions">
      <ActionButton disabledReason={can.reason} disabled={promote.isPending} onClick={() => promote.mutate(policy.data)}>Promote to enforce</ActionButton>
      <span className="muted">Sets mode to enforce in the working set; nothing changes on gateways until you review and publish.</span>
      {promote.isError && <PromoteError error={promote.error} reload={() => { promote.reset(); void policy.refetch(); }} />}
    </div>
  );
}

function Keys({ r }: { r: ShadowReport }) {
  const latest = useLatestConfig();
  const lookup = useCan("lookupHeavyHitters");
  const [who, setWho] = useState<string | null>(null);
  const policy = latest.data?.policies.find((p) => p.id === r.policy_id);
  return (
    <>
      <h2>Top keys</h2>
      {r.keys.length === 0 ? (
        <Empty>No key was throttled or would have been in this window.</Empty>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th scope="col">Key digest</th><th scope="col">Consumer</th><th scope="col">API key</th>
              <th scope="col">Would deny</th><th scope="col">Denied</th><th scope="col"><span className="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {r.keys.map((k) => (
              <tr key={k.key_digest}>
                <th scope="row"><code>{k.key_digest}</code></th>
                <td>{k.consumer_id === null ? "–" : <Link to="/consumers/$id" params={{ id: String(k.consumer_id) }} search={{}}>{k.consumer_id}</Link>}</td>
                <td>{k.api_key_id ?? "–"}</td>
                <td>{formatCount(k.shadow_denied)}</td>
                <td>{formatCount(k.denied)}</td>
                <td>
                  {lookup.allowed && (
                    <ActionButton className="link-button" disabledReason={lookupBlocker(policy)} aria-label={`Who is ${k.key_digest}?`} onClick={() => setWho(k.key_digest)}>
                      Who is this?
                    </ActionButton>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {r.keys.length > 0 && !lookup.allowed && <p className="muted">Who is this? (matching a digest to a subject) {lookup.reason?.toLowerCase()}.</p>}
      {who && policy && <LookupDialog policy={policy} digest={who} since={r.window} onClose={() => setWho(null)} />}
    </>
  );
}
