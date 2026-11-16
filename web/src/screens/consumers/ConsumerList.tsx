// Consumers (UI-06, D9): 50 a page by cursor, filtered by tenant or an
// exact name (ADR-0036), each with its tenant, plan and status. Quota
// usage arrives with quotas in M7 (RL-10).
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { Tabs, useIdentityChoices } from "./common";
import { useConsumerList } from "./queries";

const route = getRouteApi("/consumers");

export function ConsumerList() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const list = useConsumerList(search);
  const choices = useIdentityChoices();
  const create = useCan("createConsumer");
  const [name, setName] = useState(search.name ?? "");
  const filter = (patch: { name?: string; tenant_id?: number }) =>
    void navigate({ to: "/consumers", search: { ...search, ...patch, cursor: undefined } });
  return (
    <section>
      <div className="toolbar">
        <h1>Consumers &amp; keys</h1>
        <ActionButton disabledReason={create.reason} onClick={() => void navigate({ to: "/consumers/new" })}>New consumer</ActionButton>
      </div>
      <Tabs />
      <div className="toolbar">
        <form className="filters" aria-label="Filters" onSubmit={(e) => {
          e.preventDefault();
          filter({ name: name.trim() || undefined });
        }}>
          <label>
            Tenant{" "}
            <select value={search.tenant_id ?? ""} onChange={(e) => filter({ tenant_id: e.target.value ? Number(e.target.value) : undefined })}>
              <option value="">Every tenant</option>
              {search.tenant_id !== undefined && !choices.tenants.some((t) => t.value === String(search.tenant_id)) && (
                <option value={search.tenant_id}>{choices.tenantName(search.tenant_id)}</option>
              )}
              {choices.tenants.map((t) => <option key={t.value} value={t.value}>{t.label}</option>)}
            </select>
          </label>
          <label>
            Exact name <input value={name} onChange={(e) => setName(e.target.value)} />
          </label>
          <button type="submit" className="secondary">Find</button>
        </form>
      </div>
      <p className="muted">Identity changes apply at once. Quota usage arrives with quotas in M7 (RL-10).</p>
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !search.cursor}
        empty={search.name || search.tenant_id
          ? <>No consumer matches these filters. <Link to="/consumers" search={{}}>All consumers</Link></>
          : <>No consumers yet.{create.allowed ? " Create one with New consumer." : ` ${create.reason ?? ""} to create one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Name</th><th scope="col">Tenant</th><th scope="col">Plan</th><th scope="col">Status</th><th scope="col">JWT subject</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((c) => (
                  <tr key={c.id}>
                    <th scope="row"><Link to="/consumers/$id" params={{ id: String(c.id) }}>{c.name}</Link></th>
                    <td>{choices.tenantName(c.tenant_id)}</td>
                    <td>{c.plan_id ? choices.planName(c.plan_id) : <span className="muted">The tenant's plan</span>}</td>
                    <td>{c.status}</td>
                    <td>{c.jwt_subject ? <code>{c.jwt_subject}</code> : "–"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={search.cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/consumers", search: { ...search, cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}
