# 0069. Deny cache and token leasing

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** RL-18, RL-19, NFR-07, RL-11, RL-14, RL-16, RL-21, RL-23, RL-25, RL-27, RL-29, OB-01

## Context

M7 adds the two hot-path savings that the design lists under Performance: the deny cache (RL-19: after a deny with retry-after T, deny that key locally for min(T, 10 s) without calling Redis; enforce mode only; cleared on any config or identity version change) and batched mode (RL-18: a replica reserves k tokens per key and serves them locally; over-admission ≤ replicas × k, with k set per policy), measured by NFR-07 with the accuracy harness. ADR-0025 also found that a hot key bursting past one Redis node's single-call throughput makes the accuracy harness's boundary pattern answer 53–60% of its requests with timeout 503s, and named M7's batched mode as the real fix.

Today (ADR-0021, `internal/proxy/ratelimit.go`) a request's Redis policies become calls by hash slot: one `{t:<tenant>}` call (or one `{k:<digest>}` call per digest), then one call per global policy, each a single `EVALSHA` of `limiter_v1.lua` with one `cost` for every limit in it. The script returns, per call, whether the enforce limits allow, the denying limit, and per limit `remaining`, `reset_after_ms`, `retry_after_ms`, plus `now_us`. The design leaves open: where the cache and the leases sit, their keys and sizes, how a version change is observed, what a lease reservation sends and returns, what happens on Redis failure, how k is validated and carried, and how NFR-07 is judged.

## Decision

### Where they sit in the hot path

`limits.checkRedis` (ADR-0021) gets two steps before it sends any call. For a request whose local phases allowed it:

1. Build the calls as today (`redisCalls`), including quota limits (ADR-0070).
2. **Deny cache.** Look up every *enforce* limit of every call (quota limits included). If one has a live entry whose cost is ≤ the request's cost, the request is denied here: no Redis call is sent, nothing is deducted, and no refund is needed.
3. **Leases.** For each *enforce* policy with `lease_k > 0`, take the request's cost from this replica's lease for that policy and key if the lease is live and holds enough; such a policy is removed from its call. A policy whose lease cannot serve the request either asks for a new lease in the call (`lease_k = k`) or, when this replica already has a reservation in flight for that key, is checked strictly (`lease_k = 0`) in the call.
4. Send the remaining calls as today (tenant slot first, then global; one budget; failure modes). A call that ends up empty is not sent.

If a later step denies the request (another call, a failure mode), the cost taken from a lease is put back into that lease (never above k): in step 3, or from a grant made by this request's own reservation, whose lease then holds the whole grant g. Local policies (RL-17) never use either mechanism.

### Deny cache (RL-19)

- **Entry.** Keyed by the denying limit's full Redis key (`rl:v1:{t:…}:<policy_id>:<period_ms>:<digest>`, or the quota key of ADR-0070), so one entry covers exactly one limit of one policy for one filled key template. Value: `until` (monotonic), `cost` (the cost the deny was computed for), the deny's `reset_after` minus elapsed, and the generation (below).
- **Inserted** after a call answers with a deny, for its denying limit only (the script's `denying_limit`), when that limit is enforce mode, with `until = send time of the call + min(retry_after, 10 s)`. Timing from the call's send time, which precedes the script's `TIME`, means a cached deny never outlives Redis's own: the cache can only answer earlier than Redis would have stopped denying, never later. A lease reservation that is denied inserts the same way. A shadow or off limit is never inserted (RL-19: enforce mode only), and a failure-mode outcome (fail_closed 503, a degraded deny) is never inserted.
- **Hit.** A request is denied by an entry when `now < until`, the entry's generation is current and the request's cost is ≥ the entry's cost (a cheaper request might be admitted, so it goes to Redis). The answer is what a Redis deny gives: 429 with `Retry-After` = ceil(until − now) in seconds, minimum 1 (RL-25), the `RateLimit` items with `r=0` and `t` from the stored reset, `reason="ratelimited"`. Other limits of the request are not evaluated, so its shadow limits record nothing for it, as when an earlier call denies today.
- **Records.** A hit records `gw_ratelimit_decisions_total{policy,decision="deny",mode="enforce"}` for the denying policy (so the 429 rate and the shadow report stay right), counts the deny in the heavy hitters like any deny (RL-21: a cached deny is a deny), and the access log's decisions name it with `"cached": true`.
- **Size cap.** 65,536 entries per replica in 64 shards of 1,024, each a map behind its own mutex (about 160 bytes per entry, so at most about 10 MB). Eviction: an expired entry is removed when a lookup meets it; an insert into a full shard first removes that shard's expired entries and, if the shard is still full, is skipped (`result="full"`). A live entry is never evicted early: skipping an insert only sends more requests to Redis, which is always correct.
- **Clearing.** The handler's snapshot holder already swaps snapshots atomically (GW-21). It gets a **generation counter**, incremented on every swap — a new config version, a new identity version, a new stream epoch (ADR-0055), or any other swap — and the cache is cleared (shards replaced) on each increment. Entries also carry the generation of the snapshot the inserting request used, and a lookup or insert with a stale generation is ignored, so a request still running on the old snapshot cannot repopulate the cache after the swap. Clearing on every swap is a superset of RL-19's "any config or identity version change", in static and managed mode alike.
- **Metrics** (new; no key or consumer labels, OB-01): `gw_ratelimit_deny_cache_total{result}` with `result` ∈ `hit`, `insert`, `full`; `gw_ratelimit_deny_cache_entries` (gauge). A hit observes no `gw_ratelimit_check_duration_seconds{backend="redis"}`.

### Token leasing (RL-18)

**`lease_k` per policy.** `config.proto` `Policy` gets `int64 lease_k = 12;` (0 or unset = strict). The OpenAPI policy spec gets `lease_k: {type: integer, format: int64, minimum: 0, maximum: 1000000, default: 0}` (additive, so `oasdiff` stays clean), and the Policy editor (ADR-0063) gets a "Lease size k" number field, shown for Redis GCRA and sliding-window policies, with the help text "0 = strict: one Redis call per request. k > 0 = batched: each gateway reserves up to k cost units per key and serves them locally; admissions can exceed the limit by up to replicas × k. Enforce mode only." Validation, in config compile and therefore in the API's validate and publish (CP-03):

- `lease_k` ≥ 0 and ≤ 1,000,000;
- `lease_k > 0` only for a Redis policy (`local: false`) with algorithm `gcra` or `sliding_window`;
- `lease_k` ≤ every limit's `burst` (GCRA) or `rate` (sliding window), for the policy's own limits and for every plan's `default_limits` for it (RL-08). Overrides (identity, applied without publish) are not checked: the script never grants more than is available, so a larger k only means smaller grants.

`lease_k` is honoured in **enforce mode only**: a shadow policy is always checked strictly, so its would-deny counts stay exact and a promotion to enforce (ADR-0060) needs no edit.

**The script.** `limiter_v1.lua` keeps its key schema (`rl:v1:`, RL-29), so strict and batched replicas, and old and new gateways during a rollout, share the same counters; its text changes, so its SHA changes, and an old gateway keeps calling its own SHA (RL-16 reloads on `NOSCRIPT`). The contract becomes:

```
KEYS:    one key per limit in this slot, then the quota epoch key when the call has quota limits (ADR-0070)
ARGV:    cost, then per limit: level, mode, algorithm, rate, period_ms, burst (0 unless GCRA),
         lease_k (0 = strict), group (1-based index of the limit's policy within the call),
         period_start_ms, period_end_ms (UTC bounds of a quota limit's period; 0 otherwise, ADR-0070)
RETURNS: allowed (0|1), denying_level, denying_limit, remaining, reset_after_ms,
         retry_after_ms, shadow_would_deny (bitmask per limit), now_us,
         granted (per limit: units written), quota_unseeded (0|1, ADR-0070)
```

Limits of one policy are adjacent and share `lease_k` and `group`. In the check phase, for each group with `lease_k = k > 0`, the script computes the units available on each of its limits at `t` = `TIME`:

- GCRA: `avail = floor((τ − max(TAT − t, 0)) / T)`, with `T` and `τ` as in the design's maths (whole µs, ADR-0023);
- sliding window: `avail = floor(limit − estimate)`;

and the group's grant `g = min(k, min over its limits of avail)`. The group allows iff `g ≥ cost`; a denying group reports `retry_after_ms` for `cost`, exactly as a strict check would. Strict groups behave as today. The call's decision is the AND of the enforce groups; in the write phase each enforce limit of an allowed call is written with its group's units (`g` for a lease group, `cost` otherwise), and shadow limits keep today's write rule. `granted` returns the units written per limit (0 when nothing was written). A refund (RL-14) reverses only strict limits: a lease group's units stay with the replica's lease, which never gives the request's cost to a request that was denied.

**The replica's leases.** A lease table per replica, keyed by the Redis key of the policy's first limit (unique per policy, tenant and filled key), entries `{tokens, expires (monotonic), grant_now_us, grant_recv (monotonic), reserving, generation}`:

- **Serve.** A live (`now < expires`, current generation) lease with `tokens ≥ cost` serves the request: `tokens −= cost`, decision `allow` for that policy.
- **Reserve.** Otherwise, if no reservation for that key is in flight on this replica, the request sets `reserving`, **drops any leftover tokens** (fewer than its cost; counted `reason="reserve"`), and sends the policy in its call with `lease_k = k`. When the call allows, the lease becomes `tokens = g − cost`, `expires = call send time + 1 s`, `grant_now_us = now_us`, `grant_recv = now`. Whatever the outcome, `reserving` is cleared.
- **In flight.** While a reservation for the key is in flight, other requests on that replica check the policy strictly in their own calls (`lease_k = 0`), never wait.
- **Lease life.** 1 s from the reservation's send time (every period is whole seconds ≥ 1 s, ADR-0017). Tokens left at expiry are dropped, never returned to Redis: a return would be a refund made up to a second later, which GCRA gives back only partly (ADR-0028), and it would add a Redis call and a failure path. The cost is under-admission of at most k per replica per lease, only on a replica whose traffic for the key stops.
- **Generation.** The table is cleared on every snapshot swap, as the deny cache is (limits may have changed with config, plans or overrides); dropped tokens count `reason="reset"`.
- **Size cap.** 65,536 leases per replica, sharded like the deny cache. A key that finds the table full is checked strictly (`result="full"`).

**Over-admission bound.** At any instant a replica holds at most k unspent tokens for a key, and never has a second grant for it on the way: a reservation is sent only with the lease emptied, and only one per key and replica is in flight. Every admission is backed by units deducted in Redis under the script's own bound. So in any window, the admissions beyond what Redis's bound allows for that window are at most the tokens each replica held, or was about to receive, at the window's start: **≤ k per replica, ≤ replicas × k in all** (RL-18). Strict checks made meanwhile are exact.

**Redis failure.** A reservation that fails, or that the budget leaves no time for, follows the policy's failure mode exactly as a strict check does (RL-23); a live lease keeps serving its remaining tokens until it expires (they were deducted in Redis already). A degraded policy falls back to its local limiter as today.

**Headers and decision time.** A request served from a lease reports, per limit, `r` = the `remaining` at the grant plus the lease's tokens left and `t` = the grant's `reset_after` minus the time since the grant, floored at 0 (an approximation, documented in ADR-0018's sense: the replica does not know other replicas' spending). In dev mode `Spillway-Decision-Time` is `grant_now_us + (now − grant_recv)` in µs, as the design's harness step 3 describes; it can read earlier than Redis's true time by at most the reservation's one-way trip.

**Metrics** (new, no key labels): `gw_ratelimit_lease_total{result}` with `result` ∈ `local` (served from a lease), `granted` (g = k), `partial` (cost ≤ g < k), `denied`, `error`, `inflight` (checked strictly because a reservation was in flight), `full`; `gw_ratelimit_lease_tokens_dropped_total{reason}` with `reason` ∈ `expired`, `reset`, `reserve`. Redis calls saved per request are `local / (local + granted + partial + denied + inflight)`.

### Interactions

- **Deny cache and leasing.** The cache is checked first, so a denied key costs a batched policy no reservation attempts while the entry lives; a denied reservation inserts an entry.
- **Quotas (ADR-0070).** Quota limits are never leased: a request whose consumer has a quota always makes its tenant-slot call, even when every policy is served from leases. A quota deny is cached like any deny (`until` ≤ 10 s, though the quota's own retry-after is up to the period's end).
- **Heavy hitters.** Cached denies count; locally served admissions are not denies and do not count.

### NFR-07: the accuracy harness in batched mode

- `test/accuracy/batched.yaml`: `test/accuracy/gateway.yaml`'s route and policy with `lease_k: 10` (GCRA 100/s, burst 100, fail_closed) and `version: 32`.
- `cmd/accuracy` gets `-lease-k <k>` (default 0) and runs with `-config-version 32` for batched runs. `test/accuracy/run.sh` takes a second argument, `strict` (default) or `batched`, and `make accuracy` a variable `MODE`.
- The harness is otherwise unchanged: 10 keys at 2× the limit, uniform, bursty and boundary patterns of 60 s each, the window slid over each allowed response's decision time.
- **Pass rule per replica count n ∈ {1, 3, 5}:** for every key, pattern and window, `admitted − (burst + w · rate / period) ≤ n × k` = **10, 30 and 50**. Failure-mode answers (503) are reported separately and not judged, as in strict mode (ADR-0021). Each run also reports the Redis calls per admitted request from the lease metrics.
- verify-m7 runs it at 1, 3 and 5 replicas (ADR-0074).

### Tests

- Deny cache (unit, fake clock): insert only on enforce denies; `until` from the send time; min(T, 10 s); the cost rule; expiry; a shard at the cap skips inserts; clearing on a generation increment and ignoring stale-generation inserts and lookups; a hit gives the same status, `Retry-After`, `RateLimit` items and decision metric as a Redis deny; heavy hitters count a hit.
- Leasing (unit, fake Redis): serve, reserve, the in-flight rule (concurrent requests on one key never hold more than k), dropped leftovers, expiry, reset on swap, put-back on a later deny, failure modes for a failed reservation, the table cap; a `rapid` property test that drives random arrivals on several simulated replicas and checks admissions in every window ≤ the strict bound + replicas × k.
- Script (compose Redis Cluster, ADR-0004): grants `min(k, avail)` for GCRA and sliding window, multi-window groups, a mixed strict and lease call, shadow write rule unchanged, refunds skip lease groups, an old-SHA and new-SHA call on the same key.
- Validation: each `lease_k` rule; OpenAPI and UI (Vitest) field; one Playwright step in `policy.spec.ts` sets k on a policy.
- Integration (compose): a batched policy at 2× its rate through gateway-1 makes fewer than 1 call per 5 requests, and a cached deny makes no Redis call (from `gw_ratelimit_deny_cache_total` and the Redis call metrics).

## Consequences

- Hot keys cost far fewer Redis calls: a denied key at most one per retry-after (≤ 10 s) per replica, a batched key about one per k admitted cost units per replica. This is the fix ADR-0025 named for the boundary pattern's 503s (ADR-0071 measures it).
- Batched mode trades accuracy for calls with a stated bound; strict stays the default (`lease_k: 0`).
- A replica that stops getting traffic for a batched key wastes up to k units for up to 1 s; a deny cache entry can over-deny a cheaper request only when it is not cheaper (the cost rule).
- `RateLimit` headers from a lease are approximate.
- The script's SHA changes; mixed-version rollouts are safe (same keys and state format). P23 updates the script contract in docs/design.md.

## Alternatives considered

- **Cache keyed by policy and digest instead of the Redis key** — the Redis key already names policy, period, tenant and digest; a second key scheme adds a way to disagree.
- **Evicting live entries (LRU) at the cap** — evicting a live deny sends requests to Redis, the same as skipping an insert, with more code.
- **Clearing only when the config or identity version number changes** — every swap is cheaper to reason about, and a swap without a version change is rare.
- **Single-flight for reservations (requests wait for the in-flight one)** — RL-20 (coalescing) is stretch; waiting adds latency, and the strict fallback keeps the bound.
- **Returning unused lease tokens** — partial refunds (ADR-0028), an extra call per lease and its failure handling, to save at most k per idle replica.
- **Leasing quota counters too** — over-counting daily quotas by replicas × k is easy to state, but quota checks stay exact in v1; a later ADR can add it.
- **A separate lease script** — a request whose tenant slot mixes strict and batched policies would need two calls in one slot, losing RL-11's single atomic check.
