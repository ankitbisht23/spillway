import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, expect, vi } from "vitest";

import { server } from "./server";

beforeAll(() => server.listen({ onUnhandledFrame: "error" }));

// jsdom has no layout, so no scrolling: the router resets the scroll
// position on every navigation, which jsdom reports as not implemented.
if (typeof window !== "undefined") window.scrollTo = () => {};

// jsdom has no canvas, Path2D or matchMedia, which uPlot uses. Charts are
// checked through their HTML legend, so a canvas that draws nothing will
// do: every method of its context and of a Path2D does nothing.
if (typeof window !== "undefined") {
  const inert = (): object => {
    const store: Record<PropertyKey, unknown> = {};
    return new Proxy(store, {
      get: (_t, p) => (p in store ? store[p] : p === "measureText" ? () => ({ width: 0 }) : () => undefined),
      set: (_t, p, v) => {
        store[p] = v;
        return true;
      },
    });
  };
  HTMLCanvasElement.prototype.getContext = (() => inert()) as unknown as HTMLCanvasElement["getContext"];
  globalThis.Path2D ??= function Path2D() {
    return inert();
  } as unknown as typeof Path2D;
  window.matchMedia ??= (media: string) => ({
    matches: false, media, onchange: null, addEventListener: () => {}, removeEventListener: () => {},
    addListener: () => {}, removeListener: () => {}, dispatchEvent: () => false,
  });
}
afterAll(() => server.close());

// No test may log an error or a warning (ADR-0061): React reports render
// errors and bad hooks there, and a token or a key must never be logged.
let logged: unknown[][] = [];
beforeEach(() => {
  logged = [];
  for (const level of ["error", "warn"] as const) {
    vi.spyOn(console, level).mockImplementation((...args: unknown[]) => void logged.push([level, ...args]));
  }
});

afterEach(() => {
  cleanup();
  server.resetHandlers();
  vi.useRealTimers();
  expect(logged, "console.error or console.warn was called").toEqual([]);
});
