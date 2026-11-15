// A live line chart (UI-03): uPlot on a canvas, with an HTML legend that
// names each line and its latest value, so the numbers are readable as
// text too. uPlot styles its elements through the CSSOM only, which the
// CSP's style-src 'self' allows (ADR-0057); its stylesheet is bundled.
import "uplot/dist/uPlot.min.css";

import { useEffect, useRef } from "react";
import uPlot from "uplot";

import { formatMs, formatNumber, formatShare, latest, type Aligned } from "./series";

export type Unit = "rps" | "ms" | "share";

/** The chart palette; styles.css has a .swatch-N class per entry. */
export const PALETTE = ["#1f4e79", "#2e7d32", "#b26a00", "#a4262c", "#6a3d9a", "#00838f", "#5b6676"] as const;

const AXIS = "#5b6676";
const GRID = "#e5e8ec";
const HEIGHT = 200;

export function formatUnit(unit: Unit, v: number | null | undefined): string {
  switch (unit) {
    case "rps":
      return v === null || v === undefined ? "–" : `${formatNumber(v)}/s`;
    case "ms":
      return v === null || v === undefined ? "–" : formatMs(v / 1000);
    case "share":
      return formatShare(v);
  }
}

interface Props {
  /** The chart's accessible name. */
  title: string;
  data: Aligned;
  unit: Unit;
  /** A palette index per line label; by default the line's position. */
  tone?: (label: string, i: number) => number;
}

export function LineChart({ title, data, unit, tone = (_, i) => i }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const colors = data.lines.map((l, i) => tone(l.label, i) % PALETTE.length);
  // The plot's identity: its lines and their colours, as a string so that
  // an equal set from a new result does not rebuild it.
  const shape = JSON.stringify(data.lines.map((l, i) => [l.label, colors[i]]));

  // A new set of lines builds a new plot; new values on the same lines
  // only replace its data.
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const lines = JSON.parse(shape) as [string, number][];
    const u = new uPlot(
      {
        width: el.clientWidth || 600,
        height: HEIGHT,
        legend: { show: false },
        cursor: { drag: { x: false, y: false } },
        scales: { y: { range: (_u, _min, max) => [0, max > 0 ? max * 1.1 : unit === "share" ? 0.01 : 1] } },
        axes: [
          { stroke: AXIS, grid: { stroke: GRID }, ticks: { stroke: GRID } },
          { stroke: AXIS, grid: { stroke: GRID }, ticks: { stroke: GRID }, size: 64, values: (_u, splits) => splits.map((v) => formatUnit(unit, v)) },
        ],
        series: [{}, ...lines.map(([label, color]) => ({ label, stroke: PALETTE[color], width: 2 }))],
      },
      [[], ...lines.map(() => [])],
      el,
    );
    plot.current = u;
    const ro = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(() => {
      if (el.clientWidth > 0) u.setSize({ width: el.clientWidth, height: HEIGHT });
    });
    ro?.observe(el);
    return () => {
      ro?.disconnect();
      u.destroy();
      plot.current = null;
    };
  }, [shape, unit]);

  useEffect(() => {
    plot.current?.setData([data.times, ...data.lines.map((l) => l.values)] as uPlot.AlignedData);
  }, [data, shape, unit]);

  return (
    <figure className="chart" aria-label={title}>
      <div ref={box} className="chart-canvas" />
      <figcaption>
        <ul className="legend" aria-label={`${title}: latest values`}>
          {data.lines.map((l, i) => (
            <li key={l.label}>
              <span className={`swatch swatch-${colors[i] ?? 0}`} aria-hidden="true" />
              <span className="legend-label">{l.label}</span> <span className="legend-value">{formatUnit(unit, latest(l.values))}</span>
            </li>
          ))}
        </ul>
      </figcaption>
    </figure>
  );
}
