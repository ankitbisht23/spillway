-- The stream epoch (CP-06, CP-07, CP-20, ADR-0055): one random value that
-- names the history the config and identity versions belong to. A gateway
-- whose snapshot came from another epoch gets both sections in full,
-- whatever their versions, so equal version numbers from a restored
-- database can never hide different content. controlplane migrate inserts
-- the row (crypto/rand) when it is missing; the active replica sets a new
-- value when it takes the lock, when it sees a version go backwards under
-- the epoch it holds, and when an epoch set before that one comes back
-- (changed_at). 32 lowercase hex characters (16 bytes).
-- Release that may contract anything added here: none needed, this
-- release only adds.

-- +goose Up

CREATE TABLE stream_epoch (
    id         smallint PRIMARY KEY CHECK (id = 1),
    epoch      text NOT NULL CHECK (epoch ~ '^[0-9a-f]{32}$'),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
