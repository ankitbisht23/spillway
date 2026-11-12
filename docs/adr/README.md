# Architecture decision records

Decisions that shape Spillway and are not obvious from the code or from docs/design.md. Requirements live in [design.md](../design.md); an ADR records how and why one was met, or why the design changed.

- One decision per file, named `NNNN-kebab-title.md` with the next free number. Start from [0000-template.md](0000-template.md).
- Status is one of **Proposed** (a draft still under discussion), **Accepted** or **Superseded by NNNN**. An ADR is marked Accepted in the PR that proposes it and takes effect when that PR merges.
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
| [0021](0021-distributed-rate-limiting-model.md) | Distributed rate limiting model | Accepted; GCRA emission interval superseded by 0023, start-up loading by 0024, breaker rule by 0025, full-outage budget line and NFR-04 run voiding by 0027, GCRA refund maths and accuracy PASS rule by 0028 | 2026-11-10 |
| [0022](0022-multi-region.md) | Multi-region | Accepted | 2026-11-10 |
| [0023](0023-gcra-emission-interval-in-whole-microseconds.md) | GCRA emission interval in whole microseconds | Accepted | 2026-11-10 |
| [0024](0024-redis-limiter-warm-up.md) | Redis limiter warm-up | Accepted | 2026-11-11 |
| [0025](0025-health-check-driven-redis-breakers.md) | Health-check driven Redis breakers | Accepted; refill count superseded by 0026 | 2026-11-11 |
| [0026](0026-refill-redis-connections-after-an-outage.md) | Refill Redis connections after an outage | Accepted | 2026-11-11 |
| [0027](0027-tolerance-for-host-stalls-in-the-m3-exit-check.md) | Tolerance for host stalls in the M3 exit check | Accepted | 2026-11-12 |
| [0028](0028-conservative-gcra-refunds.md) | Conservative GCRA refunds | Accepted | 2026-11-12 |
| [0029](0029-methods-not-in-upper-case.md) | Methods that are not upper case | Accepted | 2026-11-12 |
| [0030](0030-resilience-model.md) | Resilience model | Accepted; connection-cap answer timing superseded by 0032; connection cap at capacity, round-robin retries and the order of retry checks superseded by 0033 | 2026-11-12 |
| [0031](0031-m4-exit-check.md) | The M4 exit check | Accepted; rule (c)'s "by construction" note corrected in 0033 | 2026-11-12 |
| [0032](0032-answering-shed-connections.md) | Answering shed connections | Accepted; which connections are shed at the cap superseded by 0033 | 2026-11-12 |
| [0033](0033-m1-m4-review-fixes.md) | M1–M4 review fixes | Accepted | 2026-11-13 |
