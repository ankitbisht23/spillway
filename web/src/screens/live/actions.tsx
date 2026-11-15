// The Throttling screen's dialogs (UI-04, RL-21, ADR-0060): block a key
// (an access-list deny entry on the API key, CP-20) and "who is this?",
// the admin's audited lookup of a subject's keyed digest (ADR-0053). The
// subject is typed by the admin, sent in a POST body only, and forgotten
// when the dialog closes: it is never in a URL, a log or the query cache.
import { useMutation } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api, call } from "../../api/client";
import { describeError } from "../../api/errors";
import type { PublishedPolicy } from "../../api/latestConfig";
import type { components } from "../../api/schema.gen";
import { formatCount } from "../../charts/series";
import { Dialog } from "../../components/Dialog";
import type { Range } from "./search";

type Subject = components["schemas"]["HeavyHitterSubject"];

const RANGE_MS: Record<Range, number> = { "15m": 15 * 60_000, "1h": 3_600_000, "6h": 6 * 3_600_000, "24h": 24 * 3_600_000, "7d": 7 * 24 * 3_600_000 };

function ErrorLine({ error }: { error: unknown }) {
  const view = describeError(error, ["admin"]);
  return (
    <p className="form-error" role="alert">
      {view.title}: {view.message}
      {view.requestId && <> (request ID <code>{view.requestId}</code>)</>}
    </p>
  );
}

export function BlockKeyDialog({ apiKeyId, digest, onClose }: { apiKeyId: number; digest: string; onClose: () => void }) {
  const [note, setNote] = useState("");
  const block = useMutation({
    mutationFn: async () =>
      (await call(api.POST("/access-lists", {
        body: { action: "deny", subject_type: "key", api_key_id: apiKeyId, note: note.trim() || null } as components["schemas"]["AccessListEntry"],
      }))).data,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    block.mutate();
  };
  return (
    <Dialog title={`Block API key ${apiKeyId}`} onClose={onClose}>
      {block.isSuccess ? (
        <>
          <p role="status">Blocked: access-list entry {block.data.id} denies API key {apiKeyId}. Gateways apply it with the next identity version.</p>
          <div className="actions"><button type="button" onClick={onClose}>Close</button></div>
        </>
      ) : (
        <form onSubmit={submit}>
          <p>
            Every request with API key {apiKeyId} (throttled as <code>{digest}</code>) will be refused with 403 by every gateway, at once and
            until the access-list entry is deleted.
          </p>
          <label className="field">
            Note (optional)
            <input value={note} maxLength={2000} onChange={(e) => setNote(e.target.value)} />
          </label>
          {block.isError && <ErrorLine error={block.error} />}
          <div className="actions">
            <button type="submit" className="danger" disabled={block.isPending}>Block key</button>
            <button type="button" className="secondary" onClick={onClose}>Cancel</button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

/** The placeholders of a key template, such as ip and header:X-Client. */
export function placeholders(template: string): string[] {
  return [...new Set([...template.matchAll(/\{([^{}]+)\}/g)].map((m) => m[1] ?? ""))];
}

/** Why the lookup cannot check a key of policy, if it cannot. */
export function lookupBlocker(policy: PublishedPolicy | undefined): string | undefined {
  if (!policy) return "Its policy is not in the latest published config";
  if (placeholders(policy.keyTemplate).includes("api_key")) return "Its key template uses {api_key}: the API key column identifies it";
  return undefined;
}

const FIELD_LABELS: Record<string, string> = {
  ip: "Client IP", consumer: "Consumer ID", tenant: "Tenant ID", route: "Route name", method: "HTTP method",
};

function fieldLabel(p: string): string {
  return p.startsWith("header:") ? `Header ${p.slice("header:".length)}` : (FIELD_LABELS[p] ?? p);
}

/** The lookup's subject from the form's values, by placeholder. */
export function subjectOf(values: Record<string, string>): Subject {
  const s: Subject = {};
  const headers: Record<string, string> = {};
  for (const [p, raw] of Object.entries(values)) {
    const v = raw.trim();
    if (!v) continue;
    if (p === "ip") s.ip = v;
    else if (p === "consumer") s.consumer_id = Number(v);
    else if (p === "tenant") s.tenant_id = Number(v);
    else if (p === "route") s.route = v;
    else if (p === "method") s.method = v.toUpperCase();
    else if (p.startsWith("header:")) headers[p.slice("header:".length)] = v;
  }
  if (Object.keys(headers).length) s.headers = headers;
  return s;
}

interface LookupProps {
  policy: PublishedPolicy;
  digest: string;
  since: Range;
  onClose: () => void;
}

export function LookupDialog({ policy, digest, since, onClose }: LookupProps) {
  const fields = placeholders(policy.keyTemplate);
  const [values, setValues] = useState<Record<string, string>>(() => Object.fromEntries(fields.map((f) => [f, ""])));
  const lookup = useMutation({
    // Dropped from the mutation cache as soon as the dialog closes: its
    // function holds the subject (by default it would stay for 5 min).
    gcTime: 0,
    mutationFn: async () =>
      (await call(api.POST("/heavy-hitters/lookup", {
        body: { policy_id: policy.id, subject: subjectOf(values), since: new Date(Date.now() - RANGE_MS[since]).toISOString() },
      }))).data,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    lookup.mutate();
  };
  const result = lookup.data;
  const denied = result?.items.reduce((s, w) => s + w.denied, 0) ?? 0;
  const shadow = result?.items.reduce((s, w) => s + w.shadow_denied, 0) ?? 0;
  return (
    <Dialog title="Who is this?" onClose={onClose}>
      <form onSubmit={submit}>
        <p>
          Enter the values policy <strong>{policy.name}</strong>'s key template <code>{policy.keyTemplate || "(empty)"}</code> takes for a
          subject you suspect; the control plane computes its digest. Each lookup is in the audit log (the digest, never the subject).
        </p>
        {fields.map((f) => (
          <label key={f} className="field">
            {fieldLabel(f)}
            <input
              value={values[f] ?? ""}
              inputMode={f === "consumer" || f === "tenant" ? "numeric" : undefined}
              autoComplete="off"
              onChange={(e) => setValues((v) => ({ ...v, [f]: e.target.value }))}
            />
          </label>
        ))}
        {lookup.isError && <ErrorLine error={lookup.error} />}
        {result && (
          <p role="status" className={result.key_digest === digest ? "match" : undefined}>
            {result.key_digest === digest
              ? `This subject is key ${digest}: ${formatCount(denied)} denied and ${formatCount(shadow)} would-deny in ${result.items.length} windows of the range.`
              : `Not this key: the subject's digest is ${result.key_digest}.`}
          </p>
        )}
        <div className="actions">
          <button type="submit" disabled={lookup.isPending}>Look up</button>
          <button type="button" className="secondary" onClick={onClose}>Close</button>
        </div>
      </form>
    </Dialog>
  );
}
