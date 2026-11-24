-- Quotas (RL-10, ADR-0070): quota overrides, the flusher's reads and
-- writes, and the flushed usage.

-- name: ListQuotaOverrides :many
-- overrides_pkey keyset scan; overrides with no policy (ADR-0071).
SELECT * FROM overrides
WHERE id > @after::bigint AND policy_id IS NULL
  AND (sqlc.narg(consumer_id)::bigint IS NULL OR consumer_id = sqlc.narg(consumer_id))
ORDER BY id LIMIT @lim;

-- name: GetQuotaOverride :one
-- overrides_pkey.
SELECT * FROM overrides WHERE id = @id AND policy_id IS NULL;

-- name: CreateQuotaOverride :one
INSERT INTO overrides (consumer_id, policy_id, limits, quotas, starts_at, ends_at, note, version)
VALUES (@consumer_id, NULL, '[]'::jsonb, @quotas, sqlc.narg(starts_at), sqlc.narg(ends_at), sqlc.narg(note), @version)
RETURNING *;

-- name: UpdateQuotaOverride :one
-- overrides_pkey.
UPDATE overrides
SET consumer_id = @consumer_id, quotas = @quotas, starts_at = sqlc.narg(starts_at),
    ends_at = sqlc.narg(ends_at), note = sqlc.narg(note), version = @version, updated_at = clock_timestamp()
WHERE id = @id AND policy_id IS NULL RETURNING *;

-- name: DeleteQuotaOverride :execrows
-- overrides_pkey.
DELETE FROM overrides WHERE id = @id AND policy_id IS NULL;

-- name: LiveQuotaOverrides :many
-- The quota overrides of consumers that have not ended (overrides_consumer_policy).
SELECT * FROM overrides
WHERE consumer_id = ANY(@consumer_ids::bigint[]) AND policy_id IS NULL
  AND (ends_at IS NULL OR ends_at > clock_timestamp())
ORDER BY id;

-- name: TryQuotaFlushLock :one
-- The flusher's transaction lock (ADR-0070): ADR-0038's lock space, key 2.
SELECT pg_try_advisory_xact_lock(1397774156, 2);

-- name: QuotaConsumers :many
-- Every consumer that may have quota counters: its plan (its own, else its
-- tenant's) has quotas in the published config, or it has a quota override
-- that has not ended.
SELECT c.id, c.tenant_id FROM consumers c JOIN tenants t ON t.id = c.tenant_id
WHERE COALESCE(c.plan_id, t.plan_id) = ANY(@plan_ids::bigint[])
   OR EXISTS (SELECT 1 FROM overrides o WHERE o.consumer_id = c.id AND o.policy_id IS NULL
              AND (o.ends_at IS NULL OR o.ends_at > clock_timestamp()))
ORDER BY c.tenant_id, c.id;

-- name: ConsumerPlans :many
-- The plan each consumer is on: its own, else its tenant's.
SELECT c.id, COALESCE(c.plan_id, t.plan_id)::bigint AS plan_id FROM consumers c JOIN tenants t ON t.id = c.tenant_id
WHERE c.id = ANY(@ids::bigint[]) ORDER BY c.id;

-- name: UpsertQuotaUsage :exec
-- Absolute writes: a repeated or late flush never double-counts or lowers
-- used (RL-10). Rows of consumers deleted meanwhile are dropped.
INSERT INTO quota_usage (consumer_id, period, used, flushed_at)
SELECT u.consumer_id, u.period, u.used, @flushed_at::timestamptz
FROM (SELECT unnest(@consumer_ids::bigint[]) AS consumer_id, unnest(@periods::text[]) AS period, unnest(@used::bigint[]) AS used) u
WHERE EXISTS (SELECT 1 FROM consumers c WHERE c.id = u.consumer_id)
ON CONFLICT (consumer_id, period) DO UPDATE
SET used = GREATEST(quota_usage.used, EXCLUDED.used), flushed_at = EXCLUDED.flushed_at;

-- name: QuotaUsage :many
-- quota_usage_pkey.
SELECT * FROM quota_usage
WHERE consumer_id = ANY(@consumer_ids::bigint[]) AND period = ANY(@periods::text[])
ORDER BY consumer_id, period;

-- name: PruneQuotaUsage :execrows
-- Daily rows of days before @before (d<YYYYMMDD>); monthly rows stay.
DELETE FROM quota_usage WHERE period LIKE 'd%' AND period < @before::text;
