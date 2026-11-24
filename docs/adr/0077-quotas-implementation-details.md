# 0077. Quotas: implementation details

- **Status:** Accepted
- **Date:** 2026-11-25
- **Requirement IDs:** RL-10, RL-11, RL-14, RL-21, RL-23, CP-03, CP-22

## Context

ADR-0070 settles quotas and the flusher; building them left a handful of choices it does not make: how many limits a tenant-slot call may carry once quotas join it, which compile the control plane validates with, the script's exact reply and writes, what a request gets when its quotas are unseeded but a policy denies it anyway, how quickly a tenant that has just got its first quota is seeded, and whether quota denies feed the heavy hitters. None changes ADR-0070's decisions; each is recorded so later tasks build on the same reading.

## Decision

- **Call limit.** The script carries at most 32 limits (`shadow_would_deny` is a 32-bit mask). In managed mode every route's tenant-slot call keeps two of them for a consumer's quotas, so its policies may take at most 30 (`route "a" would carry 31 Redis limits in one call; at most 30, beside the 2 a consumer's quotas take (RL-10)`); static mode, which has no quotas, keeps 32. The reserve applies to every managed config, since a quota override can give quotas to any consumer.
- **The control plane compiles as a managed gateway.** `POST /config/validate`, publish and the override checks compile with `config.CompileManaged`, which differs from `Compile` only in naming policies by ID, so plans with quotas validate (static mode rejects them) and the reserve above is checked before a publish, not at the gateway.
- **Script reply and writes.** `quota_unseeded` is the ninth summary field, after `now_us` and before the per-limit fields (`allowed_i, remaining_i, reset_after_ms_i, retry_after_ms_i, granted_i`). A quota limit reports `denying_level` `consumer` (its level in ARGV). Its counter is written with `SET … PX` to the new count (one atomic script, so the same as `INCRBY` plus `PEXPIRE`, and it replaces a key of another type); a refund `SET`s the decremented count with `KEEPTTL`. A refund skips quota limits only while the epoch is missing, not when `TIME` has crossed the period's end since the call, because the key names its period.
- **Unseeded quotas and a deny.** The failure-mode rule for unseeded quotas (503 if an enforce `fail_closed` policy shares the call) applies only when the call allowed; a call a policy denied is a 429 as usual, since its answer does not depend on the quota. The quota's decision in the access log and `gw_ratelimit_decisions_total` is `fail_closed` when the request is answered 503 for the call, else `fail_open`.
- **Heavy hitters.** The `quota/daily` and `quota/monthly` stand-in policies have no control-plane ID, so their denies are not reported in heartbeats' `top_keys` (RL-21 names policies); the 429 rate and the decisions metric still count them.
- **Seeding a new quota tenant.** The flusher's 1 s check reads the config and identity versions first and, when either changed since the last flush, flushes at once instead of checking epochs. A tenant that gets its first quota (a new consumer, a plan change, a publish, a quota override) is seeded within about a second, so its unseeded window is no longer than after a Redis data loss.
- **Flusher details.** The flusher's cluster client has `MaxRetries` −1 (none) and 2 s dial, read and write timeouts; the epoch value's replica name is the host name; `flushed_at` is Redis's `TIME`; counters are read with `MGET` in chunks of 1,000 keys and seeded in chunks of 500, the epoch with the last chunk; a row of a consumer deleted during the flush is dropped, not an error; `/quota-usage` uses the control plane's clock for the current day and month and answers the consumers that exist, in the order asked, once each.

## Consequences

- A managed config whose route already carries 31 or 32 Redis limits in one call fails validation from this release; none of the repository's configs comes near.
- A quota override takes effect on the gateways at once and on the counters within about a second, without waiting for a flush.
- Abusive consumers caught only by their quotas do not appear in the heavy-hitter screen; their 429s show on the Overview.

## Alternatives considered

- **Reserving the two limits only when a plan has quotas** — a quota override (identity) can give quotas to a consumer whose plan has none, and identity is not validated against the config, so such a call could exceed 32 limits and fail at the gateway.
- **Seeding new tenants only at the next 10 s flush** — with a `fail_closed` policy in the call, a newly created consumer would get 503s for up to 10 s.
- **Counting quota denies in the heavy hitters under a reserved policy ID** — needs a heartbeat and schema change for a screen that lists policies.
