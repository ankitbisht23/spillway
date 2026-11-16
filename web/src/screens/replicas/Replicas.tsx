// Replicas (UI-06, CP-08, RL-24, ADR-0059): the gateway replicas from
// their heartbeats, live over the stream every 5 s: the config and
// identity versions each runs, its staleness (seconds since it last
// synced, on Postgres's clock, not the browser's), its last heartbeat,
// whether it is live and the last config version it refused. View only.
import { Link } from "@tanstack/react-router";

import { useLatestConfig } from "../../api/latestConfig";
import { LiveState, newestAt, StreamStatusLine } from "../../components/LiveState";
import type { StreamQueryName } from "../../stream/client";
import { asReplicas, type StreamReplica } from "../../stream/data";
import { useSubscription } from "../../stream/useSubscription";
import { when } from "../consumers/common";
import { StreamFailed } from "../live/common";

const SUBS: readonly StreamQueryName[] = ["replicas"];

/** Seconds as people read them: "4.2 s", "3 min 5 s", "2 h 10 min"; "never synced" for none. */
export function staleness(s: number | null): string {
  if (s === null) return "never synced";
  if (s < 60) return `${s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ${Math.floor(s % 60)} s`;
  return `${Math.floor(m / 60)} h ${m % 60} min`;
}

export function Replicas() {
  const live = useSubscription(SUBS);
  const latest = useLatestConfig();
  const stale = live.status.state !== "open";
  return (
    <section>
      <div className="toolbar">
        <h1>Replicas</h1>
        <StreamStatusLine status={live.status} at={newestAt(live.subs)} />
      </div>
      <p className="muted">
        Gateway replicas from their heartbeats (every 5 s); live means a heartbeat in the last 15 s. Staleness is the time since a replica last synced
        with the control plane.
        {latest.data && <> The latest config version is <Link to="/history/$version" params={{ version: String(latest.data.version) }}>{latest.data.version}</Link>.</>}
      </p>
      {live.status.state === "failed" ? (
        <StreamFailed error={live.status.error} reset={null} />
      ) : (
        <LiveState
          title="replicas"
          sub={live.subs.replicas}
          stale={stale}
          select={asReplicas}
          isEmpty={(items) => items.length === 0}
          empty="No gateway replica has sent a heartbeat in the last 7 days. Gateways with static config (make dev's default) do not report."
        >
          {(items) => <ReplicaTable items={items} latest={latest.data?.version} />}
        </LiveState>
      )}
    </section>
  );
}

function ReplicaTable({ items, latest }: { items: StreamReplica[]; latest: number | undefined }) {
  return (
    <table className="table">
      <thead>
        <tr>
          <th scope="col">Replica</th><th scope="col">State</th><th scope="col">Config version</th><th scope="col">Identity version</th>
          <th scope="col">Staleness</th><th scope="col">Last heartbeat</th><th scope="col">Refused</th>
        </tr>
      </thead>
      <tbody>
        {items.map(({ replica: r, staleness_s }) => (
          <tr key={r.id}>
            <th scope="row">{r.id}</th>
            <td>
              {r.live ? "live" : "down"}
              {r.live && !r.watch_connected && <span className="muted"> · not watching</span>}
            </td>
            <td>
              {r.config_version === 0 ? "none" : <Link to="/history/$version" params={{ version: String(r.config_version) }}>{r.config_version}</Link>}
              {latest !== undefined && r.config_version < latest && <span className="muted"> · behind {latest}</span>}
            </td>
            <td>{r.identity_version === 0 ? "none" : r.identity_version}</td>
            <td>{staleness(staleness_s)}</td>
            <td>{when(r.last_heartbeat_at)}</td>
            <td>
              {r.rejected_config_version === null && r.rejected_reason === null ? "–" : (
                <>
                  {r.rejected_config_version !== null && <>version {r.rejected_config_version}: </>}
                  {r.rejected_reason}
                </>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
