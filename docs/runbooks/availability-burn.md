# Availability SLO burn (NFR-11)

Alerts: **`SpillwayAvailabilityBurnFast`** (severity `page`) and
**`SpillwayAvailabilityBurnSlow`** (severity `ticket`), from
[`spillway.yaml`](../../deploy/prometheus/rules/spillway.yaml).

## What it means

The availability SLO is 99.9% of requests over 30 days without a
**gateway-caused 5xx**: `gw_requests_total{origin="gateway", code_class="5xx"}`
with `reason` other than `shed`, `circuit_open` and `fail_closed` (NFR-11).
Those three are deliberate answers (load shedding, an open circuit, a
fail-closed policy without Redis) and are watched separately; a 5xx the
upstream itself sent (`origin="upstream"`) is not the gateway's either. What
remains is mostly `reason="upstream"`: the gateway could not get an answer
from the upstream (connection refused or reset, timeout, no endpoint) and
answered 502, 503 or 504 itself.

- **Fast**: the error ratio is over 14.4 × 0.1% = 1.44% over the last hour
  and over the last 5 minutes; the month's budget would be gone in about two
  days. Act now.
- **Slow**: over 6 × 0.1% = 0.6% over 6 hours and over 30 minutes; gone in
  five days. Look today.

The ratio covers every gateway together; the alert carries no route or
replica label, so the first checks find where it comes from.

## First checks

1. Grafana, **Spillway / Overview**: the availability SLI and burn panels,
   then the status mix by `reason`. Which `route` and which `replica` carry
   the 5xx? In Prometheus:

   ```promql
   sum by (replica, route, reason) (rate(gw_requests_total{origin="gateway",code_class="5xx"}[5m]))
   ```

2. All replicas or one? One replica alone points at that gateway (its
   network, its upstream connections, a bad deploy); compare its
   `gw_upstream_duration_seconds` with the others on **Spillway / Upstreams**.
3. One upstream? **Spillway / Upstreams**: endpoint health
   (`gw_upstream_endpoint_healthy`), ejections, retries
   (`gw_retries_total{outcome="budget_exhausted"}` means the retry budget is
   spent), attempt latency against the route's timeouts. Click an exemplar
   on the latency panel to open a failing request's trace in Tempo: its
   attempt spans name each failure (`error.type`).
4. The gateway's access log (Loki, `{service_name="spillway-gateway"}`, or
   `docker compose logs gateway-1`): every 5xx is logged, with its route,
   upstream, attempts and trace ID.
5. Was there a deploy or a config publish just before? The control plane's
   **History** screen (or `GET /api/v1/config/versions`) lists publishes; a new
   route pointing at a wrong address or port shows up here.

## Mitigation

- **An upstream is failing**: fix or scale it; meanwhile its health checks
  and outlier detection take bad endpoints out (GW-08, GW-09), and the
  circuit breaker answers fast instead of timing out
  ([circuit-open.md](circuit-open.md)).
- **A config publish caused it** (wrong endpoint, timeouts too short):
  roll back to the previous version from the **History** screen or
  `POST /api/v1/config/versions/{v}/rollback`; gateways apply it within
  seconds (NFR-08).
- **One replica is bad**: restart it (`docker compose restart gateway-1`,
  or `kubectl --context kind-spillway -n spillway delete pod <pod>`; the
  Deployment replaces it and the others carry its traffic).
- The **slow** alert alone, with the fast one quiet, is a steady leak: find
  the route from step 1 and fix its upstream or timeouts at working pace.
