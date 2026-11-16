// A consumer's overrides (UI-04, UI-06, RL-08): limits that replace its
// plan's or the policy's own for one per-consumer policy, from a start to
// an optional end (an end makes it a time-boxed burst grant). Keyed by
// (consumer, policy); each change applies at once (CP-20). Throttling's
// Add override opens the form with its policy chosen.
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, call } from "../../api/client";
import type { PublishedPolicy } from "../../api/latestConfig";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Dialog } from "../../components/Dialog";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { DeleteDialog } from "../routes/common";
import { EntityForm, useFormValues } from "../routes/EntityForm";
import { APPLIES_AT_ONCE, KEY_ADMIN, when } from "./common";
import { overrideFields } from "./fields";
import { overrideKey, useOverride, useOverrides, type Override } from "./queries";

/** "100/1s burst 200, 5000/1h". */
export const limitsText = (o: Pick<Override, "limits">) =>
  o.limits.map((l) => `${l.rate ?? "?"}/${l.period ?? "?"}${l.burst ? ` burst ${l.burst}` : ""}`).join(", ");

/** Whether o applies now, will, or did. */
export function overrideState(o: Pick<Override, "starts_at" | "ends_at">, now = Date.now()): string {
  if (o.ends_at && new Date(o.ends_at).getTime() <= now) return "Ended";
  if (o.starts_at && new Date(o.starts_at).getTime() > now) return "Scheduled";
  return o.ends_at ? "Burst grant, active" : "Active";
}

type Loaded = { data: Override; etag: string | null };

function NewOverride({ consumerId, policyId, policies, onDone }: { consumerId: number; policyId?: number; policies: readonly PublishedPolicy[]; onDone: () => void }) {
  const qc = useQueryClient();
  const allowed = useCan("createOverride");
  const specs = overrideFields(policies);
  const form = useFormValues(specs, policyId ? { policy_id: policyId } : undefined);
  return (
    <EntityForm
      noun="override"
      specs={specs}
      form={form}
      base={{ consumer_id: consumerId }}
      save={(body) => call(api.POST("/overrides", { body: body as unknown as Override }))}
      onSaved={() => {
        void qc.invalidateQueries({ queryKey: ["overrides", "list"] });
        onDone();
      }}
      disabledReason={allowed.reason}
      submitLabel="Create override"
      needs={KEY_ADMIN}
    />
  );
}

function EditOverride({ id, policies, onDone }: { id: number; policies: readonly PublishedPolicy[]; onDone: () => void }) {
  const o = useOverride(id);
  return (
    <QueryState query={o}>
      {(loaded) => <EditForm key={loaded.etag ?? ""} loaded={loaded} policies={policies} reload={() => void o.refetch()} onDone={onDone} />}
    </QueryState>
  );
}

function EditForm({ loaded, policies, reload, onDone }: { loaded: Loaded; policies: readonly PublishedPolicy[]; reload: () => void; onDone: () => void }) {
  const qc = useQueryClient();
  const allowed = useCan("updateOverride");
  const specs = overrideFields(policies);
  const form = useFormValues(specs, loaded.data);
  return (
    <EntityForm
      noun="override"
      specs={specs}
      form={form}
      base={loaded.data}
      save={(body) => call(api.PUT("/overrides/{id}", { params: { path: { id: loaded.data.id }, header: { "If-Match": loaded.etag ?? "" } }, body: body as unknown as Override }))}
      onSaved={(r) => {
        qc.setQueryData(overrideKey(loaded.data.id), r);
        void qc.invalidateQueries({ queryKey: ["overrides", "list"] });
        onDone();
      }}
      disabledReason={allowed.reason}
      reload={reload}
      submitLabel="Save"
      needs={KEY_ADMIN}
    />
  );
}

interface Props {
  consumerId: number;
  policies: readonly PublishedPolicy[];
  cursor: string | undefined;
  onPage: (cursor: string | undefined) => void;
  /** A policy to open the new override's form with (Throttling's Add override). */
  prefillPolicy: number | undefined;
  /** Called when that form closes. */
  onPrefillDone: () => void;
}

export function OverridesSection({ consumerId, policies, cursor, onPage, prefillPolicy, onPrefillDone }: Props) {
  const list = useOverrides(consumerId, cursor);
  const qc = useQueryClient();
  const create = useCan("createOverride");
  const edit = useCan("updateOverride");
  const remove = useCan("deleteOverride");
  const [open, setOpen] = useState<{ kind: "new" } | { kind: "edit" | "delete"; o: Override } | null>(null);
  const policyName = (id: number) => policies.find((p) => p.id === id)?.name ?? `Policy ${id}`;
  const newOpen = open?.kind === "new" || prefillPolicy !== undefined;
  const close = () => {
    setOpen(null);
    if (prefillPolicy !== undefined) onPrefillDone();
  };
  return (
    <section aria-labelledby="overrides-heading">
      <div className="toolbar">
        <h2 id="overrides-heading">Overrides</h2>
        <ActionButton disabledReason={create.reason} onClick={() => setOpen({ kind: "new" })}>Add override</ActionButton>
      </div>
      <p className="muted">An override replaces the plan's or the policy's limits for one policy; one with an end is a time-boxed burst grant (RL-08).</p>
      <QueryState
        query={list}
        isEmpty={(d) => d.items.length === 0 && !cursor}
        empty={<>No overrides: the plan's or the policy's limits apply.{create.allowed ? " Add one with Add override." : ` ${create.reason ?? ""} to add one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Policy</th><th scope="col">Limits</th><th scope="col">Starts</th><th scope="col">Ends</th>
                  <th scope="col">State</th><th scope="col">Note</th><th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((o) => (
                  <tr key={o.id}>
                    <th scope="row">{policyName(o.policy_id)}</th>
                    <td><code>{limitsText(o)}</code></td>
                    <td>{o.starts_at ? when(o.starts_at) : "At once"}</td>
                    <td>{o.ends_at ? when(o.ends_at) : "No end"}</td>
                    <td>{overrideState(o)}</td>
                    <td>{o.note || "–"}</td>
                    <td>
                      <span className="row-actions">
                        <ActionButton disabledReason={edit.reason} aria-label={`Edit the override of ${policyName(o.policy_id)}`} onClick={() => setOpen({ kind: "edit", o })}>Edit</ActionButton>
                        <ActionButton className="danger" disabledReason={remove.reason} aria-label={`Delete the override of ${policyName(o.policy_id)}`} onClick={() => setOpen({ kind: "delete", o })}>Delete</ActionButton>
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={cursor} nextCursor={d.next_cursor} onNavigate={onPage} />
          </>
        )}
      </QueryState>
      {newOpen && (
        <Dialog title="Add an override" onClose={close}>
          <p className="muted">{APPLIES_AT_ONCE}</p>
          <NewOverride consumerId={consumerId} policyId={prefillPolicy} policies={policies} onDone={close} />
          <div className="actions"><button type="button" className="secondary" onClick={close}>Close</button></div>
        </Dialog>
      )}
      {open?.kind === "edit" && (
        <Dialog title={`Override of ${policyName(open.o.policy_id)}`} onClose={close}>
          <p className="muted">{APPLIES_AT_ONCE}</p>
          <EditOverride id={open.o.id} policies={policies} onDone={close} />
          <div className="actions"><button type="button" className="secondary" onClick={close}>Close</button></div>
        </Dialog>
      )}
      {open?.kind === "delete" && (
        <DeleteDialog
          noun="the override of"
          name={policyName(open.o.policy_id)}
          effect={`The plan's or the policy's limits apply again. ${APPLIES_AT_ONCE}`}
          needs={KEY_ADMIN}
          del={() => call(api.DELETE("/overrides/{id}", { params: { path: { id: open.o.id }, header: { "If-Match": `"${open.o.version}"` } } }))}
          onDeleted={() => {
            qc.removeQueries({ queryKey: overrideKey(open.o.id) });
            void qc.invalidateQueries({ queryKey: ["overrides", "list"] });
            setOpen(null);
          }}
          onClose={() => setOpen(null)}
        />
      )}
    </section>
  );
}
