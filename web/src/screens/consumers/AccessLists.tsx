// Allow and deny lists (UI-04, UI-06, RL-09): every entry, 50 a page,
// filtered by action and subject type (ADR-0036), with create, edit and
// delete for key-admin and admin. An entry names an API key, a consumer or
// a CIDR; allow exempts it from rate limits, deny refuses it with 403
// (ADR-0041). Each change is an identity change that applies at once.
import { useQueryClient } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { api, call } from "../../api/client";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Dialog } from "../../components/Dialog";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { DeleteDialog } from "../routes/common";
import { EntityForm, useFormValues } from "../routes/EntityForm";
import { APPLIES_AT_ONCE, KEY_ADMIN, Tabs, when } from "./common";
import { ACCESS_LIST_FIELDS } from "./fields";
import { accessListKey, useAccessList, useAccessLists, type AccessAction, type AccessListEntry, type SubjectType } from "./queries";

/** What an entry names, for people. */
export function subjectText(e: Pick<AccessListEntry, "subject_type" | "api_key_id" | "consumer_id" | "cidr">) {
  switch (e.subject_type) {
    case "key":
      return <>API key {e.api_key_id}</>;
    case "consumer":
      return e.consumer_id ? <>Consumer <Link to="/consumers/$id" params={{ id: String(e.consumer_id) }}>{e.consumer_id}</Link></> : <>Consumer</>;
    default:
      return <>CIDR <code>{e.cidr}</code></>;
  }
}

/** A new entry's starting values. */
export type Prefill = Partial<Pick<AccessListEntry, "action" | "subject_type" | "api_key_id" | "consumer_id">>;

function NewEntry({ prefill, onDone }: { prefill: Prefill; onDone: () => void }) {
  const qc = useQueryClient();
  const allowed = useCan("createAccessList");
  const form = useFormValues(ACCESS_LIST_FIELDS, prefill);
  return (
    <EntityForm
      noun="access-list entry"
      specs={ACCESS_LIST_FIELDS}
      form={form}
      save={(body) => call(api.POST("/access-lists", { body: body as unknown as AccessListEntry }))}
      onSaved={() => {
        void qc.invalidateQueries({ queryKey: ["access-lists", "list"] });
        onDone();
      }}
      disabledReason={allowed.reason}
      submitLabel="Create entry"
      needs={KEY_ADMIN}
    />
  );
}

function EditEntry({ id, onDone }: { id: number; onDone: () => void }) {
  const entry = useAccessList(id);
  const qc = useQueryClient();
  const allowed = useCan("updateAccessList");
  return (
    <QueryState query={entry}>
      {(loaded) => <EditForm key={loaded.etag ?? ""} loaded={loaded} disabledReason={allowed.reason} reload={() => void entry.refetch()} onSaved={(r) => {
        qc.setQueryData(accessListKey(id), r);
        void qc.invalidateQueries({ queryKey: ["access-lists", "list"] });
        onDone();
      }} />}
    </QueryState>
  );
}

type Loaded = { data: AccessListEntry; etag: string | null };

function EditForm({ loaded, disabledReason, reload, onSaved }: { loaded: Loaded; disabledReason?: string; reload: () => void; onSaved: (r: Loaded) => void }) {
  const form = useFormValues(ACCESS_LIST_FIELDS, loaded.data);
  return (
    <EntityForm
      noun="access-list entry"
      specs={ACCESS_LIST_FIELDS}
      form={form}
      base={loaded.data}
      save={(body) => call(api.PUT("/access-lists/{id}", { params: { path: { id: loaded.data.id }, header: { "If-Match": loaded.etag ?? "" } }, body: body as unknown as AccessListEntry }))}
      onSaved={onSaved}
      disabledReason={disabledReason}
      reload={reload}
      submitLabel="Save"
      needs={KEY_ADMIN}
    />
  );
}

/** A new entry (with prefill) or entry id's form, in a dialog. */
export function AccessListDialog({ id, prefill = {}, onClose }: { id?: number; prefill?: Prefill; onClose: () => void }) {
  return (
    <Dialog title={id ? `Access-list entry ${id}` : "New access-list entry"} onClose={onClose}>
      <p className="muted">Allow exempts the subject from rate limits; deny refuses it with 403. {APPLIES_AT_ONCE}</p>
      {id ? <EditEntry id={id} onDone={onClose} /> : <NewEntry prefill={prefill} onDone={onClose} />}
      <div className="actions"><button type="button" className="secondary" onClick={onClose}>Close</button></div>
    </Dialog>
  );
}

const route = getRouteApi("/access-lists");

export function AccessListList() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const list = useAccessLists(search);
  const create = useCan("createAccessList");
  const edit = useCan("updateAccessList");
  const remove = useCan("deleteAccessList");
  const [open, setOpen] = useState<{ kind: "new" } | { kind: "edit" | "delete"; entry: AccessListEntry } | null>(null);
  const filter = (patch: { action?: AccessAction; subject_type?: SubjectType }) =>
    void navigate({ to: "/access-lists", search: { ...search, ...patch, cursor: undefined } });
  return (
    <section>
      <div className="toolbar">
        <h1>Consumers &amp; keys</h1>
        <ActionButton disabledReason={create.reason} onClick={() => setOpen({ kind: "new" })}>New entry</ActionButton>
      </div>
      <Tabs />
      <div className="toolbar">
        <form className="filters" aria-label="Filters" onSubmit={(e) => e.preventDefault()}>
          <label>
            Action{" "}
            <select value={search.action ?? ""} onChange={(e) => filter({ action: (e.target.value || undefined) as AccessAction | undefined })}>
              <option value="">Allow and deny</option>
              <option value="allow">allow</option>
              <option value="deny">deny</option>
            </select>
          </label>
          <label>
            Subject{" "}
            <select value={search.subject_type ?? ""} onChange={(e) => filter({ subject_type: (e.target.value || undefined) as SubjectType | undefined })}>
              <option value="">Every subject</option>
              <option value="key">key</option>
              <option value="consumer">consumer</option>
              <option value="cidr">cidr</option>
            </select>
          </label>
        </form>
      </div>
      <p className="muted">Checked before rate limits: CIDR entries before authentication, key and consumer entries right after it (RL-09).</p>
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !search.cursor}
        empty={search.action || search.subject_type
          ? <>No entry matches these filters. <Link to="/access-lists" search={{}}>All entries</Link></>
          : <>No access-list entries yet.{create.allowed ? " Create one with New entry." : ` ${create.reason ?? ""} to create one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Entry</th><th scope="col">Action</th><th scope="col">Subject</th><th scope="col">Note</th><th scope="col">Updated</th><th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((e) => (
                  <tr key={e.id}>
                    <th scope="row">{e.id}</th>
                    <td>{e.action}</td>
                    <td>{subjectText(e)}</td>
                    <td>{e.note || "–"}</td>
                    <td>{when(e.updated_at)}</td>
                    <td>
                      <span className="row-actions">
                        <ActionButton disabledReason={edit.reason} aria-label={`Edit entry ${e.id}`} onClick={() => setOpen({ kind: "edit", entry: e })}>Edit</ActionButton>
                        <ActionButton className="danger" disabledReason={remove.reason} aria-label={`Delete entry ${e.id}`} onClick={() => setOpen({ kind: "delete", entry: e })}>Delete</ActionButton>
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={search.cursor} nextCursor={d.next_cursor} onNavigate={(c) => void navigate({ to: "/access-lists", search: { ...search, cursor: c } })} />
          </>
        )}
      </QueryState>
      {open?.kind === "new" && <AccessListDialog onClose={() => setOpen(null)} />}
      {open?.kind === "edit" && <AccessListDialog id={open.entry.id} onClose={() => setOpen(null)} />}
      {open?.kind === "delete" && (
        <DeleteDialog
          noun="access-list entry"
          name={String(open.entry.id)}
          effect={`It stops ${open.entry.action === "deny" ? "refusing" : "exempting"} its subject. ${APPLIES_AT_ONCE}`}
          needs={KEY_ADMIN}
          del={() => call(api.DELETE("/access-lists/{id}", { params: { path: { id: open.entry.id }, header: { "If-Match": `"${open.entry.version}"` } } }))}
          onDeleted={() => {
            qc.removeQueries({ queryKey: accessListKey(open.entry.id) });
            void qc.invalidateQueries({ queryKey: ["access-lists", "list"] });
            setOpen(null);
          }}
          onClose={() => setOpen(null)}
        />
      )}
    </section>
  );
}
