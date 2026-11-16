// The route and upstream forms (UI-06, CP-14, ADR-0060, ADR-0064): fields
// from a list of specs, the browser's checks on submit, the server's 422
// details on their fields, and a 412 answered with Reload, which loads the
// current version into the form (never a page reload, which would mean
// signing in again).
import { useMutation } from "@tanstack/react-query";
import { useId, useState, type FormEvent, type ReactNode } from "react";

import { ApiError } from "../../api/errors";
import { ActionButton } from "../../components/ActionButton";
import { ErrorLine } from "../../components/ErrorLine";
import { bodyOf, serverErrors, valuesOf, writable, type FieldErrors, type FieldSpec, type Values } from "./fields";

/** A form's values, where they started, and whether they moved. */
export function useFormValues(specs: readonly FieldSpec[], entity?: object) {
  const [initial] = useState(() => valuesOf(specs, entity));
  const [values, setValues] = useState(initial);
  const dirty = specs.some((s) => values[s.path] !== initial[s.path]);
  return { values, setValues, dirty };
}

interface Props<R> {
  /** "route" or "upstream", for messages. */
  noun: string;
  specs: readonly FieldSpec[];
  form: ReturnType<typeof useFormValues>;
  /** The entity as loaded (its writable settings are the body's base); none for a new one. */
  base?: object;
  /** Sends the body: a POST for a new entity, else a PUT under the loaded ETag. */
  save: (body: Record<string, unknown>) => Promise<R>;
  onSaved: (r: R) => void;
  /** Why the user may not save, if they may not. */
  disabledReason: string | undefined;
  /** Loads the current version into the form after a 412. */
  reload?: () => void;
  submitLabel: string;
  /** Suggestions for text fields, by path (a datalist). */
  suggestions?: Record<string, readonly string[]>;
}

export function EntityForm<R>({ noun, specs, form, base, save, onSaved, disabledReason, reload, submitLabel, suggestions = {} }: Props<R>) {
  const [checked, setChecked] = useState<FieldErrors | null>(null);
  const mutation = useMutation({ mutationFn: save, onSuccess: onSaved });
  const server = mutation.error instanceof ApiError && mutation.error.status === 422 ? serverErrors(specs, mutation.error.details) : null;
  const stale = mutation.error instanceof ApiError && mutation.error.status === 412;
  const errors: FieldErrors = { ...server?.fields, ...checked };

  const set = (path: string, v: string | boolean) => {
    const next: Values = { ...form.values, [path]: v };
    form.setValues(next);
    if (checked) setChecked(bodyOf(specs, next, base && writable(base)).errors);
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const { body, errors: found } = bodyOf(specs, form.values, base && writable(base));
    if (Object.keys(found).length > 0) {
      setChecked(found);
      return;
    }
    setChecked(null);
    mutation.mutate(body);
  };

  const top = specs.filter((s) => !s.group);
  const groups = [...new Set(specs.flatMap((s) => (s.group ? [s.group] : [])))];
  const field = (s: FieldSpec) => (
    <Field key={s.path} spec={s} value={form.values[s.path] ?? ""} error={errors[s.path]} suggestions={suggestions[s.path]} onChange={(v) => set(s.path, v)} />
  );
  return (
    <form className="policy-form" onSubmit={submit} noValidate aria-label={noun[0]?.toUpperCase() + noun.slice(1)}>
      <fieldset disabled={disabledReason !== undefined || mutation.isPending}>
        {top.map(field)}
        {groups.map((g) => (
          <fieldset key={g} className="limits">
            <legend>{g}</legend>
            {specs.filter((s) => s.group === g).map(field)}
          </fieldset>
        ))}
      </fieldset>
      {server && server.rest.length > 0 && (
        <div role="alert"><ul className="form-error" aria-label="Other errors">{server.rest.map((m, i) => <li key={i}>{m}</li>)}</ul></div>
      )}
      {mutation.isError && !server && !stale && <ErrorLine error={mutation.error} needs={["editor", "admin"]} />}
      {stale && (
        <div className="banner banner-warn" role="alert">
          This {noun} changed since you opened it. <button type="button" className="secondary" onClick={reload}>Reload</button>{" "}
          loads the current version into the form; then make your change again.
        </div>
      )}
      {checked && <p className="form-error" role="alert">Fix the fields marked above.</p>}
      <div className="actions">
        <ActionButton type="submit" disabledReason={disabledReason} disabled={mutation.isPending}>{submitLabel}</ActionButton>
      </div>
    </form>
  );
}

function Field({ spec, value, error, suggestions, onChange }: { spec: FieldSpec; value: string | boolean; error?: string; suggestions?: readonly string[]; onChange: (v: string | boolean) => void }) {
  const id = useId();
  const describedBy = [spec.hint && `${id}-hint`, error && `${id}-error`].filter(Boolean).join(" ") || undefined;
  const invalid = error ? true : undefined;
  let control: ReactNode;
  switch (spec.kind) {
    case "bool":
      return (
        <div className="field field-check">
          <label>
            <input type="checkbox" checked={value === true} aria-invalid={invalid} aria-describedby={error ? `${id}-error` : undefined} onChange={(e) => onChange(e.target.checked)} />{" "}
            {spec.label}
          </label>
          {error && <span id={`${id}-error`} className="form-error">{error}</span>}
        </div>
      );
    case "select":
      control = (
        <select id={id} value={String(value)} aria-invalid={invalid} aria-describedby={describedBy} onChange={(e) => onChange(e.target.value)}>
          <option value="">Default</option>
          {!spec.options?.includes(String(value)) && value !== "" && <option value={String(value)}>{String(value)}</option>}
          {spec.options?.map((o) => <option key={o} value={o}>{o}</option>)}
        </select>
      );
      break;
    case "headers":
    case "headerMatch":
    case "endpoints":
      control = <textarea id={id} rows={3} value={String(value)} aria-invalid={invalid} aria-describedby={describedBy} onChange={(e) => onChange(e.target.value)} />;
      break;
    default:
      control = (
        <>
          <input
            id={id}
            value={String(value)}
            inputMode={spec.kind === "int" ? "numeric" : spec.kind === "number" ? "decimal" : undefined}
            list={suggestions ? `${id}-list` : undefined}
            aria-invalid={invalid}
            aria-describedby={describedBy}
            onChange={(e) => onChange(e.target.value)}
          />
          {suggestions && <datalist id={`${id}-list`}>{suggestions.map((s) => <option key={s} value={s} />)}</datalist>}
        </>
      );
  }
  return (
    <div className="field">
      <label htmlFor={id}>{spec.label}{spec.required ? " (required)" : ""}</label>
      {control}
      {spec.hint && <span id={`${id}-hint`} className="muted">{spec.hint}</span>}
      {error && <span id={`${id}-error`} className="form-error">{error}</span>}
    </div>
  );
}
