import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, render } from "@testing-library/react";
import type { ReactNode } from "react";

import { createQueryClient } from "../api/queryClient";
import { App } from "../app/App";
import { createAppRouter } from "../app/router";
import type { Role } from "../auth/permissions";
import { SessionContext } from "../auth/SessionContext";

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

/** The app at path inside a session of a user with roles. */
export function renderApp(path: string, roles: Role[] = ["admin"], signOut: () => void = () => {}) {
  const router = appRouterAt(path);
  const user = { subject: "sub-1", name: "test-user", roles };
  render(
    <SessionContext value={{ user, signOut }}>
      <App router={router} queryClient={createQueryClient()} />
    </SessionContext>,
  );
  return router;
}
