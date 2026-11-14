-- Gateway replicas and their heartbeats (CP-08, CP-22, RL-24, ADR-0038).
-- Each query names the index it relies on; the EXPLAIN test in
-- test/integration checks that none scans a whole table. Every time is
-- Postgres's clock_timestamp(), the clock of config_versions.published_at,
-- so NFR-08 compares one clock. A volatile clock_timestamp() cannot be an
-- index condition, so the cut-offs are scalar subqueries, evaluated once.

-- name: UpsertReplica :exec
-- replicas_pkey. last_synced_at moves only when the heartbeat was sent with
-- the Watch stream established.
INSERT INTO replicas (id, config_version, identity_version, schema_major, schema_minor, watch_connected,
                      first_seen_at, last_heartbeat_at, last_synced_at, rejected_config_version, rejected_reason)
SELECT @id::text, @config_version::bigint, @identity_version::bigint, @schema_major::int, @schema_minor::int, @watch_connected::boolean,
       now.t, now.t, CASE WHEN @watch_connected::boolean THEN now.t END,
       sqlc.narg(rejected_config_version)::bigint, sqlc.narg(rejected_reason)::text
FROM (SELECT clock_timestamp() AS t) now
ON CONFLICT (id) DO UPDATE SET
    config_version = EXCLUDED.config_version,
    identity_version = EXCLUDED.identity_version,
    schema_major = EXCLUDED.schema_major,
    schema_minor = EXCLUDED.schema_minor,
    watch_connected = EXCLUDED.watch_connected,
    last_heartbeat_at = EXCLUDED.last_heartbeat_at,
    last_synced_at = coalesce(EXCLUDED.last_synced_at, replicas.last_synced_at),
    rejected_config_version = EXCLUDED.rejected_config_version,
    rejected_reason = EXCLUDED.rejected_reason;

-- name: InsertReplicaVersions :exec
-- replica_versions_pkey. The first heartbeat that reports a version sets
-- its time, once: the replica's apply heartbeat (NFR-08). 0 is no version.
INSERT INTO replica_versions (replica_id, stream, version, first_reported_at)
SELECT @replica_id::text, v.stream, v.version, clock_timestamp()
FROM (VALUES ('config', @config_version::bigint), ('identity', @identity_version::bigint)) AS v (stream, version)
WHERE v.version > 0
ON CONFLICT DO NOTHING;

-- name: LiveReplicas :one
-- replicas_last_heartbeat_at. Replicas whose last heartbeat is under 15 s
-- old: three intervals (RL-24).
SELECT count(*)::int FROM replicas
WHERE last_heartbeat_at > (SELECT clock_timestamp() - interval '15 seconds');

-- name: ListReplicas :many
-- replicas_pkey keyset scan.
SELECT r.*, (r.last_heartbeat_at > (SELECT clock_timestamp() - interval '15 seconds'))::boolean AS live
FROM replicas r
WHERE r.id > @after::text
ORDER BY r.id LIMIT @lim;

-- name: ConfigVersionApplied :many
-- replica_versions_stream_version. The replicas that reported config
-- version @version, with their first report.
SELECT replica_id, first_reported_at FROM replica_versions
WHERE stream = 'config' AND version = @version
ORDER BY replica_id;

-- name: PruneReplicaVersions :execrows
-- replica_versions_first_reported_at, then a TID scan: one batch of first
-- reports older than 7 days (CP-22).
DELETE FROM replica_versions
WHERE ctid = ANY(ARRAY(
    SELECT ctid FROM replica_versions
    WHERE first_reported_at < (SELECT clock_timestamp() - interval '7 days')
    LIMIT 10000));

-- name: PruneReplicas :execrows
-- replicas_last_heartbeat_at, then replicas_pkey: one batch of replicas
-- without a heartbeat for 7 days (CP-22).
DELETE FROM replicas
WHERE id = ANY(ARRAY(
    SELECT id FROM replicas
    WHERE last_heartbeat_at < (SELECT clock_timestamp() - interval '7 days')
    LIMIT 10000));
