# 0054. Key rotation carries key-level access-list entries

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** CP-16, RL-09, CP-13, CP-20

## Context

ADR-0039 and ADR-0048 make a rotation one identity change: a new key for the same consumer, with `rotated_from` naming the old one, which keeps working for its grace period. ADR-0041 and ADR-0050 let an access-list entry name a key by its ID (`subject_type: key`). The two did not meet: a rotation made a new key ID that no entry named. The review after M5 found that rotating a deny-listed key gave its holder a successor the block did not cover, and rotating an allow-listed key lost the successor's exemption. On 2026-11-15 the user chose to copy the old key's entries to the new key in the rotation itself.

Constraints: an identity change is one transaction with one identity version and one audit record (CP-13, CP-20, ADR-0040); every writer of `access_lists` and `api_keys` holds the identity version's row lock (ADR-0048); `access_lists` allows one entry per (action, key) (`access_lists_key`).

## Decision

- **We copy** every access-list entry whose `api_key_id` is the old key, allow and deny, to the new key in `RotateKey`'s transaction (`CopyKeyAccessLists`, one `INSERT … SELECT`): same action, subject type `key`, `api_key_id` the new key, the **same note**, and the rotation's identity version as its `version`. The copies are new entries with their own IDs; later changes to either entry do not touch the other.
- **The old key's entries stay.** They keep blocking or exempting the old key through its grace period and lapse with it; nothing removes them (deleting the consumer still cascades to them, as before).
- **Only the key's own entries are copied.** Consumer and CIDR entries already cover the new key, which has the same consumer.
- **One identity version, one audit record.** The copies are part of the rotation's identity change, so the gateway gets the new key and its entries in the same identity message: a successor of a blocked key is never let in by any replica (unknown, so 401, until the version applies; 403 after). The rotation's `key.rotate` record on the old key gains a `new_access_lists` field in its diff, the copies in their REST form (`AccessListEntry`), next to ADR-0048's `before`, `after` and `new_key`. No `access_list.create` records are written for the copies.
- **A key with no entries** rotates exactly as before: no copies, and no `new_access_lists` field in the diff.
- The rotate answer, the gateway and the identity snapshot's format are unchanged; the `rotateKey` summary in `api/openapi.yaml` says the entries are copied.

## Consequences

- Blocking a key now survives rotation: rotating a leaked, blocked key (demo scenario D7) does not hand out a working successor, and partners on a key allow list keep their exemption across rotations.
- Unblocking a rotated key takes deleting the entry of each key that still works (the successor's and, during the grace period, the old key's); `GET /access-lists?subject_type=key` lists them all.
- `GET /audit?entity=access_list` does not show a copy's creation; the old key's history (`?entity=key&entity_id=<old id>`) does, in the rotation's record.
- The old key's entries outlive the key's grace period as inert rows, as entries of an expired or revoked key always have.

## Alternatives considered

- **Resolving entries along the `rotated_from` chain at the gateway** — every lookup would walk the chain, the identity snapshot would need the chain of keys that are no longer live, and a deny on a key would silently cover keys issued later from it with no row naming them.
- **Refusing to rotate a key that has an entry (409)** — leaves an allow-listed partner unable to rotate, and rotating a blocked key would first need its entry deleted, unblocking the old key.
- **Moving the entries to the new key** — the old key, still valid for its grace period, would lose its block.
- **An `access_list.create` record per copy** — more than one audit record for one identity change (ADR-0040's one record per mutation).
