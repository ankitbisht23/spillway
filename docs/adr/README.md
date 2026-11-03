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
| [0007](0007-route-matching.md) | Route matching: precedence and path rules | Accepted | 2026-11-03 |
| [0008](0008-gateway-settings-and-upstream-pools.md) | Gateway settings and upstream pools | Accepted | 2026-11-04 |
| [0009](0009-request-framing.md) | Request framing: re-frame instead of rejecting Content-Length with Transfer-Encoding | Accepted | 2026-11-04 |
