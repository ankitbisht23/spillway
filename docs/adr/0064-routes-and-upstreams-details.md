# 0064. Routes and upstreams: details

- **Status:** Accepted
- **Date:** 2026-11-16
- **Requirement IDs:** UI-03, UI-04, UI-06, CP-14, GW-08, GW-09, GW-14

## Context

The routes task builds the Routes & upstreams screen with ADR-0060's row of the screen table (route and upstream tables, edit, create and delete, drain, undrain, live endpoint and circuit state) and ADR-0059's `routes.endpoints` and `routes.circuits` with the new `gw_upstream_endpoint_ejected` gauge. Building them left questions ADR-0056 – ADR-0063 do not settle:

- The gateway labels `gw_upstream_endpoint_healthy` (and so the new gauge, which has the same labels, ADR-0059) by the endpoint's **resolved** address (`ip:port`, GW-06), while the config names an endpoint by its configured address (`upstream-1:8080`). One host name can resolve to several addresses, and the control plane cannot resolve names as each gateway does.
- A route has about twenty-five settings, an upstream about twenty-five more, several of them lists (methods, header transforms, header predicates, endpoints). A form that showed only some would send the others back unset and silently drop them.
- The upstream's request body carries `draining` per endpoint, and a PUT keeps an endpoint's ID when it keeps its address (the spec's `Endpoint.id`).
- ADR-0060 asks every action that removes or revokes something to confirm first; drain removes an endpoint from traffic, but only once published, and undrain reverses it.
- ADR-0061's `publish.spec.ts` edits a route header in the Routes screen; the 412 in it comes from an API edit of "another route's header", and the e2e fixture has one route.
- `routes.*` take no parameters, so their key is the name alone (ADR-0059); the `sub` enum gains two values, which oasdiff reports as non-breaking on a request parameter (checked: `make lint-api` passes).

## Decision

- **Live state by resolved address.** The upstream page shows each replica's endpoints as the gateways report them: replica, resolved address and state, and says that gateways report an endpoint by the address its host name resolves to. The configured endpoints (with drain and undrain) are a separate table. No label is added to either gauge: that would change the design's metrics table and the series' identity for a display convenience. States: `healthy 1` is "Taking traffic"; `healthy 0, ejected 1` "Ejected (outlier detection)"; `healthy 0, ejected 0` "Failing health checks"; anything else (a missing or non-finite sample) "Unknown". Circuits: 0 closed, 1 open, 2 half-open.
- **`routes.endpoints`** answers a vector of `max by (replica, upstream, endpoint) (gw_upstream_endpoint_healthy)` and the same of `…_ejected`, each sample labelled `stat` = `healthy` or `ejected`; **`routes.circuits`** a vector of `max by (replica, upstream) (gw_circuit_state)`. Both sorted by their labels so the screen's rows keep their order. `max by` drops `job` and `instance`; each (replica, upstream, endpoint) has one series.
- **Tables**: Routes (name, match, upstream linked to the upstreams list filtered by its exact name, upstream health and circuit across replicas, auth mode, priority) and Upstreams (name, endpoints with the draining count, load balancing, health, circuit), each a cursor page of 50 with the exact `name` filter (ADR-0036, ADR-0060), under one navigation item with two tabs. A table shows "…" until both live subscriptions answer and "Unavailable" when either fails; the config columns never wait for Prometheus.
- **Forms are field lists over the entity.** Each field names a path in the entity (`timeouts.connect`, `response_headers.set`) and a kind: text, select, whole number, number, checkbox, comma list, header lines (`Name: value`), header predicates (`Name: value` exact, `Name ~ regex`), endpoint lines (`host:port`). Every setting of the spec's `Route` and `Upstream` has a field. A save starts from the entity as loaded (its writable settings), sets each field's value at its path, unsets a field left empty and drops objects left empty; a setting the form does not know is sent back as it was. The browser checks the name rule, numbers, required fields and line formats; everything else is the server's, whose 422 details go to the field their `path` names, else to the field whose path the message starts with (`endpoint "…"` to Endpoints), else under the form. A kept endpoint address keeps its draining state; a new one takes traffic.
- **Drain and undrain** act on one endpoint under the upstream's ETag: drain is `drainEndpoint`; undrain is `updateUpstream` with the upstream as loaded and that endpoint's `draining` unset. Neither asks to confirm: each changes only the working set, the other undoes it before or after a publish, and the publish page's diff and note are the review. Both are disabled with "Save or discard your edits first" while the form has unsaved edits (as promote, ADR-0063), so neither drops edits; a 412 shows Reload, which loads the current version. Delete confirms, as ADR-0060 says.
- **Playwright**: `drain.spec.ts` (test-admin) drains `upstream-2:8080` of upstream `e2e` from the navigation, checks the publish page's diff (`+ draining: true`), publishes with a note, checks the version's diff through the API, then undrains and publishes so the run ends where it started; the live view shows its empty state, since make dev's gateways run no `e2e` upstream. `publish.spec.ts` now edits `e2e-orders`' response header in the Routes screen; the fixture gains route `e2e-other`, which the API edits between preview and click. `roles.spec.ts` checks New route, a route's Save, Drain and an upstream's Save per role.

## Consequences

- Operators see which replica ejected or failed which address; matching an address to a configured host name is theirs when names resolve to several addresses.
- New settings added to the spec round-trip through the forms unchanged until a field is added for them.
- verify-m6's route edit (ADR-0061) uses the "Response headers to set" field.

## Alternatives considered

- **A `host` label (the configured address) on the endpoint gauges** — a change to two metrics' labels in the design for a display join.
- **Matching resolved addresses to endpoints in the control plane** — it would resolve names from its own network, not the gateways'.
- **A form of the common settings only** — would drop or hide the rest; a raw YAML or JSON editor — no field-level checks or errors.
- **A confirmation for drain** — a dialog in front of a working-set edit that the next screen's diff already reviews.
