# 0051. Heavy hitters: details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** RL-21, CP-22, RL-26, OB-01, UI-01

## Context

M5's heavy-hitters task builds what ADR-0041 decides for RL-21: one 100-counter Space-Saving sketch per gateway, one event per deny or shadow would-deny, 22-character digests with the consumer and key IDs, `top_keys` in every heartbeat, one-minute `heavy_hitters` windows pruned after 7 days, and a read endpoint. ADR-0050 adds that exempt (allow-listed) requests get no count. Building it left these open:

- what a failed heartbeat does with the entries it took, so that "keeps accumulating" holds without losing the events that arrive while the heartbeat is in flight;
- what static-mode gateways, whose policies have no IDs, count;
- which consumer and key IDs an event carries when it comes before authentication;
- how the deny path reaches the digest of a Redis policy's filled template, and what the sketch costs there;
- what the control plane does with a malformed `top_keys` entry, in which time zone a window's minute is, and the read endpoint's defaults and limits.

## Decision

### Gateway

- **Managed mode only.** The sketch exists only when `SPILLWAY_GATEWAY_CONTROL_PLANE` is set; it is what heartbeats report, and static policies have no IDs (ADR-0050). A static gateway's deny path does one nil check.
- **Events.** The handler counts one event per policy whose decision for the request is `deny` (enforce) or a would-deny (`deny` in shadow mode, RL-26), exactly the decisions the access log records: local phase 1 and phase 2, Redis, degraded and concurrency. A policy whose several limits deny counts once. `fail_open` and `fail_closed` are not decisions to deny and count nothing. An exempt request runs no policy after its allow, so nothing after it counts (ADR-0050); a policy of phase 1 that denies before a key or consumer allow is reached still counts.
- **IDs.** An event carries the request's consumer and managed key IDs once it is authenticated; a phase-1 event, which comes before authentication, carries 0 for both, as an anonymous request does. The control plane keeps the latest non-zero IDs (ADR-0041), so a key throttled both before and after authentication still shows its consumer.
- **Key.** The sketch keys by (policy ID, 16-byte `ratelimit.Key` of the filled template). Local policies use their map key as is. A Redis policy's call keeps each limit's `ratelimit.Key` beside the Redis key built from it (`rlredis.KeyDigest`, the former `digestOf`, so the template is hashed once as before); a degraded limiter is keyed by its Redis key's digest, but its events use the template's digest, so Redis and degraded events of one key are one entry. The 22-character base64url form is computed only for the heartbeat.
- **Cost.** A mutex, a map lookup and, for a new key in a full sketch, a scan of the 100 counts for the smallest: 12 ns per event on a tracked key, 84 ns when every event replaces an entry, 307 ns per event with all 14 cores adding to one sketch at once (Apple M4 Pro, `BenchmarkAdd`); no allocation (`TestAddAllocations`). A denied request through the handler: 2,830 ns and 43 allocations without the sketch, 2,810 ns and 43 with it (`BenchmarkDenied`, `TestHeavyHitterAllocations`). Redis calls carry one more small slice per call (the keys).
- **Heartbeat.** Each heartbeat takes the entries and their interval start and empties the sketch, whose next interval starts then (`Take`). If the heartbeat fails, the entries go back as Space-Saving weighted updates older than what was counted meanwhile (`Restore`): into a tracked key's entry (counts summed, its newer IDs kept), else a free counter, else in place of the smallest, inheriting its count; the interval start goes back to the earlier one. So events that arrive while a heartbeat is in flight are never lost, the counts still sum to the events of the interval, and every key with more than 1/100 of them keeps an entry. The rapid property test checks these bounds on skewed streams with failed and successful heartbeats in between.

### Control plane

- **Storage.** One `INSERT … ON CONFLICT DO UPDATE` per heartbeat, in its transaction, from arrays (one element per key). The arrays are sorted by (policy ID, digest): replicas reporting the same keys at once (a global policy's key is the same on every replica) then lock the shared windows in one order, so their heartbeats cannot deadlock, which Postgres would end by failing one of them (`TestHeavyHitterConcurrentHeartbeats`: 5 replicas × 20 heartbeats of 100 shared keys in different orders hit `40P01` in 3 of 3 runs unsorted, none sorted). The window is `date_trunc('minute', clock_timestamp(), 'UTC')`: in UTC, so a session time zone with a non-whole-minute offset cannot shift it. An ID of 0 is stored as NULL.
- **Malformed entries** (a policy ID below 1, a digest that is not 16 bytes of canonical unpadded base64url, a negative count or no count, a negative ID) are dropped, and duplicates of one (policy, digest) in one heartbeat are summed: a gateway bug in one entry must not fail the heartbeat, which carries liveness (RL-24). The policy ID is not checked against the policies: a deleted policy's windows stay until the prune.
- **Prune.** `PruneHeavyHitters` (batches of 10,000 by `ctid`, as ADR-0046) joins the hourly prune after `replica_versions` and `replicas`, active replica only.
- **Read.** `GET /api/v1/heavy-hitters` (`listHeavyHitters`, every role, ADR-0040): `since` defaults to 15 minutes ago on Postgres's clock (the windows' clock); `until` defaults to none; with only `until`, `since` is 15 minutes before it. `until` must be after `since`, and `since` at most 7 days before `until` (or now, on the control plane's clock): otherwise 400. `limit` is the usual 1–500, default 50. With `policy_id` the query uses `(policy_id, window_start)`, without it the primary key's `window_start` range. The answer is `{"items": [...]}` with no `next_cursor` (ADR-0036); `consumer_id` and `api_key_id` are those of the latest window that has them, null when none has. Reads are not audited (CP-13 audits changes).

## Consequences

- A heartbeat's transaction writes up to 100 more rows; the end-to-end test sees a throttled key in the API within one heartbeat (5.0 s after the last request).
- A heartbeat that commits but whose answer does not reach the gateway (its 4 s deadline passes after the commit) is restored and its entries reported again, so those events count twice in the windows. It needs a control plane slower than the deadline at exactly that moment; deduplicating would need a heartbeat ID the proto does not carry.
- Static-mode gateways report nothing; `make verify-m2` and the other static-mode checks run with the sketch absent.
- A policy that throttles before authentication shows no consumer until the same key is throttled after it.
- UI-01's shadow report (M6) reads `shadow_denied` from the same windows.

## Alternatives considered

- **Reset after the answer** (read the entries, send, clear on success) — drops the events counted while the heartbeat is in flight, up to its 4 s deadline.
- **Subtract what was reported after a success** — an entry evicted and re-inserted meanwhile would be subtracted from the wrong count.
- **A heap or stream-summary for the smallest count** — O(log k) or O(1) instead of a 100-element scan, but more code and map updates on every increment; the scan costs 84 ns in the worst case.
- **A sketch in static mode keyed by policy name** — nothing reads it; heartbeats are managed-only.
- **Rejecting a heartbeat with a malformed entry** (`INVALID_ARGUMENT`) — the replica would drop out of `live_replicas` over a reporting bug.
- **A cursor on the read endpoint** — the ranking is over sums of a range; at most 500 rows answer the "top" question (ADR-0036).
