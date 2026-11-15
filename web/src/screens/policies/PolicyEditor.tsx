// A policy: create, edit with the browser's checks then the server's
// (shown on their fields), delete, and promote shadow to enforce (UI-01,
// UI-04, UI-06, CP-14, ADR-0060). Every change is to the working set; the
// page then points to the publish page, never publishes by itself.
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useId, useState, type FormEvent, type ReactNode } from "react";

import { api, call } from "../../api/client";
import { ApiError } from "../../api/errors";
import { useLatestConfig } from "../../api/latestConfig";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Dialog } from "../../components/Dialog";
import { ErrorLine } from "../../components/ErrorLine";
import { QueryState } from "../../components/QueryState";
import { NotFound } from "../../components/states";
import { Unpublished } from "../../components/Unpublished";
import {
  ALGORITHMS, bodyOf, FAILURE_MODES, formOf, LEVELS, MODES, newForm, serverErrors, validatePolicy,
  type Field, type FieldErrors, type LimitRow, type PolicyForm,
} from "./form";
import { policyKey, usePolicy, usePromote, type Loaded } from "./queries";

export function NewPolicy() {
  return <PolicyFormView initial={newForm()} />;
}

const policyRoute = getRouteApi("/policies/$id");

export function PolicyScreen() {
  const { id: raw } = policyRoute.useParams();
  const { saved } = policyRoute.useSearch();
  const id = Number(raw);
  if (!Number.isSafeInteger(id) || id < 1) return <NotFound />;
  return <ExistingPolicy id={id} saved={saved === true} />;
}

/** What the page last did to the working set, kept across the form's reloads. */
type Notice = "saved" | "promoted" | null;

function ExistingPolicy({ id, saved }: { id: number; saved: boolean }) {
  const policy = usePolicy(id);
  const [notice, setNotice] = useState<Notice>(saved ? "saved" : null);
  // The form starts again from the server's policy whenever its ETag
  // changes: after a save, a promote or a Reload.
  return (
    <QueryState query={policy}>
      {(loaded) => (
        <PolicyFormView key={loaded.etag ?? ""} initial={formOf(loaded.data)} loaded={loaded} id={id} notice={notice} onNotice={setNotice} reload={() => void policy.refetch()} />
      )}
    </QueryState>
  );
}

interface FormProps {
  initial: PolicyForm;
  /** The policy as the server has it; none for a new one. */
  loaded?: Loaded;
  id?: number;
  notice?: Notice;
  onNotice?: (n: Notice) => void;
  reload?: () => void;
}

function PolicyFormView({ initial, loaded, id, notice = null, onNotice = () => {}, reload }: FormProps) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const isNew = id === undefined;
  const allowed = useCan(isNew ? "createPolicy" : "updatePolicy");
  const [form, setForm] = useState(initial);
  const [checked, setChecked] = useState<FieldErrors | null>(null);
  const dirty = JSON.stringify(form) !== JSON.stringify(initial);

  const save = useMutation({
    mutationFn: async (f: PolicyForm) =>
      id === undefined
        ? call(api.POST("/policies", { body: bodyOf(f) }))
        : call(api.PUT("/policies/{id}", { params: { path: { id }, header: { "If-Match": loaded?.etag ?? "" } }, body: bodyOf(f) })),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ["policies", "list"] });
      const newId = r.data.id;
      qc.setQueryData(policyKey(newId), r);
      if (isNew) void navigate({ to: "/policies/$id", params: { id: String(newId) }, search: { saved: true } });
      else onNotice("saved");
    },
  });
  const server = save.error instanceof ApiError && save.error.status === 422 ? serverErrors(save.error.details) : null;
  const errors: FieldErrors = { ...server?.fields, ...checked };
  const set = (patch: Partial<PolicyForm>) => {
    setForm((f) => ({ ...f, ...patch }));
    if (checked) setChecked(validatePolicy({ ...form, ...patch }));
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const found = validatePolicy(form);
    setChecked(found);
    if (Object.keys(found).length === 0) {
      setChecked(null);
      save.mutate(form);
    }
  };

  const stale = save.error instanceof ApiError && save.error.status === 412;
  return (
    <section>
      <div className="toolbar">
        <h1>{isNew ? "New policy" : `Policy ${initial.name}`}</h1>
        {!isNew && <Link to="/policies" className="button-link">All policies</Link>}
      </div>
      {notice && !dirty && <Unpublished>{notice === "promoted" ? "Promoted to enforce in the working set." : undefined}</Unpublished>}
      {loaded && id !== undefined && <PolicyActions id={id} loaded={loaded} dirty={dirty} onPromoted={() => onNotice("promoted")} />}
      <form className="policy-form" onSubmit={submit} noValidate aria-label="Policy">
        <fieldset disabled={!allowed.allowed || save.isPending}>
          <TextField label="Name" field="name" value={form.name} errors={errors} onChange={(v) => set({ name: v })} />
          <SelectField label="Level" field="level" value={form.level} options={LEVELS} errors={errors} onChange={(v) => set({ level: v })} />
          <TextField label="Routes" field="routes" value={form.routes} errors={errors} hint="Route names, comma-separated; * for every route." onChange={(v) => set({ routes: v })} />
          <TextField label="Methods" field="methods" value={form.methods} errors={errors} hint="Optional, comma-separated, such as GET, POST." onChange={(v) => set({ methods: v })} />
          <TextField label="Key template" field="keyTemplate" value={form.keyTemplate} errors={errors}
            hint="Placeholders {ip}, {api_key}, {consumer}, {tenant}, {route}, {method}, {header:Name}." onChange={(v) => set({ keyTemplate: v })} />
          <SelectField label="Algorithm" field="algorithm" value={form.algorithm} options={ALGORITHMS} errors={errors} onChange={(v) => set({ algorithm: v })} />
          <CheckField label="Local (per gateway, no Redis)" field="local" checked={form.local} errors={errors} onChange={(v) => set({ local: v })} />
          <SelectField label="Failure mode" field="failureMode" value={form.failureMode} options={FAILURE_MODES} errors={errors}
            empty="Default (fail_open)" hint="Redis policies only." onChange={(v) => set({ failureMode: v })} />
          <SelectField label="Mode" field="mode" value={form.mode} options={MODES} errors={errors}
            hint="Shadow decides and records but never rejects (RL-26)." onChange={(v) => set({ mode: v })} />
          <CheckField label="Limits from the consumer's plan" field="fromPlan" checked={form.fromPlan} errors={errors} onChange={(v) => set({ fromPlan: v })} />
          {!form.fromPlan && <LimitsField rows={form.limits} error={errors.limits} onChange={(limits) => set({ limits })} />}
        </fieldset>
        {server && server.rest.length > 0 && (
          <div role="alert"><ul className="form-error" aria-label="Other errors">{server.rest.map((m, i) => <li key={i}>{m}</li>)}</ul></div>
        )}
        {save.isError && !server && !stale && <ErrorLine error={save.error} needs={["editor", "admin"]} />}
        {stale && (
          <div className="banner banner-warn" role="alert">
            This policy changed since you opened it. <button type="button" className="secondary" onClick={reload}>Reload</button>{" "}
            loads the current version into the form; then make your change again.
          </div>
        )}
        {checked && Object.keys(checked).length > 0 && <p className="form-error" role="alert">Fix the fields marked above.</p>}
        <div className="actions">
          <ActionButton type="submit" disabledReason={allowed.reason} disabled={save.isPending}>{isNew ? "Create policy" : "Save"}</ActionButton>
        </div>
      </form>
    </section>
  );
}

/** Shadow report, promote and delete, for a saved policy. */
function PolicyActions({ id, loaded, dirty, onPromoted }: { id: number; loaded: Loaded; dirty: boolean; onPromoted: () => void }) {
  const latest = useLatestConfig();
  const published = latest.data?.policies.find((p) => p.id === id);
  const promote = usePromote(id, onPromoted);
  const canPromote = useCan("updatePolicy");
  const canDelete = useCan("deletePolicy");
  const [confirm, setConfirm] = useState(false);
  const shadow = loaded.data.mode === "shadow";
  return (
    <div className="policy-actions">
      <dl className="facts">
        <dt>Published</dt>
        <dd>
          {latest.isPending ? "…" : published
            ? <>in version {latest.data?.version} as {published.mode} · <Link to="/policies/$id/shadow" params={{ id: String(id) }}>Shadow report</Link></>
            : "not yet: publish it to see its shadow report"}
        </dd>
      </dl>
      <div className="actions">
        {shadow && (
          <ActionButton
            disabledReason={canPromote.reason ?? (dirty ? "Save or discard your edits first" : undefined)}
            disabled={promote.isPending}
            onClick={() => promote.mutate(loaded)}
          >
            Promote to enforce
          </ActionButton>
        )}
        <ActionButton className="danger" disabledReason={canDelete.reason} onClick={() => setConfirm(true)}>Delete</ActionButton>
      </div>
      {shadow && <p className="muted">Promote sets mode to enforce in the working set; gateways keep the shadow policy until you review and publish.</p>}
      {promote.isError && <PromoteError error={promote.error} />}
      {confirm && <DeleteDialog id={id} loaded={loaded} onClose={() => setConfirm(false)} />}
    </div>
  );
}

export function PromoteError({ error }: { error: unknown }) {
  if (error instanceof ApiError && error.status === 412) {
    return <p className="form-error" role="alert">This policy changed since you opened it: reload the page to see it, then promote again.</p>;
  }
  return <ErrorLine error={error} needs={["editor", "admin"]} />;
}

function DeleteDialog({ id, loaded, onClose }: { id: number; loaded: Loaded; onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const del = useMutation({
    mutationFn: () => call(api.DELETE("/policies/{id}", { params: { path: { id }, header: { "If-Match": loaded.etag ?? "" } } })),
    onSuccess: () => {
      qc.removeQueries({ queryKey: policyKey(id) });
      void qc.invalidateQueries({ queryKey: ["policies", "list"] });
      void navigate({ to: "/policies" });
    },
  });
  return (
    <Dialog title={`Delete policy ${loaded.data.name}?`} onClose={onClose}>
      <p>It leaves the working set now and the gateways with the next publish.</p>
      {del.isError && <ErrorLine error={del.error} needs={["editor", "admin"]} />}
      <div className="actions">
        <button type="button" className="danger" disabled={del.isPending} onClick={() => del.mutate()}>Delete policy</button>
        <button type="button" className="secondary" onClick={onClose}>Cancel</button>
      </div>
    </Dialog>
  );
}

interface FieldProps {
  label: string;
  field: Field;
  errors: FieldErrors;
  hint?: string;
}

/** A field's label, hint and error, tied to its control. */
function Wrap({ label, field, errors, hint, children }: FieldProps & { children: (id: string, describedBy: string | undefined) => ReactNode }) {
  const id = useId();
  const error = errors[field];
  const describedBy = [hint && `${id}-hint`, error && `${id}-error`].filter(Boolean).join(" ") || undefined;
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      {children(id, describedBy)}
      {hint && <span id={`${id}-hint`} className="muted">{hint}</span>}
      {error && <span id={`${id}-error`} className="form-error">{error}</span>}
    </div>
  );
}

function TextField(p: FieldProps & { value: string; onChange: (v: string) => void }) {
  return (
    <Wrap {...p}>
      {(id, by) => <input id={id} value={p.value} aria-invalid={p.errors[p.field] ? true : undefined} aria-describedby={by} onChange={(e) => p.onChange(e.target.value)} />}
    </Wrap>
  );
}

function SelectField(p: FieldProps & { value: string; options: readonly string[]; empty?: string; onChange: (v: string) => void }) {
  return (
    <Wrap {...p}>
      {(id, by) => (
        <select id={id} value={p.value} aria-invalid={p.errors[p.field] ? true : undefined} aria-describedby={by} onChange={(e) => p.onChange(e.target.value)}>
          {(p.empty !== undefined || !p.options.includes(p.value)) && <option value="">{p.empty ?? "Choose…"}</option>}
          {p.options.map((o) => <option key={o} value={o}>{o}</option>)}
        </select>
      )}
    </Wrap>
  );
}

function CheckField(p: FieldProps & { checked: boolean; onChange: (v: boolean) => void }) {
  const id = useId();
  const error = p.errors[p.field];
  return (
    <div className="field field-check">
      <label>
        <input type="checkbox" checked={p.checked} aria-invalid={error ? true : undefined} aria-describedby={error ? id : undefined} onChange={(e) => p.onChange(e.target.checked)} />{" "}
        {p.label}
      </label>
      {error && <span id={id} className="form-error">{error}</span>}
    </div>
  );
}

function LimitsField({ rows, error, onChange }: { rows: LimitRow[]; error: string | undefined; onChange: (rows: LimitRow[]) => void }) {
  const id = useId();
  const edit = (i: number, patch: Partial<LimitRow>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <fieldset className="limits" aria-describedby={error ? id : undefined}>
      <legend>Limits</legend>
      {rows.map((r, i) => (
        <div key={i} className="limit-row" role="group" aria-label={`Limit ${i + 1}`}>
          <label>Rate <input inputMode="numeric" value={r.rate} onChange={(e) => edit(i, { rate: e.target.value })} /></label>
          <label>Period <input value={r.period} placeholder="1s" onChange={(e) => edit(i, { period: e.target.value })} /></label>
          <label>Burst <input inputMode="numeric" value={r.burst} placeholder="rate" onChange={(e) => edit(i, { burst: e.target.value })} /></label>
          <button type="button" className="secondary" onClick={() => onChange(rows.filter((_, j) => j !== i))}>Remove</button>
        </div>
      ))}
      <button type="button" className="secondary" onClick={() => onChange([...rows, { rate: "", period: "", burst: "" }])}>Add limit</button>
      {error && <p id={id} className="form-error">{error}</p>}
    </fieldset>
  );
}
