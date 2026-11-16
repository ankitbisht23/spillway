// Pieces the Routes & upstreams screens share (UI-03, UI-06).
import { useMutation } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { Dialog } from "../../components/Dialog";
import { ErrorLine } from "../../components/ErrorLine";
import type { StreamQueryName } from "../../stream/client";
import { useSubscription } from "../../stream/useSubscription";
import { liveCircuits, liveEndpoints, type LiveCircuit, type LiveEndpoint } from "./live";

/** The screen's two lists. */
export function Tabs() {
  return (
    <nav className="tabs" aria-label="Routes and upstreams">
      <Link to="/routes" activeOptions={{ includeSearch: false }} activeProps={{ "aria-current": "page", className: "active" }}>Routes</Link>
      <Link to="/upstreams" activeOptions={{ includeSearch: false }} activeProps={{ "aria-current": "page", className: "active" }}>Upstreams</Link>
    </nav>
  );
}

const SUBS: readonly StreamQueryName[] = ["routes.endpoints", "routes.circuits"];

/** Every replica's endpoint and circuit state, live (ADR-0059); null fields until the first result. */
export function useUpstreamLive() {
  const live = useSubscription(SUBS);
  const endpoints = live.subs["routes.endpoints"];
  const circuits = live.subs["routes.circuits"];
  return {
    live,
    endpoints: (endpoints?.result && liveEndpoints(endpoints.result.data)) || null,
    circuits: (circuits?.result && liveCircuits(circuits.result.data)) || null,
  };
}

export type UpstreamLive = ReturnType<typeof useUpstreamLive>;

/** A cell of live data: its value once both subscriptions answered, else why not. */
export function LiveCell({ live, children }: { live: UpstreamLive; children: (endpoints: LiveEndpoint[], circuits: LiveCircuit[]) => ReactNode }) {
  if (live.endpoints && live.circuits) return <>{children(live.endpoints, live.circuits)}</>;
  const failed = live.live.subs["routes.endpoints"]?.error ?? live.live.subs["routes.circuits"]?.error;
  if (failed || live.live.status.state === "failed") return <span className="muted">Unavailable</span>;
  return <span className="muted">…</span>;
}

interface DeleteProps {
  noun: string;
  name: string;
  del: () => Promise<unknown>;
  onDeleted: () => void;
  onClose: () => void;
  /** What the delete does; by default, a working-set delete's. */
  effect?: string;
  /** The roles the delete needs, named on a 403. */
  needs?: readonly string[];
}

/** Confirms a delete (ADR-0060), then sends it. */
export function DeleteDialog({ noun, name, del, onDeleted, onClose, effect = "It leaves the working set now and the gateways with the next publish.", needs = ["editor", "admin"] }: DeleteProps) {
  const m = useMutation({ mutationFn: del, onSuccess: onDeleted });
  return (
    <Dialog title={`Delete ${noun} ${name}?`} onClose={onClose}>
      <p>{effect}</p>
      {m.isError && <ErrorLine error={m.error} needs={needs} />}
      <div className="actions">
        <button type="button" className="danger" disabled={m.isPending} onClick={() => m.mutate()}>Delete {noun}</button>
        <button type="button" className="secondary" onClick={onClose}>Cancel</button>
      </div>
    </Dialog>
  );
}
