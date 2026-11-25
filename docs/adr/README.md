# Architecture decision records

Decisions that shape Spillway and are not obvious from the code or from docs/design.md. Requirements live in [design.md](../design.md); an ADR records how and why one was met, or why the design changed.

- One decision per file, named `NNNN-kebab-title.md` with the next free number. Start from [0000-template.md](0000-template.md).
- Status is one of **Proposed** (a draft still under discussion), **Accepted**, **Superseded by NNNN** or **Rejected** (a proposal measured or considered and not taken; its record and the decision taken instead stay). An ADR is marked Accepted in the PR that proposes it and takes effect when that PR merges.
- Never edit an accepted ADR's decision. To change it, write a new ADR that supersedes it and set the old one's status to "Superseded by NNNN"; typo and link fixes are fine.
- Each ADR lists the requirement IDs it affects, or "none".

| ADR | Title | Status | Date |
| --- | --- | --- | --- |
| [0001](0001-name-module-licence.md) | Project name, Go module path and licence | Accepted | 2026-11-02 |
| [0002](0002-repo-layout.md) | Repository layout: one Go module, root compose file, embedded UI | Accepted | 2026-11-02 |
| [0003](0003-benchmark-results.md) | Benchmark results on a `bench-results` branch | Accepted | 2026-11-02 |
| [0004](0004-dev-networking.md) | Dev networking: every process in compose, Redis Cluster by hostname | Accepted | 2026-11-02 |
| [0005](0005-config-and-identity.md) | Config and identity streams; one config schema | Accepted | 2026-11-02 |
| [0006](0006-dev-containers.md) | Dev containers: healthchecks, mock upstreams, image layout | Accepted | 2026-11-03 |
| [0007](0007-route-matching.md) | Route matching: precedence and path rules | Accepted; no implicit HEAD for GET superseded by 0030 (D1) and 0033 | 2026-11-03 |
| [0008](0008-gateway-settings-and-upstream-pools.md) | Gateway settings and upstream pools | Accepted; endpoint selection superseded by 0030 | 2026-11-04 |
| [0009](0009-request-framing.md) | Request framing: re-frame instead of rejecting Content-Length with Transfer-Encoding | Accepted | 2026-11-04 |
| [0010](0010-admin-port-and-compose-gateway.md) | Admin port and the gateway in compose | Accepted; shutdown superseded by 0030 | 2026-11-04 |
| [0011](0011-route-transforms-limits-timeouts.md) | Route transforms, size limits and timeouts | Accepted; stalled request bodies superseded by 0033 | 2026-11-04 |
| [0012](0012-tls-listener-and-certificates.md) | TLS listener and certificates | Accepted | 2026-11-04 |
| [0013](0013-request-ids-access-logs-metrics.md) | Request IDs, access logs and request metrics | Accepted; trace context passthrough and the access log's trace ID source superseded by 0030 | 2026-11-04 |
| [0014](0014-load-generator.md) | Load generator v1: open model, achieved rate, JSON result | Accepted | 2026-11-04 |
| [0015](0015-benchmark-workflow-and-m1-exit-check.md) | Benchmark workflow and the M1 exit check | Accepted | 2026-11-04 |
| [0016](0016-per-architecture-bench-rates-and-cpu-topology.md) | Per-architecture benchmark rates and CPU topology | Accepted | 2026-11-04 |
| [0017](0017-local-rate-limiting-model.md) | Local rate limiting model | Accepted; scope rule superseded by 0021, GCRA refund rule by 0028 | 2026-11-10 |
| [0018](0018-ratelimit-headers.md) | RateLimit headers | Accepted | 2026-11-10 |
| [0019](0019-m2-exit-check.md) | The M2 exit check | Accepted; NFR-03 method superseded by 0021 | 2026-11-10 |
| [0020](0020-method-case-in-policies.md) | Method case in rate limit policies | Accepted; forwarding of methods not in upper case superseded by 0029 | 2026-11-10 |
| [0021](0021-distributed-rate-limiting-model.md) | Distributed rate limiting model | Accepted; GCRA emission interval superseded by 0023, start-up loading by 0024, breaker rule by 0025, full-outage budget line and NFR-04 run voiding by 0027, GCRA refund maths and accuracy PASS rule by 0028, killed-primary window rule extended by 0034 | 2026-11-10 |
| [0022](0022-multi-region.md) | Multi-region | Accepted | 2026-11-10 |
| [0023](0023-gcra-emission-interval-in-whole-microseconds.md) | GCRA emission interval in whole microseconds | Accepted | 2026-11-10 |
| [0024](0024-redis-limiter-warm-up.md) | Redis limiter warm-up | Accepted | 2026-11-11 |
| [0025](0025-health-check-driven-redis-breakers.md) | Health-check driven Redis breakers | Accepted; refill count superseded by 0026 | 2026-11-11 |
| [0026](0026-refill-redis-connections-after-an-outage.md) | Refill Redis connections after an outage | Accepted | 2026-11-11 |
| [0027](0027-tolerance-for-host-stalls-in-the-m3-exit-check.md) | Tolerance for host stalls in the M3 exit check | Accepted; NFR-04 one-error rule superseded by 0034 | 2026-11-12 |
| [0028](0028-conservative-gcra-refunds.md) | Conservative GCRA refunds | Accepted | 2026-11-12 |
| [0029](0029-methods-not-in-upper-case.md) | Methods that are not upper case | Accepted | 2026-11-12 |
| [0030](0030-resilience-model.md) | Resilience model | Accepted; connection-cap answer timing superseded by 0032; connection cap at capacity, round-robin retries and the order of retry checks superseded by 0033; breaker column of the attempt-outcome table superseded by 0034; authn span results extended by 0049 | 2026-11-12 |
| [0031](0031-m4-exit-check.md) | The M4 exit check | Accepted; rule (c)'s "by construction" note corrected in 0033 | 2026-11-12 |
| [0032](0032-answering-shed-connections.md) | Answering shed connections | Accepted; which connections are shed at the cap superseded by 0033 | 2026-11-12 |
| [0033](0033-m1-m4-review-fixes.md) | M1–M4 review fixes | Accepted; open breaker question decided in 0034 | 2026-11-13 |
| [0034](0034-user-decisions-after-the-m4-review.md) | User decisions after the M4 review | Accepted | 2026-11-13 |
| [0035](0035-m5-control-plane-and-auth-plan-and-decisions.md) | M5 control plane and auth: plan and decisions | Accepted; the auth_mode interim ended by 0049; the from_plan and default_limits interim ended by 0050 | 2026-11-13 |
| [0036](0036-control-plane-data-model-migrations-and-rest-api.md) | Control-plane data model, migrations and REST API | Accepted | 2026-11-13 |
| [0037](0037-config-publish-rollback-and-the-identity-path.md) | Config publish, versions and rollback; the identity path | Accepted; versions on the stream are named with a stream epoch by 0055 | 2026-11-13 |
| [0038](0038-config-distribution-watch-heartbeat-snapshots-and-the-active-replica.md) | Config distribution: Watch, Heartbeat, snapshots and the active replica | Accepted; what Watch sends, which lower versions a gateway applies and the snapshot file refined by 0055 | 2026-11-13 |
| [0039](0039-gateway-authentication-managed-api-keys-jwt-and-auth-modes.md) | Gateway authentication: managed API keys, JWT and auth modes | Accepted; authn span outcome names settled by 0049 | 2026-11-13 |
| [0040](0040-control-plane-security-mtls-oidc-rbac-audit-and-secrets.md) | Control-plane security: mTLS, OIDC, RBAC, audit and secrets | Accepted; where RBAC is enforced superseded by 0043; the stream certificates' paths superseded by 0047 | 2026-11-13 |
| [0041](0041-limit-precedence-plans-overrides-access-lists-and-heavy-hitters.md) | Limit precedence: plans, overrides, access lists and heavy hitters | Accepted | 2026-11-13 |
| [0042](0042-m5-exit-check.md) | The M5 exit check | Accepted; t_healthy, the outage windows' edges and what a run leaves in Postgres superseded by 0052 | 2026-11-13 |
| [0043](0043-rbac-and-audit-implementation.md) | RBAC and audit: where tokens are checked, test logins and audit details | Accepted | 2026-11-14 |
| [0044](0044-config-import-and-export-details.md) | Config import and export: details | Accepted; the ratelimit section superseded by 0045 | 2026-11-14 |
| [0045](0045-config-settings-and-the-watch-task.md) | Config settings, and the watch task's details | Accepted; the interim until mtls-secrets ended by 0047; managed identity without key lookup ended by 0048 | 2026-11-14 |
| [0046](0046-heartbeats-and-live-replicas-details.md) | Heartbeats and live replicas: details | Accepted | 2026-11-14 |
| [0047](0047-stream-mtls-and-secrets-details.md) | Config stream mTLS and secrets: details | Accepted | 2026-11-14 |
| [0048](0048-managed-api-keys-and-the-identity-path-details.md) | Managed API keys and the identity path: details | Accepted; policy names in managed keys ended by 0050; a rotation's transaction and audit record refined by 0054 | 2026-11-14 |
| [0049](0049-gateway-jwt-and-auth-modes-details.md) | Gateway JWT and auth modes: details | Accepted | 2026-11-14 |
| [0050](0050-plans-overrides-and-access-lists-details.md) | Plans, overrides and access lists: details | Accepted; key-level entries across a rotation refined by 0054 | 2026-11-14 |
| [0051](0051-heavy-hitters-details.md) | Heavy hitters: details | Accepted; the reported key digest refined by 0053 | 2026-11-14 |
| [0052](0052-make-verify-m5-details.md) | make verify-m5: details | Accepted | 2026-11-14 |
| [0053](0053-keyed-heavy-hitter-digests.md) | Keyed heavy-hitter digests and the admin lookup | Accepted | 2026-11-15 |
| [0054](0054-key-rotation-carries-key-level-access-list-entries.md) | Key rotation carries key-level access-list entries | Accepted | 2026-11-15 |
| [0055](0055-stream-epoch-and-resync-after-a-database-restore.md) | Stream epoch: gateways resync after a database restore | Accepted | 2026-11-15 |
| [0056](0056-m6-admin-ui-plan-and-decisions.md) | M6 admin UI: plan and decisions | Accepted | 2026-11-15 |
| [0057](0057-admin-ui-app-toolchain-embedding-and-serving.md) | Admin UI: app, toolchain, embedding and serving | Accepted | 2026-11-15 |
| [0058](0058-admin-ui-session-and-roles.md) | Admin UI: session and roles | Accepted | 2026-11-15 |
| [0059](0059-live-data-the-sse-stream-and-named-queries.md) | Live data: the SSE stream and named queries | Accepted | 2026-11-15 |
| [0060](0060-admin-ui-screens-states-and-api-additions.md) | Admin UI: screens, states and API additions | Accepted | 2026-11-15 |
| [0061](0061-admin-ui-tests-and-make-verify-m6.md) | Admin UI tests and make verify-m6 | Accepted; the verify-m6 policy name superseded by 0072 | 2026-11-15 |
| [0062](0062-live-screens-details.md) | Live screens: details | Accepted | 2026-11-16 |
| [0063](0063-policies-and-publish-details.md) | Policies and publish: details | Accepted | 2026-11-16 |
| [0064](0064-routes-and-upstreams-details.md) | Routes and upstreams: details | Accepted | 2026-11-16 |
| [0065](0065-consumers-keys-and-access-lists-details.md) | Consumers, keys and access lists: details | Accepted | 2026-11-16 |
| [0066](0066-replicas-history-and-audit-details.md) | Replicas, History and the audit log: details | Accepted | 2026-11-16 |
| [0067](0067-m1-m6-review-fixes.md) | M1–M6 review fixes | Accepted | 2026-11-16 |
| [0068](0068-m7-performance-and-hardening-plan-and-decisions.md) | M7 performance and hardening: plan and decisions | Accepted | 2026-11-24 |
| [0069](0069-deny-cache-and-token-leasing.md) | Deny cache and token leasing | Accepted | 2026-11-24 |
| [0070](0070-quotas-and-the-flusher.md) | Quotas and the flusher | Accepted | 2026-11-24 |
| [0071](0071-hardening-carry-over.md) | Hardening carry-over | Accepted | 2026-11-24 |
| [0072](0072-verify-m6-per-run-policy-name.md) | A per-run policy name in make verify-m6 | Accepted | 2026-11-24 |
| [0073](0073-performance-pass-nfr-01-and-the-bench-gate.md) | Performance pass, NFR-01 measurement and the benchmark gate | Accepted | 2026-11-24 |
| [0074](0074-nfr-load-chaos-suite-and-make-verify-m7.md) | NFR load tests, the chaos suite and make verify-m7 | Accepted | 2026-11-24 |
| [0075](0075-kind-cluster-helm-chart-and-observability-as-code.md) | kind cluster, Helm chart and observability as code | Accepted | 2026-11-24 |
| [0076](0076-release-and-supply-chain.md) | Release v1.0.0 and the supply chain | Accepted | 2026-11-24 |
| [0077](0077-quotas-implementation-details.md) | Quotas: implementation details | Accepted | 2026-11-25 |
| [0078](0078-hardening-carry-over-implementation-details.md) | Hardening carry-over: implementation details and the boundary 503s | Accepted | 2026-11-25 |
| [0079](0079-keep-redis-connections-after-a-timed-out-limiter-call.md) | Keep Redis connections after a timed-out limiter call (rejected: boundary 503s a known limit) | Rejected | 2026-11-25 |
| [0080](0080-nfr-01-re-baseline-and-the-pprof-pass.md) | NFR-01 re-baseline (P19) and the pprof pass | Accepted | 2026-11-25 |
| [0081](0081-bench-gate-implementation-details.md) | Bench gate: implementation details and CPU-model rotation | Accepted | 2026-11-25 |
| [0082](0082-nfr-01-floor-on-any-cpu-model.md) | NFR-01's floor on any CPU model | Accepted | 2026-11-25 |
