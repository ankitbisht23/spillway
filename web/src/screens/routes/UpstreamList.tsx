// Upstreams (UI-06, D9): the working set's upstreams, 50 a page by cursor,
// each with its endpoints (draining ones counted) and its live health and
// circuit across the gateway replicas (ADR-0059).
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";

import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { newestAt, StreamStatusLine } from "../../components/LiveState";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { LiveCell, Tabs, useUpstreamLive } from "./common";
import { circuitText, healthText, summarize } from "./live";
import { useUpstreamList } from "./queries";

const api = getRouteApi("/upstreams");

export function UpstreamList() {
  const search = api.useSearch();
  const navigate = useNavigate();
  const list = useUpstreamList(search);
  const live = useUpstreamLive();
  const create = useCan("createUpstream");
  return (
    <section>
      <div className="toolbar">
        <h1>Routes &amp; upstreams</h1>
        <ActionButton disabledReason={create.reason} onClick={() => void navigate({ to: "/upstreams/new" })}>New upstream</ActionButton>
      </div>
      <Tabs />
      <div className="toolbar">
        <p className="muted">The config working set: edits and drains ship with the next publish. Health and circuits are live from the gateways.</p>
        <StreamStatusLine status={live.live.status} at={newestAt(live.live.subs)} />
      </div>
      {search.name && <p>Upstream named <code>{search.name}</code> · <Link to="/upstreams" search={{}}>All upstreams</Link></p>}
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !search.cursor}
        empty={search.name ? <>No upstream named {search.name}.</> : <>No upstreams yet.{create.allowed ? " Create one with New upstream." : ` ${create.reason ?? ""} to create one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Name</th><th scope="col">Endpoints</th><th scope="col">Load balancing</th>
                  <th scope="col">Health</th><th scope="col">Circuit</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((u) => {
                  const draining = u.endpoints.filter((e) => e.draining).length;
                  return (
                    <tr key={u.id}>
                      <th scope="row"><Link to="/upstreams/$id" params={{ id: String(u.id) }}>{u.name}</Link></th>
                      <td>{u.endpoints.length}{draining > 0 && `, ${draining} draining`}</td>
                      <td>{u.lb || "p2c"}</td>
                      <td><LiveCell live={live}>{(eps, cs) => healthText(summarize(u.name, eps, cs))}</LiveCell></td>
                      <td><LiveCell live={live}>{(eps, cs) => circuitText(summarize(u.name, eps, cs))}</LiveCell></td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            <Pager cursor={search.cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/upstreams", search: { ...search, cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}
