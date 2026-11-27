# Changelog

All notable changes to Spillway are recorded here. The format follows [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html). Requirement IDs (GW-, RL-, CP-, UI-, OB-, SEC-, NFR-) refer to [docs/design.md](docs/design.md); ADRs are in [docs/adr/](docs/adr/). A release's section is its GitHub release notes (ADR-0076).

## [Unreleased]

## [1.0.0] - 2026-11-27

The first release: a Go API gateway with a distributed rate limiter on Redis Cluster, a control plane over Postgres with an embedded React admin UI, Helm chart and kind cluster, built in milestones M0 to M7. Images: `ghcr.io/ankitbisht23/spillway-gateway:v1.0.0` and `ghcr.io/ankitbisht23/spillway-controlplane:v1.0.0` for linux/amd64 and linux/arm64, signed with cosign keyless through GitHub OIDC, each with an SPDX 2.3 SBOM attached as an attestation (see the README's "Verify a release").

### M0 Foundations

- One Go module and a pnpm workspace with pinned toolchains; Makefile entry points with pinned tools in `./bin` (ADR-0001, ADR-0002).
- `make dev`: the compose stack with Redis Cluster (3 primaries, 3 replicas), Postgres, Keycloak with realm import, Prometheus, Grafana, OTel Collector, Tempo, Loki, toxiproxy and mock upstreams; healthchecks, localhost-only ports, dev networking through an address map (ADR-0004).
- CI on every PR with the single required check `ci-ok`: lint, unit, fuzz smoke, integration, `govulncheck`, `pnpm audit`, gitleaks, multi-arch non-root images with a read-only root filesystem (SEC-10); Actions pinned to SHAs; Dependabot.

### M1 Proxy core

- Radix-tree router on host, method, path and headers (GW-01); reverse proxy on a tuned transport with pooling, keep-alive and HTTP/2, state in registries that survive reloads (GW-04, GW-06, GW-29).
- TLS listener with hot-reloaded certificates and `make certs` (GW-04, SEC-01); transforms, size limits and timeouts (GW-02, GW-03, GW-11); request framing rules (SEC-07, ADR-0009).
- One config schema in `api/proto/config.proto` with atomic snapshot swaps and `buf breaking` in CI (GW-21, CP-05, CP-21); upstream allow-list at load and dial time (SEC-06).
- Request IDs, trace passthrough, JSON access logs, RED metrics, health endpoints and an internal admin port (GW-22, GW-24, GW-25, OB-01, OB-03, OB-07, SEC-02).
- `cmd/loadgen` (open model, HDR histograms) and `bench.yml` with results on the `bench-results` branch (NFR-01, ADR-0003, ADR-0016); zero dropped requests across config reloads under load (NFR-14).

### M2 Local rate limiting

- `Limiter` interface with in-memory GCRA, sliding window, fixed window and concurrency limiters (RL-01, RL-02); per-replica limits from a static replica count (RL-31); local-only policies (RL-17).
- Policies with levels, matches, key templates, limits, burst and cost (RL-03, RL-04); static API keys looked up by prefix and compared in constant time (GW-17, SEC-03); client IP behind trusted proxies only (SEC-05).
- 429 with the pinned IETF RateLimit headers and `Retry-After`; enforce, shadow and off modes; decision metrics (RL-25 – RL-27).

### M3 Distributed rate limiting

- Lua GCRA and sliding-window scripts on Redis Cluster: multi-level, multi-window, cost, shadow writes, Redis `TIME`, hash-tagged versioned keys with fixed-length digests (RL-05 – RL-07, RL-11 – RL-16, RL-29, RL-30).
- One Redis time budget per request, per-node circuit breakers, and fail-open, fail-closed and degraded failure modes (RL-22, RL-23).
- Accuracy harness and the Redis full-outage and killed-primary chaos tests (NFR-06, NFR-13); multi-region as a documented non-goal.

### M4 Resilience

- Active health checks and outlier ejection (GW-08, GW-09); round robin, P2C least-request and consistent hash (GW-07).
- Retries with backoff and a retry budget, deadline propagation, upstream circuit breakers and graceful shutdown (GW-12 – GW-15, GW-23).
- Connection and in-flight caps with fast load shedding (GW-26); OpenTelemetry traces with a span per stage and per attempt (OB-02).

### M5 Control plane and auth

- Postgres schema and an OpenAPI-first admin API (`api/openapi.yaml`, oapi-codegen, oasdiff in CI) with cursor pagination and ETags (CP-01, CP-10, CP-14); migrations as a separate `controlplane migrate` step (CP-19); YAML import and export (CP-05).
- Validate, publish with a change note and `If-Match`, versions and rollback (CP-02 – CP-04); identity changes applied at once on their own version (CP-20).
- gRPC Watch and Heartbeat over mTLS, live replica counts, sync age and an on-disk last-good snapshot; a stream epoch so gateways resync after a database restore (CP-06 – CP-09, CP-18, RL-24, SEC-02).
- Managed API keys (issue, list, rotate, revoke), JWT/JWKS with an algorithm allowlist, identity to tenant and plan (CP-15 – CP-17, GW-17 – GW-20, SEC-04).
- Plans, overrides, allow and deny lists; Keycloak login, RBAC on every endpoint and an append-only audit log (RL-08, RL-09, CP-11 – CP-13, SEC-08); heavy hitters with keyed digests (RL-21); secrets from env or files (SEC-09).

### M6 Admin UI

- The React admin UI embedded in the control-plane binary, OIDC login with in-memory tokens, role-aware navigation and a client generated from the OpenAPI spec (UI-04, UI-05, UI-08, UI-09).
- Overview and Throttling screens on live SSE data (UI-03); policy editor with validation, diff, publish, shadow report and promote (UI-01, UI-02); Routes & upstreams, Consumers & keys, Replicas, History with rollback and the audit log (UI-06).
- Vitest and Playwright tests for the main flows in CI.

### M7 Performance and hardening

- Deny cache in enforce mode and token leasing (batched mode) (RL-18, RL-19); quotas with the control-plane flusher and a quota-usage column (RL-10, UI-06); a per-client-address connection cap (GW-26).
- A pprof-driven pass over the hot path and the NFR-01 re-baseline per architecture (ADR-0080, ADR-0082, ADR-0090); the nightly benchmark and regression gate (NFR-05, ADR-0081).
- Load tests against every NFR row and a chaos suite: key memory, soak, noisy neighbour, Redis latency, gateway kill (NFR-02, NFR-07, NFR-10, NFR-11, NFR-14, NFR-15; ADR-0083).
- The `spillway` Helm chart on a 3-node kind cluster with the gateway as the edge, NetworkPolicies on admin ports and Keycloak over HTTPS (GW-25, SEC-04, SEC-06, CP-07; ADR-0085, ADR-0086).
- Dashboards, alert rules, exemplars and runbooks as code (OB-04 – OB-06, OB-08; ADR-0087); README architecture, benchmark, accuracy and chaos reports; `make verify-m7`, the M7 exit (ADR-0088).
- Release workflow: multi-arch images, Syft SPDX SBOMs, cosign keyless signatures and SBOM attestations, third-party images pinned by digest (ADR-0076, ADR-0092).

### Known limitations

- **NFR-04 and the other Redis-budget checks on Docker Desktop for Mac** can fail a verify run when the VM pauses longer than the 5 ms Redis budget more than once in a run. ADR-0034 excuses one burst of 1 s or less. Seen on `main` a69b856: verify-m7 run m7-20261126T233441Z had 18 errors over 20 s, and a diagnostic verify-m3 had 7 timeouts within 4 ms plus 1 lone one, 25 s apart. Not a gateway defect: no gateway errors, gateway CPU about 30%. Recorded as a macOS / Docker Desktop constraint by the user's decision; no rule changed.
- **NFR-02 (scaling) is open**: no valid result on either architecture, because the 2-vCPU gateway never saturates both CPUs on the CI runner (ADR-0084).
- **NFR-05 on x64 is not demonstrated** on a CPU model that has no `main` baseline yet (ADR-0089), and **arm64 p99 varies between runners**, so a slow runner can confirm a regression that is not in the code (ADR-0088).
- **Boundary-pattern 503s** under synchronized bursts at the 5 ms Redis budget (55–61% strict, 33–42% batched in the accuracy harness); over-admission is unaffected (ADR-0079, Rejected).
- **`make verify-m3` fails some full runs on the laptop** from host and Docker VM stalls (ADR-0027, ADR-0034).
- **NFR-01** is re-baselined to 4,600 rps per gateway vCPU on x64 and 6,300 on arm64 (ADR-0080, ADR-0082, ADR-0090).
- **Kubernetes is supported on kind only** in this release; interactive admin-UI use stays on compose, and a Redis pod that restarts comes back empty.
- **Exemplars** of ordinary requests mostly link to traces the Collector's tail sampling dropped (OB-06, ADR-0091).
- One active control-plane replica (CP-22) and no Alertmanager.

[Unreleased]: https://github.com/ankitbisht23/spillway/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/ankitbisht23/spillway/releases/tag/v1.0.0
