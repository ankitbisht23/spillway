// Throttling (UI-03, RL-21, ADR-0060): the deny rate per policy (shadow
// would-denies as mode shadow) and the top throttled keys from the
// heavy-hitter windows, live every 5 s. A key is its keyed digest
// (ADR-0053), never the key itself. Its actions: open its policy, add an
// override for its consumer, block its API key, and, for admins, check a
// subject against it (the audited lookup).
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { useLatestConfig, type PublishedPolicy } from "../../api/latestConfig";
import { LineChart } from "../../charts/LineChart";
import { align, formatCount, formatNumber, isEmptyAligned, latest, type Aligned } from "../../charts/series";
import { ActionButton } from "../../components/ActionButton";
import { LiveState, newestAt, StreamStatusLine } from "../../components/LiveState";
import { useCan } from "../../auth/SessionContext";
import type { StreamQueryName } from "../../stream/client";
import { asHeavyHitters, asMatrix, type HeavyHitter } from "../../stream/data";
import { useSubscription } from "../../stream/useSubscription";
import { BlockKeyDialog, LookupDialog, lookupBlocker } from "./actions";
import { Card, RangeSelect, StreamFailed } from "./common";
import { throttlingParams, type Range, type ThrottlingSearch } from "./search";

const SUBS: readonly StreamQueryName[] = ["throttling.policies", "throttling.heavy_hitters"];

const route = getRouteApi("/throttling");

/** A deny-rate line's label: the policy's name and, for shadow, its mode. */
function policyLabel(policy: string, mode: string): string {
  return mode === "shadow" ? `${policy} (shadow)` : policy;
}

const policiesOf = (d: unknown): Aligned | null => {
  const m = asMatrix(d);
  return m && align(m, (s) => policyLabel(s.labels.policy ?? "?", s.labels.mode ?? ""));
};

export function Throttling() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const latestConfig = useLatestConfig();
  const live = useSubscription(SUBS, throttlingParams(search));
  const [selected, setSelected] = useState<HeavyHitter | null>(null);
  const set = (patch: Partial<ThrottlingSearch>) =>
    void navigate({ to: "/throttling", search: (prev) => ({ ...prev, ...patch }) });

  const policies = latestConfig.data?.policies ?? [];
  const byId = new Map(policies.map((p) => [p.id, p]));
  const byLabel = new Map(policies.flatMap((p) => [[policyLabel(p.name, "enforce"), p], [policyLabel(p.name, "shadow"), p]] as const));
  const stale = live.status.state !== "open";
  const subs = live.subs;

  return (
    <section className="live-screen">
      <h1>Throttling</h1>
      <div className="toolbar">
        <form className="filters" aria-label="Filters" onSubmit={(e) => e.preventDefault()}>
          <RangeSelect value={search.range ?? "15m"} onChange={(r) => set({ range: r === "15m" ? undefined : r })} />
          <label>
            Keys of{" "}
            <select value={search.policy ?? ""} onChange={(e) => set({ policy: e.target.value ? Number(e.target.value) : undefined })}>
              <option value="">Every policy</option>
              {search.policy !== undefined && !byId.has(search.policy) && <option value={search.policy}>Policy {search.policy}</option>}
              {policies.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
            </select>
          </label>
        </form>
        <StreamStatusLine status={live.status} at={newestAt(subs)} />
      </div>
      {latestConfig.isError && <p className="muted">The published policies could not be read; keys show policy IDs only.</p>}
      {live.status.state === "failed" ? (
        <StreamFailed error={live.status.error} reset={<Link to="/throttling" search={{}}>Reset the filters</Link>} />
      ) : (
        <div className="cards">
          <Card title="Deny rate per policy" wide>
            <LiveState title="deny rates" sub={subs["throttling.policies"]} stale={stale} select={policiesOf} isEmpty={isEmptyAligned} empty="No policy denied a request in this range.">
              {(d) => (
                <>
                  <LineChart title="Denies per second by policy" data={d} unit="rps" />
                  <DenyTable data={d} policyOf={(label) => byLabel.get(label)} />
                </>
              )}
            </LiveState>
          </Card>
          <Card title="Top throttled keys" wide>
            <LiveState title="throttled keys" sub={subs["throttling.heavy_hitters"]} stale={stale} select={asHeavyHitters} isEmpty={(items) => items.length === 0} empty="No key was throttled in this range.">
              {(items) => <KeysTable items={items} policyOf={(id) => byId.get(id)} selected={selected} onSelect={setSelected} />}
            </LiveState>
            {selected && <KeyActions key={`${selected.policy_id}/${selected.key_digest}`} hit={selected} policy={byId.get(selected.policy_id)} since={search.range ?? "15m"} onClose={() => setSelected(null)} />}
          </Card>
        </div>
      )}
    </section>
  );
}

function DenyTable({ data, policyOf }: { data: Aligned; policyOf: (label: string) => PublishedPolicy | undefined }) {
  return (
    <table className="table">
      <thead>
        <tr><th scope="col">Policy</th><th scope="col">Denies/s now</th><th scope="col"><span className="sr-only">Actions</span></th></tr>
      </thead>
      <tbody>
        {data.lines.map((l) => {
          const p = policyOf(l.label);
          return (
            <tr key={l.label}>
              <th scope="row">{l.label}</th>
              <td>{formatNumber(latest(l.values))}</td>
              <td>{p ? <Link to="/policies/$id" params={{ id: String(p.id) }}>Open policy</Link> : <span className="muted">Not in the latest published config</span>}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

interface KeysProps {
  items: HeavyHitter[];
  policyOf: (id: number) => PublishedPolicy | undefined;
  selected: HeavyHitter | null;
  onSelect: (h: HeavyHitter) => void;
}

function KeysTable({ items, policyOf, selected, onSelect }: KeysProps) {
  const isSelected = (h: HeavyHitter) => selected?.policy_id === h.policy_id && selected.key_digest === h.key_digest;
  return (
    <table className="table">
      <thead>
        <tr>
          <th scope="col">Key digest</th><th scope="col">Policy</th><th scope="col">Consumer</th><th scope="col">API key</th>
          <th scope="col">Denied</th><th scope="col">Shadow would-deny</th><th scope="col"><span className="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        {items.map((h) => (
          <tr key={`${h.policy_id}/${h.key_digest}`} className={isSelected(h) ? "selected" : undefined}>
            <th scope="row"><code>{h.key_digest}</code></th>
            <td>{policyOf(h.policy_id)?.name ?? `#${h.policy_id}`}</td>
            <td>{h.consumer_id ?? "–"}</td>
            <td>{h.api_key_id ?? "–"}</td>
            <td>{formatCount(h.denied)}</td>
            <td>{formatCount(h.shadow_denied)}</td>
            <td>
              <button type="button" className="link-button" aria-label={`Actions for ${h.key_digest}`} aria-pressed={isSelected(h)} onClick={() => onSelect(h)}>
                Actions
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

interface ActionsProps {
  hit: HeavyHitter;
  policy: PublishedPolicy | undefined;
  since: Range;
  onClose: () => void;
}

/** The selected key's actions, each disabled with its reason when the user or the key cannot have it (UI-04). */
function KeyActions({ hit, policy, since, onClose }: ActionsProps) {
  const navigate = useNavigate();
  const override = useCan("createOverride");
  const block = useCan("createAccessList");
  const lookup = useCan("lookupHeavyHitters");
  const [dialog, setDialog] = useState<"block" | "lookup" | null>(null);
  const consumer = hit.consumer_id;
  const key = hit.api_key_id;

  return (
    <aside className="key-actions" aria-label="Selected key">
      <h3>Key <code>{hit.key_digest}</code></h3>
      <dl className="facts">
        <dt>Policy</dt><dd>{policy ? policy.name : `#${hit.policy_id}`}</dd>
        <dt>Consumer</dt><dd>{consumer ?? "none"}</dd>
        <dt>API key</dt><dd>{key ?? "none"}</dd>
      </dl>
      <div className="actions">
        <Link to="/policies/$id" params={{ id: String(hit.policy_id) }} className="button-link">Open policy</Link>
        <ActionButton
          disabledReason={override.reason ?? (consumer === null ? "No consumer made these requests" : undefined)}
          onClick={() => consumer !== null && void navigate({ to: "/consumers/$id", params: { id: String(consumer) }, search: { override_policy: hit.policy_id } })}
        >
          Add override
        </ActionButton>
        <ActionButton disabledReason={block.reason ?? (key === null ? "No API key made these requests" : undefined)} onClick={() => setDialog("block")}>
          Block key
        </ActionButton>
        <ActionButton disabledReason={lookup.reason ?? lookupBlocker(policy)} onClick={() => setDialog("lookup")}>
          Who is this?
        </ActionButton>
        <button type="button" className="secondary" onClick={onClose}>Close</button>
      </div>
      {dialog === "block" && key !== null && <BlockKeyDialog apiKeyId={key} digest={hit.key_digest} onClose={() => setDialog(null)} />}
      {dialog === "lookup" && policy && <LookupDialog policy={policy} digest={hit.key_digest} since={since} onClose={() => setDialog(null)} />}
    </aside>
  );
}
