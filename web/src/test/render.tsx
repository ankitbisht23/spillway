import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, render } from "@testing-library/react";
import type { ReactNode } from "react";

import { createAppRouter } from "../app/router";

/** Renders ui inside a router, for components that link. */
export async function renderWithRouter(ui: ReactNode) {
  const router = createRouter({ routeTree: createRootRoute({ component: () => ui }), history: createMemoryHistory() });
  await act(() => router.load());
  return render(<RouterProvider router={router} />);
}

/** The app's router at path. */
export function appRouterAt(path: string) {
  return createAppRouter(createMemoryHistory({ initialEntries: [path] }));
}
