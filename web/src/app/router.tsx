// Code-based routes (ADR-0057): one route tree, no route generator.
import { createRootRoute, createRoute, createRouter, redirect, type ErrorComponentProps, type RouterHistory } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { describeError } from "../api/errors";
import type { OperationId } from "../api/roles.gen";
import { useCan } from "../auth/SessionContext";
import { ErrorState, NotAllowed, NotFound } from "../components/states";
import { Overview } from "../screens/live/Overview";
import { overviewSearch, throttlingSearch } from "../screens/live/search";
import { Throttling } from "../screens/live/Throttling";
import { Placeholder } from "../screens/Placeholder";
import { NewPolicy, PolicyScreen } from "../screens/policies/PolicyEditor";
import { PolicyList } from "../screens/policies/PolicyList";
import { WINDOWS, type Window } from "../screens/policies/queries";
import { ShadowReportScreen } from "../screens/policies/ShadowReportScreen";
import { Publish } from "../screens/publish/Publish";
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

const overviewRoute = createRoute({ getParentRoute: () => rootRoute, path: "/overview", validateSearch: overviewSearch, component: Overview });
const throttlingRoute = createRoute({ getParentRoute: () => rootRoute, path: "/throttling", validateSearch: throttlingSearch, component: Throttling });

/** Policies (m6-policies): the list's page is its cursor (D9). */
const policiesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies",
  validateSearch: (s: Record<string, unknown>): { cursor?: string } => ({ cursor: typeof s.cursor === "string" && s.cursor ? s.cursor : undefined }),
  component: PolicyList,
});
const newPolicyRoute = createRoute({ getParentRoute: () => rootRoute, path: "/policies/new", component: NewPolicy });
/** A policy; saved marks one the editor just created, for its banner. */
const policyRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies/$id",
  validateSearch: (s: Record<string, unknown>): { saved?: true } => ({ saved: s.saved === true ? true : undefined }),
  component: PolicyScreen,
});
const shadowRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies/$id/shadow",
  validateSearch: (s: Record<string, unknown>): { window?: Window } => ({
    window: WINDOWS.some((w) => w.value === s.window) ? (s.window as Window) : undefined,
  }),
  component: ShadowReportScreen,
});
const publishRoute = createRoute({ getParentRoute: () => rootRoute, path: "/publish", component: Publish });

/**
 * A consumer's screen (m6-consumers-keys). Throttling's Add override opens
 * it with override_policy, the policy to prefill an override for.
 */
const consumerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/consumers/$id",
  validateSearch: (s: Record<string, unknown>): { override_policy?: number } => {
    const p = Number(s.override_policy);
    return { override_policy: Number.isSafeInteger(p) && p >= 1 ? p : undefined };
  },
  component: () => <Placeholder title="Consumer" task="consumers-keys" />,
});

export const routeTree = rootRoute.addChildren([
  indexRoute,
  overviewRoute,
  throttlingRoute,
  policiesRoute,
  newPolicyRoute,
  policyRoute,
  shadowRoute,
  publishRoute,
  placeholder("/routes", "Routes", "routes"),
  placeholder("/routes/$id", "Route", "routes"),
  placeholder("/upstreams", "Upstreams", "routes"),
  placeholder("/upstreams/$id", "Upstream", "routes"),
  placeholder("/consumers", "Consumers & keys", "consumers-keys"),
  consumerRoute,
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
