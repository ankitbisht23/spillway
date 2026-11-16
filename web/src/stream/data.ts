// A stream result's data by shape (ADR-0059): each catalogue query answers
// one of them; anything else is treated as no data.
import type { components } from "../api/schema.gen";
import type { Matrix, Vector } from "../charts/series";

export type HeavyHitter = components["schemas"]["HeavyHitter"];

function isObject(d: unknown): d is Record<string, unknown> {
  return typeof d === "object" && d !== null;
}

export function asMatrix(d: unknown): Matrix | null {
  return isObject(d) && d.kind === "matrix" && Array.isArray(d.series) ? (d as unknown as Matrix) : null;
}

export function asVector(d: unknown): Vector | null {
  return isObject(d) && d.kind === "vector" && Array.isArray(d.series) ? (d as unknown as Vector) : null;
}

export function asHeavyHitters(d: unknown): HeavyHitter[] | null {
  return isObject(d) && Array.isArray(d.items) ? (d.items as HeavyHitter[]) : null;
}

export type StreamReplica = components["schemas"]["StreamReplica"];

/** The replicas subscription's rows: each replica with its staleness on Postgres's clock. */
export function asReplicas(d: unknown): StreamReplica[] | null {
  return isObject(d) && Array.isArray(d.items) ? (d.items as StreamReplica[]) : null;
}
