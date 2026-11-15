import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, expect, vi } from "vitest";

import { server } from "./server";

beforeAll(() => server.listen({ onUnhandledFrame: "error" }));

// jsdom has no layout, so no scrolling: the router resets the scroll
// position on every navigation, which jsdom reports as not implemented.
if (typeof window !== "undefined") window.scrollTo = () => {};
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
