// The live state of upstreams (UI-03, UI-06, ADR-0059, ADR-0064): each
// gateway replica's endpoints and circuits from routes.endpoints and
// routes.circuits. Gateways report an endpoint by its resolved address
// (ip:port), so a configured host name shows as the addresses it resolves
// to.
import { asVector } from "../../stream/data";

/** An endpoint as one replica sees it. */
export type EndpointState = "serving" | "ejected" | "failing" | "unknown";

export interface LiveEndpoint {
  replica: string;
  upstream: string;
  /** The resolved address. */
  endpoint: string;
  state: EndpointState;
}

export type CircuitState = "closed" | "open" | "half-open" | "unknown";

export interface LiveCircuit {
  replica: string;
  upstream: string;
  state: CircuitState;
}

export const ENDPOINT_STATE_TEXT: Record<EndpointState, string> = {
  serving: "Taking traffic",
  ejected: "Ejected (outlier detection)",
  failing: "Failing health checks",
  unknown: "Unknown",
};

/** routes.endpoints' series, one per replica and endpoint: healthy 1 serves; else ejected 1 is ejected, ejected 0 fails its health checks. */
export function liveEndpoints(data: unknown): LiveEndpoint[] | null {
  const v = asVector(data);
  if (!v) return null;
  const by = new Map<string, { replica: string; upstream: string; endpoint: string; healthy?: number | null; ejected?: number | null }>();
  for (const s of v.series) {
    const { replica = "", upstream = "", endpoint = "", stat } = s.labels;
    const k = `${replica}\u0000${upstream}\u0000${endpoint}`;
    const row = by.get(k) ?? { replica, upstream, endpoint };
    if (stat === "healthy") row.healthy = s.value;
    if (stat === "ejected") row.ejected = s.value;
    by.set(k, row);
  }
  return [...by.values()].map(({ healthy, ejected, ...row }) => ({
    ...row,
    state: healthy === 1 ? "serving" : ejected === 1 ? "ejected" : healthy === 0 && ejected === 0 ? "failing" : "unknown",
  }));
}

const CIRCUIT: Record<number, CircuitState> = { 0: "closed", 1: "open", 2: "half-open" };

/** routes.circuits' series, one per replica and upstream. */
export function liveCircuits(data: unknown): LiveCircuit[] | null {
  const v = asVector(data);
  if (!v) return null;
  return v.series.map((s) => ({
    replica: s.labels.replica ?? "",
    upstream: s.labels.upstream ?? "",
    state: (s.value !== null && CIRCUIT[s.value]) || "unknown",
  }));
}

/** One upstream's live summary across replicas, for the tables. */
export interface UpstreamSummary {
  replicas: number;
  serving: number;
  endpoints: number;
  /** The replicas whose circuit is not closed, with its state. */
  notClosed: LiveCircuit[];
}

export function summarize(upstream: string, endpoints: readonly LiveEndpoint[], circuits: readonly LiveCircuit[]): UpstreamSummary {
  const mine = endpoints.filter((e) => e.upstream === upstream);
  const myCircuits = circuits.filter((c) => c.upstream === upstream);
  return {
    replicas: new Set([...mine.map((e) => e.replica), ...myCircuits.map((c) => c.replica)]).size,
    serving: mine.filter((e) => e.state === "serving").length,
    endpoints: mine.length,
    notClosed: myCircuits.filter((c) => c.state !== "closed"),
  };
}

/** "3 of 3 taking traffic on 1 replica", or that no gateway reports it. */
export function healthText(s: UpstreamSummary): string {
  if (s.replicas === 0) return "No gateway reports it";
  return `${s.serving} of ${s.endpoints} taking traffic on ${s.replicas} ${s.replicas === 1 ? "replica" : "replicas"}`;
}

/** "closed", or which replicas have it open or half-open. */
export function circuitText(s: UpstreamSummary): string {
  if (s.replicas === 0) return "–";
  if (s.notClosed.length === 0) return "closed";
  return s.notClosed.map((c) => `${c.state} on ${c.replica}`).join(", ");
}
