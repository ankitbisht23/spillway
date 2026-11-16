// A consumer (UI-04, UI-06, CP-15 – CP-17, RL-08, RL-09): its settings,
// keys, overrides and allow or deny entries. Every change is an identity
// change that applies at once (CP-20), so there is nothing to publish.
import { useQueryClient } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { api, call } from "../../api/client";
import { useCan } from "../../auth/SessionContext";
import { ActionButton } from "../../components/ActionButton";
import { QueryState } from "../../components/QueryState";
import { NotFound } from "../../components/states";
import { DeleteDialog } from "../routes/common";
import { EntityForm, useFormValues } from "../routes/EntityForm";
import { AccessListDialog, type Prefill } from "./AccessLists";
import { APPLIES_AT_ONCE, KEY_ADMIN, LatestConfigNote, useIdentityChoices } from "./common";
import { consumerFields } from "./fields";
import { KeysSection } from "./Keys";
import { OverridesSection } from "./Overrides";
import { consumerKey, useConsumer, type Consumer } from "./queries";

export function NewConsumer() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const allowed = useCan("createConsumer");
  const choices = useIdentityChoices();
  const specs = consumerFields(choices.tenants, choices.plans);
  const form = useFormValues(specs);
  return (
    <section>
      <div className="toolbar">
        <h1>New consumer</h1>
        <Link to="/consumers" className="button-link">All consumers</Link>
      </div>
      <LatestConfigNote />
      <EntityForm
        noun="consumer"
        specs={specs}
        form={form}
        save={(body) => call(api.POST("/consumers", { body: body as unknown as Consumer }))}
        onSaved={(r) => {
          qc.setQueryData(consumerKey(r.data.id), r);
          void qc.invalidateQueries({ queryKey: ["consumers", "list"] });
          void navigate({ to: "/consumers/$id", params: { id: String(r.data.id) }, search: { saved: true } });
        }}
        disabledReason={allowed.reason}
        submitLabel="Create consumer"
        needs={KEY_ADMIN}
      />
    </section>
  );
}

const route = getRouteApi("/consumers/$id");

export function ConsumerScreen() {
  const { id: raw } = route.useParams();
  const id = Number(raw);
  if (!Number.isSafeInteger(id) || id < 1) return <NotFound />;
  return <ExistingConsumer id={id} />;
}

function ExistingConsumer({ id }: { id: number }) {
  const consumer = useConsumer(id);
  const search = route.useSearch();
  const navigate = useNavigate();
  const list = useCan("createAccessList");
  const choices = useIdentityChoices();
  const [notice, setNotice] = useState(search.saved === true);
  const [entry, setEntry] = useState<Prefill | null>(null);
  const page = (patch: { keys?: string; overrides?: string; override_policy?: undefined }, replace = false) =>
    void navigate({ to: "/consumers/$id", params: { id: String(id) }, search: { ...search, ...patch }, replace });
  // The settings form starts again from the server's consumer whenever
  // its ETag changes (after a save or a Reload); the sections below it
  // keep their state.
  return (
    <QueryState query={consumer}>
      {(loaded) => (
        <section>
          <ConsumerSettings key={loaded.etag ?? ""} loaded={loaded} notice={notice} onSaved={() => setNotice(true)} reload={() => void consumer.refetch()} />
          <KeysSection consumerId={id} consumerName={loaded.data.name} cursor={search.keys} onPage={(k) => page({ keys: k })} />
          <OverridesSection
            consumerId={id}
            policies={choices.policies}
            cursor={search.overrides}
            onPage={(o) => page({ overrides: o })}
            prefillPolicy={search.override_policy}
            onPrefillDone={() => page({ override_policy: undefined }, true)}
          />
          <section aria-labelledby="access-heading">
            <h2 id="access-heading">Allow and deny</h2>
            <p className="muted">
              Allow exempts this consumer from rate limits; deny refuses its requests with 403 (RL-09). Block one key from its row above.
              Every entry is on the <Link to="/access-lists" search={{}}>Access lists</Link> tab.
            </p>
            <div className="actions">
              <ActionButton disabledReason={list.reason} onClick={() => setEntry({ action: "deny", subject_type: "consumer", consumer_id: id })}>Deny this consumer</ActionButton>
              <ActionButton disabledReason={list.reason} onClick={() => setEntry({ action: "allow", subject_type: "consumer", consumer_id: id })}>Allow this consumer</ActionButton>
            </div>
          </section>
          {entry && <AccessListDialog prefill={entry} onClose={() => setEntry(null)} />}
        </section>
      )}
    </QueryState>
  );
}

function ConsumerSettings({ loaded, notice, onSaved, reload }: { loaded: { data: Consumer; etag: string | null }; notice: boolean; onSaved: () => void; reload: () => void }) {
  const c = loaded.data;
  const qc = useQueryClient();
  const navigate = useNavigate();
  const edit = useCan("updateConsumer");
  const remove = useCan("deleteConsumer");
  const choices = useIdentityChoices();
  const specs = consumerFields(choices.tenants, choices.plans);
  const form = useFormValues(specs, c);
  const [confirm, setConfirm] = useState(false);
  return (
    <>
      <div className="toolbar">
        <h1>Consumer {c.name}</h1>
        <Link to="/consumers" className="button-link">All consumers</Link>
      </div>
      {notice && !form.dirty && <p className="banner" role="status">Saved. {APPLIES_AT_ONCE}</p>}
      <dl className="facts">
        <dt>Tenant</dt>
        <dd>{choices.tenantName(c.tenant_id)}</dd>
        <dt>Plan</dt>
        <dd>{c.plan_id ? choices.planName(c.plan_id) : "The tenant's plan"}</dd>
        <dt>Quota usage</dt>
        <dd className="muted">Arrives with quotas in M7 (RL-10)</dd>
      </dl>
      <div className="actions">
        <ActionButton className="danger" disabledReason={remove.reason} onClick={() => setConfirm(true)}>Delete</ActionButton>
      </div>
      <LatestConfigNote />
      <EntityForm
        noun="consumer"
        specs={specs}
        form={form}
        base={c}
        save={(body) => call(api.PUT("/consumers/{id}", { params: { path: { id: c.id }, header: { "If-Match": loaded.etag ?? "" } }, body: body as unknown as Consumer }))}
        onSaved={(r) => {
          onSaved();
          qc.setQueryData(consumerKey(c.id), r);
          void qc.invalidateQueries({ queryKey: ["consumers", "list"] });
        }}
        disabledReason={edit.reason}
        reload={reload}
        submitLabel="Save"
        needs={KEY_ADMIN}
      />
      {confirm && (
        <DeleteDialog
          noun="consumer"
          name={c.name}
          effect={`Its keys, overrides and access-list entries go with it, and its requests stop authenticating. ${APPLIES_AT_ONCE}`}
          needs={KEY_ADMIN}
          del={() => call(api.DELETE("/consumers/{id}", { params: { path: { id: c.id }, header: { "If-Match": loaded.etag ?? "" } } }))}
          onDeleted={() => {
            qc.removeQueries({ queryKey: consumerKey(c.id) });
            void qc.invalidateQueries({ queryKey: ["consumers", "list"] });
            void navigate({ to: "/consumers" });
          }}
          onClose={() => setConfirm(false)}
        />
      )}
    </>
  );
}
