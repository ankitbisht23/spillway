// The audit log (CP-13, UI-06, D9; admin only, ADR-0058): every change,
// newest first, 50 a page by cursor, filtered by the server's exact
// filters. Each record shows who, what, when, why and its before/after,
// which the server never fills with a key, a hash, a token or a password.
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";

import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { when } from "../consumers/common";
import { localInput } from "../routes/fields";
import { useAuditList, type AuditFilters, type AuditRecord } from "./queries";

const route = getRouteApi("/audit");

/** The filters as the search has them: text, and since and until as RFC 3339 instants. */
export function filtersOf(s: AuditFilters): AuditFilters {
  return { entity: s.entity, entity_id: s.entity_id, actor_sub: s.actor_sub, since: s.since, until: s.until };
}

/** A datetime-local value as an RFC 3339 instant; undefined when empty or not a date. */
export function instant(local: string): string | undefined {
  if (!local) return undefined;
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

export function Audit() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const filters = filtersOf(search);
  const list = useAuditList(search.cursor, filters);
  const filtered = Object.values(filters).some((v) => v !== undefined);
  return (
    <section>
      <h1>Audit log</h1>
      <p className="muted">Every change, newest first: config, identity, drains, key lifecycle and heavy-hitter lookups. Filters match exactly.</p>
      <AuditFilterForm
        key={JSON.stringify(filters)}
        filters={filters}
        onApply={(f) => void navigate({ to: "/audit", search: f })}
      />
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !search.cursor}
        empty={filtered ? "No audit record matches these filters." : "No change has been made yet."}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">When</th><th scope="col">Actor</th><th scope="col">Action</th><th scope="col">Entity</th>
                  <th scope="col">Note</th><th scope="col">Versions</th><th scope="col">Details</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((r) => <Row key={r.id} r={r} />)}
              </tbody>
            </table>
            <Pager cursor={search.cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/audit", search: { ...filters, cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}

function Row({ r }: { r: AuditRecord }) {
  const versionLink = r.entity === "config" && r.entity_id && /^\d+$/.test(r.entity_id);
  return (
    <tr>
      <th scope="row">{when(r.at)}</th>
      <td><span title={r.actor.sub}>{r.actor.name || r.actor.sub}</span><br /><span className="muted">{r.actor.roles.join(", ")}</span></td>
      <td><code>{r.action}</code></td>
      <td>
        {r.entity}
        {r.entity_id && (versionLink
          ? <> <Link to="/history/$version" params={{ version: r.entity_id }}>{r.entity_id}</Link></>
          : <> {r.entity_id}</>)}
      </td>
      <td>{r.note ?? "–"}</td>
      <td>
        {r.config_version !== null && <>config {r.config_version}</>}
        {r.config_version !== null && r.identity_version !== null && <br />}
        {r.identity_version !== null && <>identity {r.identity_version}</>}
        {r.config_version === null && r.identity_version === null && "–"}
      </td>
      <td>
        <details>
          <summary>Before and after</summary>
          <pre className="diff">{JSON.stringify(r.diff, null, 2)}</pre>
          <p className="muted">Request ID <code>{r.request_id}</code></p>
        </details>
      </td>
    </tr>
  );
}

function AuditFilterForm({ filters, onApply }: { filters: AuditFilters; onApply: (f: AuditFilters) => void }) {
  const [entity, setEntity] = useState(filters.entity ?? "");
  const [entityId, setEntityId] = useState(filters.entity_id ?? "");
  const [actor, setActor] = useState(filters.actor_sub ?? "");
  const [since, setSince] = useState(localInput(filters.since));
  const [until, setUntil] = useState(localInput(filters.until));
  const text = (v: string) => v.trim() || undefined;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    onApply({ entity: text(entity), entity_id: text(entityId), actor_sub: text(actor), since: instant(since), until: instant(until) });
  };
  return (
    <form className="filters" aria-label="Filters" onSubmit={submit}>
      <label>Entity <input value={entity} placeholder="route" onChange={(e) => setEntity(e.target.value)} /></label>
      <label>Entity ID <input value={entityId} onChange={(e) => setEntityId(e.target.value)} /></label>
      <label>Actor (sub) <input value={actor} onChange={(e) => setActor(e.target.value)} /></label>
      <label>Since <input type="datetime-local" value={since} onChange={(e) => setSince(e.target.value)} /></label>
      <label>Until <input type="datetime-local" value={until} onChange={(e) => setUntil(e.target.value)} /></label>
      <button type="submit">Apply</button>
      <button type="button" className="secondary" onClick={() => onApply({})}>Clear</button>
    </form>
  );
}
