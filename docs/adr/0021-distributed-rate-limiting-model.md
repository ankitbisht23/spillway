# 0021. Distributed rate limiting model

- **Status:** Accepted; GCRA emission interval formula superseded by [ADR-0023](0023-gcra-emission-interval-in-whole-microseconds.md); "nothing is loaded at start" superseded by [ADR-0024](0024-redis-limiter-warm-up.md); breaker rule superseded by [ADR-0025](0025-health-check-driven-redis-breakers.md); full-outage budget line and NFR-04 run voiding superseded by [ADR-0027](0027-tolerance-for-host-stalls-in-the-m3-exit-check.md)
- **Date:** 2026-11-10
- **Requirement IDs:** RL-01, RL-03 – RL-07, RL-11 – RL-17, RL-22 – RL-27, RL-29 – RL-31, GW-01, GW-24, GW-29, OB-01, OB-03, NFR-03, NFR-04, NFR-06, NFR-13

## Context

M3 moves every policy that is not local into Redis Cluster. The design fixes the hierarchy (RL-05, RL-14), the key layout and hash tags (Redis keys, RL-13), the script contract and its write rule (Limiter script contract), `TIME` (RL-12), TTLs (RL-15), no retry once sent (RL-16), versioning (RL-29), the time budget and per-node breakers (RL-22), the three failure modes (RL-23), the accuracy harness and the two Redis chaos scenarios. ADR-0017 built the local half and rejected every policy without `local: true` "until M3". Still open:

- the Go packages and APIs, how the go-redis client is set up so that a sent call is never resent, and how Redis 7+ is checked (RL-30);
- the script's exact ARGV and reply, how a refund knows which window it belongs to, number formatting in Lua, TTLs, and what the gateway needs beyond the contract to fill RateLimit headers for every limit;
- the proto fields for `failure_mode` and the budget, their validation, and the gateway settings that name the cluster;
- how a request's policies are grouped into calls, in which order, what a deny refunds and how, and how far a refund that fails or arrives late can over-deduct;
- the budget split, breaker parameters, error kinds, failure-mode precedence, metrics and the dev-only decision-time header;
- the accuracy harness's patterns and parameters, the chaos pass rules and their timing slack, and `make verify-m3`;
- which M3 task builds what, and what the gateway does between tasks;
- four items that M2's review left for the user (local key cap, IPv6 `{ip}`, `..;` paths, the NFR-03 method).

## Decision

### Scope: non-local policies run in Redis (RL-17, RL-31)

A policy runs in Redis unless it says `local: true` or is a concurrency policy (always local, RL-02). This supersedes ADR-0017's rule that rejected such policies ("only local policies … until M3"); the rest of ADR-0017 stands. Local policies behave exactly as in M2 and never touch Redis (RL-17). Redis policies use the configured, cluster-wide `rate` and `burst`: the per-replica division of RL-31 applies to them only in degraded mode (below).

### Packages and client (RL-01, RL-11, RL-16, RL-30)

`internal/ratelimit/redis` holds, next to the existing address-map dialer:

```go
// NewClient returns the limiter's cluster client. It never contacts Redis
// itself; the first call does (GW-24).
func NewClient(addrs []string, addrMap map[string]string, clock ratelimit.Clock, log *slog.Logger) *Client

// Limit is one limit in a call. All Keys of one call share a hash slot.
type Limit struct {
    Key       string        // full Redis key (Keys below)
    Level     string        // global | tenant | consumer | route
    Enforce   bool          // false: shadow
    Algorithm string        // config.GCRA or config.SlidingWindow
    Rate      int64         // configured, cluster-wide
    Period    time.Duration // whole seconds
    Burst     int64         // GCRA only, after its default
}

// Result is a call's reply (Script below).
type Result struct {
    Allowed         bool
    DenyingLevel    string               // "" when allowed
    DenyingLimit    int                  // index into the call's limits; -1 when allowed
    Remaining       int64                // contract field: of DenyingLimit, else of the tightest enforce limit
    ResetAfter      time.Duration
    RetryAfter      time.Duration        // 0 when allowed
    ShadowWouldDeny uint32               // bit i: limits[i] is shadow and would deny
    NowUS           int64                // Redis TIME the script used, µs since the Unix epoch
    Limits          []ratelimit.Decision // per limit, in call order (the extension below)
}

// Call checks every limit and writes per the write rule, in one script call.
func (c *Client) Call(ctx context.Context, cost int64, limits []Limit) (Result, error)

// Refund reverses an earlier Call's deduction on the limits it wrote.
// admittedUS is that Call's NowUS.
func (c *Client) Refund(ctx context.Context, cost, admittedUS int64, limits []Limit) (Result, error)

// Limiter adapts one limit to ratelimit.Limiter (RL-01): Allow(ctx, key, cost)
// makes a one-limit Call (a negative cost makes a Refund with admittedUS 0,
// the current window) on the global key
// layout, with key a ratelimit.Key digest. Script tests use it; the gateway
// uses Call.
func (c *Client) Limiter(policy, algorithm string, l config.Limit) ratelimit.Limiter

// Error is every error Call and Refund return.
type Error struct {
    Kind string // KindTimeout ("timeout"), KindConn ("conn") or KindScript ("script")
    Node string // primary address owning the call's slot, e.g. "redis-1:6379"; "" if unknown
    Err  error
}
```

- **go-redis options.** `ClusterOptions{Addrs, Dialer: Dialer(addrMap) when the map is set, MaxRetries: 0, ContextTimeoutEnabled: true}`; everything else is go-redis v9.23.0's default (`MaxRedirects` 3). For a cluster client `MaxRetries: 0` means its node clients never retry, and `ContextTimeoutEnabled` makes the call's deadline reach the socket reads and writes.
- **No resend once sent (RL-16).** go-redis's cluster loop resends some commands after a network error or a timeout, independently of `MaxRetries`. The limiter therefore sends each call with `ClusterClient.Process` and a command whose `NoRetry()` is true (a small wrapper around `*redis.Cmd`): go-redis still follows `MOVED` and `ASK` replies and re-routes a `READONLY` refusal, all of which mean the script did not run, but never resends after an error that may follow execution. If a later go-redis version stops honouring `NoRetry`, the fallback is to send to `MasterForKey`'s node client and follow one `MOVED`/`ASK` in the limiter. Either way two tests guard RL-16: a unit test whose fake node drops the connection after reading the command sees exactly one send, and the script test injects a timeout after execution and sees one deduction.
- **Script loading.** The script is `internal/ratelimit/redis/limiter_v1.lua`, embedded with `go:embed`; its SHA-1 is computed at start. Every call is `EVALSHA`. On a `NOSCRIPT` reply (the script never ran) the same call is sent once as `EVAL` with the source, which also caches the script on that node. Nothing is loaded at start.
- **Redis 7+ (RL-30).** The first call to each primary address (the slot owner per `MasterForKey`) is preceded by `INFO server` on that node under the same deadline. `redis_version` major ≥ 7 marks the address supported for the life of the process. Below 7, the address is marked unsupported for good: an error is logged once, its breaker is held open, and every call to it fails as `conn` without being sent. An `INFO` that fails counts as a failed call and is tried again on the next call. Readiness never depends on Redis (GW-24).
- **Error kinds.** `timeout`: the deadline passed during the call, or the request's budget was spent before it (not sent). `conn`: no reply from Redis — dial errors, resets, EOF, pool timeout, a closed client — plus the cluster-state replies `CLUSTERDOWN`, `TRYAGAIN`, `MASTERDOWN`, `LOADING` and `READONLY`, redirects that ran out, an open breaker and an unsupported node. `script`: any other error reply (a Lua error, `OOM`, `WRONGTYPE`, `CROSSSLOT`, `ERR`) and a reply the client cannot decode.

### Keys (RL-13, RL-15, RL-29)

Exactly the design's Redis keys table:

| Level and request | Key |
| --- | --- |
| tenant, consumer, route; the request has a tenant | `rl:v1:{t:<tenant_id>}:<policy_id>:<period_ms>:<digest>` |
| global | `rl:v1:{g:<policy_id>}:<period_ms>:<digest>` |
| tenant, consumer, route; the request has no tenant | `rl:v1:{k:<digest>}:<policy_id>:<period_ms>` |

- `<digest>` is `base64.RawURLEncoding` (22 characters) of `ratelimit.Key(filled template)`, the 16-byte SHA-256 prefix the local limiters already use. `<period_ms>` is the limit's period in milliseconds (`1000`, `3600000`).
- In static mode `<policy_id>` is the policy name and `<tenant_id>` the tenant name. Both are restricted to 1–64 characters from `[A-Za-z0-9._-]` (policy names since ADR-0017; tenant names from m3-global, checked when the identity file loads), so neither can contain `{`, `}` or `:` and break a hash tag or a key's fields.
- Examples, with `digest("acme")` = `giszrYfBSKCiClunzV68qg`, `digest("alice")` = `K9gGyX8OAK8aH8Myj6djqQ`, `digest("carol")` = `TCbZB0wn2J7eWScMCsFLcQ`, `digest("bytes")` = `J3CJ2RwL308uaGK6fkoHYA`:
  - `rl:v1:{t:acme}:tenant-default:1000:giszrYfBSKCiClunzV68qg`
  - `rl:v1:{t:acme}:consumer-minute:60000:K9gGyX8OAK8aH8Myj6djqQ`
  - `rl:v1:{k:TCbZB0wn2J7eWScMCsFLcQ}:consumer-minute:60000`
  - `rl:v1:{g:global-bytes}:1000:J3CJ2RwL308uaGK6fkoHYA`
- **Versioning (RL-29).** `v1` names both the key layout and `limiter_v1.lua`. A change to either the layout or the stored state's meaning is `v2`: a new prefix and a new script file, with v1 code removed one release later. Old and new gateways then run side by side on separate keys (bound below).

### Script (RL-05 – RL-07, RL-11, RL-12, RL-15)

`limiter_v1.lua` starts with `#!lua` and no flags. Redis 7 then treats it as a script that writes: a node over `maxmemory` (`noeviction`, ADR-0004) or a replica refuses it before it runs, so a call never writes part of its limits. Script flags are one more reason for RL-30.

**ARGV** (the design's contract, plus one trailing field on refunds):

| ARGV | Value |
| --- | --- |
| 1 | `cost`: a positive integer for a decision; negative for a refund; 0 is an error |
| 2 + 6(i−1) … 7 + 6(i−1) | for `KEYS[i]`: `level` (`global`, `tenant`, `consumer`, `route`), `mode` (`enforce`, `shadow`), `algorithm` (`gcra`, `sliding_window`), `rate`, `period_ms`, `burst` (0 unless GCRA) |
| 2 + 6n | refunds only: `admitted_us`, the `now_us` of the call being refunded; `0` means the current window (the one-limit `Limiter` adapter, which keeps no state and which the gateway does not use) |

A wrong argument count, an unknown value or a cost of 0 is an error reply (`spillway: …`), so a client and script of different versions fail loudly instead of deciding wrongly.

**Reply**, an array:

| Index | Field |
| --- | --- |
| 1 | `allowed`: 1 or 0 (refunds: 1) |
| 2 | `denying_level`: the level of `denying_limit`, `""` when allowed |
| 3 | `denying_limit`: 1-based index into KEYS, 0 when allowed |
| 4, 5 | `remaining`, `reset_after_ms`: of `denying_limit` when denied; when allowed, of the enforce limit with the smallest `remaining` (the first on a tie), or of limit 1 when the call has no enforce limit |
| 6 | `retry_after_ms`: of `denying_limit`, 0 when allowed |
| 7 | `shadow_would_deny`: bitmask, bit i−1 set when limit i is shadow and would deny |
| 8 | `now_us` |
| 9 + 4(i−1) … 12 + 4(i−1) | **extension**, per limit i: `allowed_i` (this limit alone admits `cost`), `remaining_i`, `reset_after_ms_i`, `retry_after_ms_i` (0 when `allowed_i`) |

Fields 1–8 are the contract; the per-limit fields are additive, so the gateway can list every limit in the RateLimit headers (ADR-0018) and record a decision per policy. When several enforce limits deny, `denying_limit` is the one with the largest `retry_after`, first on a tie, so field 6 is the request's `Retry-After`. Millisecond fields are rounded up from µs, so a short wait never reads 0. `ShadowWouldDeny` is a 32-bit mask because a call carries at most 32 limits (Config).

**Time and maths.** `t` = `redis.call('TIME')` as µs (`sec·1e6 + usec`, exact in a Lua double). Redis 7 replicates scripts by effects, so reading `TIME` before writing is allowed. For each limit, with `c` = cost:

- **GCRA**: `T = period_ms·1000 / rate` µs (a double), `τ = T·burst`. Stored: TAT in µs as an integer string. `TAT' = ceil(max(TAT, t) + c·T)`. `allowed_i ⟺ TAT' − t ≤ τ`; `retry_after = TAT' − τ − t`. After the call, with `TAT_s` the stored value (`TAT'` if written, else `max(TAT, t)`): `remaining = max(0, floor((τ − (TAT_s − t)) / T))`, `reset_after = TAT_s − t`. Rounding up each `TAT'` only ever admits less, so the GCRA bound holds exactly; the price is that one key cannot admit more than one cost unit per µs (10⁶/s), far above any design limit.
- **Sliding window**: hash `{w, cur, prev}`, `w` the window start in µs, windows aligned to the Unix epoch: `w_now = t − t mod period`. Rotation on read: same window, nothing; one window later, `prev = cur, cur = 0`; older, both 0. `e = t − w_now`, `estimate = prev·(1 − e/period) + cur`. `allowed_i ⟺ estimate + c ≤ rate`; written: `cur += c`. `remaining = max(0, floor(rate − estimate_s))` with `estimate_s` the estimate after the call, `reset_after = period − e`, and on a deny `retry_after` is ADR-0017's formula. The rotated state is written only when the limit is written.
- **Write rule** (design): every limit is read and checked before any is written. `allowed` = AND of the enforce limits' `allowed_i`. If allowed, every enforce limit is written and each shadow limit whose `allowed_i` is 1; if denied, nothing is written. A denied call therefore needs no refund.
- **Refunds** (`cost` < 0, no decision, `allowed` = 1): GCRA `TAT = TAT − floor(|c|·T)`; when that is ≤ `t` the key is deleted. Sliding window: rotate, then subtract `|c|` from `cur` if `admitted_us` falls in the current window, from `prev` if it falls in the previous one, and from nothing if older (the deduction has left the window); never below 0. A missing key is left missing. The per-limit fields describe the state after the refund.
- **Wrong type**: a key holding the other algorithm's type (left by a policy whose algorithm changed; the key name has no algorithm) is read with `redis.pcall` as empty and deleted before it is written.
- **TTL on every write** (RL-15): GCRA `SET key TAT PX <period_ms + ceil(τ/1000)>`, which is at least the window plus the burst time and outlives any `TAT' − t ≤ τ`; sliding window `HSET` then `PEXPIRE key <2·period_ms>`. Refunds that keep a key set the same TTLs.
- **Numbers**: every number the script writes or returns goes through `string.format('%.0f', n)` or is a Lua integer below 2⁵³; Lua's default `%.14g` would truncate a 16-digit µs timestamp.

### Config (RL-03, RL-22, RL-23)

Additive proto fields (`buf breaking` stays green):

```proto
message RateLimitSettings {
  int32 replicas = 1;
  // One Redis time budget per request (RL-22): Go duration, unset means 5ms.
  string redis_budget = 2;
}

message Policy {
  // … fields 1–8 …
  // Redis policies only: fail_open | fail_closed | degraded; unset means fail_open (RL-23).
  string failure_mode = 9;
}
```

Compile adds these rules; ADR-0017's other rules stay:

| Field | Rule and error |
| --- | --- |
| `ratelimit.redis_budget` | unset means `5ms`; otherwise a Go duration > 0 and ≤ `1s`: `ratelimit.redis_budget: "<v>" is not a duration greater than 0 and at most 1s`. A budget over a second would hold requests far longer than any failure mode is meant to take |
| `local` | no longer required. ADR-0017's rejection message goes away |
| `algorithm` | a Redis policy is `gcra` or `sliding_window`: `algorithm fixed_window must be local (local: true); Redis policies use gcra or sliding_window`. Concurrency stays local whatever `local` says |
| `failure_mode` | `fail_open`, `fail_closed` or `degraded` (`failure_mode "<v>": want fail_open, fail_closed or degraded`); only on Redis policies: `failure_mode applies only to Redis policies; local policies never fail` |
| limits per call | a Redis policy has at most 32 limits, and for every route, the Redis limits of tenant-, consumer- and route-level policies that match it number at most 32: `route "<r>" would carry <n> Redis limits in one call; at most 32` |
| cost | Redis policies: the route's cost ≤ the configured `burst` (GCRA) or `rate` (sliding window). `degraded` policies also: cost ≤ the per-replica value, `route "<r>" costs <c>, more than the <m> that limits[<i>] admits per replica in degraded mode, so its requests could never be admitted during a Redis outage` |

- **Compiled form.** `config.Policy` gains `Local bool` and `FailureMode string`; the compiled `Limit` of a Redis policy holds the configured rate and burst, a local one the per-replica values as before. `Snapshot` gains `RedisBudget time.Duration` and `Replicas int64`.
- **Identity.** A consumer's `tenant`, when set, must be 1–64 characters from `[A-Za-z0-9._-]`; an identity file that breaks it is rejected like any invalid identity (the last good one stays).

**Gateway settings** (env, ADR-0008):

| Variable | Meaning | Default |
| --- | --- | --- |
| `SPILLWAY_GATEWAY_REDIS_ADDRS` | comma-separated `host:port` seeds of the limiter's cluster; an entry that is not `host:port` stops the gateway at start | empty: no Redis |
| `SPILLWAY_REDIS_ADDR_MAP` | as ADR-0004; the gateway reads it too, so a gateway run on the host reaches the compose cluster | empty |
| `SPILLWAY_GATEWAY_DEV_MODE` | `true` or `false` (`strconv.ParseBool`); turns on dev-only behaviour, so far only the decision-time header | `false` |

Compose sets `SPILLWAY_GATEWAY_REDIS_ADDRS: redis-0:6379,redis-1:6379,redis-2:6379,redis-3:6379,redis-4:6379,redis-5:6379` and `SPILLWAY_GATEWAY_DEV_MODE: "true"` on gateway-1 … gateway-5, with a comment each; `.env.example` stays secrets-only. A snapshot with a Redis policy while `SPILLWAY_GATEWAY_REDIS_ADDRS` is empty fails to apply (the watcher's apply hook, so the last good snapshot stays; at start the gateway stops): `policy "<name>" runs in Redis (local is not true), but SPILLWAY_GATEWAY_REDIS_ADDRS is not set`.

### Pipeline (RL-05, RL-13, RL-14)

1. Route match, phase 1 local policies, API key resolution (401), phase 2 local policies: exactly ADR-0017. A local deny ends the request (429) before any Redis call.
2. The applying Redis policies (mode not `off`, match covers route and upper-cased method, template fills; ADR-0017, ADR-0020) are grouped into calls:
   - **Tenant slot.** When the request has a tenant (a valid API key whose consumer has one), every applying tenant-, consumer- and route-level Redis policy goes into one call with `{t:<tenant>}` keys, whatever its template.
   - **No tenant.** Those policies use `{k:<digest>}` keys instead: one call per distinct digest, so policies whose templates fill to the same string share a call. Calls are ordered by their first policy in config order.
   - **Global.** Each applying global-level Redis policy is one `{g:<policy_id>}` call, in config order.
   - Within a call, limits follow config order: policies in config order, each policy's limits in config order; `KEYS[i]` is limit i.
3. Calls run one at a time in that order: the tenant call or the `{k:}` calls, then the global calls. A call is sent only if every call before it allowed (RL-14); a failed call counts as allowed for this rule once its failure modes have been applied and did not end the request (Failure modes). Every policy in a call that ran records a decision; policies in calls that were never sent record none.
4. **Deny and refunds.** A request denied by a Redis call (429) or ended by a failure mode (429 or 503) consumes nothing it can give back:
   - every earlier Redis call that allowed is refunded on the limits it wrote: its enforce limits, and its shadow limits whose `ShadowWouldDeny` bit is 0;
   - every local deduction (enforce and shadow, concurrency released) and every degraded-limiter deduction is refunded at once, synchronously, as in M2.

   Redis refunds run in a goroutine per refunded call, each with its own timeout equal to `redis_budget` and its own context (not the request's), never retried, through the same breaker as other calls. A failed refund is counted in `gw_ratelimit_refund_failures_total` and `gw_redis_errors_total`, and logged at debug level without keys.
5. **Over-deduction bound** (design Risks: cross-slot refunds are not atomic). Per denied request and per limit an earlier call wrote, at most `cost` units are wrongly held:
   - until the refund lands: at most `redis_budget` (5 ms) after the deny, during which concurrent requests on that key see `cost` fewer units;
   - if the refund fails or times out: until the state decays on its own — GCRA regains the units after `cost·T` (`T = period/rate`) at the latest, a sliding window when the window holding them ages out (at most 2 periods).

   A call that timed out after the script ran may also have deducted, and is never refunded because its outcome is unknown: at most `cost` per limit of that call, with the same decay. All of these only under-admit.

### Budget and breakers (RL-22)

- **Budget.** One deadline per request: the start of its first Redis call plus `redis_budget`. Every call runs with `context.WithDeadline(context.WithoutCancel(r.Context()), deadline)`, so the request's trace values reach it but a client that goes away cannot cut a sent call short. When the deadline has passed before a call, that call is not sent and fails as `timeout`; it counts in `gw_redis_errors_total` but not against a breaker. A sent call that timed out is never resent.
- **Breakers.** One per primary address, the address `MasterForKey(first key)` names for the call's slot (`redis-1:6379`). Closed: every call is sent; each failed call (any kind, including a script error from Redis) adds one to a consecutive-failure count, any success resets it. After **5 consecutive failures** it opens for **1 s**: calls to that node fail at once as `conn` without being sent. Then it is half-open: the first call is sent as the probe and any others meanwhile fail as `conn`; a successful probe closes it, a failed one opens it for another second. Calls refused by the breaker are not failures and never extend it. Each time a breaker opens or a probe fails, the limiter calls `ClusterClient.ReloadState`, so a slot that failed over reaches the promoted replica as soon as the cluster has elected it instead of waiting for go-redis's periodic refresh. Transitions are logged (warn on open, info on close) with the node address. If `MasterForKey` itself fails (no slot map loaded yet, for example a gateway started during an outage), the call fails as `timeout` if its deadline passed and as `conn` otherwise, with `Node` empty and no breaker involved. The breaker reads a `ratelimit.Clock`, so its unit tests run on the fake clock.
- **No `breaker_open` error kind.** A call refused by an open breaker counts as `kind="conn"`: the design's three kinds stay the label set, and during an outage the error rate still counts every request that could not reach Redis rather than dropping to five per node per second once the breaker opens. The transition log lines say when a breaker was open.

### Failure modes (RL-23)

When a call fails (any error kind, sent or not), each policy in it follows its `failure_mode`:

- `fail_open`: the policy allows. Its decision is recorded as `fail_open`.
- `fail_closed`: if the policy is enforce-mode, the request is answered **503** by the gateway: `Content-Type: application/json`, body `{"error":"rate limit unavailable"}`, `Retry-After: 1` (the breaker's open time), `gw_requests_total{origin="gateway", reason="fail_closed"}` (the design's OB table names this reason). Its decision is recorded as `fail_closed`.
- `degraded`: the policy is checked against an in-memory limiter of the same algorithm and period with `max(1, floor(rate / ratelimit.replicas))` and `max(1, floor(burst / ratelimit.replicas))` (RL-23, RL-31; heartbeats replace the static count in M5). These limiters live in the local registry under the policy's stable ID with a degraded marker (`local.ID{Policy, Algorithm, Period, Degraded: true}`), created on every snapshot apply for each Redis policy with `failure_mode: degraded` and reconciled like the others (GW-29), so their state survives both reloads and repeated outages. Its decision is `allow` or `deny`, recorded as usual.
- **Precedence within a failed call:** any enforce `fail_closed` policy → 503 and nothing else in that call is evaluated; otherwise the degraded limiters run (all of them, like local phases), and an enforce degraded deny → 429; otherwise the call counts as allowed and the next call may run. Across calls the first call that ends the request decides, and the refunds above apply to a 503 too.
- Shadow policies never change the response on failure: a shadow `fail_closed` policy records `fail_closed` with `mode="shadow"` and the request continues; a shadow degraded policy records its degraded decision.
- After a failure-mode outcome, the RateLimit headers list the degraded limits (with their per-replica `q`) and leave out `fail_open` and `fail_closed` policies, which have no state to report.

### Responses, metrics and logs (RL-25, RL-27, OB-01, OB-03)

- **Headers** (ADR-0018, extended). Items for Redis limits follow the local ones, in call order, then config order within a call; `q` is the configured, cluster-wide `rate` (it is a shared limit), `w` the period, `r` and `t` the per-limit `remaining_i` and `reset_after_ms_i` (rounded up to seconds). For a limit that a later call's deny refunds, the gateway does not wait for the asynchronous refund: it reports `r = min(remaining_i + cost, capacity)` (capacity = burst for GCRA, rate for a sliding window) and, for GCRA, `t` from `max(0, reset_after − cost·T)`; a sliding window's `t` is unchanged. `Retry-After` is the largest `retry_after` among denying enforce limits, local, Redis or degraded.
- `gw_ratelimit_decisions_total{policy, decision, mode}`: one increment per policy that recorded a decision. `decision` is `allow`, `deny`, `fail_open` or `fail_closed`.
- `gw_redis_errors_total{kind}` (counter; `timeout`, `conn`, `script`): one per failed call, refunds included.
- `gw_ratelimit_check_duration_seconds{backend="redis"}`: one observation per request that needed at least one Redis call, from just before its first call to the end of its last call, failure-mode handling included and asynchronous refunds excluded. The histogram gains a 7 ms bucket (the default budget plus the chaos slack below).
- `gw_ratelimit_refund_failures_total` (counter, no labels): Redis refunds that failed.
- Access log `decisions` lists Redis policies after local ones, with the decision values above. Keys, digests of Redis keys and node replies are never logged.

### Decision time header (dev mode only)

When `SPILLWAY_GATEWAY_DEV_MODE` is `true` and the request made at least one Redis call and none of its calls failed (a call skipped because the budget ran out counts as failed), the response carries `Spillway-Decision-Time: <now_us>` (decimal integer) from the last call that ran, on 2xx and 429 alike. Any other response gets no header, and no response does when dev mode is off. The accuracy harness reads it, and the killed-primary chaos test uses its absence to spot failure-mode decisions.

### Correctness bounds (RL-12, RL-29)

- **Clock offset after a failover (RL-12).** Each key lives on one primary, so its decisions use one clock until a failover hands the slot to a replica with its own clock, offset by δ. If the new clock is ahead, stored TATs and window starts look δ older: a key can admit up to `min(burst, δ·rate/period)` extra (GCRA) or up to `δ·rate/period` extra (sliding window), once; if it is behind, the key admits less for δ. Nodes are NTP-synced, so δ is milliseconds at most; in compose every node reads the same host clock, so δ = 0.
- **Lost writes after a failover.** Replication is asynchronous, so writes the old primary acknowledged but did not replicate are lost and those keys look emptier: up to the writes of the replication lag (sub-millisecond in compose) are admitted again. This and δ are why the accuracy bound is checked away from failovers.
- **Key-version rollout (RL-29).** While gateways on `v1` and `v2` run side by side, each key's admissions are split across two independent states, each enforcing the full limit: admission can reach 2× the limit (for GCRA, `2·(burst + w·rate/period)` in a window w) for as long as the rollout lasts, and drops back once only one version runs.

### Example (used by the M3 tests)

Config, with ADR-0017's identity file (alice and bob in tenant `acme`, carol without a tenant):

```yaml
version: 1
ratelimit: { replicas: 3, redis_budget: 5ms }
upstreams:
  - name: httpbin
    endpoints:
      - address: upstream-1:8080
      - address: upstream-2:8080
      - address: upstream-3:8080
routes:
  - name: bytes
    match: { path_prefix: /bytes }
    upstream: httpbin
  - name: anything
    match: { path_prefix: /anything }
    upstream: httpbin
    cost: 5
  - name: delay
    match: { path_prefix: /delay }
    upstream: httpbin
  - name: default
    match: { path_prefix: / }
    upstream: httpbin
policies:
  - name: per-ip-flood               # local, phase 1, never touches Redis (RL-17)
    level: global
    match: { routes: ['*'] }
    key_template: '{ip}'
    algorithm: gcra
    limits: [{ rate: 600, period: 1s }]           # 200/s, burst 200 per replica
    local: true
  - name: delay-inflight             # concurrency: always local
    level: route
    match: { routes: [delay] }
    key_template: '{route}'
    algorithm: concurrency
    limits: [{ rate: 20 }]
  - name: tenant-default             # tenant slot; multi-window (RL-06)
    level: tenant
    match: { routes: ['*'] }
    key_template: '{tenant}'
    algorithm: gcra
    limits:
      - { rate: 100, period: 1s, burst: 200 }
      - { rate: 10000, period: 1h, burst: 500 }
    failure_mode: degraded           # 33/s burst 66 and 3333/h burst 166 per replica
  - name: consumer-minute            # tenant slot with a tenant, {k:} without
    level: consumer
    match: { routes: ['*'] }
    key_template: '{consumer}'
    algorithm: sliding_window
    limits: [{ rate: 1000, period: 1m }]
    failure_mode: fail_open
  - name: anything-writes            # cost 5 per request (RL-07)
    level: route
    match: { routes: [anything], methods: [POST, PUT, PATCH, DELETE] }
    key_template: '{route}:{ip}'
    algorithm: gcra
    limits: [{ rate: 50, period: 1s }]
    failure_mode: fail_closed
  - name: consumer-shadow            # never rejects, even on failure
    level: consumer
    match: { routes: [bytes], methods: [GET] }
    key_template: '{consumer}:{route}'
    algorithm: gcra
    limits: [{ rate: 5, period: 1s }]
    mode: shadow
    failure_mode: fail_closed
  - name: global-bytes               # its own {g:} call after the tenant slot (RL-14)
    level: global
    match: { routes: [bytes] }
    key_template: '{route}'
    algorithm: gcra
    limits: [{ rate: 2000, period: 1s }]
    failure_mode: fail_open
  - name: global-anything
    level: global
    match: { routes: [anything] }
    key_template: '{route}'
    algorithm: sliding_window
    limits: [{ rate: 6000, period: 1m }]
    failure_mode: degraded
```

What runs:

- alice, `GET /bytes/1`: local `per-ip-flood`; call 1 in `{t:acme}` with `tenant-default` (2 limits), `consumer-minute`, `consumer-shadow`; then, if it allowed, call 2 on `{g:global-bytes}`. If call 2 denies, call 1's two `tenant-default` limits and `consumer-minute` are refunded, and `consumer-shadow` too if it did not would-deny.
- carol, `GET /bytes/1`: `tenant-default` does not apply (`{tenant}` unfilled); `consumer-minute` on `{k:TCbZB0wn2J7eWScMCsFLcQ}`, then `consumer-shadow` on `{k:pOkXVg4CPtGamCKbyrDVBQ}` (`digest("carol:bytes")`), then `{g:global-bytes}`.
- anonymous `POST /anything`: `anything-writes` on `{k:<digest("anything:<ip>")>}`, then `{g:global-anything}`, each deducting 5.
- During a Redis outage: alice's `GET /bytes/1` is limited by `tenant-default`'s degraded limiters, `consumer-minute` and `global-bytes` allow, `consumer-shadow` records `fail_closed` without effect; anyone's `POST /anything` gets 503 from `anything-writes`.

### Accuracy harness v1 (NFR-06)

- `cmd/accuracy`, a Go program run as the compose service `accuracy` (profile `harness`, `read_only`, `ulimits.nofile` 65536), so it reaches gateway-1 … gateway-N inside the network (ADR-0004). It sends round-robin over `http://gateway-1:8000` … `gateway-N:8000` (`-replicas N`, 1 to 5), open model like loadgen: requests leave on schedule whatever the responses do.
- Its gateway config is `test/accuracy/gateway.yaml` (version 31), swapped into the mounted `deploy/compose/gateway/config.yaml` and restored on exit as the verify scripts do (ADR-0019); every replica reads the same file. One policy:

  ```yaml
  - name: accuracy
    level: route
    match: { routes: [accuracy] }      # a route to upstream-1 at /bytes
    key_template: '{header:X-Accuracy-Key}'
    algorithm: gcra
    limits: [{ rate: 100, period: 1s, burst: 100 }]
    failure_mode: fail_closed
  ```

  Requests are anonymous, so each key is its own `{k:}` call and the M keys spread over all three primaries. `ratelimit.replicas` stays 1: a fail-closed policy has no degraded limiter.
- **Load.** M = 10 keys per pattern, each driven at 2× its limit (200/s, so 2,000 rps in all) for 60 s; the three patterns run one after another, each with fresh keys (`<pattern>-<run id>-<k>`), so state never carries over:
  - **uniform**: key k sends every 5 ms, offset by k·0.5 ms;
  - **bursty**: key k sends 100 requests at once every 500 ms, keys offset by k·50 ms;
  - **boundary**: at every whole second of the harness clock, key k sends 200 requests: 100 evenly spread over the 10 ms before the second and 100 over the 10 ms after it.
- **Check.** Every 2xx is an admission at its `Spillway-Decision-Time`. Per key, timestamps are sorted, and for each window length w in {10 ms, 100 ms, 1 s, 10 s, 60 s} and each admission `t_i`, the admissions in the closed window `[t_i, t_i + w]` are counted against `floor(burst + w·rate/period)`. Over-admission is the count above the bound.
- **Report** (JSON to stdout, one summary line per pattern to stderr): per pattern the keys, scheduled and sent requests, 2xx, 429, 503 and other answers, failure-mode admissions (2xx without the header), max over-admission with the key and window where it happened, mean admitted rate per key vs the limit (expected ≈ 100/s plus burst/60 s), and p99 response latency over all responses as the harness measured it, reported as the design's p99 decision latency (the harness cannot see inside the gateway, and a response's latency bounds its decision's from above); plus the replica count and the hardware like loadgen.
- **PASS** when, for every pattern: max over-admission is 0, at least 99% of the scheduled requests were sent, and there are no failure-mode admissions (the policy is fail-closed, so a 2xx without the header means a check the harness cannot verify). 503s are printed, not judged.
- `make accuracy REPLICAS=N` (default 3) starts gateway-2 … gateway-N (profile `replicas`) and the run; the PR records runs at 1 and 3 replicas, and 5 when the laptop sustains it.

### Chaos (NFR-13)

Scripts in `test/chaos/`, run from the host with the docker CLI (no container gets the Docker socket), loadgen and the harness sender in the compose network, each with its own config (`test/chaos/*.yaml`, versions 41 and 42) swapped in and restored like the verify configs. Each script restores the cluster on every exit (trap): the paused or killed node is brought back, and the script waits until all six nodes report `cluster_state:ok`, all 16,384 slots are covered, there are three primaries with a replica each, and gateway-1 answers a request with `Spillway-Decision-Time` again. The killed node comes back empty (its `nodes.conf` is on tmpfs), so the script runs `CLUSTER FORGET <old node ID>` on the other five nodes and rejoins it as a replica with `redis-cli --cluster add-node … --cluster-slave`; if that fails, the script recreates the cluster (`docker compose up -d --wait --force-recreate redis-0 … redis-5 redis-init`). The scripts stop gateway-2 … gateway-5 when they finish.

- **(a) Full outage** (`make chaos-redis-outage`). gateway-1 … gateway-3, `ratelimit.replicas: 3`, three routes and one route-level policy each, keyed `{route}`: `chaos-open` (GCRA 10,000/s, `fail_open`), `chaos-closed` (GCRA 10,000/s, `fail_closed`), `chaos-degraded` (GCRA 300/s, burst 30, `degraded`: 100/s, burst 10 per replica). After a 5 s warm-up with Redis up, the script runs `docker pause` on all six nodes, then drives for 20 s: 200 rps on the open route and 200 rps on the closed route through gateway-1, and 200 rps on the degraded route to each of gateway-1, gateway-2 and gateway-3 separately; then it unpauses and restores. Pass, all of:
  - fail-open: ≥ 99.9% of the open route's responses are 2xx;
  - fail-closed: every response of the closed route is 503;
  - degraded: for each replica, `(2xx − 10) / 20 s` is within ±10% of 100/s;
  - within one budget: across gateway-1 … gateway-3, every `gw_ratelimit_check_duration_seconds{backend="redis"}` observation made during the 20 s falls in the `le="0.007"` bucket — the 5 ms budget plus a **2 ms slack** for timer firing, goroutine wake-up and GC assists inside Docker Desktop's VM;
  - every loadgen run is valid (≥ 99% of its target sent).
- **(b) Killed primary** (`make chaos-redis-primary`). gateway-1 alone; one route-level `fail_open` GCRA policy at 10,000/s keyed `{header:X-Chaos-Key}`, so each of 30 keys is its own `{k:}` call; the sender knows each key's slot (CRC16 of `k:<digest>`) and which primary owns it from `CLUSTER SHARDS`. It drives every key at 20 rps for 40 s and `docker kill`s the primary owning slot 0 after 10 s. A response is a Redis decision when it carries `Spillway-Decision-Time`, a failure-mode decision when it is a 2xx without it. Pass, all of:
  - every request for a key on the two surviving primaries is a Redis decision, for the whole run;
  - keys on the killed primary get failure-mode decisions only between the kill and the end of their failover window, and Redis decisions after it;
  - the failover window, from the kill to the first Redis decision for each of the killed primary's keys (the maximum over those keys), is printed and at most 10 s. Expected: `cluster-node-timeout` (3 s) plus the replica election (about 1 s) plus up to one breaker interval (1 s) before the client reloads the slot map.
- **Partial coverage.** With Redis's default `cluster-require-full-coverage yes`, every node answers `CLUSTERDOWN` for every slot from the moment the dead primary is marked failed until its replica is promoted, so keys on the surviving primaries would fail too and (b) could not pass. The compose `redis.conf` therefore adds `cluster-require-full-coverage no` (m3-chaos; ADR-0004's other node settings stay): limiter keys are independent, so a slot whose owner is down should cost only its own keys. Any deployment of Spillway's limiter cluster wants the same setting.

The measured numbers go into docs/runbooks or the README with m3-chaos. CI runs neither scenario: each pauses or kills shared cluster nodes for tens of seconds and would make the integration job slow and order-dependent.

### Exit check (`make verify-m3`)

`test/verify/m3.sh`, in `m2.sh`'s style, one PASS/FAIL line per measured number:

- (a) **NFR-06**: the accuracy harness at 3 replicas, all three patterns, PASS per the harness rule above (zero over-admission for GCRA in strict mode).
- (b) **NFR-13**: both chaos scenarios, PASS per their rules.
- (c) **NFR-04**: the U4 method below at 1,000 rps, 30 s warm-up and 60 s measured per run (`VERIFY_M3_RATE`, `VERIFY_M3_WARMUP`, `VERIFY_M3_DURATION` override them for a quick look), direct to `upstream-1:8080/bytes/1024` vs through gateway-1 with `test/verify/m3-latency.yaml` (version 32): one route to upstream-1 and one global-level Redis GCRA policy (NFR-04: "with a global policy active"), `verify-redis`, keyed `{route}` at 100,000/s, `fail_closed`, so every request makes one `{g:verify-redis}` call that allows it and a failure shows up as a 503 instead of a faster local answer. Each gateway run also needs `gw_redis_errors_total` unchanged and `gw_ratelimit_decisions_total{policy="verify-redis", decision="allow"}` to cover every request. PASS when 0 ≤ added p99 ≤ 5 ms.

CI keeps running the script tests and the M3 integration tests through `make test-integration`; the harness, the chaos scenarios and the latency pairs stay local, as ADR-0019 argued for NFR-03.

### Tasks and interim behaviour

| Task (branch) | Builds | Gateway behaviour once merged |
| --- | --- | --- |
| m3-design-adrs | this ADR, ADR-0022 | unchanged |
| m3-scripts | `internal/ratelimit/redis`: client, `limiter_v1.lua`, keys, `Call`, `Refund`, `Limiter`, `Error`, the Redis 7 check; unit tests (keys, ARGV, reply decoding, the no-resend test); script tests in `test/integration` (below) | unchanged: Redis policies are still rejected |
| m3-global | proto fields and compile rules above (`failure_mode`, `redis_budget`, tenant names); `SPILLWAY_GATEWAY_REDIS_ADDRS` and `SPILLWAY_REDIS_ADDR_MAP` in the gateway; the pipeline: grouping, order, refunds, headers, decisions, access log | Redis policies run. **Interim:** `failure_mode` and `redis_budget` are validated but not acted on; each call gets its own deadline of `redis_budget`; there is no breaker; any failed call is treated as allowed for all its policies (recorded as `fail_open`) and logged at warn with its node and kind, never keys |
| m3-failure | one budget per request, breakers, failure modes, degraded limiters, the 503, `gw_redis_errors_total`, `gw_ratelimit_check_duration_seconds{backend="redis"}` with its 7 ms bucket, `gw_ratelimit_refund_failures_total` | as this ADR, minus the decision-time header |
| m3-accuracy | `SPILLWAY_GATEWAY_DEV_MODE` and `Spillway-Decision-Time`; `cmd/accuracy`, its compose service and config, `make accuracy` | as this ADR |
| m3-chaos | `test/chaos/`, `make chaos-redis-outage`, `make chaos-redis-primary`, `cluster-require-full-coverage no` in the compose `redis.conf`, the measured failover window in the docs | as this ADR |
| m3-verify | `make verify-m3`; U4 for NFR-04 and for verify-m2's NFR-03; ADR-0019's status line becomes "Accepted; NFR-03 method superseded by ADR-0021" | as this ADR |
| M3 review-fix PR | U1, U2, U3 | as this ADR, with U1–U3 |

**Script tests** (m3-scripts; build tag `integration`, on the compose cluster through the address-map dialer, locally and in CI): decisions come from `TIME` (two calls report increasing `now_us` and GCRA decisions match the maths for those times); every key of a call shares one slot, and `{g:}` keys of different policies land on different slots; the write rule (a denied call writes nothing; a shadow would-deny is not written while the enforce limits are); refunds reverse only what they are given and never go below empty, including a sliding-window refund across a window boundary; TTLs on every key after every write; `SCRIPT FLUSH` on the node, then a call that succeeds through the `EVAL` fallback; multi-window and cost; a wrong-type key; and a timeout injected after execution — a dialer whose connection delays the reply past the call's deadline — that leaves exactly one deduction.

### Decisions taken for the user (review)

M2's review left four items for the user. The run that builds M3 took the user's own suggested defaults; each is recorded here for the user to confirm or change.

- **U1. Key cap for local limiters.** Templates with client-chosen values (`{header:X}`, `{ip}`) can grow a local limiter's maps without bound. Each local limiter table, degraded limiters and concurrency included, holds at most **100,000 keys**, enforced per shard as 1,563 of its 64 shards (100,032 in all); digests spread keys evenly, so a per-shard count needs no shared counter. A request whose key is new to a full shard skips that limit: no decision, no header item, no deduction, as if the template had not filled; the limit's other keys keep working and its policy's other limits still apply. Each skip increments `gw_ratelimit_key_cap_skips_total{policy}` (counter). The sweep (ADR-0017) frees room as keys go idle. Memory at the cap: a Go swiss-map slot holds the 16-byte string header, a value of 8–24 bytes and a control byte at up to 7/8 load, plus the 16-byte key allocation, about 60–80 bytes per key and twice that just after the map grows, so at most about 16 MB per limiter. A constant, not a setting: nothing in the design needs it tuned, and it bounds memory against an attacker without touching ordinary traffic (100,000 distinct live keys per policy limit per replica). Rationale: skipping keeps every key that already has state limited and fails open only for new keys under attack, whereas evicting would reset a limited key and rejecting would turn a memory guard into an outage. Implemented in the M3 review-fix PR.
- **U2. IPv6 `{ip}`.** An IPv6 client fills `{ip}` with its /64 network in prefix form (`netip.Prefix.String()`, e.g. `2001:db8:1:2::/64`); IPv4 and IPv4-mapped addresses (normalised to IPv4, ADR-0017) stay the full address. Applies to local and Redis keys alike; the access log and `X-Forwarded-For` keep the full address. Rationale: a /64 is the smallest block a single subscriber is normally given, so a full /128 lets one client rotate through 2⁶⁴ addresses and get a fresh bucket each time. Implemented in the M3 review-fix PR.
- **U3. `;` dot segments and backslashes.** Before routing, the gateway answers **400** (`invalid request path`, plain text like the other M1 errors, `origin="gateway"`) when `r.URL.Path` — the path percent-decoded once by Go's server, so `%2e`, `%2E`, `%3b`, `%3B`, `%5c` and `%5C` arrive decoded, in either hex case — contains a backslash, or has a segment (split on `/`) whose text before its first `;` is `.` or `..` (`..;`, `.;`, `..;jsessionid=x`). Nothing is decoded twice: `%252e` stays the literal `%2e` and is not rejected. Rationale: ADR-0007's cleaning treats `..;` as an ordinary segment and keeps `\`, while Java servlet containers and some Windows stacks treat them as `..` and `/`, so a path routed and limited as `/public/..;/admin` could reach `/admin` upstream. This adds a rejection before ADR-0007's cleaning and changes nothing else in it. Implemented in the M3 review-fix PR.
- **U4. Added-latency method (NFR-03, NFR-04).** Three alternating run pairs: direct, gateway, direct, gateway, direct, gateway, each with its warm-up. Added p99 = median of the three gateway p99s − median of the three direct p99s. A run below 99% of its target rate, or with any non-2xx answer, fails the check, and so does a negative added p99: the gateway cannot make requests faster, so a negative result means the measurement was noise. Rationale: ADR-0019's single pair passed on a negative difference and let one noisy run decide; medians of alternating runs cancel drift in the laptop's load. Implemented in m3-verify for NFR-04 and, in the same PR, for verify-m2's NFR-03 check (target 2 ms); this supersedes ADR-0019's NFR-03 measurement.

## Consequences

- Every policy without `local: true` now needs Redis: an M2 config with a non-local, non-concurrency policy was rejected before, so no valid config changes meaning, but a gateway with Redis policies and no `SPILLWAY_GATEWAY_REDIS_ADDRS` refuses to start.
- A request with Redis policies pays one round trip for the tenant slot (or one per `{k:}` digest) and one per global policy; configs keep global policies few. A global policy keyed by a client value (`{ip}`) puts all those keys in one slot on one node.
- Non-local identity-free policies run after API key resolution, so floods of bad keys get 401 without meeting them; IP flood limits belong in local policies (RL-17), as the example does.
- Denials cost more than admissions: refunds are extra Redis calls, though off the request's path.
- Failure handling is per node: one slow primary costs its keys at most one budget per request until its breaker opens, and other primaries are untouched.
- With the decision-time header only in dev mode, production responses reveal nothing about Redis timing.
- U1 lets an attacker who fills a limiter's table bypass that one limit for new keys; the metric shows it, and other policies (the IP flood limit) still apply.
- go-redis's `NoRetry` path is a dependency on library internals; the RL-16 tests catch a regression on upgrade.

## Alternatives considered

- **Letting go-redis retry and relying on `MaxRetries: 0` alone** — the cluster loop still resends after EOF and some timeouts, which can deduct twice.
- **Sending only to `MasterForKey`'s node client** — kept as the fallback; it needs the limiter to follow `MOVED`/`ASK` itself, which go-redis already does correctly.
- **`SCRIPT LOAD` on every primary at start or on `NOSCRIPT`** — a broadcast on the request path, and start-up would depend on Redis (GW-24); `EVAL` on the one node that lacks the script is one call.
- **Refunds without `admitted_us`** — a sliding-window refund that crosses a window boundary would subtract from the wrong window and could admit more than the limit.
- **Synchronous refunds** — every global deny would wait one more round trip inside the budget, for state that only ever errs towards denying.
- **The algorithm in the key name** — changes the design's key format; reading a wrong-typed key as empty costs nothing on the normal path.
- **A `breaker_open` error kind** — clearer in dashboards, but it adds a label value the design does not list, and the transition logs already show when a breaker was open.
- **`reason="ratelimit_unavailable"` for the 503** — the design's OB table already names `fail_closed`, and NFR-11 excludes it by that name.
- **A per-request failure mode instead of per policy** — RL-23 makes it per policy.
- **Local `Allow` per limit with a Redis adapter for each** — one round trip per limit instead of one per slot, and the in-slot write rule could not hold.
- **Recording failure-mode outcomes as `allow`/`deny`** — the chaos tests and dashboards could not tell a Redis decision from a fallback.
