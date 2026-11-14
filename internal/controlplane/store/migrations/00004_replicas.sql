-- Gateway replicas (CP-08, CP-22, ADR-0036, ADR-0038): what each replica
-- last reported in a heartbeat, and the first heartbeat that reported each
-- config and identity version (NFR-08's apply time, on Postgres's clock).
-- The active control-plane replica writes both on every heartbeat and
-- prunes them hourly: replica_versions after 7 days, replicas 7 days after
-- their last heartbeat. Release that may contract anything added here: none
-- needed, this release only adds.

-- +goose Up

-- id is the replica name (ADR-0040); last_synced_at is the last heartbeat
-- sent while the replica's Watch stream was established.
CREATE TABLE replicas (
    id                      text PRIMARY KEY,
    config_version          bigint NOT NULL,
    identity_version        bigint NOT NULL,
    schema_major            int NOT NULL,
    schema_minor            int NOT NULL,
    watch_connected         boolean NOT NULL,
    first_seen_at           timestamptz NOT NULL,
    last_heartbeat_at       timestamptz NOT NULL,
    last_synced_at          timestamptz,
    rejected_config_version bigint,
    rejected_reason         text
);
-- live_replicas and the prune.
CREATE INDEX replicas_last_heartbeat_at ON replicas (last_heartbeat_at);

CREATE TABLE replica_versions (
    replica_id        text NOT NULL,
    stream            text NOT NULL CHECK (stream IN ('config', 'identity')),
    version           bigint NOT NULL,
    first_reported_at timestamptz NOT NULL,
    PRIMARY KEY (replica_id, stream, version)
);
-- A version's applied list (GET /config/versions/{v}).
CREATE INDEX replica_versions_stream_version ON replica_versions (stream, version);
-- The prune.
CREATE INDEX replica_versions_first_reported_at ON replica_versions (first_reported_at);
