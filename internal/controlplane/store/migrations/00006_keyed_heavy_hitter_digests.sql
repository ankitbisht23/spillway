-- Keyed heavy-hitter digests (RL-21, SEC-09, ADR-0053): from this release
-- gateways report each key's digest as an HMAC under the deployment's
-- heavy-hitter secret, so the windows written before hold unkeyed digests
-- that no lookup can match and that, for an {ip} policy, anyone could
-- reverse by trying addresses. They are deleted: at most 7 days of
-- windows, dev data only so far. The new index serves POST
-- /heavy-hitters/lookup, one (policy, digest)'s windows. Release that may
-- contract the index added here: none needed.

-- +goose Up

-- contract: the deleted rows are data, not schema; the previous release's
-- binary reads and writes heavy_hitters as before, and its unkeyed rows
-- written after this migration age out with the 7-day prune (ADR-0053).
DELETE FROM heavy_hitters;

CREATE INDEX heavy_hitters_policy_id_key_digest_window_start ON heavy_hitters (policy_id, key_digest, window_start);
