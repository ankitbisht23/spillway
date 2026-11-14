# 0044. Config import and export: details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** CP-05, CP-02, CP-13, CP-14, CP-21, SEC-06, RL-24, RL-31

## Context

ADR-0037 settles the shape of CP-05's API: export writes the working set or a published version in the static file's shape with IDs; import replaces the working set under the row lock, with `If-Match` on the working-set revision, matching by ID, then name, refusing identity, entity rules only, one audit record. Building it left these open:

- A static config file may have a `ratelimit` section (`replicas`, `redis_budget`; RL-31, ADR-0021). The control plane's config has no place for it: the design's config section is upstreams, routes, policies and plans, ADR-0036 has no table for it, and no REST operation edits it. The repo's own files (the compose gateways', ADR-0017's example, the chaos outage config) set it, and CP-05's import must take them.
- What an import that changes nothing does, which rows get the new revision as their version, how names move between rows in one import, and what an endpoint `id` in the file means (REST `PUT` ignores it and matches endpoints by address).
- Which errors are 400 and which 422, and how an error points into a large file.
- Where the import's optional note (ADR-0040) goes, the body is YAML; what the audit record's `entity_id` is.
- `GET /api/v1/identity/export` writes "the shape of the managed `Identity` message (ADR-0038)", which the watch task adds to `config.proto` with the identity snapshot it streams.

## Decision

### Export

- `GET /api/v1/config/export[?version=N]`, every role: `application/yaml`, the canonical YAML the diff preview already uses (entities sorted by name, set fields only in proto order, IDs and endpoint IDs, 64-bit integers as YAML integers), with `version: N` first for a published version. An empty config is `{}`. The working set's answer has `ETag: "<revision>"`, read in the same snapshot as the rows, so an export and an import with that `If-Match` round-trip safely; a version's answer has no ETag. A version that does not exist is 404; one this binary cannot read is 422 (ADR-0037).
- Export never writes `ratelimit` (below) or identity.

### Import

- `POST /api/v1/config/import?note=…` with `If-Match: "<revision>"`, editor and admin. The body is `application/yaml` (anything else is 415), at most 10 MiB (413); `note` is optional, 1–2,000 characters (400 otherwise), and goes to the audit record.
- **Parsing** follows the gateway's loader (YAML → JSON → protojson) one entity at a time, so an error names the entity: `body: routes[2] ("orders-api"): unknown field "upstrem"`. Malformed YAML, a document that is not a mapping, an unknown top-level field, a section that is not a list, an unknown or mistyped entity field: **400** `invalid_request`, as malformed JSON is (ADR-0036). protojson's JSON positions are dropped from messages: the sender never saw that JSON.
- **Sections the control plane does not keep.** A top-level `version` is dropped silently (ADR-0037). A `ratelimit` section is dropped and reported: the answer's `ignored` lists `ratelimit: not kept: …`, and so does the audit record. Managed gateways take their replica count from heartbeats (RL-24); `redis_budget` has no managed source today (see Consequences).
- **Identity** — any of `identity`, `tenants`, `consumers`, `api_keys`, `access_lists`, `overrides` at the top level — is **422**, one detail per section, before anything else is checked.
- **Entity checks**, before the lock, all reported at once as **422** `validation_failed` with one detail per error and `path` pointing into the file (`plans[1].name`, `routes[0].id`, `routes[3]`): each entity's name and entity rules (as a `POST` checks them), names unique per type, IDs positive and unique per type, and SEC-06 for every endpoint (resolved concurrently, 2 s, ADR-0037). References between entities wait for validate and publish (ADR-0037); an import may leave a dangling route, which validate reports.
- **Under the lock** (`SELECT … FOR UPDATE`, then `If-Match`: 428 / 412 as for publish), entities are matched per type: first every entity with an `id` to the row with that ID (none: **422** `… id N does not exist; ids are assigned by the control plane, so leave it out to create the …`), then every entity without one to the unclaimed row of the same name, else it is created; rows matched by nothing are deleted. Endpoints are matched by address within their upstream, as `PUT` does (ADR-0036); an endpoint `id` in the file must be the one its address already has in that upstream (**422** otherwise), so a hand-edited address cannot silently keep or take another endpoint's ID.
- **Writes**: deletes first; rows whose name changes are first renamed to `.import.<id>`, so names can move or swap between rows in one import; then updates and creates. Only rows whose entity differs from the file's (`proto.Equal`, endpoints included) are written, so unchanged rows keep their `version` and ETag and an editor's `If-Match` on them stays valid. The revision is bumped with `updated_by` set to the actor.
- **No change**: when the working set after the writes equals the one before (by the publish preview's change list), the transaction is rolled back and the answer is 200 with the unchanged revision and no changes: no revision bump, no audit record, nothing for publish to do. A GitOps job that re-imports the same file on every run therefore leaves no trace and creates no publishable change.
- **Answer** 200 `{"working_set_revision", "changes", "ignored"}` with `ETag: "<revision>"`; `changes` is the preview's list (ADR-0037), from the working set before to after.
- **Audit**: one record per import that changed something, in its transaction: action `config.import`, entity `config`, `entity_id` the new working-set revision, the note, and `diff` = `{"from_revision", "to_revision", "changes"}` plus `"ignored"` when a section was dropped. `config.publish` records on entity `config` carry a version as their `entity_id`; the diff's keys tell the two apart.

### Round trip

Export → import → export is byte-stable: IDs match every row, nothing is written, the revision stays. A static file → import → export is semantically equal: `config.Load` of the export is the file's config with entities sorted by name, IDs added, and `version` and `ratelimit` removed. Comments and formatting are not kept.

### Identity export moves to the watch task

`GET /api/v1/identity/export` (admin, ADR-0040) writes the `Identity` message that the watch task adds to `config.proto` (ADR-0038) and builds for the stream from one `REPEATABLE READ` read of the identity tables (ADR-0037). Building it here would mean adding those proto fields, and the static-mode checks they need, one task early, and a second reader of the identity tables that the snapshot builder would then replace. The watch task builds it as the YAML of that snapshot; the design table carries the approved row (P5) from this task on.

## Consequences

- Every repo static config imports as it is; its `ratelimit` section is reported, not lost silently.
- **Open for the user:** in managed mode nothing sets `ratelimit.redis_budget` (default 5 ms) or the replica-count fallback before the first heartbeat (default 1, ADR-0038). If managed gateways need either from the control plane, a later ADR adds a config settings row (an additive migration, a REST operation and a `ConfigChange` entity) and import keeps the section instead of dropping it.
- Import and export add no queries: they use the working-set reads, the per-table create, update and delete, and the endpoint queries that CRUD, publish and rollback already have.
- An import that only reorders entities or endpoints, or re-states defaults, changes nothing.

## Alternatives considered

- **Refuse `ratelimit` (422)** — the compose gateways' file and ADR-0017's example could not be imported without editing.
- **Keep `ratelimit` in the control plane now** — a new section of the managed config with a table, an operation, diff and validation support; a design change for the user, not an import detail.
- **Replace the working tables like a rollback** — simpler, but every row would get the new revision as its version, invalidating every editor's ETag even for untouched entities.
- **Bump the revision and audit a no-op import** — a re-sync would make publish see a change that is not there and fill the audit log.
- **Ignore endpoint IDs, as `PUT` does** — a hand-edited address would silently drop its ID; refusing a wrong ID is as strict as the entity rule.
- **The note in a header** — the body is the file itself; a query parameter is in the spec's parameter list with its length rule, as other operations' parameters are.
- **Identity export now** — see above.
