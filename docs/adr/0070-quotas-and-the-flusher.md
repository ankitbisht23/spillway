# 0070. Quotas and the flusher

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** RL-10, RL-08, RL-11, RL-12, RL-13, RL-14, RL-23, RL-25, CP-01, CP-10, CP-12, CP-13, CP-14, CP-20, CP-22, UI-04, UI-06

## Context

RL-10 asks for daily and monthly quotas (UTC calendar days and months) set per plan as `quotas: { daily, monthly }` in route cost units, with per-consumer overrides, counted in Redis per consumer and period, and a control-plane flusher that, one at a time under a Postgres advisory lock, reads the counters every 10 s and writes absolute values (`used = GREATEST(used, $n)`); a per-tenant epoch marker detects lost Redis data, the flusher then re-seeds the tenant's counters from Postgres, and until then its quota checks follow "the policy's failure mode"; maximum under-count is one flush interval. The design's Redis keys table names `q:v1:{t:<tenant_id>}:<consumer_id>:<period>` (TTL period + 1 day) and `q:v1:{t:<tenant_id>}:epoch`; the schema table names `quota_usage` and overrides with `policy_id` null for a quota override; the Consumers & keys screen gets its quota-usage column now (deferred from M6, ADR-0065).

M5 left hooks: `plans.spec` has room for `quotas` (ADR-0036), `overrides.quotas jsonb` exists, and `overrides.policy_id` carries `CHECK (policy_id IS NOT NULL)` "until M7". Not settled: where the check runs and in what order, which failure mode applies to a quota (a quota is not a policy), how periods are chosen without gateway clocks deciding (RL-12), the exact keys, scripts, schema and API, and what the flusher does at rollover, on data loss and with two control planes. ADR-0069 extends the limiter script contract; this ADR adds quota limits to it.

## Decision

### Model (D4)

- **Plans.** `config.proto`: `message Quotas { int64 daily = 1; int64 monthly = 2; }`, `Plan.quotas = 4`. 0 or unset means no quota for that period. Validation: each ≥ 0 and ≤ 10^15. In YAML and the API it is the design's `quotas: { daily, monthly }`, in route cost units (RL-07).
- **Quota overrides** (identity, CP-20). `Override.quotas = 7` (`Quotas`); an override with `policy_id` 0 (NULL in Postgres) is a quota override. A quota override replaces both values of the consumer's plan quotas (0 = no quota for that period) while it applies (`starts_at` ≤ now < `ends_at`, the same rule and gateway clock as limit overrides, ADR-0041); at most one standing quota override per consumer, time-boxed ones allowed as burst grants.
- **Effective quotas** of a request: its consumer's applying quota override, else the quotas of the consumer's plan (its own `plan_id`, else its tenant's). Anonymous requests and requests without a tenant have none.
- **Managed mode only.** Keys need tenant and consumer IDs, and the flusher lives in the control plane. Static config with a plan that has non-zero quotas is rejected at load with `quotas need managed mode (RL-10)`.

### Redis keys

- Counter: `q:v1:{t:<tenant_id>}:<consumer_id>:<period>`, `<period>` = `d<YYYYMMDD>` for a UTC day or `m<YYYYMM>` for a UTC month (`q:v1:{t:42}:1007:d20261124`). Value: an integer count of cost units. TTL: set on every write to the time left in the period + 1 day.
- Epoch: `q:v1:{t:<tenant_id>}:epoch`. Value: `<unix ms of the seed>-<control-plane replica name>` (for diagnosis only; only existence matters). TTL 32 days (the longest month + 1 day), refreshed by every flush of the tenant.
- Both carry the tenant's hash tag, so they live in the tenant's slot (RL-13), next to its rate-limit keys.

### The check on the hot path

- **Where.** Quota limits ride in the request's **tenant-slot call** (ADR-0021), so a request's tenant, consumer and route limits and its quotas are decided atomically in one script call (RL-11). A request with effective quotas whose tenant slot has no applying policy gets a call with just its quota limits.
- **Order.** The call's decision is the AND of its enforce limits; quota limits are always enforce. By the script's write rule a quota is counted only when every limit of the call allows, and a rate-limit deduction happens only when the quota allows too. If the global level then denies, the refund reverses the quota increment as well (RL-14). So a quota counts exactly the requests the limiter admitted, whatever the upstream then answers. Local phases run before, so a locally denied request is never counted.
- **Script.** Each period with a non-zero quota is one limit in the call: algorithm `quota`, `rate` = the quota, `period_ms` = 0, `burst` 0, `lease_k` 0, its own `group`, and `period_start_ms` / `period_end_ms` = the UTC bounds of the period the gateway chose (ADR-0069's per-limit ARGV), and KEYS gets the counter key and, once per call, the epoch key (ADR-0069's contract). The script:
  - if the epoch key does not exist, skips every quota limit (neither decides nor is written) and returns `quota_unseeded = 1`;
  - if Redis `TIME` is outside `[period_start_ms, period_end_ms)`, the gateway's clock and Redis disagree about the period (only within the clock offset of a UTC boundary): it skips the quota limits and returns `quota_unseeded = 1` as well, so a request is never counted into the wrong period by a gateway clock (RL-12);
  - otherwise allows iff `count + cost ≤ quota`, reports `remaining = quota − count` (after the write when allowed), `reset_after_ms = retry_after_ms = period_end_ms − now`, and on write `INCRBY cost` and `PEXPIRE` to `period_end_ms − now + 86,400,000`. A refund (negative cost) `DECRBY`s a counter it wrote, never below 0.
- **Unseeded and Redis failure (the failure mode).** A quota is not a policy, so RL-10's "the policy's failure mode" is read as the failure modes of **the policies checked in the same tenant-slot call**: when that call fails (Redis error, no budget left) or returns `quota_unseeded = 1`, the request is answered **503** (`reason="fail_closed"`) if any enforce policy in the call is `fail_closed`; otherwise its quota is not enforced for that request (it is allowed by the quota and not counted). A degraded policy's local limiter still applies to its own limits; there is no local quota counter (a per-replica daily count is meaningless after a restart). A quota-only call has no policy and is fail_open. P24 proposes this wording for RL-10.
- **Exhaustion.** 429 like any deny (`reason="ratelimited"`), `Retry-After` = seconds until the exhausted period ends, rounded up (ADR-0018's largest-denying rule). `RateLimit-Policy` / `RateLimit` items, added after the policy items whenever the consumer has a quota: `"quota/daily";q=<daily>;w=86400` and `"quota/monthly";q=<monthly>;w=<seconds in this UTC month>`, with `r` = remaining and `t` = seconds to the period's end. The `/` in the name cannot clash with a policy item (policy names have no `/`, and `<policy>/<n>s` ends in `s`). `gw_ratelimit_decisions_total` records them as `policy="quota/daily"` and `policy="quota/monthly"`, mode `enforce`. Denies are cached (ADR-0069) for min(retry-after, 10 s).

### Postgres

Migration `00008_quotas.sql` (expand only; v1.0.0 is the first release, so no earlier release's binary is affected):

```sql
ALTER TABLE overrides DROP CONSTRAINT overrides_policy_id_check;
ALTER TABLE overrides ADD CONSTRAINT overrides_kind CHECK (
  (policy_id IS NOT NULL AND quotas IS NULL) OR
  (policy_id IS NULL AND quotas IS NOT NULL AND limits = '[]'::jsonb));
CREATE UNIQUE INDEX overrides_standing_quota ON overrides (consumer_id) WHERE policy_id IS NULL AND ends_at IS NULL;

CREATE TABLE quota_usage (
  consumer_id bigint NOT NULL REFERENCES consumers ON DELETE CASCADE,
  period      text   NOT NULL CHECK (period ~ '^(d[0-9]{8}|m[0-9]{6})$'),
  used        bigint NOT NULL CHECK (used >= 0),
  flushed_at  timestamptz NOT NULL,
  PRIMARY KEY (consumer_id, period)
);
```

The constraint name `overrides_policy_id_check` is Postgres's generated name for the M5 column CHECK; the migration test asserts it. `quotas` is protojson `Quotas`. `plans.spec` gains `quotas` (protojson, no migration). Daily rows older than 90 days are deleted by the flusher once an hour; monthly rows are kept.

### The flusher (D5)

Runs in the active control-plane replica only (CP-22), started and stopped with the active role.

- **Connection.** New settings `SPILLWAY_CP_REDIS_ADDRS` (required in managed deployments with quotas; unset disables the flusher and logs it once), `SPILLWAY_CP_REDIS_USERNAME`, `SPILLWAY_CP_REDIS_PASSWORD` / `_FILE` (SEC-09). A go-redis cluster client of its own, not the gateway's limiter client (no 5 ms budget: 2 s per command, no retries for scripts, as RL-16).
- **One at a time.** Each flush runs in a Postgres transaction that first takes `pg_try_advisory_xact_lock(1397774156, 2)` (ADR-0038's lock space, key 2; key 1 is the active-replica lock). Not acquired: the flush is skipped (`cp_quota_flushes_total{result="locked"}`). So even a failover overlap, or two control planes on one database, never flush at once.
- **Every 10 s** (a fixed ticker; tests inject the clock), for every tenant with at least one consumer that has non-zero effective quotas (one query over consumers, tenants, plans and overrides):
  1. Read Redis `TIME` once; the periods are the UTC day and month containing it, plus the previous day and month while `TIME` is within 60 s after their end (so the last increments of a period are flushed after midnight).
  2. Per tenant, in one pipeline to its slot's node: `EXISTS` the epoch, `MGET` its consumers' counter keys for those periods.
  3. **Epoch present:** upsert each counter: `INSERT INTO quota_usage (consumer_id, period, used, flushed_at) VALUES (…) ON CONFLICT (consumer_id, period) DO UPDATE SET used = GREATEST(quota_usage.used, EXCLUDED.used), flushed_at = EXCLUDED.flushed_at`, then `PEXPIRE` the epoch to 32 days. Writes are absolute, so a repeated or late flush never double-counts and never lowers `used`.
  4. **Epoch missing:** re-seed the tenant (below) instead of reading.
- **Epoch check every 1 s.** Between flushes the flusher `EXISTS`-checks every quota tenant's epoch once a second (pipelined per node) and re-seeds a tenant at once when its epoch is missing, so a tenant's quotas stay unseeded for about a second after a data loss, not up to 10 s.
- **Re-seed.** `quota_seed_v1.lua` in the tenant's slot, atomic: KEYS = the epoch and the tenant's counter keys for the current periods (and previous ones inside their 60 s); ARGV = the epoch value, its TTL, and per key the `used` from `quota_usage` (0 if no row) and its TTL. For each key it sets `max(current value or 0, used)` with the TTL, then sets the epoch. Taking the larger keeps any count Redis still has. Counting resumes with the epoch.
- **First seed.** A tenant that gets its first quota has no epoch, so the first check seeds it from Postgres (zero rows: zeros), like a data loss.
- **Failures.** A Redis error for a tenant skips it this round (`cp_quota_flush_errors_total`, logged at most once a minute per tenant); a Postgres error rolls the flush back. Both retry on the next tick.
- **Under-count.** Counts lost with Redis are at most the increments since the tenant's last successful flush: **one flush interval** (RL-10). Requests during the ~1 s unseeded window follow the failure mode above (fail_open: admitted and not counted, which RL-10 defines as that window's behaviour).
- **Metrics:** `cp_quota_flushes_total{result}` (`ok`, `error`, `locked`), `cp_quota_flush_duration_seconds`, `cp_quota_reseeds_total`, `cp_quota_last_flush_timestamp_seconds`.

### API and UI (D4)

OpenAPI-first and additive (`oasdiff` clean):

- **Plans**: `quotas: {daily, monthly}` (optional, `minimum: 0`) on the plan schema (editor, admin; config, needs a publish).
- **Quota overrides**: a new resource over the same `overrides` rows with a NULL `policy_id`: `GET, POST /api/v1/quota-overrides` (list paginated per ADR-0060, filter `consumer_id`), `GET, PUT, DELETE /api/v1/quota-overrides/{id}` (`If-Match`, CP-14). Schema `QuotaOverride`: `id`, `consumer_id`, `quotas` (required), `starts_at`, `ends_at`, `note`, `version`, `created_at`, `updated_at`. Roles: read all five, write `key-admin`, `admin` (as overrides, CP-12). Every mutation is an identity change (version bump, Watch message) with an audit record `quota_override.create|update|delete` (CP-13). `/api/v1/overrides` keeps returning only policy overrides, so its schema keeps `policy_id` required (ADR-0071 §D6 explains why the NULL policy gets its own resource).
- **Usage**: `GET /api/v1/quota-usage?consumer_id=<id>` (repeatable, 1–50 IDs, the page size of the consumer list) → `{items: [{consumer_id, daily: {period, used, quota, flushed_at}, monthly: {…}}]}`, where `quota` is the effective quota now (0 = none), `used` the flushed value (0 without a row) and `flushed_at` null without a row. Read by all five roles. Values are Postgres's, so up to one flush interval old; the screen says so.
- **Consumers & keys** (ADR-0065's placeholders): the consumer list gets a "Quota (today · month)" column, `used / quota` per period from one `quota-usage` call per page, "—" for no quota, and a red value at or over the quota; a consumer's detail shows both periods with `flushed_at`, and its Overrides section lists quota overrides as rows "Quota override" (daily, monthly, window) with create, edit and delete for key-admin and admin (UI-04). The Plans page gets the two quota fields.

### Tests

- Unit: period IDs and bounds across midnight, month ends and 29 February 2028; effective-quota resolution (override, consumer plan, tenant plan, windowed override); header items and `Retry-After`; the failure-mode rule per call mix; static config rejection.
- Script (compose Redis Cluster): count and deny at the quota, refund, write rule with a denying rate limit, epoch missing → `quota_unseeded`, `TIME` outside the bounds → `quota_unseeded`, TTLs.
- Flusher (integration, compose Redis + testcontainers Postgres): a repeated flush and a late flush with a lower value leave `used` unchanged; deleting the epoch and counters (Redis data loss) → re-seed within 2 s at Postgres's values, counting resumes from there; two flushers on one database never flush concurrently (the second logs `locked`); rollover flushes the previous period inside 60 s; pruning.
- API: RBAC per role, audit records, identity version bump, `If-Match`, validation (both quota kinds, one standing quota override); `oasdiff` stays additive.
- UI: Vitest for the column, detail and quota override form; `keys.spec.ts` (Playwright) creates a quota override and sees the column.
- Gateway integration: a consumer with `daily: 5` gets 429 on the sixth request with `Retry-After` to UTC midnight and the `quota/daily` items; the control plane's `quota_usage` shows 5 within 15 s.

## Consequences

- Quotas are exact per request while Redis keeps its data, and bounded (one flush interval) when it does not.
- A consumer with a quota always costs one tenant-slot call per request, even when its policies are batched (ADR-0069).
- Requests within the gateway–Redis clock offset of a UTC boundary follow the failure mode instead of being counted; with NTP that is milliseconds a day.
- The control plane now talks to Redis; Helm and compose give it the Redis settings.
- P24 (RL-10 wording), P25 (admin API rows), P26 (schema rows) and P27 (Redis key rows) update docs/design.md once approved. The open question "Quotas: is up to 10 s of under-count acceptable?" is answered by this design (P30).

## Alternatives considered

- **A separate quota call** — a second round trip in the same slot, and a quota counted for a request a rate limit then denies, or the reverse.
- **Choosing the period from the gateway's clock alone** — a skewed gateway would count requests into the wrong day, against RL-12.
- **A `quota_failure_mode` field on plans** — clearer, but a new configuration the requirement does not ask for; the call's policies already carry the operator's failure choice. The user can ask for it at the checkpoint.
- **Reading counters with `SCAN`** — the flusher knows every quota consumer from Postgres; `SCAN` across the cluster every 10 s would cost more and find nothing more.
- **Making `Override.policy_id` nullable in place** — breaks the `/overrides` response schema (`oasdiff` ERR); see ADR-0071.
