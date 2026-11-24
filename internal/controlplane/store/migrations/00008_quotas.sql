-- Quotas (RL-10, ADR-0070, ADR-0071 D6c). An override with no policy is a
-- quota override: it has quotas (protojson Quotas) and empty limits, and
-- replaces the consumer's plan quotas while it applies; a policy override
-- has a policy and no quotas. At most one standing quota override per
-- consumer. quota_usage holds the flushed counters: the active control
-- plane's flusher writes used = GREATEST(used, $n) every 10 s under advisory
-- lock (1397774156, 2); period is d<YYYYMMDD> or m<YYYYMM>, UTC; daily rows
-- older than 90 days are pruned hourly. Release that may contract anything
-- added here: none needed, this release only adds.
-- contract: overrides_policy_id_check (Postgres's name for the M5 column
-- CHECK) is replaced by overrides_kind, which allows every row earlier
-- binaries wrote (a policy, quotas NULL); no released binary predates
-- quota overrides, since v1.0.0 is the first release (ADR-0070).

-- +goose Up

ALTER TABLE overrides DROP CONSTRAINT overrides_policy_id_check;
ALTER TABLE overrides ADD CONSTRAINT overrides_kind CHECK (
    (policy_id IS NOT NULL AND quotas IS NULL) OR
    (policy_id IS NULL AND quotas IS NOT NULL AND limits = '[]'::jsonb));
CREATE UNIQUE INDEX overrides_standing_quota ON overrides (consumer_id) WHERE policy_id IS NULL AND ends_at IS NULL;

CREATE TABLE quota_usage (
    consumer_id bigint NOT NULL REFERENCES consumers ON DELETE CASCADE,
    period      text   NOT NULL CHECK (period ~ '^(d[0-9]{8}|m[0-9]{6})$'),
    used        bigint NOT NULL CHECK (used >= 0),
    flushed_at  timestamptz NOT NULL,
    PRIMARY KEY (consumer_id, period)
);
