# 0104. Demo Grafana and the demo proxy: implementation details

- **Status:** Accepted
- **Date:** 2026-11-28
- **Requirement IDs:** SC-05, SC-09, OB-01, OB-04

## Context

ADR-0096 §4 settles the demo Grafana (`grafana-demo`) and its Demo dashboard, and ADR-0097 §1 the demo proxy (Caddy on 8888, local only until `make demo-share`), both built by the grafana-demo task (ADR-0093). Building them showed two places where the accepted text does not work as written, and left a few details open. Accepted ADRs are not edited, so the choices are recorded here.

## Decision

1. **No plugin installs at start (`GF_PLUGINS_PREINSTALL_DISABLED=true`).** Grafana 13.3.0 runs a background installer at every start that downloads newer versions of its bundled plugins (Prometheus, Loki, Tempo, CloudWatch and others) from grafana.com into `/var/lib/grafana/plugins`. On `grafana-demo`, whose `/var/lib/grafana` is a tmpfs as ADR-0096 §4 requires (and Docker mounts tmpfs `noexec`), the updated Prometheus plugin's backend could not start ("fork/exec … permission denied"), so the datasource was unregistered and every panel showed "Datasource prometheus was not found". With the installer disabled, `grafana-demo` uses only the plugins in the pinned image: no download from the internet at start, and the plugin versions are the image's. This setting is added to ADR-0096 §4's list. The dev Grafana does the same downloads today (its root filesystem is writable, so they work); it stays unchanged here, as ADR-0096 says, and is reported for review.
2. **The proxy refuses `/auth/admin` with a `handle` block.** ADR-0097 §1's Caddyfile has `respond @kcadmin 404` next to `handle /auth/* { reverse_proxy keycloak:8080 … }`. Caddy orders directives by its fixed directive order, in which `handle` comes before `respond`, so every `/auth/admin` request reached Keycloak (`/auth/admin/` answered 302 to the admin console). `deploy/demo/Caddyfile` has instead:

   ```caddyfile
   @kcadmin path /auth/admin /auth/admin/*
   handle @kcadmin {
   	respond 404
   }
   ```

   Sibling `handle` blocks are mutually exclusive, and `caddy adapt` puts this one first. The rest of the Caddyfile is ADR-0097's. Through the proxy, `/auth/admin`, `/auth/admin/`, `/auth/admin/realms/spillway`, `/auth//admin/`, `/auth/./admin/`, `/auth/ADMIN/`, `/auth/%61dmin/`, `/auth/admin%2F` and `/auth/realms/spillway/../../admin/` all answer 404; `test/demo` checks four of them on every run.
3. **Files.** The dashboard is `deploy/grafana/demo/demo.json` (ADR-0096 §4); its provisioning is `deploy/grafana/demo/provisioning/datasources/datasources.yaml` (Prometheus only, uid `prometheus`, not editable) and `deploy/grafana/demo/provisioning/dashboards/demo.yaml` (one file provider for `/var/lib/grafana-demo`). Compose mounts the two provisioning directories and `demo.json` alone (at `/var/lib/grafana-demo/demo.json`, which is also `GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH`), each read-only, so the provider sees exactly one dashboard. kind does not get the demo Grafana (ADR-0096 §4); `make kind-smoke`'s count stays five.
4. **Hardening beyond ADR-0096 §4.** `grafana-demo` runs with a read-only root filesystem and tmpfs `/var/lib/grafana` and `/tmp` (owned by Grafana's user 472), and its healthcheck asks `/grafana/api/health` (it serves from the sub-path). Neither `grafana-demo` nor `caddy` has compose's `env_file` (`.env.local`): they need none of its variables (compose's `${…}` interpolation does not use `env_file`), and a dev setting or secret put there for other services, such as a `GF_` key meant for the dev Grafana, must not reach the two anonymous surfaces; `TestDemoGrafana` checks it. Grafana's home page for the file-based home dashboard is `/grafana/d/default-home-dashboard/home`; the provisioned dashboard is also at `/grafana/d/spillway-demo`. Both show the same panels to anonymous Viewers.
5. **The dashboard's layout.** Nine panels on the 24-column grid, the queries exactly ADR-0096 §4's table (eleven targets): the replica count (stat) beside requests per replica; allowed and denied per tenant; p99 per tenant and refused otherwise per tenant; limiter mode per policy and limiter backend with Redis errors; upstream protection across the bottom, the circuit state on a right axis from 0 to 2 (closed, open, half-open). No links to other dashboards (the demo Grafana has none) and no variables.
6. **Caddy only, no other share settings yet.** This task adds the `caddy` service with ADR-0097 §1's settings and fixed address, and `grafana-demo`'s `GF_SERVER_ROOT_URL` from `DEMO_PUBLIC_URL` with the local default. Keycloak's proxy settings (`KC_PROXY_HEADERS`, `KC_PROXY_TRUSTED_ADDRESSES`), `DEMO_KC_HOSTNAME`, `DEMO_ISSUER` and the control plane's `SPILLWAY_CP_TRUSTED_PROXIES` belong to the share and land with demo-share: locally Keycloak's hostname is fixed (`http://localhost:8180/auth`), so logins and the UI work through 8888 without them, and Keycloak is not overridden (so `make demo` does not recreate it).
7. **Tests.** `make lint-obs` (`test/obs`): `TestDashboards`' rules moved into `checkDashboard`, which `TestDemoDashboard` applies to `demo.json` along with uid, title, refresh, time range and ADR-0096's exact query list; `TestDemoGrafana` checks the provisioning (one datasource, Prometheus; one provider) and the compose settings (image = compose's Grafana, profile `demo`, no host port, ADR-0096's settings and this ADR's, read-only mounts, no `env_file` on either service, Caddy on `127.0.0.1:8888` only, no `GF_AUTH_` setting on the dev Grafana). `make test-demo` (`test/demo`): `TestDemoProxyAndGrafana` (what an anonymous visitor reaches through 8888 and what is refused, the dev Grafana still refusing anonymous requests) and `TestGrafanaDemoPanels`, which runs every panel query through the demo Grafana's query API as an anonymous Viewer over the base load plus `make demo-d5` and `make demo-d6` (the base load alone gives no 429, Redis error, `fail_closed`, `circuit_open` or retry) and requires from each a value above 0 in the run (a series an earlier run left at 0 proves nothing), the three tenants on allowed, a replica count of 3 and gateway-1 … gateway-3 on the per-replica panels. Host stalls cannot fail it: it needs any value over about three minutes of traffic, sampled every 5 s with 30 s rate windows.

## Consequences

- `make demo` serves the Demo dashboard at `http://localhost:8888/grafana` to anyone on the machine without a login, with nothing but the dashboard and Prometheus behind it.
- `make test-demo` grows by about three minutes (`TestGrafanaDemoPanels`).
- The demo-share task builds on this Caddyfile and these settings; its s5 and s8 checks (ADR-0097 §4) test the same things through the stand-in tunnel.

## Alternatives considered

- **Keep the installer and mount the tmpfs `exec`** — the demo would download code from grafana.com on every start and run plugin versions no one pinned.
- **`/var/lib/grafana` on a volume instead of tmpfs** — the same downloads, and state a visitor could leave behind would outlive the container.
- **Put `respond @kcadmin 404` inside `handle /auth/*`** — works too, but a sibling `handle` block makes the refusal visible at the top level, as ADR-0097 meant it.
- **Check panel data by querying Prometheus directly** — would not test what a visitor's dashboard does: the proxy, anonymous Viewer access and the datasource.
