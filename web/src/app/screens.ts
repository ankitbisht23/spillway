// The navigation (docs/design.md's screen table, ADR-0057's routes).

/** The screens in the navigation, in its order. */
export const navigation = [
  { to: "/overview", label: "Overview" },
  { to: "/throttling", label: "Throttling" },
  { to: "/policies", label: "Policies" },
  { to: "/publish", label: "Publish" },
  { to: "/routes", label: "Routes & upstreams" },
  { to: "/consumers", label: "Consumers & keys" },
  { to: "/tenants", label: "Tenants" },
  { to: "/replicas", label: "Replicas" },
  { to: "/history", label: "History" },
  { to: "/audit", label: "Audit" },
] as const;
