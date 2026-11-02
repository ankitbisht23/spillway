# 0005. Config and identity streams; one config schema

- **Status:** Accepted
- **Date:** 2026-11-02
- **Requirement IDs:** CP-01 – CP-05, CP-10, CP-13, CP-17, CP-18, CP-20, CP-21, RL-08, RL-09, RL-19, UI-02, NFR-09

## Context

As first written, CP-02 and CP-04 versioned every entity into one snapshot. Rolling back a bad policy would then also restore revoked keys, removed access-list entries and expired burst grants, and a revocation or a block (CP-17, D7) would wait for a reviewed publish, missing the < 5 s target (NFR-09). Publishing was also racy: an edit landing between the diff preview and the publish would ship unreviewed. Separately, the config shape was defined four times — the YAML loader, the gRPC proto, the Postgres jsonb snapshot and the OpenAPI spec — and the YAML example had already drifted from the table columns (its failure-mode field had a different name from `failure_mode`).

## Decision

- **Two streams.** Config — upstreams (with endpoints), routes, policies, plans — follows draft → validate → publish with a change note → rollback (CP-02 – CP-04); each publish is a new config version. Identity — tenants, consumers, API keys (prefix + hash), revocations, access lists, overrides and burst grants — is applied on every change with its own monotonically increasing identity version (a Postgres sequence bumped in the same transaction), audited (CP-13), and never rolled back (CP-20). Both reach gateways on the same `ConfigService.Watch` stream as separate messages; gateways track `config_version` and `identity_version` independently and report both in heartbeats. Revocation (CP-17) and one-click blocking (D7) go through identity (≤ 5 s).
- **Publish is conditional.** `POST /api/v1/config/publish` takes `If-Match` with the working-set revision shown in the diff preview (UI-02). Every config edit bumps the revision, so a publish after a later edit fails with 412 and the UI asks for a fresh preview.
- **One schema** (CP-21). `api/proto/config.proto` defines the snapshot from M1: YAML → JSON → proto (protojson), and the Postgres jsonb snapshot stores the same protojson form. Sections: config (upstreams, routes, policies, plans) and identity (tenants, consumers, api\_keys with prefix + hash, access\_lists, overrides). Field names are the proto’s snake\_case names (`failure_mode`, `retry_budget`, `from_plan`, …). Durations are Go-syntax strings and enum-like values lower-case strings, validated when the snapshot compiles. `buf lint` and `buf breaking` run from M1; CP-18 adds the version-skew rules in M5.
- **New entity and endpoints.** `access_lists` (allow or deny by key, consumer or CIDR) at `/api/v1/access-lists`; `/api/v1/tenants`; `GET /api/v1/consumers/{id}/keys` (prefix, created, expires, revoked; never the key); `POST /api/v1/upstreams/{id}/endpoints/{eid}/drain`.
- **OpenAPI-first admin API in M5.** `api/openapi.yaml` is written first; Go server stubs (oapi-codegen) and the UI client are generated from it; `oasdiff` guards it in CI.

## Consequences

- A config rollback never undoes a revocation, block or override; identity mistakes are fixed forward with another identity change. A plan’s limits are config and roll back; moving a consumer to another plan is identity and does not.
- Cross-stream references are checked on both sides: a publish cannot remove a policy that an override uses, and an identity change cannot reference a policy that is not published.
- Gateways hold two versions. The deny cache (RL-19) is cleared on either change, and the on-disk snapshot (CP-07) stores both sections with their versions.
- Draining an endpoint is a config edit, so it ships with the next publish; draining without a publish would need it on the identity path.
- The OpenAPI schemas for config entities and the proto describe the same fields but are not generated from each other; an M5 test checks that their field names match.
- Before M5 (static mode) both sections come from YAML files, and a file change bumps whichever section changed.

## Alternatives considered

- **One versioned snapshot for everything** (the original CP-02 / CP-04) — rollback un-revokes keys and lifts blocks; revocation waits on a reviewed publish.
- **Identity on its own gRPC stream** — a second connection and reconnect path, and no ordering between the two; one stream with typed messages keeps per-gateway order.
- **Last-writer-wins publish** — simpler, but edits made after the preview ship unreviewed.
- **Go structs with YAML tags as the schema** — the proto, jsonb and OpenAPI shapes would each need their own mapping and would drift.
- **Proto enums and `google.protobuf.Duration` in the YAML** — type-checked by protojson, but the YAML would need `AUTH_MODE_API_KEY` and `0.2s`.
