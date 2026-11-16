// Routes (UI-06, D9): the working set's routes, 50 a page by cursor, each
// with its match, upstream and that upstream's live health and circuit
// across the gateway replicas (ADR-0059).
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";

import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { newestAt, StreamStatusLine } from "../../components/LiveState";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { LiveCell, Tabs, useUpstreamLive } from "./common";
import type { Route } from "./fields";
import { circuitText, healthText, summarize } from "./live";
import { useRouteList } from "./queries";

const api = getRouteApi("/routes");

/** "GET, POST api.example.com /v1/orders*", from the route's match. */
export function matchText(r: Route): string {
  const m = r.match;
  const path = m.path ?? (m.path_prefix !== undefined ? `${m.path_prefix}*` : "");
  const headers = m.headers?.length ? ` + ${m.headers.length} header ${m.headers.length === 1 ? "predicate" : "predicates"}` : "";
  return [m.methods?.join(", "), m.host, path].filter(Boolean).join(" ") + headers || "–";
}

export function RouteList() {
  const search = api.useSearch();
  const navigate = useNavigate();
  const list = useRouteList(search);
  const live = useUpstreamLive();
  const create = useCan("createRoute");
  return (
    <section>
      <div className="toolbar">
        <h1>Routes &amp; upstreams</h1>
        <ActionButton disabledReason={create.reason} onClick={() => void navigate({ to: "/routes/new" })}>New route</ActionButton>
      </div>
      <Tabs />
      <div className="toolbar">
        <p className="muted">The config working set: edits ship with the next publish. Health and circuits are live from the gateways.</p>
        <StreamStatusLine status={live.live.status} at={newestAt(live.live.subs)} />
      </div>
      {search.name && <p>Route named <code>{search.name}</code> · <Link to="/routes" search={{}}>All routes</Link></p>}
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !search.cursor}
        empty={search.name ? <>No route named {search.name}.</> : <>No routes yet.{create.allowed ? " Create one with New route." : ` ${create.reason ?? ""} to create one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Name</th><th scope="col">Match</th><th scope="col">Upstream</th><th scope="col">Upstream health</th>
                  <th scope="col">Circuit</th><th scope="col">Auth</th><th scope="col">Priority</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((r) => (
                  <tr key={r.id}>
                    <th scope="row"><Link to="/routes/$id" params={{ id: String(r.id) }}>{r.name}</Link></th>
                    <td><code>{matchText(r)}</code></td>
                    <td><Link to="/upstreams" search={{ name: r.upstream }}>{r.upstream}</Link></td>
                    <td><LiveCell live={live}>{(eps, cs) => healthText(summarize(r.upstream, eps, cs))}</LiveCell></td>
                    <td><LiveCell live={live}>{(eps, cs) => circuitText(summarize(r.upstream, eps, cs))}</LiveCell></td>
                    <td>{r.auth_mode || "none"}</td>
                    <td>{r.priority ?? 0}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={search.cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/routes", search: { ...search, cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}
