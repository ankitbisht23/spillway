-- Identity entities (CP-01, CP-20, ADR-0036, ADR-0037). Each query names the
-- index it relies on; the EXPLAIN test in test/integration checks that none
-- scans a whole table.

-- name: BumpIdentityVersion :one
-- identity_state_pkey. Takes the row lock that orders identity changes.
UPDATE identity_state SET version = version + 1, changed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = 1
RETURNING version;

-- name: ListTenants :many
-- tenants_pkey keyset scan; tenants_name_key for ?name=.
SELECT * FROM tenants
WHERE id > @after::bigint AND (sqlc.narg(name)::text IS NULL OR name = sqlc.narg(name))
ORDER BY id LIMIT @lim;

-- name: GetTenant :one
-- tenants_pkey.
SELECT * FROM tenants WHERE id = @id;

-- name: CreateTenant :one
INSERT INTO tenants (name, plan_id, status, version) VALUES (@name, @plan_id, @status, @version) RETURNING *;

-- name: UpdateTenant :one
-- tenants_pkey.
UPDATE tenants SET name = @name, plan_id = @plan_id, status = @status, version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeleteTenant :execrows
-- tenants_pkey; the RESTRICT check on consumers uses consumers_tenant_id_name_key.
DELETE FROM tenants WHERE id = @id;

-- name: ListConsumers :many
-- consumers_pkey keyset scan, or consumers_tenant_id_name_key for ?tenant_id=.
SELECT * FROM consumers
WHERE id > @after::bigint
  AND (sqlc.narg(tenant_id)::bigint IS NULL OR tenant_id = sqlc.narg(tenant_id))
  AND (sqlc.narg(name)::text IS NULL OR name = sqlc.narg(name))
ORDER BY id LIMIT @lim;

-- name: GetConsumer :one
-- consumers_pkey.
SELECT * FROM consumers WHERE id = @id;

-- name: CreateConsumer :one
INSERT INTO consumers (tenant_id, name, plan_id, status, jwt_subject, version)
VALUES (@tenant_id, @name, sqlc.narg(plan_id), @status, sqlc.narg(jwt_subject), @version) RETURNING *;

-- name: UpdateConsumer :one
-- consumers_pkey.
UPDATE consumers
SET tenant_id = @tenant_id, name = @name, plan_id = sqlc.narg(plan_id), status = @status,
    jwt_subject = sqlc.narg(jwt_subject), version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeleteConsumer :execrows
-- consumers_pkey; cascades through api_keys_consumer_id and the overrides
-- and access-list indexes.
DELETE FROM consumers WHERE id = @id;

-- name: ListConsumerKeys :many
-- api_keys_consumer_id. Never the hash (ADR-0036).
SELECT id, consumer_id, prefix, expires_at, revoked_at, rotated_from, version, created_at, updated_at
FROM api_keys
WHERE consumer_id = @consumer_id AND id > @after::bigint
ORDER BY id LIMIT @lim;

-- name: GetAPIKey :one
-- api_keys_pkey. Never the hash. Rotations and revocations read it under
-- the identity version's row lock, which every writer of api_keys holds.
SELECT id, consumer_id, prefix, expires_at, revoked_at, rotated_from, version, created_at, updated_at,
       (expires_at IS NOT NULL AND expires_at <= clock_timestamp())::boolean AS expired
FROM api_keys WHERE id = @id;

-- name: InsertAPIKey :one
-- api_keys_prefix_key: a prefix that is taken inserts nothing and returns
-- no row, and the caller draws another (ADR-0039). Returns no hash.
INSERT INTO api_keys (consumer_id, prefix, hash, expires_at, rotated_from, version)
VALUES (@consumer_id, @prefix, @hash, sqlc.narg(expires_at), sqlc.narg(rotated_from), @version)
ON CONFLICT (prefix) DO NOTHING
RETURNING id, consumer_id, prefix, expires_at, revoked_at, rotated_from, version, created_at, updated_at;

-- name: ExpireAPIKey :one
-- api_keys_pkey. A rotated key's expiry: the earlier of its own and now
-- plus the grace period, on Postgres's clock (D2, ADR-0039).
UPDATE api_keys
SET expires_at = LEAST(coalesce(expires_at, 'infinity'), clock_timestamp() + make_interval(secs => @grace_seconds::float8)),
    version = @version, updated_at = clock_timestamp()
WHERE id = @id
RETURNING id, consumer_id, prefix, expires_at, revoked_at, rotated_from, version, created_at, updated_at;

-- name: RevokeAPIKey :one
-- api_keys_pkey (CP-17).
UPDATE api_keys SET revoked_at = clock_timestamp(), version = @version, updated_at = clock_timestamp()
WHERE id = @id
RETURNING id, consumer_id, prefix, expires_at, revoked_at, rotated_from, version, created_at, updated_at;

-- name: ListAccessLists :many
-- access_lists_pkey keyset scan.
SELECT * FROM access_lists
WHERE id > @after::bigint
  AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action))
  AND (sqlc.narg(subject_type)::text IS NULL OR subject_type = sqlc.narg(subject_type))
ORDER BY id LIMIT @lim;

-- name: GetAccessList :one
-- access_lists_pkey.
SELECT * FROM access_lists WHERE id = @id;

-- name: CreateAccessList :one
INSERT INTO access_lists (action, subject_type, api_key_id, consumer_id, cidr, note, version)
VALUES (@action, @subject_type, sqlc.narg(api_key_id), sqlc.narg(consumer_id), sqlc.narg(cidr), sqlc.narg(note), @version)
RETURNING *;

-- name: UpdateAccessList :one
-- access_lists_pkey.
UPDATE access_lists
SET action = @action, subject_type = @subject_type, api_key_id = sqlc.narg(api_key_id), consumer_id = sqlc.narg(consumer_id),
    cidr = sqlc.narg(cidr), note = sqlc.narg(note), version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeleteAccessList :execrows
-- access_lists_pkey.
DELETE FROM access_lists WHERE id = @id;

-- name: ListOverrides :many
-- overrides_pkey keyset scan.
SELECT * FROM overrides
WHERE id > @after::bigint
  AND (sqlc.narg(consumer_id)::bigint IS NULL OR consumer_id = sqlc.narg(consumer_id))
  AND (sqlc.narg(policy_id)::bigint IS NULL OR policy_id = sqlc.narg(policy_id))
ORDER BY id LIMIT @lim;

-- name: GetOverride :one
-- overrides_pkey.
SELECT * FROM overrides WHERE id = @id;

-- name: CreateOverride :one
INSERT INTO overrides (consumer_id, policy_id, limits, starts_at, ends_at, note, version)
VALUES (@consumer_id, @policy_id, @limits, sqlc.narg(starts_at), sqlc.narg(ends_at), sqlc.narg(note), @version)
RETURNING *;

-- name: UpdateOverride :one
-- overrides_pkey.
UPDATE overrides
SET consumer_id = @consumer_id, policy_id = @policy_id, limits = @limits, starts_at = sqlc.narg(starts_at),
    ends_at = sqlc.narg(ends_at), note = sqlc.narg(note), version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeleteOverride :execrows
-- overrides_pkey.
DELETE FROM overrides WHERE id = @id;

-- name: ConsumerDependents :many
-- What deleting a consumer cascades to, for its audit record (ADR-0040):
-- api_keys_consumer_id, overrides_consumer_policy, and access_lists_consumer
-- and access_lists_key (skip scans past their leading action column).
SELECT 'api_key'::text AS kind, k.id FROM api_keys k WHERE k.consumer_id = @consumer_id::bigint
UNION ALL
SELECT 'override'::text, o.id FROM overrides o WHERE o.consumer_id = @consumer_id::bigint
UNION ALL
SELECT 'access_list'::text, a.id FROM access_lists a WHERE a.consumer_id = @consumer_id::bigint
UNION ALL
SELECT 'access_list'::text, a.id FROM access_lists a
WHERE a.api_key_id = ANY(ARRAY(SELECT k.id FROM api_keys k WHERE k.consumer_id = @consumer_id::bigint))
ORDER BY 1, 2;

-- name: NotifyIdentity :exec
-- Delivered at commit to the Watch streams' LISTEN (CP-20, ADR-0038).
SELECT pg_notify('spillway_identity', @version::text);

-- name: IdentityVersion :one
-- identity_state_pkey.
SELECT version FROM identity_state WHERE id = 1;

-- The identity snapshot (CP-20, ADR-0037): every row that can reach a
-- gateway, read in one REPEATABLE READ transaction with the version.

-- name: AllTenants :many
SELECT * FROM tenants ORDER BY id;

-- name: AllConsumers :many
SELECT * FROM consumers ORDER BY id;

-- name: LiveAPIKeys :many
-- Keys that are neither revoked nor expired.
SELECT id, consumer_id, prefix, hash, expires_at FROM api_keys
WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > clock_timestamp())
ORDER BY id;

-- name: AllAccessLists :many
SELECT * FROM access_lists ORDER BY id;

-- name: LiveOverrides :many
-- Overrides that have not ended.
SELECT * FROM overrides WHERE ends_at IS NULL OR ends_at > clock_timestamp() ORDER BY id;
