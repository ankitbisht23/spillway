# Latency SLO burn

Alerts: **`SpillwayLatencyBurnFast`** (severity `page`) and
**`SpillwayLatencyBurnSlow`** (severity `ticket`), from
[`spillway.yaml`](../../deploy/prometheus/rules/spillway.yaml).

## What it means

The latency SLO is 99% of requests answered within 0.5 s over 30 days,
measured by the gateway from receiving a request to the end of its response
(`gw_request_duration_seconds`, the `le="0.5"` bucket), for every gateway
together.

- **Fast**: over 14.4 × 1% = 14.4% of requests took longer than 0.5 s over
  the last hour and over the last 5 minutes. Act now.
- **Slow**: over 6% over 6 hours and over 30 minutes. Look today.

The gateway's own share is small (NFR-03 and NFR-04 bound it at a few
milliseconds at p99); slow requests nearly always mean slow upstreams, a
route whose upstream is slow by design, or a gateway short of CPU.

## First checks

1. Grafana, **Spillway / Overview**: p50 and p99 per route and per tenant.
   Is it one route, one tenant, or everything?
2. **Spillway / Upstreams**: attempt latency per upstream
   (`gw_upstream_duration_seconds`), retries and their outcome. Retries add
   whole attempts to a request's time. Click an exemplar on a slow bucket to
   open that request's trace in Tempo and see where the time went (rate
   limit checks, each attempt, the backoff between them).
3. **Spillway / Redis**: `gw_ratelimit_check_duration_seconds{backend="redis"}`.
   A Redis check is bounded by the request's 5 ms budget (RL-22), so Redis
   alone cannot make a request take 0.5 s; if it is near its budget, see
   [redis-errors.md](redis-errors.md).
4. The gateways' CPU: `rate(process_cpu_seconds_total{job="gateway"}[5m])`
   against their limit (on kind, the chart's `resources`), and
   `gw_connections_capped_total` and `reason="shed"` answers for overload.
5. A config publish just before (History screen): a new route to a slow
   upstream, or longer timeouts with more attempts.

## Mitigation

- **A slow upstream**: fix or scale it. If a route is slow by design (long
  reports, uploads), it burns this SLO by definition: propose a separate SLO
  for it rather than raising the 0.5 s for everyone.
- **Retries make it worse**: lower the route's attempts or per-try timeout
  in a new config version, or roll back a recent change.
- **Gateways short of CPU**: add replicas (compose `--profile replicas`; on
  kind, `gateway.replicas` in the chart's values) or raise their CPU limit.
- **Overload from one tenant**: tighten its rate limits (plans, overrides)
  so its excess is answered 429 at once instead of queueing upstream.
