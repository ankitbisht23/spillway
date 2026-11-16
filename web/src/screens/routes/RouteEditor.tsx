// A route: create, edit and delete in the config working set (UI-04,
// UI-06, CP-14, ADR-0060). The page then points to the publish page; it
// never publishes by itself.
import { useQueryClient } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { api, call } from "../../api/client";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { QueryState } from "../../components/QueryState";
import { NotFound } from "../../components/states";
import { Unpublished } from "../../components/Unpublished";
import { DeleteDialog } from "./common";
import { EntityForm, useFormValues } from "./EntityForm";
import { ROUTE_FIELDS, type Route } from "./fields";
import { routeKey, useRoute, useUpstreamNames, type Loaded } from "./queries";

/** The route form's suggestions: the working set's upstream names. */
function useSuggestions() {
  const names = useUpstreamNames();
  return { upstream: names.data?.map((u) => u.name) ?? [] };
}

export function NewRoute() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const allowed = useCan("createRoute");
  const form = useFormValues(ROUTE_FIELDS);
  const suggestions = useSuggestions();
  return (
    <section>
      <div className="toolbar">
        <h1>New route</h1>
        <Link to="/routes" className="button-link">All routes</Link>
      </div>
      <EntityForm
        noun="route"
        specs={ROUTE_FIELDS}
        form={form}
        save={(body) => call(api.POST("/routes", { body: body as unknown as Route }))}
        onSaved={(r) => {
          qc.setQueryData(routeKey(r.data.id), r);
          void qc.invalidateQueries({ queryKey: ["routes", "list"] });
          void navigate({ to: "/routes/$id", params: { id: String(r.data.id) }, search: { saved: true } });
        }}
        disabledReason={allowed.reason}
        submitLabel="Create route"
        suggestions={suggestions}
      />
    </section>
  );
}

const routeApi = getRouteApi("/routes/$id");

export function RouteScreen() {
  const { id: raw } = routeApi.useParams();
  const { saved } = routeApi.useSearch();
  const id = Number(raw);
  if (!Number.isSafeInteger(id) || id < 1) return <NotFound />;
  return <ExistingRoute id={id} saved={saved === true} />;
}

function ExistingRoute({ id, saved }: { id: number; saved: boolean }) {
  const route = useRoute(id);
  const [notice, setNotice] = useState(saved);
  // The form starts again from the server's route whenever its ETag
  // changes: after a save or a Reload.
  return (
    <QueryState query={route}>
      {(loaded) => <RouteView key={loaded.etag ?? ""} id={id} loaded={loaded} notice={notice} onSaved={() => setNotice(true)} reload={() => void route.refetch()} />}
    </QueryState>
  );
}

function RouteView({ id, loaded, notice, onSaved, reload }: { id: number; loaded: Loaded<Route>; notice: boolean; onSaved: () => void; reload: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const edit = useCan("updateRoute");
  const remove = useCan("deleteRoute");
  const form = useFormValues(ROUTE_FIELDS, loaded.data);
  const suggestions = useSuggestions();
  const [confirm, setConfirm] = useState(false);
  const upstream = useUpstreamNames().data?.find((u) => u.name === loaded.data.upstream);
  return (
    <section>
      <div className="toolbar">
        <h1>Route {loaded.data.name}</h1>
        <Link to="/routes" className="button-link">All routes</Link>
      </div>
      {notice && !form.dirty && <Unpublished />}
      <dl className="facts">
        <dt>Upstream</dt>
        <dd>{upstream ? <Link to="/upstreams/$id" params={{ id: String(upstream.id) }}>{upstream.name}</Link> : loaded.data.upstream}</dd>
      </dl>
      <div className="actions">
        <ActionButton className="danger" disabledReason={remove.reason} onClick={() => setConfirm(true)}>Delete</ActionButton>
      </div>
      <EntityForm
        noun="route"
        specs={ROUTE_FIELDS}
        form={form}
        base={loaded.data}
        save={(body) => call(api.PUT("/routes/{id}", { params: { path: { id }, header: { "If-Match": loaded.etag ?? "" } }, body: body as unknown as Route }))}
        onSaved={(r) => {
          onSaved();
          qc.setQueryData(routeKey(id), r);
          void qc.invalidateQueries({ queryKey: ["routes", "list"] });
        }}
        disabledReason={edit.reason}
        reload={reload}
        submitLabel="Save"
        suggestions={suggestions}
      />
      {confirm && (
        <DeleteDialog
          noun="route"
          name={loaded.data.name}
          del={() => call(api.DELETE("/routes/{id}", { params: { path: { id }, header: { "If-Match": loaded.etag ?? "" } } }))}
          onDeleted={() => {
            qc.removeQueries({ queryKey: routeKey(id) });
            void qc.invalidateQueries({ queryKey: ["routes", "list"] });
            void navigate({ to: "/routes" });
          }}
          onClose={() => setConfirm(false)}
        />
      )}
    </section>
  );
}
