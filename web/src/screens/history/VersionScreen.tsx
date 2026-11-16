// One published config version (UI-06, CP-02, CP-04): who published it,
// when and why, where it applied, its diff against another version (the
// one before it unless the reader picks one; 0 is the empty config), and
// Roll back to it (publisher, admin).
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";

import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { ChangesList } from "../../components/Changes";
import { DiffView } from "../../components/DiffView";
import { Published } from "../../components/Published";
import { QueryState } from "../../components/QueryState";
import { NotFound } from "../../components/states";
import { when } from "../consumers/common";
import { useVersion, useVersionDiff, type ConfigVersion } from "./queries";
import { RollbackDialog } from "./RollbackDialog";

const route = getRouteApi("/history/$version");

export function VersionScreen() {
  const { version: param } = route.useParams();
  const v = Number(param);
  if (!Number.isSafeInteger(v) || v < 1) return <NotFound message={`There is no config version ${param}.`} />;
  return <Version key={v} v={v} />;
}

/** How a version came to be: a publish, or a rollback that copies another. */
export function KindText({ version }: { version: Pick<ConfigVersion, "kind" | "rollback_of"> }) {
  if (version.kind === "rollback" && version.rollback_of !== null) {
    return <>rollback to <Link to="/history/$version" params={{ version: String(version.rollback_of) }}>version {version.rollback_of}</Link></>;
  }
  return <>{version.kind}</>;
}

function Version({ v }: { v: number }) {
  const { against } = route.useSearch();
  const navigate = useNavigate();
  const version = useVersion(v);
  const diff = useVersionDiff(v, against);
  const rollback = useCan("rollbackConfig");
  const [confirm, setConfirm] = useState(false);
  const [done, setDone] = useState<ConfigVersion | null>(null);
  const [compare, setCompare] = useState(against === undefined ? "" : String(against));

  const onCompare = (e: FormEvent) => {
    e.preventDefault();
    const n = Number(compare);
    void navigate({ to: "/history/$version", params: { version: String(v) }, search: compare.trim() === "" || !Number.isSafeInteger(n) || n < 0 ? {} : { against: n } });
  };

  return (
    <section>
      <div className="toolbar">
        <h1>Version {v}</h1>
        <Link to="/history" className="button-link">All versions</Link>
      </div>
      {done && <Published version={done} />}
      <QueryState query={version}>
        {(cv) => (
          <>
            <dl className="facts">
              <dt>Kind</dt><dd><KindText version={cv} /></dd>
              <dt>Published</dt><dd>{when(cv.published_at)}</dd>
              <dt>By</dt><dd><code>{cv.published_by}</code></dd>
              <dt>Note</dt><dd>{cv.note}</dd>
              <dt>Working-set revision</dt><dd>{cv.working_set_revision}</dd>
              <dt>Schema</dt><dd>{cv.schema_version.major}.{cv.schema_version.minor}</dd>
            </dl>
            <div className="actions">
              <ActionButton className="danger" disabledReason={rollback.reason} onClick={() => setConfirm(true)}>Roll back to version {v}</ActionButton>
            </div>
            <h2>Applied on</h2>
            {(cv.applied ?? []).length === 0 ? (
              <p className="muted">No replica has reported this version: static gateways do not report, and reports are kept for 7 days.</p>
            ) : (
              <table className="table" aria-label="Applied on">
                <thead><tr><th scope="col">Replica</th><th scope="col">First reported</th></tr></thead>
                <tbody>
                  {(cv.applied ?? []).map((a) => <tr key={a.replica}><th scope="row">{a.replica}</th><td>{when(a.at)}</td></tr>)}
                </tbody>
              </table>
            )}
            <h2>Changes</h2>
            <form className="filters" aria-label="Compare" onSubmit={onCompare}>
              <label>
                Compare with version{" "}
                <input type="number" min={0} step={1} value={compare} placeholder={String(v - 1)} onChange={(e) => setCompare(e.target.value)} />
              </label>
              <button type="submit" className="secondary">Compare</button>
              <span className="muted">Empty: the version before it. 0: the empty config, so the whole version.</span>
            </form>
            <QueryState query={diff}>
              {(d) => (
                <>
                  <p className="muted">From version {d.from_version} to version {d.to_version}.</p>
                  <ChangesList changes={d.changes} />
                  <DiffView diff={d.diff} label={`Diff from version ${d.from_version} to version ${d.to_version}`} />
                </>
              )}
            </QueryState>
          </>
        )}
      </QueryState>
      {confirm && (
        <RollbackDialog
          version={v}
          onClose={() => setConfirm(false)}
          onDone={(nv) => {
            setConfirm(false);
            setDone(nv);
          }}
        />
      )}
    </section>
  );
}
