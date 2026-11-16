// History (UI-06, CP-02, CP-04, D9): published config versions, newest
// first, 50 a page by cursor; each opens its page with its diff and
// Roll back. Admins also have the audit log.
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";

import { useCan } from "../../auth/SessionContext";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { when } from "../consumers/common";
import { useVersionList } from "./queries";
import { KindText } from "./VersionScreen";

const route = getRouteApi("/history");

export function HistoryList() {
  const { cursor } = route.useSearch();
  const navigate = useNavigate();
  const list = useVersionList(cursor);
  const audit = useCan("listAudit");
  return (
    <section>
      <h1>History</h1>
      <p className="muted">
        Published config versions, newest first. Open one for its diff and to roll back to it.
        {audit.allowed && <> Every change, identity included, is in the <Link to="/audit">audit log</Link>.</>}
      </p>
      <QueryState query={list} isEmpty={(d) => d.items.length === 0 && !cursor} empty="No config version is published yet.">
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Version</th><th scope="col">Kind</th><th scope="col">Published</th>
                  <th scope="col">By</th><th scope="col">Note</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((v) => (
                  <tr key={v.version}>
                    <th scope="row"><Link to="/history/$version" params={{ version: String(v.version) }}>Version {v.version}</Link></th>
                    <td><KindText version={v} /></td>
                    <td>{when(v.published_at)}</td>
                    <td><code>{v.published_by}</code></td>
                    <td>{v.note}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/history", search: { cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}
