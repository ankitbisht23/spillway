# 0053. Keyed heavy-hitter digests and the admin lookup

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** RL-21, SEC-09, CP-12, CP-13, SEC-08

## Context

ADR-0041 and ADR-0051 report each throttled key in heartbeats' `top_keys`, keep it in `heavy_hitters` and show it through `GET /heavy-hitters` to every role, as a 22-character digest: base64url of `ratelimit.Key`, the first 16 bytes of the SHA-256 of the policy's filled key template. That digest has no key. For a template of low entropy it is a pseudonym anyone can reverse: an `{ip}` policy's digest is the SHA-256 of an IPv4 address, and trying all 2³² addresses (or a log's worth of candidates) finds the client. Every role, and anyone who can read the table or a heartbeat, could do it, and it is also the digest in the limiter's Redis key, so the two can be matched. The review after M5 found this; on 2026-11-15 the user chose the recommended fix: report a digest keyed with a per-deployment secret, and give admins alone, audited, a way to find a subject's windows.

Constraints: the limiter's own keys (local map keys and Redis key names, docs/design.md "Redis keys") must not change, so no counter resets; the deny path's cost (ADR-0051's `BenchmarkDenied`) must not grow; secrets come from env or files only, never from config snapshots (SEC-09), so the control plane cannot hand the secret to gateways over the stream; and windows already stored hold unkeyed digests.

## Decision

### The reported digest

- **We report** `Pseudonym(secret, k)` = base64url without padding (22 characters, as before) of the first 16 bytes of HMAC-SHA256 under the deployment's heavy-hitter secret of `k`, the 16-byte `ratelimit.Key` of the filled template (`internal/heavyhitters.Pseudonym`). The format, length and the store's checks are unchanged, so `config_service.proto` and the REST schemas only change their descriptions.
- **The input is the 16-byte digest, not the template.** The sketch keeps `ratelimit.Key` as it does now (ADR-0051) and the template is gone by the time a heartbeat is built; HMAC is a PRF, so keying a deterministic function of the template is as good as keying the template, and two subjects collide only if their 16-byte digests do (2⁻⁶⁴ per pair, as before). Without the secret, a digest cannot be tested against a candidate subject.
- **Computed at heartbeat time only** (`managed.topKeys`), at most 100 per heartbeat: 240 ns each (Apple M4 Pro), so 25 µs per 5 s heartbeat. The deny path, the sketch and `Restore` are untouched; the limiter keys, local and Redis, are exactly as they were.
- `heavyhitters.Entry.KeyDigest`, which returned the unkeyed form, is removed, so nothing can report it by mistake.

### The secret

- **Gateway:** `SPILLWAY_GATEWAY_HEAVY_HITTER_SECRET` or `_FILE` (`internal/secret`, ADR-0047). A managed gateway **refuses to start** without it, so digests are never silently unkeyed; `managed.New` also panics if given a sketch without a secret. Static gateways have no sketch (ADR-0051) and ignore the setting, so compose sets it on every gateway, as it does the stream certificates.
- **Control plane:** `SPILLWAY_CP_HEAVY_HITTER_SECRET` or `_FILE`, required by `controlplane serve` (not by `migrate`), for the lookup below. One value for a deployment's gateways and control plane.
- **Compose** sets both from one interpolation variable, `SPILLWAY_HEAVY_HITTER_SECRET`, default `dev-only-not-secret` (`.env.example`); `.gitleaks.toml` is unchanged. No minimum length is enforced, because the one dev value the allowlist admits is 19 bytes; `.env.example` and `docs/setup.md` say to use 32 random bytes or more (`openssl rand -base64 32`).
- Logged as `[redacted]` only; tests check settings dumps, log lines and errors for it on both binaries, and the integration tests' processes for it.
- **Rotation** is a restart of every gateway and the control plane with the new value. Windows reported before it carry the old digests; a lookup finds only windows under the current secret, and during a rolling change one subject has two digests for at most the rollout. Old windows age out with the 7-day prune. There is no second, previous secret.

### The lookup

- **`POST /api/v1/heavy-hitters/lookup`** (`lookupHeavyHitters`, `x-spillway-roles: [admin]`), OpenAPI first (`make api`; additive for oasdiff). A POST so that the subject is in no URL, query string or proxy log. Body: `policy_id` (required), `subject` (`ip`, `consumer_id`, `tenant_id`, `route`, `method`, `headers`), `since`, `until`. The range rules are `GET /heavy-hitters`' (15 minutes by default, at most 7 days; one shared function now). Answer: `key_digest` and the policy's windows under it, newest first, each with `window_start`, the IDs and counts; at most one per minute of a range of at most 7 days, so at most 10,080.
- **The template** is the policy's in the latest published config version, which is what gateways run; a policy not in it is 404. The control plane fills it as a gateway does: `{ip}` an IP address (an IPv4-mapped one as IPv4, an IPv6 one as its /64, the same `config.IPKey` the gateway now calls), `{consumer}` and `{tenant}` the IDs in decimal (managed identity, ADR-0048), `{method}` upper-cased, `{header:<Name>}` by canonical name, `{route}` the route name. Values the template does not name are ignored; a placeholder without a value is 400.
- **`{api_key}` templates are refused (400).** A gateway fills `{api_key}` with the SHA-256 of the whole key, and the control plane keeps only the SHA-256 of the key's secret part (D1), so it cannot compute that digest; such windows already carry `api_key_id`, readable by every role. Accepting the raw key in the body would put credentials in requests for nothing gained.
- **Audit:** every answered lookup writes an audit record in the read's transaction (a lookup that cannot be audited answers nothing): action `heavy_hitters.lookup`, entity `heavy_hitters`, entity ID the policy's, diff `{policy_id, key_digest, since, until, windows}`. It **never holds the subject**: the audit log is append-only and kept forever, and an IP is personal data. Refused lookups (400, 403, 404) are not audited; 403s are logged as every denial is (ADR-0043).
- **No echo:** error messages name a field, never its value; the control plane logs no request body. Tests check answers, logs and audit records for the subject.

### Stored windows

Migration `00006_keyed_heavy_hitter_digests.sql` deletes every `heavy_hitters` row (at most 7 days of windows, dev data only so far; unkeyed digests no lookup can match and anyone could reverse) and adds the index `(policy_id, key_digest, window_start)` for the lookup. A gateway still on the previous release keeps reporting unkeyed digests until it is replaced; those rows cannot be told apart and age out in 7 days. Compose restarts every service together, so dev never has them.

## Consequences

- No role can match a digest to an IP by trying addresses, and digests no longer match Redis key names; only an admin can go from a subject to its windows, and each time is in the audit log.
- The same subject has the same digest on every gateway of a deployment (one secret), so windows still sum across replicas, and a lookup on the control plane finds what the gateways reported.
- Operators distribute one more secret, to the gateways and the control plane; a managed gateway or a control plane without it does not start. Static mode is unaffected.
- The heavy-hitter windows of dev stacks start empty after the migration.
- UI-01's shadow report (M6) and the Throttling screen show keyed digests; a "who is this?" action is admin only.

## Alternatives considered

- **HMAC of the filled template at deny time** — the same secrecy, but a second hash on every deny and the template carried through the deny path, for nothing the digest of the digest does not give.
- **A reversible encryption of the subject** — admins could read raw subjects back, which means storing personal data in a recoverable form for 7 days.
- **Hiding the digest from non-admin roles** — the table, heartbeats and Redis key names would still carry the reversible digest, and UI-01 needs the per-key breakdown for viewers.
- **Sending the secret to gateways over the config stream** — SEC-09 keeps secrets out of snapshots, and every gateway's on-disk snapshot would hold it.
- **A GET lookup with query parameters** — the subject would land in URLs and access logs.
- **A previous secret alongside the current one for rotation** — doubles every lookup for a case v1 does not need; windows age out in 7 days.
- **Keeping the old rows** — they hold the reversible digests this fixes, and no lookup can match them.
