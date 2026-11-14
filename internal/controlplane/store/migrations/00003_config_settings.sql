-- The config settings (ADR-0045): a config file's ratelimit section
-- (replicas, redis_budget; RL-31, ADR-0021) as one row of the config working
-- set. Every change to it is a config edit: it bumps the working-set
-- revision, ships with the next publish and rolls back with config. spec is
-- the RateLimitSettings message's protojson; version is the working-set
-- revision of its last change, as for the config entities. Release that may
-- contract anything added here: none needed, this release only adds.

-- +goose Up

CREATE TABLE config_settings (
    id         smallint PRIMARY KEY CHECK (id = 1),
    spec       jsonb NOT NULL,
    version    bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO config_settings (id, spec, version) VALUES (1, '{}', 1);
