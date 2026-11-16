// A new config version, from a publish or a rollback (UI-02, CP-04,
// ADR-0060): its number with a link to History, its note, and on how many
// live replicas it applied, checked every second for 30 s.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import { api, call } from "../api/client";
import type { components } from "../api/schema.gen";

type Version = components["schemas"]["ConfigVersion"];

/** How long and how often the banner checks where a new version applied (ADR-0060). */
const APPLIED_POLLS = 30;
const APPLIED_EVERY_MS = 1000;

export function Published({ version }: { version: Version }) {
  const qc = useQueryClient();
  const key = ["config", "versions", version.version, "applied"] as const;
  const replicas = useQuery({
    queryKey: ["replicas", "live"],
    queryFn: async () => (await call(api.GET("/replicas", { params: { query: { limit: 500 } } }))).data.items.filter((r) => r.live).map((r) => r.id),
  });
  const live = replicas.data ?? [];
  const applied = useQuery({
    queryKey: key,
    queryFn: async () => (await call(api.GET("/config/versions/{v}", { params: { path: { v: version.version } } }))).data.applied ?? [],
    enabled: live.length > 0,
    gcTime: 0,
    refetchInterval: (q) => {
      const done = live.every((r) => (q.state.data ?? []).some((a) => a.replica === r));
      return done || q.state.dataUpdateCount >= APPLIED_POLLS ? false : APPLIED_EVERY_MS;
    },
  });
  const n = live.filter((r) => (applied.data ?? []).some((a) => a.replica === r)).length;
  const gaveUp = (qc.getQueryState(key)?.dataUpdateCount ?? 0) >= APPLIED_POLLS;
  const history = <Link to="/history/$version" params={{ version: String(version.version) }}>see it in History</Link>;
  return (
    <div className="banner" role="status">
      <p>
        {version.kind === "rollback" && version.rollback_of !== null
          ? <>Published version {version.version}, a copy of version {version.rollback_of}: {history}.</>
          : <>Published version {version.version}: {history}.</>}
      </p>
      <p className="muted">Note: {version.note}</p>
      {replicas.isSuccess && (
        <p>
          {live.length === 0
            ? "No live replica reports applied versions (static gateways do not)."
            : `Applied on ${n} of ${live.length} live replicas${n < live.length && gaveUp ? " after 30 s" : ""}.`}
        </p>
      )}
    </div>
  );
}
