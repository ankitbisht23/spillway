# Circuit open

Alert: **`SpillwayCircuitOpen`** (severity `page`), from
[`spillway.yaml`](../../deploy/prometheus/rules/spillway.yaml), per gateway
and upstream (`replica`, `upstream` labels).

## What it means

An upstream's circuit breaker on a gateway has not been closed for 5 minutes
(`gw_circuit_state > 0`: 1 open, 2 half-open). The breaker opens when too
many of the upstream's recent attempts failed (GW-14: over its `circuit`
settings' `error_rate` of the attempts in its `window`, once it holds
`min_requests`); while open, the gateway answers that
upstream's requests **503 `circuit_open`** at once, without trying it. After
`open_for` (10 s by default) it lets a few trial requests through
(half-open); if they fail it opens again. Five minutes of this means the
upstream has not recovered, not a blip.

`circuit_open` answers are deliberate and excluded from the availability SLI
(NFR-11), so this alert is the one that reports them.

## First checks

1. Grafana, **Spillway / Upstreams**: circuit state, endpoint health
   (`gw_upstream_endpoint_healthy`) and ejections
   (`gw_upstream_endpoint_ejected`) for that upstream, and its attempt
   latency and failures before the circuit opened.
2. Is the upstream up? Every endpoint unhealthy means it is down or
   unreachable from the gateways; on kind, check its pods and Service, and
   the gateway's upstream allow-list (`SPILLWAY_GATEWAY_ALLOWED_NETWORKS`,
   SEC-06) if its addresses changed.
3. All replicas or one? One gateway alone with an open circuit points at its
   own network path or connection cap (`max_pending`, `max_connections`
   in the upstream's `circuit` settings: a full cap answers `circuit_open`
   too).
4. The gateway's log and the traces of failed attempts (exemplars on
   **Spillway / Upstreams**' latency panel) name the failures: refused,
   reset, timeout or 5xx.

## Mitigation

- **The upstream is down or failing**: fix or scale it. The breaker closes
  by itself once trial requests succeed; nothing needs resetting.
- **The caps are too low for the traffic** (one gateway, no upstream
  failures): raise the upstream's `circuit` caps in a new config version.
- **The breaker is too eager** (opens on a few slow requests): raise
  `min_requests` or `error_rate`, or the route's timeouts, in a new
  config version; or roll back a recent change that lowered them.
- **A wrong address after a publish**: roll back from the **History** screen.
