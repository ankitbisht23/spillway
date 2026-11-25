# Redis limiter errors

Alert: **`SpillwayRedisErrors`** (severity `page`), from
[`spillway.yaml`](../../deploy/prometheus/rules/spillway.yaml), per gateway
(`replica` label).

## What it means

For 5 minutes, over 1% of one gateway's Redis rate-limit checks failed:
`rate(gw_redis_errors_total[5m])` over
`rate(gw_ratelimit_check_duration_seconds_count{backend="redis"}[5m])`.
`gw_redis_errors_total{kind}` says how: `timeout` (a call ran past the
request's 5 ms Redis budget, RL-22, or the budget left no time for it),
`conn` (no connection, or a node's circuit breaker refused the call) or
`script` (the limiter script failed).

While a check fails, each Redis policy follows its **failure mode** (RL-23):
`fail_open` allows the request, `fail_closed` answers 503 `fail_closed`, and
`degraded` checks a local limiter at its limit ÷ live replicas. So clients
see either unlimited traffic, 503s or approximate limits, depending on the
policies; quotas follow the policies of the same call (RL-10). Traffic itself
keeps flowing: readiness never depends on Redis (GW-24).

## First checks

1. Grafana, **Spillway / Redis**: errors by `kind` and by replica, Redis
   check latency, and each node's `redis_up`, memory and clients from
   redis_exporter. One replica or all?
2. **All replicas, `conn`**: is the cluster up? Compose:
   `docker compose exec redis-0 redis-cli cluster info | grep cluster_state`;
   kind: `kubectl --context kind-spillway -n spillway-deps get pods -l app.kubernetes.io/name=redis`.
   `redis_cluster_state` should be 1 on every node.
3. **All replicas, `timeout`**: Redis is slow, not down. Check
   `redis_commands_processed_total` (load), `redis_memory_used_bytes`
   (a node near `maxmemory`, see [redis-memory.md](redis-memory.md)), and
   the Redis containers' CPU. A failover in progress (a primary killed)
   gives a burst of timeouts that ends once its replica takes over
   (seconds, `cluster-node-timeout 3000`).
4. **One replica**: its network path to Redis (on kind, its node), or its
   connection pool after a restart. Its log has the limiter's errors.
5. **`script`**: a script error after an upgrade means a gateway and the
   loaded script disagree; the gateway's log names the error.

## Mitigation

- **A node or the cluster is down**: bring it back (compose
  `docker compose up -d redis-N`; kind: the StatefulSet recreates the pod).
  A replaced primary's replica takes over its slots by itself; limiter state
  is ephemeral, so nothing is restored (counts restart, quotas are re-seeded
  by the control plane's flusher, ADR-0070).
- **Redis is overloaded**: reduce the Redis calls: enable token leasing
  (`lease_k`, RL-18) on the hottest policies, or make high-volume coarse
  limits local (`local: true`).
- **Clients get too many 503 `fail_closed`**: that is the policy's choice;
  if availability matters more than the limit for a policy during an
  outage, switch it to `fail_open` or `degraded` in a new config version.
- **One replica**: restart it.
