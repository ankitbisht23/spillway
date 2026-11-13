# 0037. Config publish, versions and rollback; the identity path

- **Status:** Accepted
- **Date:** 2026-11-13
- **Requirement IDs:** CP-01 – CP-05, CP-13, CP-17, CP-20, RL-08, RL-09, SEC-06, UI-02, NFR-09

## Context

ADR-0005 split config (draft → validate → publish → rollback) from identity (applied at once, own version, never rolled back) and made publish conditional on the working-set revision. M5 has to make both paths exact:

- what each config edit does to the working-set revision, and how two editors, two publishers, an import and a rollback interleave;
- what CP-03's validation checks, concretely, including SEC-06's allow-list in the API, and what the diff preview (UI-02) contains;
- how config versions stay monotonic, what a rollback writes (D5), and what import and export do (CP-05);
- how each identity change gets "its own monotonically increasing identity version in the same transaction" (CP-20) such that a gateway can never skip a change, and in what form identity reaches gateways;
- how references between the two streams (overrides → policies, tenants and consumers → plans) stay valid when either side changes.

The design's data model says the identity version is "a Postgres sequence". A sequence hands out numbers in call order, not commit order: if change A takes 5 and change B takes 6 and commits first, a gateway that has seen 6 would ignore A's 5, which commits later. That is the problem this ADR solves for identity, and the same reasoning fixes config versions.

## Decision

### The config working set (CP-02)

- **Every config edit** — create, update or delete of an upstream, route, policy or plan; an endpoint drain; an import; a rollback — runs in one transaction that starts with
  `UPDATE config_working_set SET revision = revision + 1, updated_at = clock_timestamp(), updated_by = $actor WHERE id = 1 RETURNING revision`.
  The row lock it takes serializes every config writer, and the returned revision becomes the edited rows' `version` (ADR-0036). The audit record (ADR-0040) is written in the same transaction. Config edits never touch gateways.
- **Entity rules on every write** (create, update, import): the spec parses with protojson into its proto message, and the rules of `internal/config`'s compile that concern that one entity hold (names, enum-like strings, durations, limits and burst rules, header names, regex compile, cost bounds). Upstream endpoints are checked against `SPILLWAY_CP_ALLOWED_NETWORKS` (SEC-06, below). A failure is **422** with `details`. Rules that span entities run at validate, publish and rollback only, because an edit sequence may pass through inconsistent states.

### Validation (CP-03, SEC-06)

`POST /api/v1/config/validate` (no body) and every publish and rollback run the same checks on the whole working set (or, for a rollback, on the target snapshot), assembled into one `Config` proto:

1. **Schema**: each entity's entity rules, again.
2. **References**: every route's `upstream` exists; every policy's `match.routes` entry exists (or is `*`); every plan's `default_limits[].policy` exists.
3. **Cross-stream references** (ADR-0005): every `tenants.plan_id` and non-null `consumers.plan_id` names a plan in this config; every override's `policy_id` names a policy in it, and that policy still accepts the override (ADR-0041: level `consumer` or `route`, template with `{consumer}` or `{api_key}`, the override's limits valid for its algorithm and the cost of every route it matches). "A policy still used by an override cannot be removed" is this check.
4. **Plans** (ADR-0041): every plan defines `default_limits` for every `from_plan: true` policy; each plan entry's limits pass the policy's algorithm rules and RL-03's cost ≤ burst for every route the policy matches.
5. **Regex compile, burst and cost rules** (RL-03): part of the dry compile below.
6. **Policy conflicts**: two policies conflict when both have `mode: enforce`, the same `level`, the same `algorithm` and the same `key_template`, and their matches share at least one (route, method) pair. This narrows CP-03's text, which would also reject a shadow policy beside the enforce policy it is meant to replace (demo scenario D4's promote flow) and ADR-0017's own example (a GCRA and a fixed-window policy at consumer level on every route). Same-template enforce policies are the case that should be one policy with several windows (RL-06). ADR-0035 lists the proposed CP-03 wording for the user.
7. **Dry compile**: `config.Compile` — the gateway's own compile, routing table included — on the assembled snapshot. Settings that only a gateway knows (Redis seeds, JWT settings) are not checked here; a gateway that rejects a published version keeps its last good one and reports the rejection in its heartbeat (ADR-0038).
8. **SEC-06**: every endpoint address is checked against `SPILLWAY_CP_ALLOWED_NETWORKS` with ADR-0008's rules: an IP literal outside the list, or in the always-rejected ranges (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`, `100.100.100.200`), is an error; a host name is resolved with a 2 s timeout and is an error if any address it resolves to is outside the list; a name that does not resolve is not an error (the gateway's resolve- and dial-time checks still apply, ADR-0008).

**The answer** of validate is always 200 (validation errors are data, not a failed request), with `ETag: "<revision>"`:

```json
{"working_set_revision": 57, "base_version": 41, "valid": false,
 "errors": [{"entity": "route", "id": 12, "name": "orders-api", "path": "upstream", "message": "upstream \"orders\" does not exist"}],
 "changes": [{"entity": "route", "name": "orders-api", "change": "changed"}, {"entity": "policy", "name": "burst-cap", "change": "added"}],
 "diff": "--- version 41\n+++ working set (revision 57)\n@@ …"}
```

`changes` lists each added, removed or changed entity against `base_version`, the latest published version. `diff` is a unified diff of the canonical YAML of the two (export's form: entities sorted by name, fields in proto order). `GET /config/versions/{v}/diff` returns `changes` and `diff` between two published versions. This is the preview UI-02 shows; its `working_set_revision` is what publish's `If-Match` carries.

### Publish (CP-02)

`POST /api/v1/config/publish` with `If-Match: "<revision>"` and body `{"note": "…"}` (1–2,000 characters, required). One transaction:

1. `SELECT revision FROM config_working_set WHERE id = 1 FOR UPDATE`. From here every config writer, publisher, importer and rollback waits.
2. `If-Match` missing: **428**; not equal to the revision: **412** with the current revision in `details` (the UI asks for a fresh preview).
3. The revision equals the latest version's `working_set_revision`: **409** `nothing_to_publish`. Two publishers of the same preview therefore produce one version, and the second gets 409.
4. Validation above; any error: **422**, nothing written.
5. `version = (SELECT coalesce(max(version), 0) + 1 FROM config_versions)`. Under the row lock this is monotonic and gapless, and versions commit in order.
6. `INSERT INTO config_versions (…, published_at, snapshot, kind) VALUES (…, clock_timestamp(), <protojson Config with version and IDs>, 'publish')`. `published_at` is "publish committed" for NFR-08 (ADR-0042): taken just before the audit insert, the `NOTIFY` and the commit, it is never later than the commit, so the measure can only overstate.
7. The audit record: action `config.publish`, entity `config`, `diff` = `{"from_version", "to_version", "changes"}` (the snapshot itself is in `config_versions`), the note, `config_version`.
8. `NOTIFY spillway_config, '<version>'` (delivered at commit, ADR-0038). Commit; answer **200** `{"version", "published_at"}`.

### Rollback (CP-04, D5)

`POST /api/v1/config/versions/{v}/rollback` with `If-Match: "<revision>"` (a rollback resets the working tables, so it must not discard edits its caller has not seen) and `{"note": "…"}` (required). One transaction, under the same row lock:

1. `If-Match` as for publish; version `v` must exist (404).
2. Parse `v`'s snapshot with the current proto (a snapshot of an older schema major fails parsing and is **422**) and run validation on it against today's identity: an override or plan reference that `v` lacks is **422** naming the identity rows to change first. Identity is never rolled back (CP-20), so a rollback can be blocked by identity but never changes it.
3. Bump the revision; replace the config working tables with `v`'s entities under their original IDs (rows not in `v` deleted, rows in `v` inserted or updated); every restored row's `version` = the new revision.
4. Insert a new version (`max + 1`) with `v`'s snapshot (its `version` field replaced), `kind = 'rollback'`, `rollback_of = v`, `working_set_revision` = the new revision. History is never rewritten: rolling back to 38 from 41 creates 42, a copy of 38.
5. Audit (`config.rollback`), `NOTIFY spillway_config`, commit; answer as publish.

Rolling back to the latest version is allowed: it discards unpublished edits and publishes an identical copy.

### Import and export (CP-05)

- **Export** `GET /api/v1/config/export` answers `application/yaml`: the working set, or published version `?version=N`, in the static-mode config file's shape (ADR-0017) plus each entity's `id` (and endpoint IDs), with `version: N` when a published version is exported. Entities are sorted by name; `int64` fields are written as YAML integers.
- **Import** `POST /api/v1/config/import` with `If-Match: "<revision>"` and a YAML body (≤ 10 MiB) in the same shape: YAML → JSON → protojson, as the gateway's loader. One transaction under the row lock: each entity is matched to a row of its type by `id` when the file gives one (so a rename keeps the ID and the Redis counters; an `id` that does not exist is **422**: IDs are assigned by the control plane), otherwise by name, otherwise created; rows matched by nothing are deleted. Entity rules apply (422); cross-entity validation waits for validate and publish. A top-level `version` is ignored. An `identity` key, or identity entities, are **422**: identity has no working set and is never bulk-replaced by a config import. The audit record holds the changes list. The answer is 200 with the new revision and the changes list.
- `GET /api/v1/identity/export` writes the identity section in the shape of the managed `Identity` message (ADR-0038): tenants, consumers, keys as prefix and hash, access lists, overrides. It is for backup and inspection; there is no identity import in M5.

### The identity path (CP-20)

**Identity entities**: tenants, consumers, API keys (issue, rotate, revoke), access lists, overrides (burst grants included). Everything else is config.

**Every identity change** — any create, update or delete of those, a key issue, rotation or revocation — runs in one transaction:

1. `UPDATE identity_state SET version = version + 1, changed_at = clock_timestamp() WHERE id = 1 RETURNING version`, first. The row lock serializes identity writers, so identity versions are assigned and committed in the same order, and they have no gaps. The returned number is the change's identity version; written rows get it as `version`.
2. If the change refers to config (a tenant's or consumer's `plan_id`, an override's `policy_id`): `SELECT 1 FROM config_working_set WHERE id = 1 FOR SHARE`, then the reference is checked against the **latest published** config version (not the working set): the plan or policy must exist there, and an override must suit its policy (ADR-0041). No published version yet, or a failed check: **422**. The share lock waits for a publish or rollback in progress, so a publish can never remove a policy that an override created at the same moment refers to. The lock order — `identity_state`, then `config_working_set` — is the only one used with both rows, and publish never waits on `identity_state`, so the two cannot deadlock.
3. The change itself, then its audit record with `identity_version`.
4. `NOTIFY spillway_identity, '<version>'`; commit.

A revocation is therefore visible to the Watch stream at commit (ADR-0038), within NFR-09's 5 s.

This replaces the design's "Postgres sequence" with a single-row counter: a sequence does not give commit order (Context). ADR-0035 lists the proposed wording.

**What a gateway receives.** Every identity version is delivered as a **full identity snapshot** (the `Identity` message, ADR-0038), never as a list of changes. The control plane builds it in one `REPEATABLE READ` read-only transaction that reads `identity_state.version` and the identity tables together, so the snapshot and its version always match. It holds tenants; consumers; API keys that are not revoked and not expired (`expires_at IS NULL OR expires_at > now()`); access lists; overrides that have not ended. Notifications that arrive while a snapshot is being built or sent are coalesced: a gateway receives the latest version, which contains every earlier change.

Size: a key is about 100 bytes of protobuf (8-byte prefix, 64-character hex hash, IDs, expiry), a consumer 60–150 bytes. 1,000 consumers with two keys each is about 350 KB; the 1 MB mark is near 3,000 consumers with two keys. Below that, a full snapshot per change costs a few milliseconds per replica in compose. Above it, incremental identity messages are added (with version gaps impossible, a gateway can detect a missed change), as CP-06 already defers config diffs until snapshots exceed 1 MB. M5's demo and checks hold well under 100 keys.

**What a gateway does with it.** It compiles the identity against its current config (plan and policy IDs resolve to the published config's entries; a reference the config lacks is skipped and logged, which validation makes impossible except during a version skew), swaps it in atomically like a config snapshot (GW-21), writes the on-disk snapshot and heartbeats at once (ADR-0038). Expiry needs no message: a key's `expires_at` and an override's `starts_at` and `ends_at` travel in the snapshot and are compared with the gateway's clock at use.

**Never rolled back.** Config rollback never touches identity tables (above); an identity mistake is fixed by another identity change, with its own version and audit record. Moving a consumer to another plan is identity and stays; the plan's limits are config and roll back (ADR-0005).

## Consequences

- Config edits, publishes, imports and rollbacks run one at a time. Edits are small transactions; a publish holds the lock for validation, which is milliseconds at M5's sizes. Identity changes run one at a time too, so a burst of thousands of key issues is serialized; at admin-action rates that costs nothing.
- An identity change that names a plan or policy waits for a publish in progress. A revocation never names config, so it never waits for one.
- Publishing an unchanged working set is a 409; the NFR-08 check therefore edits before each publish (ADR-0042), which also measures the real edit → publish path.
- A config version can be published that every gateway rejects (a JWT route on gateways without JWT settings). Heartbeats show the rejection per replica (ADR-0038), and the fix is a new publish or a rollback.
- A full identity snapshot per change is simple and self-healing, and it is the first thing to change if identity grows past 1 MB.

## Alternatives considered

- **A Postgres sequence for identity versions** (the design text) — numbers in call order, not commit order; a gateway can skip a change (Context).
- **A sequence plus `pg_advisory_xact_lock` before `nextval`** — the same ordering as the row lock, but the lock and the counter would live in two places.
- **Incremental identity messages now** — needs gap detection and a resync path; at M5's sizes the full snapshot costs nothing.
- **`SERIALIZABLE` transactions instead of the row locks** — retries on every conflicting edit, and revocations could fail under load.
- **Publish without `nothing_to_publish`** — two publishers of one preview would create two identical versions.
- **Rollback without `If-Match`** — a rollback would silently discard edits made after its caller looked.
- **CP-03's conflict rule as written** — rejects shadow-beside-enforce, which the promote flow and ADR-0017's example need.
- **Import of identity** — a bulk, immediate replace of keys and blocks with no review; identity changes stay one at a time and audited.
