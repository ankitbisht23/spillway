// Stream data for charts and tables (UI-03, ADR-0059): a matrix's series
// aligned on one time axis for uPlot, the latest value of a series, a
// vector grouped by a label, and number formats.
import type { components } from "../api/schema.gen";

export type Matrix = components["schemas"]["StreamMatrix"];
export type Vector = components["schemas"]["StreamVector"];
export type RangeSeries = components["schemas"]["StreamRangeSeries"];

/** One line of a chart: its label and its values on the chart's time axis. */
export interface Line {
  label: string;
  values: (number | null)[];
}

/** Lines on one time axis (unix seconds, ascending), as uPlot takes them. */
export interface Aligned {
  times: number[];
  lines: Line[];
}

/**
 * The series of m on the union of their timestamps, labelled by label and
 * ordered by order (then by label); a series without a point at a time has
 * null there. scale multiplies every value (1000 for seconds to ms).
 */
export function align(m: Matrix, label: (s: RangeSeries) => string, scale = 1, order: (a: string, b: string) => number = compare): Aligned {
  const times = [...new Set(m.series.flatMap((s) => s.points.map((p) => p[0])))]
    .filter((t): t is number => typeof t === "number")
    .sort((a, b) => a - b);
  const index = new Map(times.map((t, i) => [t, i]));
  const lines = m.series.map((s) => {
    const values: (number | null)[] = times.map(() => null);
    for (const [t = null, v = null] of s.points) {
      const i = t === null ? undefined : index.get(t);
      if (i !== undefined) values[i] = v === null ? null : v * scale;
    }
    return { label: label(s), values };
  });
  lines.sort((a, b) => order(a.label, b.label));
  return { times, lines };
}

function compare(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** The last value of values that is not null, if any. */
export function latest(values: readonly (number | null)[]): number | null {
  for (let i = values.length - 1; i >= 0; i--) {
    const v = values[i];
    if (v !== null && v !== undefined) return v;
  }
  return null;
}

/** Whether a chart of a has nothing to draw: no series or only nulls. */
export function isEmptyAligned(a: Aligned): boolean {
  return a.lines.every((l) => l.values.every((v) => v === null));
}

/** A vector's samples grouped by the label by, each with its stat values. */
export function byLabel(v: Vector, by: string): { name: string; stats: Record<string, number | null> }[] {
  const rows = new Map<string, Record<string, number | null>>();
  for (const s of v.series) {
    const name = s.labels[by];
    const stat = s.labels.stat;
    if (name === undefined || stat === undefined) continue;
    const row = rows.get(name) ?? {};
    row[stat] = s.value;
    rows.set(name, row);
  }
  return [...rows].map(([name, stats]) => ({ name, stats }));
}

/** A rate or count: up to three significant digits, "–" for none. */
export function formatNumber(v: number | null | undefined): string {
  if (v === null || v === undefined) return "–";
  if (v === 0) return "0";
  const abs = Math.abs(v);
  if (abs >= 1000) return Math.round(v).toLocaleString("en-US");
  if (abs >= 100) return v.toFixed(0);
  if (abs >= 10) return v.toFixed(1);
  if (abs >= 0.01) return v.toFixed(2);
  return "<0.01";
}

/** A share in [0, 1] as a percentage. */
export function formatShare(v: number | null | undefined): string {
  if (v === null || v === undefined) return "–";
  const p = v * 100;
  if (p === 0) return "0 %";
  if (p < 0.1) return "<0.1 %";
  return `${p < 10 ? p.toFixed(1) : p.toFixed(0)} %`;
}

/** Seconds as milliseconds. */
export function formatMs(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return "–";
  const ms = seconds * 1000;
  return `${ms < 10 ? ms.toFixed(1) : ms.toFixed(0)} ms`;
}

/** A count, with thousands separators. */
export function formatCount(n: number): string {
  return n.toLocaleString("en-US");
}
