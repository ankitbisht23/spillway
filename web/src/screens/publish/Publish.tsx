// The publish page (UI-02, CP-02, ADR-0060): the working set validated and
// diffed against the latest published version, a required change note,
// and Publish with If-Match on the revision of the preview on screen,
// never a newer one. A 412 (the working set moved) shows the new preview
// and asks for another click; a 409 (nothing to publish) refreshes it.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api, call } from "../../api/client";
import { ApiError } from "../../api/errors";
import type { components } from "../../api/schema.gen";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { ChangeNote, noteError } from "../../components/ChangeNote";
import { ChangesList, ValidationErrors } from "../../components/Changes";
import { DiffView } from "../../components/DiffView";
import { ErrorLine } from "../../components/ErrorLine";
import { Published } from "../../components/Published";
import { QueryState } from "../../components/QueryState";

type Validation = components["schemas"]["ConfigValidation"];

/** Why the preview cannot be published, if it cannot (the role aside). */
export function blocker(p: Validation): string | undefined {
  if (!p.valid) return "Fix the errors above first";
  if (p.diff === "" && p.base_version !== null) return `Nothing to publish: the working set is version ${p.base_version}`;
  return undefined;
}

/** "Nothing to publish: the working set is already version N", from the server's 409 message. */
export function nothingToPublish(message: string): string {
  const v = /version (\d+)/.exec(message)?.[1];
  return v ? `Nothing to publish: the working set is already version ${v}.` : `Nothing to publish: ${message}`;
}

export function Publish() {
  const qc = useQueryClient();
  const can = useCan("publishConfig");
  // A preview stays as shown until the user (or a 412 or 409) refreshes
  // it, so the diff on screen is the revision Publish sends.
  const preview = useQuery({
    queryKey: ["config", "validate"],
    queryFn: async () => (await call(api.POST("/config/validate"))).data,
    staleTime: Infinity,
    gcTime: 0,
    refetchOnReconnect: false,
  });
  const [note, setNote] = useState("");
  const [noteTried, setNoteTried] = useState(false);
  const [banner, setBanner] = useState<string | null>(null);

  const publish = useMutation({
    mutationFn: (v: { revision: number; note: string }) =>
      call(api.POST("/config/publish", { params: { header: { "If-Match": `"${v.revision}"` } }, body: { note: v.note } })),
    onSuccess: () => {
      setNote("");
      setNoteTried(false);
      setBanner(null);
      void qc.invalidateQueries({ queryKey: ["config", "latest"] });
      void preview.refetch();
    },
    onError: (err) => {
      if (!(err instanceof ApiError)) return;
      if (err.status === 412) {
        setBanner("The working set changed since your preview: someone edited, imported or rolled back. Review the new diff, then publish again.");
        void preview.refetch();
      } else if (err.status === 409) {
        setBanner(nothingToPublish(err.message));
        void preview.refetch();
      }
    },
  });

  const submit = (p: Validation) => (e: FormEvent) => {
    e.preventDefault();
    setNoteTried(true);
    if (noteError(note)) return;
    setBanner(null);
    publish.mutate({ revision: p.working_set_revision, note });
  };

  const handled = publish.error instanceof ApiError && (publish.error.status === 412 || publish.error.status === 409);
  const invalid = publish.error instanceof ApiError && publish.error.status === 422 ? publish.error : null;
  return (
    <section>
      <div className="toolbar">
        <h1>Publish</h1>
        <button type="button" className="secondary" disabled={preview.isFetching} onClick={() => { setBanner(null); void preview.refetch(); }}>
          Refresh preview
        </button>
      </div>
      {publish.isSuccess && <Published version={publish.data.data} />}
      {banner && <p className="banner banner-warn" role="alert">{banner}</p>}
      <QueryState query={preview}>
        {(p) => (
          <>
            <p className="muted">
              Working-set revision {p.working_set_revision} against {p.base_version === null ? "no published version" : `version ${p.base_version}`}.
            </p>
            {!p.valid && (
              <>
                <h2>Errors</h2>
                <ValidationErrors errors={p.errors} />
              </>
            )}
            <h2>Changes</h2>
            <ChangesList changes={p.changes} />
            <h2>Diff</h2>
            <DiffView diff={p.diff} label="Diff against the published version" />
            <form onSubmit={submit(p)} noValidate aria-label="Publish">
              <ChangeNote value={note} onChange={setNote} error={noteTried ? noteError(note) : undefined} disabled={publish.isPending} />
              {invalid && (
                <>
                  <p className="form-error" role="alert">{invalid.message}</p>
                  <ValidationErrors errors={invalid.details} />
                </>
              )}
              {publish.isError && !handled && !invalid && <ErrorLine error={publish.error} needs={["publisher", "admin"]} />}
              <div className="actions">
                <ActionButton
                  type="submit"
                  disabledReason={can.reason ?? (preview.isFetching ? "Refreshing the preview…" : blocker(p))}
                  disabled={publish.isPending}
                >
                  Publish revision {p.working_set_revision}
                </ActionButton>
              </div>
            </form>
          </>
        )}
      </QueryState>
    </section>
  );
}
