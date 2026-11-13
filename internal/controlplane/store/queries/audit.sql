-- The audit log (CP-13, ADR-0040). Each query names the index it relies on;
-- the EXPLAIN test in test/integration checks that none scans a whole
-- table. Lists are newest first: a backward keyset scan below @before.

-- name: InsertAudit :exec
INSERT INTO audit_log (actor_sub, actor_name, actor_roles, action, entity, entity_id, diff, note, request_id, config_version, identity_version)
VALUES (@actor_sub, @actor_name, @actor_roles, @action, @entity, sqlc.narg(entity_id), @diff, sqlc.narg(note), @request_id,
        sqlc.narg(config_version), sqlc.narg(identity_version));

-- name: ListAudit :many
-- audit_log_pkey backward keyset scan.
SELECT * FROM audit_log
WHERE id < @before::bigint
  AND (sqlc.narg(since)::timestamptz IS NULL OR at >= sqlc.narg(since))
  AND (sqlc.narg(until)::timestamptz IS NULL OR at < sqlc.narg(until))
ORDER BY id DESC LIMIT @lim;

-- name: ListAuditByEntity :many
-- audit_log_entity, its leading column.
SELECT * FROM audit_log
WHERE entity = @entity AND id < @before::bigint
  AND (sqlc.narg(actor_sub)::text IS NULL OR actor_sub = sqlc.narg(actor_sub))
  AND (sqlc.narg(since)::timestamptz IS NULL OR at >= sqlc.narg(since))
  AND (sqlc.narg(until)::timestamptz IS NULL OR at < sqlc.narg(until))
ORDER BY id DESC LIMIT @lim;

-- name: ListAuditByEntityID :many
-- audit_log_entity backward keyset scan.
SELECT * FROM audit_log
WHERE entity = @entity AND entity_id = @entity_id AND id < @before::bigint
  AND (sqlc.narg(actor_sub)::text IS NULL OR actor_sub = sqlc.narg(actor_sub))
  AND (sqlc.narg(since)::timestamptz IS NULL OR at >= sqlc.narg(since))
  AND (sqlc.narg(until)::timestamptz IS NULL OR at < sqlc.narg(until))
ORDER BY id DESC LIMIT @lim;

-- name: ListAuditByActor :many
-- audit_log_actor backward keyset scan.
SELECT * FROM audit_log
WHERE actor_sub = @actor_sub AND id < @before::bigint
  AND (sqlc.narg(since)::timestamptz IS NULL OR at >= sqlc.narg(since))
  AND (sqlc.narg(until)::timestamptz IS NULL OR at < sqlc.narg(until))
ORDER BY id DESC LIMIT @lim;
