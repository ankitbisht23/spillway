-- Heavy-hitter windows (RL-21, CP-22, ADR-0036, ADR-0041): the throttled
-- keys gateways report in their heartbeats' top_keys, summed per minute
-- over replicas and heartbeats. key_digest is the 22-character digest of
-- the policy's filled key template, never a raw key; consumer_id and
-- api_key_id are the latest non-zero ones reported, NULL for none. The
-- active control-plane replica writes them on every heartbeat and prunes
-- windows older than 7 days hourly. Release that may contract anything
-- added here: none needed, this release only adds.

-- +goose Up

CREATE TABLE heavy_hitters (
    window_start  timestamptz NOT NULL,
    policy_id     bigint NOT NULL,
    key_digest    text NOT NULL,
    consumer_id   bigint,
    api_key_id    bigint,
    denied        bigint NOT NULL,
    shadow_denied bigint NOT NULL,
    PRIMARY KEY (window_start, policy_id, key_digest)
);
-- GET /heavy-hitters?policy_id=.
CREATE INDEX heavy_hitters_policy_id_window_start ON heavy_hitters (policy_id, window_start);
