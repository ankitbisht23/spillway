// A consumer's quota overrides (UI-04, UI-06, RL-10, ADR-0070, ADR-0071):
// daily and monthly quotas that replace both of its plan's while they
// apply, from a start to an optional end (an end makes a burst grant); at
// most one standing one. Each change applies at once (CP-20).
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, call } from "../../api/client";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Dialog } from "../../components/Dialog";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { DeleteDialog } from "../routes/common";
import { EntityForm, useFormValues } from "../routes/EntityForm";
import { APPLIES_AT_ONCE, KEY_ADMIN, when } from "./common";
import { QUOTA_OVERRIDE_FIELDS } from "./fields";
import { overrideState } from "./Overrides";
import { quotaOverrideKey, useQuotaOverride, useQuotaOverrides, type QuotaOverride } from "./queries";
import { quotasText } from "./quota";

type Loaded = { data: QuotaOverride; etag: string | null };

const invalidate = (qc: ReturnType<typeof useQueryClient>) => {
  void qc.invalidateQueries({ queryKey: ["quota-overrides", "list"] });
  void qc.invalidateQueries({ queryKey: ["quota-usage"] }); // the effective quotas change
};

function NewQuotaOverride({ consumerId, onDone }: { consumerId: number; onDone: () => void }) {
  const qc = useQueryClient();
  const allowed = useCan("createQuotaOverride");
  const form = useFormValues(QUOTA_OVERRIDE_FIELDS);
  return (
    <EntityForm
      noun="quota override"
      specs={QUOTA_OVERRIDE_FIELDS}
      form={form}
      base={{ consumer_id: consumerId, quotas: {} }}
      save={(body) => call(api.POST("/quota-overrides", { body: body as unknown as QuotaOverride }))}
      onSaved={() => {
        invalidate(qc);
        onDone();
      }}
      disabledReason={allowed.reason}
      submitLabel="Create quota override"
      needs={KEY_ADMIN}
    />
  );
}

function EditQuotaOverride({ id, onDone }: { id: number; onDone: () => void }) {
  const o = useQuotaOverride(id);
  return (
    <QueryState query={o}>
      {(loaded) => <EditForm key={loaded.etag ?? ""} loaded={loaded} reload={() => void o.refetch()} onDone={onDone} />}
    </QueryState>
  );
}

function EditForm({ loaded, reload, onDone }: { loaded: Loaded; reload: () => void; onDone: () => void }) {
  const qc = useQueryClient();
  const allowed = useCan("updateQuotaOverride");
  const form = useFormValues(QUOTA_OVERRIDE_FIELDS, loaded.data);
  return (
    <EntityForm
      noun="quota override"
      specs={QUOTA_OVERRIDE_FIELDS}
      form={form}
      base={loaded.data}
      save={(body) => call(api.PUT("/quota-overrides/{id}", { params: { path: { id: loaded.data.id }, header: { "If-Match": loaded.etag ?? "" } }, body: body as unknown as QuotaOverride }))}
      onSaved={(r) => {
        qc.setQueryData(quotaOverrideKey(loaded.data.id), r);
        invalidate(qc);
        onDone();
      }}
      disabledReason={allowed.reason}
      reload={reload}
      submitLabel="Save"
      needs={KEY_ADMIN}
    />
  );
}

export function QuotaOverridesSection({ consumerId, cursor, onPage }: { consumerId: number; cursor: string | undefined; onPage: (cursor: string | undefined) => void }) {
  const list = useQuotaOverrides(consumerId, cursor);
  const qc = useQueryClient();
  const create = useCan("createQuotaOverride");
  const edit = useCan("updateQuotaOverride");
  const remove = useCan("deleteQuotaOverride");
  const [open, setOpen] = useState<{ kind: "new" } | { kind: "edit" | "delete"; o: QuotaOverride } | null>(null);
  const close = () => setOpen(null);
  return (
    <section aria-labelledby="quota-overrides-heading">
      <div className="toolbar">
        <h2 id="quota-overrides-heading">Quota overrides</h2>
        <ActionButton disabledReason={create.reason} onClick={() => setOpen({ kind: "new" })}>Add quota override</ActionButton>
      </div>
      <p className="muted">A quota override replaces both of the plan's quotas, daily and monthly (0: none); one with an end is a time-boxed burst grant (RL-10).</p>
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !cursor}
        empty={<>No quota overrides: the plan's quotas apply.{create.allowed ? " Add one with Add quota override." : ` ${create.reason ?? ""} to add one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Override</th><th scope="col">Quotas</th><th scope="col">Starts</th><th scope="col">Ends</th>
                  <th scope="col">State</th><th scope="col">Note</th><th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((o) => (
                  <tr key={o.id}>
                    <th scope="row">Quota override</th>
                    <td>{quotasText(o.quotas)}</td>
                    <td>{o.starts_at ? when(o.starts_at) : "At once"}</td>
                    <td>{o.ends_at ? when(o.ends_at) : "No end"}</td>
                    <td>{overrideState(o)}</td>
                    <td>{o.note || "–"}</td>
                    <td>
                      <span className="row-actions">
                        <ActionButton disabledReason={edit.reason} aria-label={`Edit quota override ${o.id}`} onClick={() => setOpen({ kind: "edit", o })}>Edit</ActionButton>
                        <ActionButton className="danger" disabledReason={remove.reason} aria-label={`Delete quota override ${o.id}`} onClick={() => setOpen({ kind: "delete", o })}>Delete</ActionButton>
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={cursor} nextCursor={d.next_cursor} onNavigate={onPage} list="quota overrides" />
          </>
        )}
      </QueryState>
      {open?.kind === "new" && (
        <Dialog title="Add a quota override" onClose={close}>
          <p className="muted">{APPLIES_AT_ONCE}</p>
          <NewQuotaOverride consumerId={consumerId} onDone={close} />
          <div className="actions"><button type="button" className="secondary" onClick={close}>Close</button></div>
        </Dialog>
      )}
      {open?.kind === "edit" && (
        <Dialog title={`Quota override ${open.o.id}`} onClose={close}>
          <p className="muted">{APPLIES_AT_ONCE}</p>
          <EditQuotaOverride id={open.o.id} onDone={close} />
          <div className="actions"><button type="button" className="secondary" onClick={close}>Close</button></div>
        </Dialog>
      )}
      {open?.kind === "delete" && (
        <DeleteDialog
          noun="quota override"
          name={String(open.o.id)}
          effect={`The plan's quotas apply again. ${APPLIES_AT_ONCE}`}
          needs={KEY_ADMIN}
          del={() => call(api.DELETE("/quota-overrides/{id}", { params: { path: { id: open.o.id }, header: { "If-Match": `"${open.o.version}"` } } }))}
          onDeleted={() => {
            qc.removeQueries({ queryKey: quotaOverrideKey(open.o.id) });
            invalidate(qc);
            close();
          }}
          onClose={close}
        />
      )}
    </section>
  );
}
