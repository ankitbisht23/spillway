// Policies (UI-01, UI-06, D9): the working set's policies, 50 a page by
// cursor, each with level, mode, match and limits.
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";

import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { limitsText, matchText, usePolicyList } from "./queries";

const route = getRouteApi("/policies");

export function PolicyList() {
  const { cursor } = route.useSearch();
  const navigate = useNavigate();
  const list = usePolicyList(cursor);
  const create = useCan("createPolicy");
  return (
    <section>
      <div className="toolbar">
        <h1>Policies</h1>
        <ActionButton disabledReason={create.reason} onClick={() => void navigate({ to: "/policies/new" })}>New policy</ActionButton>
      </div>
      <p className="muted">The config working set: edits ship with the next publish.</p>
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !cursor}
        empty={<>No policies yet.{create.allowed ? " Create one with New policy." : ` ${create.reason ?? ""} to create one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Name</th><th scope="col">Level</th><th scope="col">Mode</th><th scope="col">Match</th>
                  <th scope="col">Key</th><th scope="col">Algorithm</th><th scope="col">Limits</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((p) => (
                  <tr key={p.id}>
                    <th scope="row"><Link to="/policies/$id" params={{ id: String(p.id) }}>{p.name}</Link></th>
                    <td>{p.level}</td>
                    <td><span className={`mode mode-${p.mode || "enforce"}`}>{p.mode || "enforce"}</span></td>
                    <td>{matchText(p)}</td>
                    <td><code>{p.key_template}</code></td>
                    <td>{p.algorithm}{p.local || p.algorithm === "concurrency" ? " (local)" : ""}</td>
                    <td>{limitsText(p)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/policies", search: { cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}
