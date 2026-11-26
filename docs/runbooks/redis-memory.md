# Redis memory high

Alert: **`SpillwayRedisMemoryHigh`** (severity `ticket`), from
[`spillway.yaml`](../../deploy/prometheus/rules/spillway.yaml), per Redis
node (`node` label, from redis_exporter).

## What it means

For 5 minutes a Redis node used over 80% of its `maxmemory`
(`redis_memory_used_bytes / redis_memory_max_bytes`). Spillway's Redis runs
with `maxmemory 256mb` and **`maxmemory-policy noeviction`** (no limiter
state is silently dropped), so a full node **fails writes**: the limiter
script's calls to that node's slots fail with an out-of-memory error, which
gateways count as `gw_redis_errors_total{kind="script"}` and answer with each
policy's failure mode ([redis-errors.md](redis-errors.md)). At 80% there is
still room; this is a ticket, not a page.

Limiter keys expire on their own (each key's TTL is its window or its GCRA
state's lifetime; quota keys the rest of their period + 1 day), so memory
tracks the number of distinct keys in use: NFR-10 measured about 200 bytes
per key or less.

## First checks

1. Grafana, **Spillway / Redis**: memory per node. One node or all? One hot
   node means a hot slot (one tenant's keys share a hash slot by design,
   RL-13); all nodes means more keys overall.
2. The key count per node: `redis_db_keys{node="redis-N"}`. A sudden jump
   follows a new policy with a high-cardinality `key_template` (`{ip}` on a
   public route, `{header:…}` with free-form values).
3. Expiring keys: `redis_db_keys_expiring` should be close to `redis_db_keys`;
   keys without a TTL are not the limiter's.
4. On kind, the pod's memory limit: the Redis container limit must exceed
   `maxmemory` plus Redis's overhead, or the pod is OOM-killed before Redis
   refuses writes.

## Mitigation

- **A new high-cardinality policy**: change its `key_template` or make it a
  local policy (`local: true`, capped per gateway) in a new config version;
  the old keys expire with their windows.
- **Steady growth with traffic**: raise `maxmemory` in
  `deploy/compose/redis/redis.conf` (compose and kind share it) together
  with the containers' memory limits, or add primaries to the cluster.
- **A hot slot**: one tenant's keys stay in one slot by design (the
  tenant-slot script needs them together); spread big tenants by raising
  their limits' granularity rather than adding keys.
