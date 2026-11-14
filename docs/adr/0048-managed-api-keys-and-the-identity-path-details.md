# 0048. Managed API keys and the identity path: details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** CP-15, CP-16, CP-17, CP-20, GW-17, GW-19, NFR-09, SEC-03, OB-01

## Context

M5's keys-identity task builds ADR-0039's managed keys (D1, D2) on the identity path of ADR-0037 and the Watch stream of ADR-0038, and ends ADR-0045's interim in which a managed identity compiled with no key lookup (ADR-0045's status line notes it). ADR-0039 fixes the key grammar, the stored form, the endpoints, the lookup and the answers. Building it left these open:

- what an audit record of a rotation holds, which changes two keys at once, and which entity name key records use;
- how an issue answers when there is no `GET` of a single key for its `Location` and `ETag` (ADR-0036's rule for a `POST` that creates);
- whether the issue, rotate and revoke bodies are required, and the limits of rotate's and revoke's optional notes;
- how a prefix collision is retried inside one transaction, and how "revoking a revoked key changes nothing" and "rotating a revoked or expired key is 409" are checked under the identity version's row lock;
- what a managed key's consumer gives the gateway: the template values and the tenant metric label (GW-19, OB-01);
- how NFR-09 is measured on the compose gateways, of which only gateway-1 publishes a port, before make verify-m5 exists.

## Decision

### Control plane

- **Generation** in `internal/apikey`, shared by the control plane and the gateway: ADR-0039's grammar (`apikey.Pattern`), `crypto/rand` only, the secret held in `secret.Value` (ADR-0040's redacted type) so that only the issue and rotate answers ever print it.
- **Endpoints** as ADR-0039: `issueKey`, `rotateKey`, `revokeKey`, roles key-admin and admin (ADR-0040). Their bodies are optional (`{}` or none is the default). Issue answers 201 with `Cache-Control: no-store` and no `Location` or `ETag`: a single key has no `GET` and no `If-Match` use. Rotate answers `Cache-Control: no-store` too. Rotate's and revoke's `note`, when given, is 1–2,000 characters (CP-02's note limit); otherwise 422. Issue's `expires_at` must be after the control plane's clock (422); rotate's `grace` is a Go duration from `0s` to `168h` (422 otherwise).
- **One transaction per change**, under the identity version's row lock (ADR-0037), which every writer of `api_keys` holds (issues, rotations, revocations and the consumer delete's cascade), so the key row is read without its own lock. Rotating a revoked key, or one whose `expires_at` is at or before Postgres's `clock_timestamp()`, is 409; the old key's new expiry is `LEAST(coalesce(expires_at, 'infinity'), clock_timestamp() + grace)`, so it never moves later. Revoking a revoked key returns the row as it is: the transaction rolls back, taking its version bump with it, so there is no new identity version, audit record or `NOTIFY`. Revoking an expired key is allowed and sets `revoked_at`.
- **Prefix collisions.** The insert is `ON CONFLICT (prefix) DO NOTHING RETURNING …`, so a taken prefix returns no row without aborting the transaction; the control plane draws again, four draws in all (the first and ADR-0039's three more), then answers 500.
- **Audit.** Entity `key`, entity ID the key's ID, actions `key.issue`, `key.rotate`, `key.revoke`, each with its identity version; the diff holds listing fields only (ADR-0040). A rotation is one record on the **old** key: `before` and `after` of the old key plus `new_key`, the new key's listing. `?entity=key&entity_id=<new id>` therefore does not find the rotation that created a key; its `rotated_from` names the old key, whose history does.
- **No hash leaves the store** except in the identity snapshot and the admin-only identity export (ADR-0036, ADR-0039): the key queries return listing columns only.

### Gateway

- **Lookup** as ADR-0039, in `config.Identity`: in managed mode the key must match the grammar, its 8 prefix characters find the entry, SHA-256 of its 43 secret characters is compared in constant time, and an `expires_at` at or before the gateway's clock fails. Static identity keeps ADR-0017's 12-byte prefix and whole-key hash. Every failure is the same 401 `{"error":"invalid API key"}`.
- **Identity resolution for keys** (GW-19): a managed key's consumer fills `{consumer}` and `{tenant}` with the consumer's and its tenant's decimal IDs (and so RL-13's hash tag `{t:<tenant id>}`), and its tenant's **name** becomes the request's `tenant` metric label (OB-01: the top 50 tenants by traffic, the rest `other`). Static mode keeps names for the templates and the label `none`, as before. Suspended consumers and tenants, plans and access lists are the plans task's (ADR-0041).
- A managed identity that cannot be right (a key without an 8-character base62 prefix, two keys with one prefix, a hash that is not 64 lower-case hex digits, a key whose consumer or a consumer whose tenant is missing) is rejected and the last good identity kept (ADR-0037); errors name keys by ID only.

### Measuring revocation before make verify-m5

- **`chaos revocation`**, a new `cmd/chaos` subcommand, does ADR-0042's rule (b): it reads a control-plane bearer token on stdin, and per round issues a key for `-consumer`, probes `-gateways` until each answers 200, takes its clock and revokes, then probes each replica every 50 ms until a 401 and 10 more 401s (any other answer, a transport error or a timeout fails); a round's sample is the latest replica's first 401; PASS when every sample is under 5 s. Probes have a 4 s timeout and REST calls 10 s, each over 4× the 820 ms planning stall (ADR-0042), and no rule re-runs anything. The key and the token are never printed. make verify-m5's `chaos controlplane` may reuse it.
- **Tests.** `TestManagedKeys` runs it against five host-process gateways (gateway-1 … gateway-5) on a testcontainer, 10 rounds; `TestComposeRevocation` runs it in the compose network against the five compose gateways in managed mode (`--profile replicas`), 20 rounds, then deletes its tenant and consumer (their keys go with them) so later imports of other configs can still publish. Both are part of `make test-integration`.

## Consequences

- Managed mode now authenticates keys end to end, and revocation, rotation and expiry need no config change on any gateway.
- A key's probes measure 50 ms at best: the probe interval is the measurement's resolution, and the identity path itself is faster.
- Managed policies still use policy **names** in Redis and local limiter keys; ADR-0036's decimal policy IDs there are not built yet (left for a later M5 task or M7; counters reset once when they change).
- Static identity files are unchanged; a static file still cannot carry managed fields (ADR-0045).

## Alternatives considered

- **Two audit records per rotation** (`key.issue` on the new key and `key.rotate` on the old) — two records for one change; ADR-0040 has one per mutation.
- **`Location: /consumers/{id}/keys`** on issue — names a list, not the key; no single-key `GET` exists to name.
- **Retrying a prefix collision with a savepoint or a new transaction** — the `ON CONFLICT DO NOTHING` insert needs neither.
- **Probing gateway-2 … gateway-5 through published ports** — docs/ports.md gives them none; the sender runs in the compose network instead, as M3's and M4's senders do.
- **Timing revocation from heartbeats only** — ADR-0042 rejects it: a heartbeat shows a version applied, not a key refused.
