-- Config working set and config entities (CP-01, CP-02, ADR-0036,
-- ADR-0037). Each query names the index it relies on; the EXPLAIN test in
-- test/integration checks that none scans a whole table.

-- name: BumpRevision :one
-- config_working_set_pkey. Takes the row lock that serializes config writers.
UPDATE config_working_set
SET revision = revision + 1, updated_at = clock_timestamp(), updated_by = sqlc.narg(actor)
WHERE id = 1
RETURNING revision;

-- name: ShareWorkingSet :exec
-- config_working_set_pkey. Waits for a publish or rollback in progress.
SELECT 1 FROM config_working_set WHERE id = 1 FOR SHARE;

-- name: LatestConfigVersion :one
-- config_versions_pkey, backward scan.
SELECT * FROM config_versions ORDER BY version DESC LIMIT 1;

-- name: ListUpstreams :many
-- upstreams_pkey keyset scan; upstreams_name_key for ?name=.
SELECT * FROM upstreams
WHERE id > @after::bigint AND (sqlc.narg(name)::text IS NULL OR name = sqlc.narg(name))
ORDER BY id LIMIT @lim;

-- name: GetUpstream :one
-- upstreams_pkey.
SELECT * FROM upstreams WHERE id = @id;

-- name: CreateUpstream :one
INSERT INTO upstreams (name, spec, version) VALUES (@name, @spec, @version) RETURNING *;

-- name: UpdateUpstream :one
-- upstreams_pkey.
UPDATE upstreams SET name = @name, spec = @spec, version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeleteUpstream :execrows
-- upstreams_pkey; endpoints go by endpoints_upstream_id_address_key.
DELETE FROM upstreams WHERE id = @id;

-- name: ListEndpoints :many
-- endpoints_upstream_id_address_key.
SELECT * FROM endpoints WHERE upstream_id = ANY(@upstream_ids::bigint[]) ORDER BY upstream_id, id;

-- name: CreateEndpoint :one
INSERT INTO endpoints (upstream_id, address, draining) VALUES (@upstream_id, @address, @draining) RETURNING *;

-- name: SetEndpointDraining :exec
-- endpoints_pkey.
UPDATE endpoints SET draining = @draining, updated_at = clock_timestamp() WHERE id = @id;

-- name: DeleteEndpoint :exec
-- endpoints_pkey.
DELETE FROM endpoints WHERE id = @id;

-- name: ListRoutes :many
-- routes_pkey keyset scan; routes_name_key for ?name=.
SELECT * FROM routes
WHERE id > @after::bigint AND (sqlc.narg(name)::text IS NULL OR name = sqlc.narg(name))
ORDER BY id LIMIT @lim;

-- name: GetRoute :one
-- routes_pkey.
SELECT * FROM routes WHERE id = @id;

-- name: CreateRoute :one
INSERT INTO routes (name, spec, version) VALUES (@name, @spec, @version) RETURNING *;

-- name: UpdateRoute :one
-- routes_pkey.
UPDATE routes SET name = @name, spec = @spec, version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeleteRoute :execrows
-- routes_pkey.
DELETE FROM routes WHERE id = @id;

-- name: ListPolicies :many
-- policies_pkey keyset scan; policies_name_key for ?name=.
SELECT * FROM policies
WHERE id > @after::bigint AND (sqlc.narg(name)::text IS NULL OR name = sqlc.narg(name))
ORDER BY id LIMIT @lim;

-- name: GetPolicy :one
-- policies_pkey.
SELECT * FROM policies WHERE id = @id;

-- name: CreatePolicy :one
INSERT INTO policies (name, spec, version) VALUES (@name, @spec, @version) RETURNING *;

-- name: UpdatePolicy :one
-- policies_pkey.
UPDATE policies SET name = @name, spec = @spec, version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeletePolicy :execrows
-- policies_pkey.
DELETE FROM policies WHERE id = @id;

-- name: ListPlans :many
-- plans_pkey keyset scan; plans_name_key for ?name=.
SELECT * FROM plans
WHERE id > @after::bigint AND (sqlc.narg(name)::text IS NULL OR name = sqlc.narg(name))
ORDER BY id LIMIT @lim;

-- name: GetPlan :one
-- plans_pkey.
SELECT * FROM plans WHERE id = @id;

-- name: CreatePlan :one
INSERT INTO plans (name, spec, version) VALUES (@name, @spec, @version) RETURNING *;

-- name: UpdatePlan :one
-- plans_pkey.
UPDATE plans SET name = @name, spec = @spec, version = @version, updated_at = clock_timestamp()
WHERE id = @id RETURNING *;

-- name: DeletePlan :execrows
-- plans_pkey.
DELETE FROM plans WHERE id = @id;

-- name: GetSettings :one
-- config_settings_pkey.
SELECT * FROM config_settings WHERE id = 1;

-- name: UpdateSettings :one
-- config_settings_pkey.
UPDATE config_settings SET spec = @spec, version = @version, updated_at = clock_timestamp()
WHERE id = 1 RETURNING *;
