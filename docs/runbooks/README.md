# Runbooks

One file per operational situation: **What it means**, **First checks** and
**Mitigation** (OB-08). Every alert in
[`deploy/prometheus/rules/spillway.yaml`](../../deploy/prometheus/rules/spillway.yaml)
links to its runbook here through its `runbook_url` annotation, and
`test/obs` checks that each link names a file that exists (ADR-0075,
ADR-0087).

Commands use compose's names (`docker compose`, Prometheus on
`127.0.0.1:9091`, Grafana on `127.0.0.1:3000`) and, where they differ, the
kind cluster's (context `kind-spillway`, namespaces `spillway`,
`spillway-deps`, `spillway-obs`; `make kind-prometheus` on 19091,
`make kind-grafana` on 13000). Substitute your own. There is no
Alertmanager in v1: firing alerts show in Prometheus (`/alerts`) and in
Grafana (the Overview dashboard's alerts panel).

| Runbook | Alert(s) | When |
| --- | --- | --- |
| [availability-burn.md](availability-burn.md) | `SpillwayAvailabilityBurnFast` (page), `SpillwayAvailabilityBurnSlow` (ticket) | Gateway-caused 5xx burn the 99.9% availability SLO's budget too fast |
| [latency-burn.md](latency-burn.md) | `SpillwayLatencyBurnFast` (page), `SpillwayLatencyBurnSlow` (ticket) | Requests slower than 0.5 s burn the 99% latency SLO's budget too fast |
| [redis-errors.md](redis-errors.md) | `SpillwayRedisErrors` (page) | Over 1% of a gateway's Redis limiter calls fail for 5 minutes |
| [redis-memory.md](redis-memory.md) | `SpillwayRedisMemoryHigh` (ticket) | A Redis node uses over 80% of its `maxmemory` for 5 minutes |
| [config-sync-stale.md](config-sync-stale.md) | `SpillwayConfigSyncStale` (page) | A managed gateway has not synced with the control plane for over 60 s |
| [circuit-open.md](circuit-open.md) | `SpillwayCircuitOpen` (page) | An upstream's circuit breaker has not closed for 5 minutes |
| [gateway-snapshot.md](gateway-snapshot.md) | (none; seen as an unready pod) | A gateway pod started during a control-plane outage has no on-disk snapshot and stays unready (CP-07) |
