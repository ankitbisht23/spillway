// Code-based routes (ADR-0057): one route tree, no route generator.
import { createRootRoute, createRoute, createRouter, redirect, type ErrorComponentProps, type RouterHistory } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { describeError } from "../api/errors";
import type { OperationId } from "../api/roles.gen";
import { useCan } from "../auth/SessionContext";
import { ErrorState, NotAllowed, NotFound } from "../components/states";
import { Placeholder } from "../screens/Placeholder";
import { Layout } from "./Layout";
import { navigation, screenOp } from "./screens";

const rootRoute = createRootRoute({ component: Layout, notFoundComponent: () => <NotFound /> });

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/overview", replace: true });
  },
});

/**
 * A screen whose M6 task (m6-<task>, ADR-0056) has not landed yet: a
 * placeholder that calls no API. Each task replaces its own.
 */
function placeholder<P extends string>(path: P, title: string, task: string) {
  const nav = navigation.find((s) => s.to === path);
  const op = nav && screenOp(nav);
  const screen = () => <Placeholder title={title} task={task} />;
  return createRoute({ getParentRoute: () => rootRoute, path, component: op ? () => <Hidden op={op}>{screen()}</Hidden> : screen });
}

/**
 * A screen hidden from roles that cannot call op (ADR-0058): its URL shows
 * Not allowed, and nothing in it renders, so it calls no API.
 */
function Hidden({ op, children }: { op: OperationId; children: ReactNode }) {
  const { allowed, reason } = useCan(op);
  return allowed ? children : <NotAllowed message={`${reason ?? ""}.`} />;
}

export const routeTree = rootRoute.addChildren([
  indexRoute,
  placeholder("/overview", "Overview", "live-screens"),
  placeholder("/throttling", "Throttling", "live-screens"),
  placeholder("/policies", "Policies", "policies"),
  placeholder("/policies/new", "New policy", "policies"),
  placeholder("/policies/$id", "Policy", "policies"),
  placeholder("/policies/$id/shadow", "Shadow report", "policies"),
  placeholder("/publish", "Publish", "policies"),
  placeholder("/routes", "Routes", "routes"),
  placeholder("/routes/$id", "Route", "routes"),
  placeholder("/upstreams", "Upstreams", "routes"),
  placeholder("/upstreams/$id", "Upstream", "routes"),
  placeholder("/consumers", "Consumers & keys", "consumers-keys"),
  placeholder("/consumers/$id", "Consumer", "consumers-keys"),
  placeholder("/access-lists", "Access lists", "consumers-keys"),
  placeholder("/tenants", "Tenants", "consumers-keys"),
  placeholder("/replicas", "Replicas", "replicas-history"),
  placeholder("/history", "History", "replicas-history"),
  placeholder("/history/$version", "Config version", "replicas-history"),
  placeholder("/audit", "Audit log", "replicas-history"),
]);

/** A render error in a screen: the error state with a Reload button (UI-06). */
export function RouteError({ error }: ErrorComponentProps) {
  return <ErrorState view={{ ...describeError(error), retry: true }} onRetry={() => location.reload()} retryLabel="Reload" />;
}

export function createAppRouter(history?: RouterHistory) {
  return createRouter({ routeTree, history, defaultErrorComponent: RouteError });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
