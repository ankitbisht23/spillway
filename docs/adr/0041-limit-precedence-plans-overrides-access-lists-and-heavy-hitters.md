# 0041. Limit precedence: plans, overrides, access lists and heavy hitters

- **Status:** Accepted
- **Date:** 2026-11-13
- **Requirement IDs:** RL-02, RL-03, RL-08, RL-09, RL-21, RL-23, RL-25, RL-26, CP-03, CP-20, UI-01, OB-01

## Context

RL-08 adds plans with `default_limits` per policy, `from_plan: true` policies that take their limits from the consumer's plan, per-consumer overrides keyed by (consumer, policy) on consumer- and route-level policies, and time-boxed burst grants (overrides with an end). RL-09 adds allow and deny lists by key, consumer or CIDR: the CIDR denylist before authentication, key and consumer lists right after it, all identity changes. RL-21 adds a per-replica Space-Saving sketch of the top 100 throttled keys, reported in heartbeats and kept as `heavy_hitters` windows. The user set D3: deny list > allow list > (consumer, policy) override > plan `default_limits` > the policy's own limit. Open:

- what an allow list does (exempt from limits, or the only subjects let in), what a deny or a suspended consumer answers, and where each check sits in the pipeline;
- which policies plans and overrides may target, so that one Redis or local key is never checked against two consumers' different limits;
- which of several overlapping overrides wins, and what a `from_plan` policy does when no plan gives it limits;
- the sketch's key, what counts as "throttled", the heartbeat field and the window size.

## Decision

### Plans and `from_plan` (RL-08)

- Plans are config (ADR-0005): `Plan { id, name, default_limits: [{policy, limits}] }`, policy by name within the config. A plan's limits replace the named policy's own `limits` for consumers on that plan.
- A policy with `from_plan: true` has no `limits` of its own (setting both is an entity-rule error).
- **A plan entry, a `from_plan` policy and an override may target only a policy that** has `level: consumer` or `route` (RL-08's override rule, applied to plans too), and whose `key_template` contains `{consumer}` or `{api_key}`. The second condition makes every counter belong to one consumer: a route-level policy keyed `{route}` is one counter shared by all consumers, and checking it against each consumer's own limit would make the limit depend on who asked last. Global- and tenant-level policies keep one limit for all.
- **Validation** (CP-03, ADR-0037): every plan has a `default_limits` entry for every `from_plan` policy; every entry's limits pass the policy algorithm's rules (`burst` GCRA-only, defaults and bounds of RL-03, one limit for concurrency) and RL-03's cost rule for every route the policy matches. So every consumer, whose tenant must have a plan (`tenants.plan_id NOT NULL`), always finds limits for a `from_plan` policy.
- **A consumer's plan** is `consumers.plan_id`, or its tenant's `plan_id` when that is null. Changing either is an identity change and applies at once; a plan's limits are config and change with a publish (ADR-0005).

### Overrides and burst grants (RL-08)

- Overrides are identity (CP-20): `{consumer_id, policy_id, limits, starts_at, ends_at, note}`. `limits` replaces the policy's limits entirely (all windows), checked like a plan entry against the latest published policy when the override is written, and again at every publish (ADR-0037).
- An override with `ends_at` is a **burst grant**; without it, a **standing override**. At most one standing override per (consumer, policy) (a partial unique index, ADR-0036); burst grants may overlap each other and a standing override.
- **Active** at time t (the gateway's clock): `starts_at` is null or ≤ t, and `ends_at` is null or > t.
- **Which override applies**, among the active ones for (consumer, policy): a burst grant beats a standing override; among burst grants, the latest `starts_at` (null counting as earliest), then the highest `id`. A grant is the newer, deliberate exception, so it wins while it lasts, and the standing override returns when it ends — without any message, since the gateway compares the times itself (ADR-0037).

### Access lists and status (RL-09)

- **Subjects**: `cidr` (matched against the client IP of SEC-05 and ADR-0017), `key` (an API key's ID) and `consumer` (a consumer's ID). Actions: `deny` and `allow`. Every change is an identity change (CP-20), so a block reaches all replicas like a revocation (≤ 5 s, demo scenario D7).
- **Deny** answers **403** `{"error":"forbidden"}` (ADR-0039), `origin="gateway"`, `reason="none"`, always access-logged. A **suspended** consumer, or a consumer of a suspended tenant, is answered the same way.
- **Allow** exempts a request from **every** rate-limit policy that has not run yet, concurrency included: no decision, no metric, no `RateLimit` header, no heavy-hitter count. An allow entry never lets in what a deny entry or a status blocks (D3: deny > allow), and never bypasses authentication.
- **Pipeline** (the design's, made exact; ADR-0017's phases):
  1. Route match (404 if none).
  2. **CIDR deny** → 403. **CIDR allow** → mark the request exempt.
  3. Phase 1 (identity-free local policies), skipped when exempt.
  4. Authentication (ADR-0039): 401s.
  5. **Key deny, consumer deny, suspended consumer or tenant** → 403, exempt or not. **Key allow, consumer allow** → mark exempt.
  6. Phase 2 and the Redis policies, skipped when exempt, each with its effective limits (below).
  
  A CIDR-allowed client with a revoked key still gets 401, and with a denied key 403.

### Precedence (D3)

For a request resolved to consumer C (tenant T) and an applying policy P, after steps 2 and 5 above:

1. **Deny** (CIDR, key, consumer, or a suspended status) → 403; nothing else runs.
2. **Allow** (CIDR, key, consumer) → P does not run.
3. **Override**: the active override for (C, P) chosen above → its `limits`.
4. **Plan**: C's plan has an entry for P → that entry's `limits`.
5. **Policy**: P's own `limits`. A `from_plan` policy that reaches this step (an anonymous request cannot, as its template does not fill; a valid config gives every plan an entry) does not apply to the request, like an unfilled template (ADR-0017).

The chosen limits then go through the usual machinery: per-replica division by the live replica count for local and degraded limiters (RL-02, RL-23, ADR-0038), the Redis script's ARGV for Redis policies, the `RateLimit` and `RateLimit-Policy` fields (ADR-0018) and the metrics, which keep their labels (no consumer label, OB-01). Redis key names include the period, so limits with other periods use other counters: a consumer switching between a plan and an override with different windows starts those windows fresh.

### Worked examples

Config: policy `per-consumer` (consumer level, `{consumer}`, GCRA 100/s, burst 100); policy `orders-plan` (consumer level, `{consumer}:{route}`, GCRA, `from_plan: true`); plans `free` (`orders-plan`: 10/s, burst 20) and `pro` (`orders-plan`: 100/s, burst 200; `per-consumer`: 500/s, burst 500). Tenants `acme` (plan `pro`) and `tiny` (plan `free`); consumers `acme/bot` (no plan of its own), `acme/batch` (plan `free`), `tiny/app`.

| Request | `per-consumer` | `orders-plan` | Why |
| --- | --- | --- | --- |
| `tiny/app` | 100/s | 10/s | `free` has no entry for `per-consumer`, so the policy's own limit; `orders-plan` from `free` |
| `acme/bot` | 500/s | 100/s | tenant's plan `pro` has both |
| `acme/batch` | 100/s | 10/s | its own plan `free` beats the tenant's |
| `acme/batch`, standing override `orders-plan` 30/s | 100/s | 30/s | override > plan |
| … and a burst grant `orders-plan` 50/s, 10:00–11:00 | 100/s | 50/s from 10:00 to 11:00, then 30/s | grant > standing override, ends by itself |
| `acme/bot` from a CIDR on the allow list | — | — | exempt: no policy runs |
| `acme/bot` on the consumer allow list, its key on the deny list | — | — | 403: deny > allow |
| anonymous | skipped | skipped | `{consumer}` does not fill |

### Heavy hitters (RL-21)

- **Sketch.** Each gateway keeps one Space-Saving sketch of **100 counters** over all policies, keyed by (policy ID, key digest). An **event** is one policy's decision on one request that is a `deny` in enforce mode or a would-deny in shadow mode (RL-26), from any limiter: local, Redis, degraded or concurrency. Failure-mode outcomes (`fail_open`, `fail_closed`) are not throttling and do not count. A request denied by two policies is two events. Each event counts 1, whatever the route's cost.
- **Entry**: policy ID; `key_digest`, the base64url form (22 characters) of the 16-byte digest of the filled key template, the same digest as the policy's Redis key or local map key, so raw keys and header values never leave the gateway; `consumer_id` and `api_key_id` of the latest event (0 when anonymous), so the UI can block the key or the consumer (demo scenario D7); `denied` and `shadow_denied`, counted since the entry entered the sketch. When the sketch is full, a new key replaces the entry with the smallest total and starts from that total plus one, Space-Saving's rule, which keeps every key with more than 1/100 of the events. The reported `denied` and `shadow_denied` count only events since insertion, so they never overstate.
- **Heartbeat.** Every heartbeat carries all entries as `top_keys`, with `interval_start` (ADR-0038). A successful heartbeat resets the sketch; a failed one keeps it accumulating, so nothing is lost while the control plane is down beyond what 100 counters can hold. Managed mode only.
- **Storage.** The control plane adds each entry to the row (`window_start` = the minute of receipt, `date_trunc('minute', clock_timestamp())`, `policy_id`, `key_digest`), summing `denied` and `shadow_denied` across replicas and heartbeats, and keeping the latest non-zero `consumer_id` and `api_key_id`. One-minute windows give UI-01's shadow report a resolution finer than its windows; at most 100 keys per replica per 5 s, they stay small. The active replica prunes windows older than **7 days** every hour (ADR-0038).
- **Read.** `GET /api/v1/heavy-hitters?since=&until=&policy_id=&limit=` sums the windows in [since, until) (default the last 15 minutes, at most 7 days), groups by (policy, digest) and orders by `denied + shadow_denied`, at most 500 rows (ADR-0036). M6's shadow report reads `shadow_denied` from the same windows.

## Consequences

- A key template without `{consumer}` or `{api_key}` cannot carry plan or override limits; a per-route shared limit stays one number for everyone, which is what a shared counter can enforce.
- Every consumer has a plan, because every tenant must; creating the first tenant therefore needs a published plan.
- An allow-listed client is never rate limited, concurrency included. Allow-listing is for trusted internal callers, not for raising a limit: that is what overrides are for.
- Local limiters now take their limits per call (the effective limits differ by consumer), and the limiter registry creates limiters for periods that plans and overrides introduce; the plans task carries that change and GW-29's reload test.
- The sketch is per replica; the control plane's sum over replicas is exact for keys every replica tracked and a lower bound otherwise.

## Alternatives considered

- **Allow list as "only these may pass"** — D3 places allow above overrides in the limit order, so it is about limits; restricting access is what `auth_mode` and deny entries do.
- **Plans and overrides on any policy** — a shared counter would be judged against whichever consumer's limits came with each request.
- **Rejecting overlapping overrides** — burst grants on top of a standing override are the normal case; a deterministic order is simpler than an exclusion constraint.
- **A `from_plan` policy denying when no plan applies** — validation makes it unreachable for authenticated requests, and denying anonymous traffic is `auth_mode`'s job.
- **Counting by cost in the sketch** — "top throttled keys" is about how often a key is throttled, not how expensive its requests are.
- **One sketch per policy** — up to 100 × policies entries per heartbeat; RL-21 asks for the top 100.
- **Five-minute windows** — coarser than UI-01's shortest useful window.
