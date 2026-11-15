# 0055. Stream epoch: gateways resync after a database restore

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** CP-06, CP-07, CP-20, NFR-12

## Context

ADR-0038's Watch sends a section when the gateway's version number differs from the control plane's, lower ones included, so that gateways follow a restored database down. The review after M5 found the gap: a version number does not name content once a database has been restored. A gateway that is offline (stopped, or cut off from the control plane) during a restore keeps its versions; if the restored database then reaches the same numbers again with other changes, the gateway reconnects at equal numbers, Watch sends nothing, and it keeps stale config or identity — keys revoked since, for example — until the next change. On 2026-11-15 the user chose option (a): a database epoch carried on the stream.

Forces:

- A value fixed once by `controlplane migrate` does not detect the common case. A dump of the database holds the value; restoring a dump **taken from the same database** brings the same value back, at lower version numbers, and the case the review found follows.
- Nothing in the restored database can tell that it was restored: every table, its epoch included, is as it was at the dump. Only something outside it remembers the newer history: gateways (their snapshots) and an active control plane (its broadcaster's cache). A control plane that started after the restore remembers nothing.
- Versions within one history only grow: config versions are gapless `max + 1` under the working-set lock, identity versions a counter under its row lock (ADR-0037). Each version is made once, at a time Postgres records: `config_versions.published_at`, `identity_state.changed_at`.
- Gateways only ever hold what a broadcaster sent them, and a broadcaster holds the newest version of each section it has read.

## Decision

### The epoch

- **The stream epoch** is 16 bytes from `crypto/rand`, in lowercase hex (32 characters). It lives in a one-row table `stream_epoch (id = 1, epoch, changed_at)`, migration 00007.
- **`controlplane migrate`** inserts it when the row is missing (`ON CONFLICT DO NOTHING`); later runs never replace it.
- **The active replica sets a new epoch when it takes the lock**, before its broadcaster reads either section (`RotateEpoch` with no expected value, retried with the LISTEN backoff, 100 ms doubling to 5 s, until it succeeds or the term ends). Every term — a start, a restart, a failover — therefore begins a new epoch. This is what catches a restore done while no replica was active, or followed by a restart: the new term's epoch differs from every gateway's, whatever the database brought back.
- **The broadcaster detects a restore it lives through.** It reads each section in one `REPEATABLE READ` transaction together with the epoch, and keeps each section's version, the time that version was made (`published_at`; `changed_at` for identity) and the epoch. If, under the epoch it holds, a section comes back with a **lower version**, or with **the version it holds but made at another time** (the restored database reached that number again before the broadcaster read it), the database went back. It then sets a new epoch only if the epoch is still the one it read (compare and set, so two replicas in a failover overlap set one), discards what it read, and reads the section again under the new epoch; the other section, held under the old epoch, is read again too. It logs `the <section> went back under the same stream epoch: the database was restored; new stream epoch` at warn level with both versions and times.
- **The broadcaster detects an earlier epoch coming back.** It also holds the epoch's `changed_at`: its term's, or that of a later epoch it set or read. An epoch read that differs from the one held and was set **no later** than it is a copy of this database taken in an earlier term (a gateway offline since then may hold that epoch at the versions the restored database reaches again): it sets a new epoch the same way (compare and set on the epoch read, then read again), and logs `a stream epoch set before the one held came back: the database was restored; new stream epoch`. An epoch set **later** than the one held (another replica's new term in a failover overlap, or a restore from another database) is adopted: both sections are rebuilt under it without setting a new one. An epoch the broadcaster sets itself becomes the one held whatever its time, so a Postgres clock that steps back cannot make it set epochs in a loop; at worst it sets one extra.
- **Why this covers "a gateway offline during a restore, the control plane later at the same version numbers with different content".** A restore brings back the epoch the copy was taken under. If no replica was active throughout, the new term set an epoch that no gateway holds. Otherwise let E be the epoch the active broadcaster holds and H the newest version of a section it held under E before the restore. A gateway holds either E at a version ≤ H, or an epoch set before E (it was offline across the term change or an earlier new epoch; no gateway can hold an epoch set after E, which only a broadcaster sets and sends). If the copy's epoch is E, the database's version can collide with a gateway's only while it is at H or below; the broadcaster reads the section on every notification, every 10 s and after every LISTEN reconnect, and any read at or below H under E is a lower version or H made at another time, and sets a new epoch; if every read after the restore is above H, no gateway's version (all ≤ H) can ever equal the database's, which only grows. If the copy's epoch is one set before E, its `changed_at` comes back with it, earlier than E's, and the first read after the restore sets a new epoch, before any gateway can be sent content under the old one. Gateways connected at the time follow the new epoch at once; one that was offline sees a different epoch when it returns and gets both sections.

### The stream (additive proto, `buf breaking` passes)

- `WatchRequest.epoch` (5): the epoch both of the gateway's sections came from; empty when they came from different epochs, or none (an older gateway, a first boot). `ConfigUpdate.epoch` (4) and `IdentityUpdate.epoch` (3): the epoch the section was read under. `SnapshotFile.epoch` (6): as `WatchRequest`'s.
- **Watch** sends a section when its version **or its epoch** differs from the request's; an empty request epoch always differs, so an older gateway gets both sections on every connect. With equal versions and the same epoch nothing is sent, as before.
- **Heartbeats** carry no epoch: what a replica runs is reported by version as before, and the stream alone decides what to send. After a restore, `replica_versions` and `replicas` are the restored database's rows, which the next heartbeats overwrite or extend.

### The gateway

- It keeps each section's epoch with it. Its Watch request carries the common epoch, or none when the two differ (so an interrupted resync, one section applied under the new epoch, is finished on the next connect).
- **Within one epoch a lower version is ignored**: the newer one is kept and a warning logged. Within one epoch versions only grow, so a lower one comes from a sender that lags, such as a replica that lost the lock and has not yet read the new epoch (ADR-0038's failover overlap). It is not a rejection: `rejected_config_version` is for configs that fail to compile.
- **From another epoch, or with no epoch** (a control plane older than this ADR), any version is applied, lower ones included: ADR-0038's "follow it down", now only where it is safe.
- The on-disk snapshot (D9) is written with the common epoch; a gateway that boots from it applies both sections under the file's epoch and asks the stream with it. A file written before this ADR has none, and the first connect sends both sections.

## Consequences

- A gateway can no longer keep stale config or identity after a restore, whether it was connected, cut off or stopped, and a restored database's revocations reach it on its next connect.
- Every new active term — a control-plane restart, deploy or failover — makes every gateway receive and apply both sections once, at the size of one publish plus one identity change (ADR-0037 sizes: well under 1 MB in M5). An apply of identical content is an atomic swap that keeps limiter state (GW-21, GW-29), so serving is unaffected; NFR-12's outage check sees one resync when the control plane returns.
- The broadcaster reads one more row and two timestamps per refresh; a section whose version and time are unchanged is still not read again.
- The CP-07 runbook (M7, with the Helm chart) needs no restore-specific step: restarting the control plane after a restore is not required, though it is harmless.
- Gateways keep accepting control planes without epochs, so a rolling upgrade in either order works; only the old pairing keeps the old gap.

## Alternatives considered

- **An epoch set by migrate only, never rotated** (the review's first sketch) — a restore of the same database's own dump brings the same epoch back, at lower versions: exactly the reported case stays open.
- **Migrate regenerates the epoch when versions went backwards** — migrate sees only the restored database, which is self-consistent; nothing in it shows that it went back.
- **The control plane compares the gateway's versions with its own and resends when a gateway is ahead** — catches a gateway that returns while the database is still behind it, but not one that returns after the database has caught up, which is the reported case; and with no gateway connected during the gap nothing is detected at all.
- **An operator rotates the epoch after every restore** — correct only when someone remembers; the new epoch per active term does the same automatically, at the cost of one resync per term.
- **Comparing content digests on Watch** (the review's option b) — the user chose the epoch; digests would need a canonical encoding of both sections on both sides.
- **Rejecting lower versions from the stream regardless of epoch** — a restored database could then never be followed down.
