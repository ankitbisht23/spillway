// An upstream: its endpoints with drain and undrain, each gateway
// replica's live view of them and of its circuit (ADR-0059), and create,
// edit and delete in the config working set (UI-04, UI-06, CP-14,
// ADR-0060). Drain and undrain are working-set edits under the upstream's
// ETag that ship with the next publish; the page points to the publish
// page and never publishes by itself.
import { useQueryClient } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { api, call } from "../../api/client";
import { ApiError } from "../../api/errors";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { ErrorLine } from "../../components/ErrorLine";
import { LiveState, newestAt, StreamStatusLine } from "../../components/LiveState";
import { QueryState } from "../../components/QueryState";
import { NotFound } from "../../components/states";
import { Unpublished } from "../../components/Unpublished";
import { Card, StreamFailed } from "../live/common";
import { DeleteDialog, useUpstreamLive, type UpstreamLive } from "./common";
import { EntityForm, useFormValues } from "./EntityForm";
import { UPSTREAM_FIELDS, type Upstream } from "./fields";
import { ENDPOINT_STATE_TEXT, liveCircuits, liveEndpoints } from "./live";
import { upstreamKey, useEndpointChange, useUpstream, type Loaded } from "./queries";

export function NewUpstream() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const allowed = useCan("createUpstream");
  const form = useFormValues(UPSTREAM_FIELDS);
  return (
    <section>
      <div className="toolbar">
        <h1>New upstream</h1>
        <Link to="/upstreams" className="button-link">All upstreams</Link>
      </div>
      <EntityForm
        noun="upstream"
        specs={UPSTREAM_FIELDS}
        form={form}
        save={(body) => call(api.POST("/upstreams", { body: body as unknown as Upstream }))}
        onSaved={(r) => {
          qc.setQueryData(upstreamKey(r.data.id), r);
          void qc.invalidateQueries({ queryKey: ["upstreams"] });
          void navigate({ to: "/upstreams/$id", params: { id: String(r.data.id) }, search: { saved: true } });
        }}
        disabledReason={allowed.reason}
        submitLabel="Create upstream"
      />
    </section>
  );
}

const upstreamApi = getRouteApi("/upstreams/$id");

export function UpstreamScreen() {
  const { id: raw } = upstreamApi.useParams();
  const { saved } = upstreamApi.useSearch();
  const id = Number(raw);
  if (!Number.isSafeInteger(id) || id < 1) return <NotFound />;
  return <ExistingUpstream id={id} saved={saved === true} />;
}

/** What the page last did to the working set, kept across the form's reloads. */
type Notice = { kind: "saved" } | { kind: "drained" | "undrained"; address: string } | null;

function ExistingUpstream({ id, saved }: { id: number; saved: boolean }) {
  const upstream = useUpstream(id);
  const live = useUpstreamLive();
  const [notice, setNotice] = useState<Notice>(saved ? { kind: "saved" } : null);
  // The form starts again from the server's upstream whenever its ETag
  // changes: after a save, a drain, an undrain or a Reload.
  return (
    <QueryState query={upstream}>
      {(loaded) => <UpstreamView key={loaded.etag ?? ""} id={id} loaded={loaded} live={live} notice={notice} onNotice={setNotice} reload={() => void upstream.refetch()} />}
    </QueryState>
  );
}

interface ViewProps {
  id: number;
  loaded: Loaded<Upstream>;
  live: UpstreamLive;
  notice: Notice;
  onNotice: (n: Notice) => void;
  reload: () => void;
}

function UpstreamView({ id, loaded, live, notice, onNotice, reload }: ViewProps) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const edit = useCan("updateUpstream");
  const remove = useCan("deleteUpstream");
  const form = useFormValues(UPSTREAM_FIELDS, loaded.data);
  const [confirm, setConfirm] = useState(false);
  const name = loaded.data.name;
  return (
    <section>
      <div className="toolbar">
        <h1>Upstream {name}</h1>
        <Link to="/upstreams" className="button-link">All upstreams</Link>
      </div>
      {notice && !form.dirty && (
        <Unpublished>
          {notice.kind === "drained" ? `Endpoint ${notice.address} draining in the working set.`
            : notice.kind === "undrained" ? `Endpoint ${notice.address} taking traffic again in the working set.` : undefined}
        </Unpublished>
      )}
      <Endpoints id={id} loaded={loaded} dirty={form.dirty} onNotice={onNotice} reload={reload} />
      <LiveView name={name} live={live} />
      <h2>Settings</h2>
      <div className="actions">
        <ActionButton className="danger" disabledReason={remove.reason} onClick={() => setConfirm(true)}>Delete</ActionButton>
      </div>
      <EntityForm
        noun="upstream"
        specs={UPSTREAM_FIELDS}
        form={form}
        base={loaded.data}
        save={(body) => call(api.PUT("/upstreams/{id}", { params: { path: { id }, header: { "If-Match": loaded.etag ?? "" } }, body: body as unknown as Upstream }))}
        onSaved={(r) => {
          onNotice({ kind: "saved" });
          qc.setQueryData(upstreamKey(id), r);
          void qc.invalidateQueries({ queryKey: ["upstreams"] });
        }}
        disabledReason={edit.reason}
        reload={reload}
        submitLabel="Save"
      />
      {confirm && (
        <DeleteDialog
          noun="upstream"
          name={name}
          del={() => call(api.DELETE("/upstreams/{id}", { params: { path: { id }, header: { "If-Match": loaded.etag ?? "" } } }))}
          onDeleted={() => {
            qc.removeQueries({ queryKey: upstreamKey(id) });
            void qc.invalidateQueries({ queryKey: ["upstreams"] });
            void navigate({ to: "/upstreams" });
          }}
          onClose={() => setConfirm(false)}
        />
      )}
    </section>
  );
}

/** The configured endpoints, each with drain or undrain. */
function Endpoints({ id, loaded, dirty, onNotice, reload }: { id: number; loaded: Loaded<Upstream>; dirty: boolean; onNotice: (n: Notice) => void; reload: () => void }) {
  const drain = useEndpointChange(id, "drain", (address) => onNotice({ kind: "drained", address }));
  const undrain = useEndpointChange(id, "undrain", (address) => onNotice({ kind: "undrained", address }));
  const canDrain = useCan("drainEndpoint");
  const canUndrain = useCan("updateUpstream");
  const pending = drain.isPending || undrain.isPending;
  const error = drain.error ?? undrain.error;
  const unsaved = dirty ? "Save or discard your edits first" : undefined;
  return (
    <section aria-label="Endpoints">
      <h2>Endpoints</h2>
      <p className="muted">A draining endpoint gets no new requests once published; its requests in flight finish.</p>
      <table className="table">
        <thead>
          <tr><th scope="col">Address</th><th scope="col">In the working set</th><th scope="col">Action</th></tr>
        </thead>
        <tbody>
          {loaded.data.endpoints.map((e) => (
            <tr key={e.id ?? e.address}>
              <th scope="row"><code>{e.address}</code></th>
              <td>{e.draining ? "Draining" : "Taking traffic"}</td>
              <td>
                {e.id !== undefined && (e.draining ? (
                  <ActionButton aria-label={`Undrain ${e.address}`} disabledReason={canUndrain.reason ?? unsaved} disabled={pending}
                    onClick={() => undrain.mutate({ loaded, endpointId: e.id ?? 0 })}>Undrain</ActionButton>
                ) : (
                  <ActionButton aria-label={`Drain ${e.address}`} disabledReason={canDrain.reason ?? unsaved} disabled={pending}
                    onClick={() => drain.mutate({ loaded, endpointId: e.id ?? 0 })}>Drain</ActionButton>
                ))}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {error instanceof ApiError && error.status === 412 ? (
        <div className="banner banner-warn" role="alert">
          This upstream changed since you opened it. <button type="button" className="secondary" onClick={reload}>Reload</button>{" "}
          loads the current version; then drain or undrain again if it still applies.
        </div>
      ) : error ? <ErrorLine error={error} needs={["editor", "admin"]} /> : null}
    </section>
  );
}

/** Each replica's view of the upstream: its endpoints by resolved address, and its circuit. */
function LiveView({ name, live }: { name: string; live: UpstreamLive }) {
  const subs = live.live.subs;
  const stale = live.live.status.state !== "open";
  const notReported = "No gateway reports this upstream: it is not in the published config, or no gateway is up.";
  return (
    <section aria-label="Live state">
      <div className="toolbar">
        <h2>Live, per gateway replica</h2>
        <StreamStatusLine status={live.live.status} at={newestAt(subs)} />
      </div>
      <p className="muted">Gateways report each endpoint by the address its host name resolves to.</p>
      {live.live.status.state === "failed" ? (
        <StreamFailed error={live.live.status.error} reset={null} />
      ) : (
        <div className="cards">
          <Card title="Endpoints per replica">
            <LiveState title="endpoint states" sub={subs["routes.endpoints"]} stale={stale}
              select={(d) => liveEndpoints(d)?.filter((e) => e.upstream === name) ?? null} isEmpty={(d) => d.length === 0} empty={notReported}>
              {(eps) => (
                <table className="table" aria-label="Endpoint states">
                  <thead><tr><th scope="col">Replica</th><th scope="col">Endpoint</th><th scope="col">State</th></tr></thead>
                  <tbody>
                    {eps.map((e) => (
                      <tr key={`${e.replica} ${e.endpoint}`}>
                        <td>{e.replica}</td><td><code>{e.endpoint}</code></td>
                        <td className={`endpoint-${e.state}`}>{ENDPOINT_STATE_TEXT[e.state]}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </LiveState>
          </Card>
          <Card title="Circuit per replica">
            <LiveState title="circuit states" sub={subs["routes.circuits"]} stale={stale}
              select={(d) => liveCircuits(d)?.filter((c) => c.upstream === name) ?? null} isEmpty={(d) => d.length === 0} empty={notReported}>
              {(cs) => (
                <table className="table" aria-label="Circuit states">
                  <thead><tr><th scope="col">Replica</th><th scope="col">Circuit</th></tr></thead>
                  <tbody>
                    {cs.map((c) => (
                      <tr key={c.replica}><td>{c.replica}</td><td className={`circuit-${c.state}`}>{c.state}</td></tr>
                    ))}
                  </tbody>
                </table>
              )}
            </LiveState>
          </Card>
        </div>
      )}
    </section>
  );
}
