-- Heavy-hitter windows (RL-21, CP-22, ADR-0041). Each query names the index
-- it relies on; the EXPLAIN test in test/integration checks that none scans
-- a whole table. Windows are minutes of Postgres's clock_timestamp(), in
-- UTC; a volatile clock_timestamp() cannot be an index condition, so the
-- prune's cut-off is a scalar subquery, evaluated once.

-- name: AddHeavyHitters :exec
-- heavy_hitters_pkey (the conflict target). Adds one heartbeat's top_keys,
-- unique by (policy, digest), to the current minute's windows: the counts
-- summed, the consumer and key IDs the latest non-NULL ones. The arrays
-- have one element per key, in step (set-returning functions in the select
-- list run in lockstep); an ID of 0 is none, stored as NULL.
INSERT INTO heavy_hitters (window_start, policy_id, key_digest, consumer_id, api_key_id, denied, shadow_denied)
SELECT w.t, unnest(@policy_ids::bigint[]), unnest(@key_digests::text[]), nullif(unnest(@consumer_ids::bigint[]), 0),
       nullif(unnest(@api_key_ids::bigint[]), 0), unnest(@denied::bigint[]), unnest(@shadow_denied::bigint[])
FROM (SELECT date_trunc('minute', clock_timestamp(), 'UTC') AS t) w
ON CONFLICT (window_start, policy_id, key_digest) DO UPDATE SET
    consumer_id = coalesce(EXCLUDED.consumer_id, heavy_hitters.consumer_id),
    api_key_id = coalesce(EXCLUDED.api_key_id, heavy_hitters.api_key_id),
    denied = heavy_hitters.denied + EXCLUDED.denied,
    shadow_denied = heavy_hitters.shadow_denied + EXCLUDED.shadow_denied;

-- name: TopHeavyHitters :many
-- heavy_hitters_pkey range scan on window_start. The windows that start in
-- [since, until), summed by (policy, digest), most throttled first; the
-- consumer and key IDs of the latest window that has them, 0 for none.
-- since defaults to 15 minutes ago and until to none, on Postgres's clock,
-- the windows' (now() is stable, so both stay index conditions).
SELECT policy_id, key_digest,
       coalesce((array_agg(consumer_id ORDER BY window_start DESC) FILTER (WHERE consumer_id IS NOT NULL))[1], 0)::bigint AS consumer_id,
       coalesce((array_agg(api_key_id ORDER BY window_start DESC) FILTER (WHERE api_key_id IS NOT NULL))[1], 0)::bigint AS api_key_id,
       sum(denied)::bigint AS denied, sum(shadow_denied)::bigint AS shadow_denied
FROM heavy_hitters
WHERE window_start >= coalesce(sqlc.narg(since)::timestamptz, now() - interval '15 minutes')
  AND window_start < coalesce(sqlc.narg(until)::timestamptz, 'infinity')
GROUP BY policy_id, key_digest
ORDER BY sum(denied) + sum(shadow_denied) DESC, policy_id, key_digest
LIMIT @lim;

-- name: TopHeavyHittersByPolicy :many
-- heavy_hitters_policy_id_window_start. TopHeavyHitters for one policy.
SELECT policy_id, key_digest,
       coalesce((array_agg(consumer_id ORDER BY window_start DESC) FILTER (WHERE consumer_id IS NOT NULL))[1], 0)::bigint AS consumer_id,
       coalesce((array_agg(api_key_id ORDER BY window_start DESC) FILTER (WHERE api_key_id IS NOT NULL))[1], 0)::bigint AS api_key_id,
       sum(denied)::bigint AS denied, sum(shadow_denied)::bigint AS shadow_denied
FROM heavy_hitters
WHERE policy_id = @policy_id
  AND window_start >= coalesce(sqlc.narg(since)::timestamptz, now() - interval '15 minutes')
  AND window_start < coalesce(sqlc.narg(until)::timestamptz, 'infinity')
GROUP BY policy_id, key_digest
ORDER BY sum(denied) + sum(shadow_denied) DESC, policy_id, key_digest
LIMIT @lim;

-- name: PruneHeavyHitters :execrows
-- heavy_hitters_pkey, then a TID scan: one batch of windows older than
-- 7 days (CP-22).
DELETE FROM heavy_hitters
WHERE ctid = ANY(ARRAY(
    SELECT ctid FROM heavy_hitters
    WHERE window_start < (SELECT clock_timestamp() - interval '7 days')
    LIMIT 10000));

-- name: HeavyHitterWindows :many
-- heavy_hitters_policy_id_key_digest_window_start. One (policy, digest)'s
-- windows that start in [since, until), newest first, for POST
-- /heavy-hitters/lookup (ADR-0053); the defaults are TopHeavyHitters'. An
-- ID of NULL is 0.
SELECT window_start, policy_id, key_digest, coalesce(consumer_id, 0)::bigint AS consumer_id,
       coalesce(api_key_id, 0)::bigint AS api_key_id, denied, shadow_denied
FROM heavy_hitters
WHERE policy_id = @policy_id AND key_digest = @key_digest
  AND window_start >= coalesce(sqlc.narg(since)::timestamptz, now() - interval '15 minutes')
  AND window_start < coalesce(sqlc.narg(until)::timestamptz, 'infinity')
ORDER BY window_start DESC;

-- name: ShadowReportKeys :many
-- heavy_hitters_policy_id_window_start. One policy's windows that start at
-- or after since, summed by digest, most shadow would-denies first, then
-- most denies, for GET /policies/{id}/shadow-report (UI-01, ADR-0060); the
-- IDs as TopHeavyHitters gives them.
SELECT key_digest,
       coalesce((array_agg(consumer_id ORDER BY window_start DESC) FILTER (WHERE consumer_id IS NOT NULL))[1], 0)::bigint AS consumer_id,
       coalesce((array_agg(api_key_id ORDER BY window_start DESC) FILTER (WHERE api_key_id IS NOT NULL))[1], 0)::bigint AS api_key_id,
       sum(shadow_denied)::bigint AS shadow_denied, sum(denied)::bigint AS denied
FROM heavy_hitters
WHERE policy_id = @policy_id AND window_start >= @since::timestamptz
GROUP BY key_digest
ORDER BY sum(shadow_denied) DESC, sum(denied) DESC, key_digest
LIMIT @lim;

-- name: ShadowReportTotal :one
-- heavy_hitters_policy_id_window_start. The shadow would-denies of every
-- key of ShadowReportKeys' windows.
SELECT coalesce(sum(shadow_denied), 0)::bigint AS shadow_denied
FROM heavy_hitters
WHERE policy_id = @policy_id AND window_start >= @since::timestamptz;

-- name: OldestHeavyHitterWindow :one
-- heavy_hitters_policy_id_window_start. The start of a policy's oldest
-- window kept; no row when it has none.
SELECT window_start FROM heavy_hitters
WHERE policy_id = @policy_id
ORDER BY window_start
LIMIT 1;
