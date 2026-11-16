// Roll back to a version (CP-04, UI-02, ADR-0060): a dialog that checks
// the working set (POST /config/validate: its revision becomes the
// If-Match, its changes are the unpublished edits a rollback discards),
// shows what the rollback changes against the latest version, takes a
// required change note and publishes a copy of the version. A 412 (the
// working set moved) checks again, keeps the note and asks for another
// click; a 422 lists what blocks it (identity that refers past the
// version, CP-20).
import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api, call } from "../../api/client";
import { ApiError } from "../../api/errors";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { ChangeNote, noteError } from "../../components/ChangeNote";
import { ChangesList, ValidationErrors } from "../../components/Changes";
import { Dialog } from "../../components/Dialog";
import { DiffView } from "../../components/DiffView";
import { ErrorLine } from "../../components/ErrorLine";
import { QueryState } from "../../components/QueryState";
import { useRollback, useVersionDiff, type ConfigVersion } from "./queries";

/** What rolling back discards, from the working set's preview; undefined when nothing. */
export function discardWarning(p: { diff: string; changes: readonly unknown[] }): string | undefined {
  if (p.diff === "" && p.changes.length === 0) return undefined;
  const n = p.changes.length;
  return n > 0
    ? `Rolling back discards ${n} unpublished ${n === 1 ? "change" : "changes"} in the working set.`
    : "Rolling back discards the working set's unpublished edits.";
}

interface Props {
  version: number;
  onClose: () => void;
  onDone: (v: ConfigVersion) => void;
}

export function RollbackDialog({ version, onClose, onDone }: Props) {
  const can = useCan("rollbackConfig");
  // Like the publish page's preview: never refetched by itself, so the
  // revision sent is the one whose warning is on screen.
  const preview = useQuery({
    queryKey: ["config", "validate", "rollback", version],
    queryFn: async () => (await call(api.POST("/config/validate"))).data,
    staleTime: Infinity,
    gcTime: 0,
    refetchOnReconnect: false,
  });
  const latest = preview.data?.base_version ?? undefined;
  const diff = useVersionDiff(version, latest, latest !== undefined);
  const [note, setNote] = useState("");
  const [noteTried, setNoteTried] = useState(false);
  const [banner, setBanner] = useState<string | null>(null);
  const rollback = useRollback(version, onDone);

  const submit = (revision: number) => (e: FormEvent) => {
    e.preventDefault();
    setNoteTried(true);
    if (noteError(note)) return;
    setBanner(null);
    rollback.mutate({ revision, note }, {
      onError: (err) => {
        if (err instanceof ApiError && err.status === 412) {
          setBanner("The working set changed since this dialog opened: someone edited, imported, published or rolled back. Check the warning again, then roll back.");
          void preview.refetch();
        }
      },
    });
  };

  const err = rollback.error;
  const conflict = err instanceof ApiError && err.status === 412;
  const invalid = err instanceof ApiError && err.status === 422 ? err : null;
  return (
    <Dialog title={`Roll back to version ${version}?`} onClose={onClose}>
      <p>
        This publishes a new version that copies version {version}, and resets the working set to it. Identity (tenants, consumers, keys) is not
        rolled back.
      </p>
      {banner && <p className="banner banner-warn" role="alert">{banner}</p>}
      <QueryState query={preview}>
        {(p) => {
          const warning = discardWarning(p);
          return (
            <form onSubmit={submit(p.working_set_revision)} noValidate aria-label="Roll back">
              {warning && <p className="banner banner-warn" role="status">{warning}</p>}
              {p.base_version === version && <p className="muted">Version {version} is the latest: rolling back to it changes no published config.</p>}
              {p.base_version !== null && p.base_version !== version && (
                <>
                  <h3>What changes against version {p.base_version}</h3>
                  <QueryState query={diff}>
                    {(d) => (
                      <>
                        <ChangesList changes={d.changes} />
                        <DiffView diff={d.diff} label={`Diff from version ${p.base_version} to version ${version}`} />
                      </>
                    )}
                  </QueryState>
                </>
              )}
              <ChangeNote value={note} onChange={setNote} error={noteTried ? noteError(note) : undefined} disabled={rollback.isPending} />
              {invalid && (
                <>
                  <p className="form-error" role="alert">{invalid.message}</p>
                  <ValidationErrors errors={invalid.details} />
                </>
              )}
              {rollback.isError && !conflict && !invalid && <ErrorLine error={err} needs={["publisher", "admin"]} />}
              <div className="actions">
                <ActionButton
                  type="submit"
                  className="danger"
                  disabledReason={can.reason ?? (preview.isFetching ? "Checking the working set…" : undefined)}
                  disabled={rollback.isPending}
                >
                  Roll back to version {version}
                </ActionButton>
                <button type="button" className="secondary" disabled={rollback.isPending} onClick={onClose}>Cancel</button>
              </div>
            </form>
          );
        }}
      </QueryState>
    </Dialog>
  );
}
