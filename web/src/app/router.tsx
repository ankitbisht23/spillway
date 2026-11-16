// Code-based routes (ADR-0057): one route tree, no route generator.
import { createRootRoute, createRoute, createRouter, redirect, type ErrorComponentProps, type RouterHistory } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { describeError } from "../api/errors";
import type { OperationId } from "../api/roles.gen";
import { useCan } from "../auth/SessionContext";
import { ErrorState, NotAllowed, NotFound } from "../components/states";
import { AccessListList } from "../screens/consumers/AccessLists";
import { ConsumerList } from "../screens/consumers/ConsumerList";
import { ConsumerScreen, NewConsumer } from "../screens/consumers/ConsumerScreen";
import { PlanList } from "../screens/consumers/PlanList";
import { NewTenant, TenantList, TenantScreen } from "../screens/consumers/Tenants";
import { Overview } from "../screens/live/Overview";
import { overviewSearch, throttlingSearch } from "../screens/live/search";
import { Throttling } from "../screens/live/Throttling";
import { Placeholder } from "../screens/Placeholder";
import { NewPolicy, PolicyScreen } from "../screens/policies/PolicyEditor";
import { PolicyList } from "../screens/policies/PolicyList";
import { WINDOWS, type Window } from "../screens/policies/queries";
import { ShadowReportScreen } from "../screens/policies/ShadowReportScreen";
import { Publish } from "../screens/publish/Publish";
import { NewRoute, RouteScreen } from "../screens/routes/RouteEditor";
import { RouteList } from "../screens/routes/RouteList";
import { UpstreamList } from "../screens/routes/UpstreamList";
import { NewUpstream, UpstreamScreen } from "../screens/routes/UpstreamScreen";
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

/** A list's search: its page's cursor (D9) and an exact name filter (ADR-0036); every key set (ADR-0062). */
function listSearch(s: Record<string, unknown>): { cursor?: string; name?: string } {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  return { cursor: str(s.cursor), name: str(s.name) };
}
/** An entity page's search: saved marks one the form just created, for its banner. */
function savedSearch(s: Record<string, unknown>): { saved?: true } {
  return { saved: s.saved === true ? true : undefined };
}

/** Routes & upstreams (m6-routes). */
const routesRoute = createRoute({ getParentRoute: () => rootRoute, path: "/routes", validateSearch: listSearch, component: RouteList });
const newRouteRoute = createRoute({ getParentRoute: () => rootRoute, path: "/routes/new", component: NewRoute });
const routeRoute = createRoute({ getParentRoute: () => rootRoute, path: "/routes/$id", validateSearch: savedSearch, component: RouteScreen });
const upstreamsRoute = createRoute({ getParentRoute: () => rootRoute, path: "/upstreams", validateSearch: listSearch, component: UpstreamList });
const newUpstreamRoute = createRoute({ getParentRoute: () => rootRoute, path: "/upstreams/new", component: NewUpstream });
const upstreamRoute = createRoute({ getParentRoute: () => rootRoute, path: "/upstreams/$id", validateSearch: savedSearch, component: UpstreamScreen });

/** Consumers & keys (m6-consumers-keys). */
const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
const id = (v: unknown) => {
  const n = Number(v);
  return v !== undefined && v !== "" && Number.isSafeInteger(n) && n >= 1 ? n : undefined;
};
const consumersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/consumers",
  validateSearch: (s: Record<string, unknown>): { cursor?: string; name?: string; tenant_id?: number } => ({ ...listSearch(s), tenant_id: id(s.tenant_id) }),
  component: ConsumerList,
});
const newConsumerRoute = createRoute({ getParentRoute: () => rootRoute, path: "/consumers/new", component: NewConsumer });
/**
 * A consumer: keys and overrides each have their page's cursor; saved
 * marks one the form just created. Throttling's Add override opens it
 * with override_policy, the policy to prefill an override for.
 */
const consumerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/consumers/$id",
  validateSearch: (s: Record<string, unknown>): { override_policy?: number; keys?: string; overrides?: string; saved?: true } => ({
    override_policy: id(s.override_policy), keys: str(s.keys), overrides: str(s.overrides), ...savedSearch(s),
  }),
  component: ConsumerScreen,
});
const ACTIONS = ["allow", "deny"] as const;
const SUBJECTS = ["key", "consumer", "cidr"] as const;
const accessListsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/access-lists",
  validateSearch: (s: Record<string, unknown>): { cursor?: string; action?: (typeof ACTIONS)[number]; subject_type?: (typeof SUBJECTS)[number] } => ({
    cursor: str(s.cursor),
    action: ACTIONS.find((a) => a === s.action),
    subject_type: SUBJECTS.find((t) => t === s.subject_type),
  }),
  component: AccessListList,
});
const plansRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/plans",
  validateSearch: (s: Record<string, unknown>): { cursor?: string } => ({ cursor: str(s.cursor) }),
  component: PlanList,
});
/** Tenants: hidden from every role but admin (ADR-0058), so no API is called for them. */
const tenantsRoute = createRoute({
  getParentRoute: () => rootRoute, path: "/tenants", validateSearch: listSearch, component: () => <Hidden op="createTenant"><TenantList /></Hidden>,
});
const newTenantRoute = createRoute({ getParentRoute: () => rootRoute, path: "/tenants/new", component: () => <Hidden op="createTenant"><NewTenant /></Hidden> });
const tenantRoute = createRoute({
  getParentRoute: () => rootRoute, path: "/tenants/$id", validateSearch: savedSearch, component: () => <Hidden op="createTenant"><TenantScreen /></Hidden>,
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
  routesRoute,
  newRouteRoute,
  routeRoute,
  upstreamsRoute,
  newUpstreamRoute,
  upstreamRoute,
  consumersRoute,
  newConsumerRoute,
  consumerRoute,
  accessListsRoute,
  plansRoute,
  tenantsRoute,
  newTenantRoute,
  tenantRoute,
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
