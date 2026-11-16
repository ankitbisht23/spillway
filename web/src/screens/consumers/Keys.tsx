// A consumer's API keys (UI-04, UI-06, CP-15 – CP-17): the list (prefix,
// created, expires, revoked; 50 a page), issue, rotate with a grace
// period, revoke after confirming, and block (a deny entry on the key).
//
// The key itself is shown once, in its dialog, and lives only in that
// dialog's state: the mutations hand it over through a callback and keep
// none of it (queries.ts), it is set to null when the dialog closes, and
// it is never logged, stored or put in a URL (ADR-0058).
import { useId, useState, type FormEvent } from "react";

import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { Dialog } from "../../components/Dialog";
import { ErrorLine } from "../../components/ErrorLine";
import { Pager } from "../../components/Pager";
import { QueryState } from "../../components/QueryState";
import { AccessListDialog } from "./AccessLists";
import { KEY_ADMIN, when } from "./common";
import { useIssueKey, useKeys, useRevokeKey, useRotateKey, type ApiKey, type NewKey } from "./queries";

/** A key as its owner recognises it: spw_<prefix>_…. */
export const shown = (k: { prefix: string }) => `spw_${k.prefix}_…`;

/** Whether k still authenticates: not revoked and not past its expiry. */
export function isLive(k: ApiKey, now = Date.now()): boolean {
  return !k.revoked_at && !(k.expires_at && new Date(k.expires_at).getTime() <= now);
}

const UNITS: Record<string, number> = { h: 3600, m: 60, s: 1, ms: 1e-3, us: 1e-6, "µs": 1e-6, ns: 1e-9 };
const MAX_GRACE_S = 168 * 3600;

/** Why grace is not a rotation's grace period (a Go duration from 0s to 168h, CP-16), if it is not. */
export function graceError(grace: string): string | undefined {
  const g = grace.trim();
  const want = "Want a duration from 0s to 168h, such as 24h or 90m";
  if (g === "0") return undefined;
  if (!/^(\d+(\.\d+)?(h|ms|us|µs|ns|m|s))+$/.test(g)) return want;
  let s = 0;
  for (const m of g.matchAll(/(\d+(?:\.\d+)?)(h|ms|us|µs|ns|m|s)/g)) s += Number(m[1]) * (UNITS[m[2] ?? ""] ?? 0);
  return s > MAX_GRACE_S ? "At most 168h (7 days)" : undefined;
}

/**
 * The key, shown once, with a Copy button. Nothing here keeps it: the
 * parent's state does, until its dialog closes.
 */
function Secret({ secret }: { secret: string }) {
  const [copy, setCopy] = useState<"" | "copied" | "failed">("");
  const doCopy = () => {
    const clip = typeof navigator !== "undefined" ? navigator.clipboard : undefined;
    if (!clip) return setCopy("failed");
    clip.writeText(secret).then(() => setCopy("copied"), () => setCopy("failed"));
  };
  return (
    <div className="secret">
      <p><strong>Copy the key now: it is shown only this once.</strong> The control plane keeps only its prefix and a hash (CP-15).</p>
      <p><code aria-label="The new API key">{secret}</code></p>
      <div className="actions">
        {/* The form it replaces had the focus; it moves here, so Escape still closes the dialog. */}
        <button type="button" autoFocus onClick={doCopy}>Copy key</button>
        {copy === "copied" && <span role="status">Copied.</span>}
        {copy === "failed" && <span role="alert" className="form-error">Could not copy: select the key and copy it.</span>}
      </div>
      <p className="muted">Send it in the <code>X-API-Key</code> header.</p>
    </div>
  );
}

/**
 * The dialog's end: the key is dropped from state, then the dialog goes.
 * While the request runs it stays, so the key cannot arrive unseen.
 */
function useSecret(onClose: () => void) {
  const [secret, setSecret] = useState<string | null>(null);
  return {
    secret,
    setSecret,
    closer: (pending: boolean) => () => {
      if (pending) return;
      setSecret(null);
      onClose();
    },
  };
}

function NewKeyFacts({ k }: { k: NewKey }) {
  return <p role="status">Issued key {k.id}, <code>{shown(k)}</code>, {k.expires_at ? `expiring ${when(k.expires_at)}` : "with no expiry"}.</p>;
}

export function IssueKeyDialog({ consumerId, consumerName, onClose }: { consumerId: number; consumerName: string; onClose: () => void }) {
  const { secret, setSecret, closer } = useSecret(onClose);
  const id = useId();
  const [expires, setExpires] = useState("");
  const [error, setError] = useState<string>();
  const issue = useIssueKey(consumerId, setSecret);
  const close = closer(issue.isPending);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const at = expires ? new Date(expires) : null;
    if (at && Number.isNaN(at.getTime())) return setError("Must be a date and time");
    setError(undefined);
    issue.mutate({ expires_at: at ? at.toISOString() : null });
  };
  return (
    <Dialog title={`Issue an API key for ${consumerName}`} onClose={close}>
      {secret !== null && issue.data ? (
        <>
          <NewKeyFacts k={issue.data} />
          <Secret secret={secret} />
          <div className="actions"><button type="button" onClick={close}>Done</button></div>
        </>
      ) : (
        <form onSubmit={submit} noValidate>
          <p>The key works on every gateway within seconds (CP-20).</p>
          <div className="field">
            <label htmlFor={id}>Expires (optional, your local time)</label>
            <input id={id} type="datetime-local" value={expires} aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-error` : undefined} onChange={(e) => setExpires(e.target.value)} />
            {error && <span id={`${id}-error`} className="form-error">{error}</span>}
          </div>
          {issue.isError && <ErrorLine error={issue.error} needs={KEY_ADMIN} />}
          <div className="actions">
            <button type="submit" disabled={issue.isPending}>Issue key</button>
            <button type="button" className="secondary" disabled={issue.isPending} onClick={close}>Cancel</button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

export function RotateKeyDialog({ apiKey, onClose }: { apiKey: ApiKey; onClose: () => void }) {
  const { secret, setSecret, closer } = useSecret(onClose);
  const id = useId();
  const [grace, setGrace] = useState("24h");
  const [note, setNote] = useState("");
  const [error, setError] = useState<string>();
  const rotate = useRotateKey(apiKey, setSecret);
  const close = closer(rotate.isPending);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const err = graceError(grace);
    setError(err);
    if (!err) rotate.mutate({ grace: grace.trim(), ...(note.trim() ? { note: note.trim() } : {}) });
  };
  return (
    <Dialog title={`Rotate key ${shown(apiKey)}`} onClose={close}>
      {secret !== null && rotate.data ? (
        <>
          <NewKeyFacts k={rotate.data.new} />
          <p>The old key {rotate.data.old.id} keeps working until {when(rotate.data.old.expires_at)}.</p>
          <Secret secret={secret} />
          <div className="actions"><button type="button" onClick={close}>Done</button></div>
        </>
      ) : (
        <form onSubmit={submit} noValidate>
          <p>Rotation issues a new key; the old one keeps working for the grace period, then stops (CP-16). The new key gets the old key's own allow and deny entries.</p>
          <div className="field">
            <label htmlFor={id}>Grace period</label>
            <input id={id} value={grace} aria-invalid={error ? true : undefined} aria-describedby={`${id}-hint${error ? ` ${id}-error` : ""}`} onChange={(e) => setGrace(e.target.value)} />
            <span id={`${id}-hint`} className="muted">From 0s to 168h, such as 24h or 90m. The old key never gets a later expiry than it has.</span>
            {error && <span id={`${id}-error`} className="form-error">{error}</span>}
          </div>
          <label className="field">
            Note (optional, kept in the audit log)
            <input value={note} maxLength={2000} onChange={(e) => setNote(e.target.value)} />
          </label>
          {rotate.isError && <ErrorLine error={rotate.error} needs={KEY_ADMIN} />}
          <div className="actions">
            <button type="submit" disabled={rotate.isPending}>Rotate key</button>
            <button type="button" className="secondary" disabled={rotate.isPending} onClick={close}>Cancel</button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

export function RevokeKeyDialog({ apiKey, onClose }: { apiKey: ApiKey; onClose: () => void }) {
  const [note, setNote] = useState("");
  const revoke = useRevokeKey(apiKey);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    revoke.mutate(note.trim() ? { note: note.trim() } : {}, { onSuccess: onClose });
  };
  return (
    <Dialog title={`Revoke key ${shown(apiKey)}?`} onClose={onClose}>
      <form onSubmit={submit}>
        <p>Every gateway refuses it with 401 within 5 s (CP-17). A revoked key cannot be restored: issue a new one instead.</p>
        <label className="field">
          Note (optional, kept in the audit log)
          <input value={note} maxLength={2000} onChange={(e) => setNote(e.target.value)} />
        </label>
        {revoke.isError && <ErrorLine error={revoke.error} needs={KEY_ADMIN} />}
        <div className="actions">
          <button type="submit" className="danger" disabled={revoke.isPending}>Revoke key</button>
          <button type="button" className="secondary" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Dialog>
  );
}

type Open = { kind: "issue" } | { kind: "rotate" | "revoke" | "block"; key: ApiKey } | null;

interface Props {
  consumerId: number;
  consumerName: string;
  cursor: string | undefined;
  onPage: (cursor: string | undefined) => void;
}

export function KeysSection({ consumerId, consumerName, cursor, onPage }: Props) {
  const keys = useKeys(consumerId, cursor);
  const issue = useCan("issueKey");
  const rotate = useCan("rotateKey");
  const revoke = useCan("revokeKey");
  const block = useCan("createAccessList");
  const [open, setOpen] = useState<Open>(null);
  const close = () => setOpen(null);
  return (
    <section aria-labelledby="keys-heading">
      <div className="toolbar">
        <h2 id="keys-heading">API keys</h2>
        <ActionButton disabledReason={issue.reason} onClick={() => setOpen({ kind: "issue" })}>Issue key</ActionButton>
      </div>
      <QueryState
        query={keys}
        isEmpty={(d) => d.items.length === 0 && !cursor}
        empty={<>No API keys yet.{issue.allowed ? " Issue one with Issue key." : ` ${issue.reason ?? ""} to issue one.`}</>}
      >
        {(d) => (
          <>
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Key</th><th scope="col">Created</th><th scope="col">Expires</th><th scope="col">Revoked</th>
                  <th scope="col">Rotated from</th><th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                {d.items.map((k) => (
                  <tr key={k.id}>
                    <th scope="row"><code>{shown(k)}</code> <span className="muted">(ID {k.id})</span></th>
                    <td>{when(k.created_at)}</td>
                    <td>{k.expires_at ? <>{when(k.expires_at)}{!k.revoked_at && !isLive(k) && " (expired)"}</> : "Never"}</td>
                    <td>{when(k.revoked_at)}</td>
                    <td>{k.rotated_from ? `Key ${k.rotated_from}` : "–"}</td>
                    <td>
                      {isLive(k) ? (
                        <span className="row-actions">
                          <ActionButton disabledReason={rotate.reason} aria-label={`Rotate ${shown(k)}`} onClick={() => setOpen({ kind: "rotate", key: k })}>Rotate</ActionButton>
                          <ActionButton className="danger" disabledReason={revoke.reason} aria-label={`Revoke ${shown(k)}`} onClick={() => setOpen({ kind: "revoke", key: k })}>Revoke</ActionButton>
                          <ActionButton disabledReason={block.reason} aria-label={`Block ${shown(k)}`} onClick={() => setOpen({ kind: "block", key: k })}>Block</ActionButton>
                        </span>
                      ) : <span className="muted">{k.revoked_at ? "Revoked" : "Expired"}</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager cursor={cursor} nextCursor={d.next_cursor} onNavigate={onPage} />
          </>
        )}
      </QueryState>
      {open?.kind === "issue" && <IssueKeyDialog consumerId={consumerId} consumerName={consumerName} onClose={close} />}
      {open?.kind === "rotate" && <RotateKeyDialog apiKey={open.key} onClose={close} />}
      {open?.kind === "revoke" && <RevokeKeyDialog apiKey={open.key} onClose={close} />}
      {open?.kind === "block" && <AccessListDialog prefill={{ action: "deny", subject_type: "key", api_key_id: open.key.id }} onClose={close} />}
    </section>
  );
}
