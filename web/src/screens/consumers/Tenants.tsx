// Tenants (UI-04, UI-06, CP-20): admin only, so the screen is hidden from
// every other role (ADR-0058). The list (50 a page), create, edit and
// delete; each change applies at once.
import { useQueryClient } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { api, call } from "../../api/client";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { NotFound } from "../../components/states";
import { DeleteDialog } from "../routes/common";
import { EntityForm, useFormValues } from "../routes/EntityForm";
import { APPLIES_AT_ONCE, LatestConfigNote, useIdentityChoices, when } from "./common";
import { tenantFields } from "./fields";
import { tenantKey, useTenant, useTenantList, type Tenant } from "./queries";

const ADMIN = ["admin"];
const listRoute = getRouteApi("/tenants");

export function TenantList() {
  const search = listRoute.useSearch();
  const navigate = useNavigate();
  const list = useTenantList(search);
  const choices = useIdentityChoices();
  const create = useCan("createTenant");
  return (
    <section>
      <div className="toolbar">
        <h1>Tenants</h1>
        <ActionButton disabledReason={create.reason} onClick={() => void navigate({ to: "/tenants/new" })}>New tenant</ActionButton>
      </div>
      <p className="muted">Identity changes apply at once. Quota usage arrives with quotas in M7 (RL-10).</p>
      {search.name && <p>Tenant named <code>{search.name}</code> · <Link to="/tenants" search={{}}>All tenants</Link></p>}
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !search.cursor}
        empty={search.name ? <>No tenant named {search.name}.</> : <>No tenants yet. Create one with New tenant.</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr><th scope="col">Name</th><th scope="col">Plan</th><th scope="col">Status</th><th scope="col">Consumers</th><th scope="col">Updated</th></tr>
              </thead>
              <tbody>
                {d.items.map((t) => (
                  <tr key={t.id}>
                    <th scope="row"><Link to="/tenants/$id" params={{ id: String(t.id) }}>{t.name}</Link></th>
                    <td>{choices.planName(t.plan_id)}</td>
                    <td>{t.status}</td>
                    <td><Link to="/consumers" search={{ tenant_id: t.id }} aria-label={`Consumers of ${t.name}`}>Consumers</Link></td>
                    <td>{when(t.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={search.cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/tenants", search: { ...search, cursor: c } })} />
          </>
        )}
      </QueryState>
    </section>
  );
}

export function NewTenant() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const allowed = useCan("createTenant");
  const choices = useIdentityChoices();
  const specs = tenantFields(choices.plans);
  const form = useFormValues(specs);
  return (
    <section>
      <div className="toolbar">
        <h1>New tenant</h1>
        <Link to="/tenants" className="button-link">All tenants</Link>
      </div>
      <LatestConfigNote />
      <EntityForm
        noun="tenant"
        specs={specs}
        form={form}
        save={(body) => call(api.POST("/tenants", { body: body as unknown as Tenant }))}
        onSaved={(r) => {
          qc.setQueryData(tenantKey(r.data.id), r);
          void qc.invalidateQueries({ queryKey: ["tenants"], predicate: (q) => q.queryKey[1] !== r.data.id });
          void navigate({ to: "/tenants/$id", params: { id: String(r.data.id) }, search: { saved: true } });
        }}
        disabledReason={allowed.reason}
        submitLabel="Create tenant"
        needs={ADMIN}
      />
    </section>
  );
}

const tenantRoute = getRouteApi("/tenants/$id");

export function TenantScreen() {
  const { id: raw } = tenantRoute.useParams();
  const id = Number(raw);
  if (!Number.isSafeInteger(id) || id < 1) return <NotFound />;
  return <ExistingTenant id={id} />;
}

function ExistingTenant({ id }: { id: number }) {
  const tenant = useTenant(id);
  const { saved } = tenantRoute.useSearch();
  const [notice, setNotice] = useState(saved === true);
  return (
    <QueryState query={tenant}>
      {(loaded) => <TenantView key={loaded.etag ?? ""} loaded={loaded} notice={notice} onSaved={() => setNotice(true)} reload={() => void tenant.refetch()} />}
    </QueryState>
  );
}

function TenantView({ loaded, notice, onSaved, reload }: { loaded: { data: Tenant; etag: string | null }; notice: boolean; onSaved: () => void; reload: () => void }) {
  const t = loaded.data;
  const qc = useQueryClient();
  const navigate = useNavigate();
  const edit = useCan("updateTenant");
  const remove = useCan("deleteTenant");
  const choices = useIdentityChoices();
  const specs = tenantFields(choices.plans);
  const form = useFormValues(specs, t);
  const [confirm, setConfirm] = useState(false);
  return (
    <section>
      <div className="toolbar">
        <h1>Tenant {t.name}</h1>
        <Link to="/tenants" className="button-link">All tenants</Link>
      </div>
      {notice && !form.dirty && <p className="banner" role="status">Saved. {APPLIES_AT_ONCE}</p>}
      <p><Link to="/consumers" search={{ tenant_id: t.id }}>Its consumers</Link></p>
      <div className="actions">
        <ActionButton className="danger" disabledReason={remove.reason} onClick={() => setConfirm(true)}>Delete</ActionButton>
      </div>
      <LatestConfigNote />
      <EntityForm
        noun="tenant"
        specs={specs}
        form={form}
        base={t}
        save={(body) => call(api.PUT("/tenants/{id}", { params: { path: { id: t.id }, header: { "If-Match": loaded.etag ?? "" } }, body: body as unknown as Tenant }))}
        onSaved={(r) => {
          onSaved();
          qc.setQueryData(tenantKey(t.id), r);
          void qc.invalidateQueries({ queryKey: ["tenants"], predicate: (q) => q.queryKey[1] !== t.id });
        }}
        disabledReason={edit.reason}
        reload={reload}
        submitLabel="Save"
        needs={ADMIN}
      />
      {confirm && (
        <DeleteDialog
          noun="tenant"
          name={t.name}
          effect={`Only a tenant with no consumers can be deleted. ${APPLIES_AT_ONCE}`}
          needs={ADMIN}
          del={() => call(api.DELETE("/tenants/{id}", { params: { path: { id: t.id }, header: { "If-Match": loaded.etag ?? "" } } }))}
          onDeleted={() => {
            qc.removeQueries({ queryKey: tenantKey(t.id) });
            void qc.invalidateQueries({ queryKey: ["tenants"] });
            void navigate({ to: "/tenants" });
          }}
          onClose={() => setConfirm(false)}
        />
      )}
    </section>
  );
}
