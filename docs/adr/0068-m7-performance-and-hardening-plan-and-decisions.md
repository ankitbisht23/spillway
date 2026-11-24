# 0068. M7 performance and hardening: plan and decisions

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** RL-10, RL-18, RL-19, NFR-01 – NFR-15, GW-25, GW-26, SEC-04, SEC-06, CP-07, CP-19, OB-04 – OB-06, OB-08

## Context

M7 (Performance and hardening) turns the M0–M6 system into one that meets, or honestly re-baselines, every NFR row, runs on Kubernetes, and can be released. The user approved the task order — design-adrs, deny-cache, leasing, quotas, hardening-carryover, perf, bench-gate, nfr-load, kind, helm, readme, verify, then review, verify-main and release (prepare only) — and referred to twelve decisions D1–D12 without listing them. This ADR defines them in the twelve areas the run gives, indexes the eight M7 ADRs that detail them, maps every M7 box and requirement to a task, describes what the system does between tasks, lists the design-text changes the ADRs need (P19–P32) and two task-plan changes (T4, T5), and records the two open items from the M6 review (O1, O2). The user sees all of it at a checkpoint before any M7 code.

## Decision

### The M7 ADRs

| ADR | Settles | Decisions |
| --- | --- | --- |
| [0069](0069-deny-cache-and-token-leasing.md) | deny cache and token leasing in the hot path, the extended limiter script contract, `lease_k`, NFR-07's harness runs | D2, D3 |
| [0070](0070-quotas-and-the-flusher.md) | plan quotas and quota overrides, keys, the check and its failure mode, headers, the flusher, schema, API and UI | D4, D5 |
| [0071](0071-hardening-carry-over.md) | per-IP connection cap, the accuracy boundary 503s, NULL `overrides.policy_id`, the failover test's port, O1 | D6, O1 |
| [0072](0072-verify-m6-per-run-policy-name.md) | verify-m6's per-run policy name (supersedes ADR-0061's naming) | O2 |
| [0073](0073-performance-pass-nfr-01-and-the-bench-gate.md) | NFR-01's measurement, the pprof pass, the NFR-05 nightly gate | D1, D7, D8 |
| [0074](0074-nfr-load-chaos-suite-and-make-verify-m7.md) | NFR-02, NFR-10, NFR-11, NFR-15, the chaos suite, `make verify-m7` NFR by NFR, stall tolerance, run time, README | D9, verify-m7 plan |
| [0075](0075-kind-cluster-helm-chart-and-observability-as-code.md) | the kind cluster and its memory plan, the Helm chart, gateway identity in Kubernetes (M5's P10), CP-07's runbook and Helm parts, dashboards, alerts, exemplars, runbooks, NFR-14 | D10, D11 |
| [0076](0076-release-and-supply-chain.md) | changelog, SBOM, the release workflow, signing, pins, what is prepared in this run | D12 |

### Task plan check

Every M7 box and requirement has a task. A box that spans tasks is ticked by the task that completes it; this PR ticks no box. The release box is never ticked in this run.

| M7 checklist box (abridged) | IDs | Built by | Ticked by |
| --- | --- | --- | --- |
| Token leasing; deny cache in enforce mode only, min(retry-after, 10 s), cleared on any config or identity version change | RL-18, RL-19 | deny-cache (RL-19), leasing (RL-18, NFR-07's harness mode) | leasing |
| Quotas with the flusher: advisory lock, absolute writes, epoch and re-seed, UTC days and months; quota-usage column | RL-10 | quotas (incl. the NULL `overrides.policy_id` migration, T5) | quotas |
| pprof-driven pass to cut hot-path allocations | — | perf | perf |
| Nightly schedule and regression gate for `bench.yml` | NFR-05 | bench-gate | bench-gate |
| Load tests against every NFR row; chaos suite; noisy-neighbour test | NFR-02, NFR-07, NFR-10, NFR-11, NFR-14, NFR-15 | leasing (NFR-07), nfr-load (NFR-02, -10, -11, -15, chaos suite), helm (NFR-14 on kind) | helm |
| Helm chart on a 3-node kind cluster; dependencies as plain manifests; HTTPS Keycloak; `make kind-up` / `kind-down`; dashboards, alerts and runbooks as code | SEC-04, OB-04 – OB-06, OB-08 | kind (cluster, dependencies, HTTPS Keycloak, targets, smoke), helm (chart, observability as code; or T4's split) | helm (or observability under T4) |
| kind specifics: gateway as the edge, preStop and `maxUnavailable: 0`, admin ports behind a NetworkPolicy, kind ranges in the allow-list, control plane at `replicas: 1`, snapshot volume and emptyDir runbook | GW-25, SEC-06, CP-07, CP-22 | kind (NodePort mapping, ranges), helm (the rest) | helm |
| README: architecture, benchmark, accuracy and chaos reports with reproduction steps | — | readme (structure, reproduction steps, numbers measured so far); verify refreshes the numbers block | readme |
| Release v1.0.0 | — | release (prepare only, ADR-0076) | not in this run |
| Exit: `make verify-m7` | NFR-01 – NFR-15 | verify | verify (its output in the PR) |

| Requirement | Tasks |
| --- | --- |
| RL-19 | deny-cache |
| RL-18, NFR-07 | leasing |
| RL-10 | quotas |
| GW-26 (per-IP cap) | hardening-carryover |
| NFR-01 | perf (measured, D1), verify |
| NFR-05 | bench-gate, verify |
| NFR-02, NFR-10, NFR-11, NFR-15 | nfr-load, verify |
| NFR-14 | helm (rolling restart; reload since M1), verify |
| NFR-03, -04, -06, -08, -09, -12, -13 | already measured (M2–M5); verify re-runs their checks |
| SEC-04 on kind | kind |
| SEC-06 kind ranges | kind (cluster CIDRs), helm (values) |
| GW-25 in Kubernetes | helm |
| CP-07 runbook and Helm parts; CP-19 Helm Job; SEC-02 Helm Secret | helm |
| OB-04, OB-05, OB-06, OB-08 | helm (or observability under T4) |
| M5's P10 (gateway identity in Kubernetes) | helm |

### What each task delivers, and how the system behaves meanwhile

| Task | Builds | Behaviour once merged |
| --- | --- | --- |
| design-adrs | ADR-0068 – ADR-0076, docs/ports.md rows | unchanged |
| deny-cache | ADR-0069's deny cache, metrics, tests | enforce denies cached per replica; everything else as M6 |
| leasing | `lease_k` (proto, OpenAPI, UI, validation), the script change, leases, NFR-07 harness mode | batched policies available; `lease_k: 0` everywhere by default, so nothing changes until a policy opts in |
| quotas | ADR-0070 entire: proto, migration 00008, script quota limits, flusher, API, UI column, compose gives the control plane Redis settings | plan quotas and quota overrides enforced in managed mode; static mode rejects quotas |
| hardening-carryover | per-IP cap, boundary-503 measurements, failover test fix (if a port race), O1 help text, O2 in verify-m6 | one address capped at 1,024 connections per gateway |
| perf | benchmarks, allocation cuts, bench dispatches before and after (D1) | same behaviour, fewer allocations; stops blocked if NFR-01 misses its target (P19 open) |
| bench-gate | `cmd/benchgate`, nightly schedule, gated results | `bench.yml` runs nightly at 03:17 UTC and fails on a confirmed regression |
| nfr-load | `scaling.sh`, `loadgen serve`, loadgen per-tenant mode, `chaos keys/latency/gwkill`, soak and noisy configs, `make chaos` | new tests only |
| kind | tool pins, `deploy/kind/`, certificates, `make kind-up/down/smoke`, port-forwards | a kind cluster with dependencies; smoke checks dependencies only |
| helm | the chart, fleet identity, snapshot subcommands, NetworkPolicies, migrate Job, dashboards, alerts, exemplars, runbooks, NFR-14 rollout | full kind deployment; compose Prometheus loads the alert rules and exemplars |
| readme | README architecture and reports with reproduction steps | — |
| verify | `test/verify/m7.sh`, `make verify-m7`, README numbers block | the M7 exit |

Until bench-gate merges, `bench.yml` stays manual; until kind merges there is no cluster; between kind and helm, `make kind-up` deploys dependencies only.

### Proposed design-text changes (for the user's approval)

docs/design.md is not edited in this PR. Each item names the task that applies it once approved; if the user declines one, the ADR's decision still binds the code where it can and the text stays.

- **(P19) NFR-01 re-baseline** (the open M1 proposal; perf applies it with a re-baselining ADR, as M7's exit allows). Overview: "Targets: ≥ 10k rps per gateway vCPU, ≤ 5 ms p99 added latency, and limit accuracy proven by a test harness." → "Targets: per-vCPU throughput floors per CI architecture (NFR-01), ≤ 5 ms p99 added latency, and limit accuracy proven by a test harness." NFR-01's Target: "≥ 10k rps per gateway vCPU, 1 KB responses, no-op upstream" → "≥ 4,600 rps per gateway vCPU on x64 (`ubuntu-latest`) and ≥ 8,800 on arm64 (`ubuntu-24.04-arm`), each on its baseline's recorded CPU model, 1 KB responses, no-op upstream (re-baselined from M1's runs, ADR-0016)". The perf task may later propose higher floors from its measured capacity; the user decides those too.
- **(P20) Benchmark method** (step 1: nfr-load; step 4: bench-gate). Step 1, "…scaling runs (NFR-02) use gateway 1 then 2 vCPUs, loadgen 1, upstream 1 and no Redis." → "…scaling runs (NFR-02) use gateway 1 then 2 vCPUs (CPU 0, then CPUs 0 and 2, so the two never share an SMT core), loadgen 1, a no-op upstream (`loadgen serve`) on 1 and no Redis, and are valid only if the gateway, not loadgen or the upstream, saturates its CPUs." Step 4, "Comparisons are made only between results with the same recorded CPU model." → "Comparisons are made only between results with the same recorded CPU model, target rate and setup. The nightly run starts at 03:17 UTC."
- **(P21) Metrics table**, new rows (deny-cache, leasing, hardening-carryover): `gw_ratelimit_deny_cache_total` · counter · result (hit, insert, full); `gw_ratelimit_deny_cache_entries` · gauge · none; `gw_ratelimit_lease_total` · counter · result (local, granted, partial, denied, error, inflight, full); `gw_ratelimit_lease_tokens_dropped_total` · counter · reason (expired, reset, reserve); `gw_connections_capped_total` · counter · cap (global, per\_ip), action (closed\_waiting, shed).
- **(P22) Request pipeline step 8** (quotas): "Evaluate global limiters in Redis: the tenant-slot script (tenant → consumer → route levels), then the global level only if that allows (RL-14); multi-window, cost-weighted." → "Evaluate global limiters: the deny cache and token leases first (RL-19, RL-18), then in Redis the tenant-slot script (tenant → consumer → route levels and the consumer's quotas, RL-10), then the global level only if that allows (RL-14); multi-window, cost-weighted."
- **(P23) Limiter script contract** (quotas, after leasing): the ARGV and RETURNS block of ADR-0069 (adds `lease_k`, `group`, `period_start_ms` and `period_end_ms` per limit, the quota epoch key, `granted` and `quota_unseeded`), and one sentence after the write rule: "A lease group writes its grant g = min(k, available) instead of the cost; quota limits (algorithm `quota`) count cost units against the period's quota and are skipped, with `quota_unseeded` = 1, when the tenant's epoch is missing or `TIME` is outside the period."
- **(P24) RL-10** (quotas): "…and until then its quota checks follow the policy’s failure mode. Maximum under-count = one flush interval." → "…and until then its quota checks follow the failure modes of the policies checked in the same tenant-slot call (fail-closed if any enforce policy there is, otherwise not enforced; fail-open when there is none). Counts lost with Redis are at most one flush interval's."
- **(P25) Admin API table**, new rows (quotas): "GET, POST · `/api/v1/quota-overrides` · List, create quota overrides (overrides with no policy)"; "GET, PUT, DELETE · `/api/v1/quota-overrides/{id}` · Read, update (`If-Match`), delete"; "GET · `/api/v1/quota-usage?consumer_id=` · Flushed daily and monthly quota usage and the effective quotas, up to 50 consumers".
- **(P26) Postgres schema table** (quotas): `overrides` "…policy\_id (null for a quota override), limits, quotas, …" → "…policy\_id (null for a quota override, which has quotas and empty limits), limits, quotas, …"; `quota_usage` "consumer\_id, period, used (written as `GREATEST(used, $n)`), flushed\_at" → "consumer\_id, period (`d<YYYYMMDD>` or `m<YYYYMM>`, UTC), used (written as `GREATEST(used, $n)`), flushed\_at".
- **(P27) Redis keys table** (quotas): the Quota row's value "count; `<period>` is the UTC day or month" → "count of cost units; `<period>` is `d<YYYYMMDD>` or `m<YYYYMM>` (UTC)", TTL "period + 1 day" → "the rest of the period + 1 day, set on every write".
- **(P28) Repo layout** (readme, gathering what kind, helm and nfr-load added): `cmd/` gains `benchgate/`, `accuracy/` and `chaos/` (existing, unlisted); `deploy/` reads "compose/, kind/ (cluster, dependency and observability manifests, values), helm/spillway/, grafana/dashboards/, prometheus/rules/"; `test/` gains `kind/`, `obs/`, `verify/`; the root gains `CHANGELOG.md`.
- **(P29) NFR-02, only if x64's SMT layout is what fails it** (nfr-load): Measured by "CI runner with CPU pinning: …" → "arm64 CI runner (physical cores) with CPU pinning: …; x64 (2 cores × 2 threads) is reported for information". Answering it now saves a blocked stop later.
- **(P30) Open question "Quotas: is up to 10 s of under-count acceptable?"** (quotas): tick it with "→ yes: lost counts are bounded by one 10 s flush interval; the ~1 s unseeded window follows the failure mode (ADR-0070)", if the user agrees.
- **(P31) GW-26** (hardening-carryover): "Per-replica caps on open connections and in-flight requests; …" → "Per-replica caps on open connections (also per client address, trusted proxies exempt) and in-flight requests; …".
- **(P32) Admin UI table, Consumers & keys** (quotas): "…; quota-usage column arrives with RL-10 in M7" → "…; quota usage today and this month (RL-10)".

### Proposed changes to the task plan

- **(T4) Split helm** (recommended): helm carries the chart, the fleet identity, NetworkPolicies, the migrate Job, CP-07's parts, the NFR-14 rollout and all of observability as code. Proposed: `helm` (everything but observability, ticking box 7 and box 5's NFR-14) then `observability` (dashboards, alert rules, exemplars, runbooks, (c6) in `m5.sh`, ticking box 6). Without approval, helm does all of it.
- **(T5) The NULL `overrides.policy_id` moves from hardening-carryover to quotas** (needed): quota overrides are those rows, and quotas runs first. hardening-carryover keeps ADR-0071's other items. If declined, quotas ships plan quotas only and quota overrides land with hardening-carryover.

### Questions for the user at the checkpoint

- **P19 first:** without it, verify-m7's NFR-01 line checks 10k and x64 will very likely fail, and the perf task stops blocked if profiling does not close a ~2× gap.
- P20 – P32, T4 and T5.
- **Gateway identity in Kubernetes** (ADR-0075): a fleet certificate with pod-name replica IDs, rather than a StatefulSet (which cannot honour `maxUnavailable: 0`). Confirm.
- **Quota failure mode** (ADR-0070): the tenant-slot call's policies decide, rather than a new `quotas.failure_mode` plan field. Confirm or ask for the field.
- **Quota overrides as their own resource** (ADR-0071): rather than a nullable `Override.policy_id` with an `oasdiff` exception. Confirm.
- **NFR-02** may prove unmeasurable on a 4-vCPU runner (loadgen or the no-op upstream saturating first); the probes will tell, and nfr-load stops for a decision if so.

### Decisions taken for the user (review)

Each is detailed in the named ADR section.

- **D1 NFR-01 measurement** — `bench.yml` on x64 and arm64, unchanged layout and 5 × (30 s + 3 min) protocol, ADR-0015's validity, CPU model recorded; PASS = a valid run set at 10,000 rps per vCPU on both arches; capacity from a 20,000-rps run for information; no re-baseline by any task, the M1 floors go to the user as P19 (ADR-0073 §D1). *Why:* a valid run at the target is the requirement's own form, and only the user re-baselines.
- **D2 Deny cache** — before any Redis call, keyed by the denying limit's Redis key, `until` = call send time + min(retry-after, 10 s), enforce limits only, a cost rule, 65,536 entries in 64 shards with skip-on-full, cleared on every snapshot swap via a generation counter, decision and heavy-hitter records as a Redis deny, new metrics (ADR-0069). *Why:* timing from the send time can never outlast Redis's own deny, and clearing on every swap covers every version change.
- **D3 Token leasing** — `lease_k` (proto 12, OpenAPI, UI) validated ≤ every burst/rate and ≤ 10^6; the script grants min(k, available) per policy group in the same atomic tenant-slot call; one reservation in flight per key and replica with leftovers dropped, 1 s lease life, no returns, cleared on swaps; failure modes as strict; enforce mode only; NFR-07 at 1/3/5 replicas with `lease_k: 10`, pass over-admission ≤ 10/30/50 (ADR-0069). *Why:* emptying before reserving and one reservation in flight make replicas × k a hard bound.
- **D4 Quotas** — `Plan.quotas` and quota overrides (NULL policy) in cost units, managed mode only; keys `q:v1:{t:<tenant>}:<consumer>:d<YYYYMMDD>|m<YYYYMM>`; counted inside the tenant-slot call (so only fully admitted requests count, refunded with the global level); the period checked against Redis `TIME`; failure mode from the call's policies; 429 with `Retry-After` to the period's end and `quota/daily`, `quota/monthly` items; `/quota-overrides`, `/quota-usage`, the UI column; RBAC as overrides (ADR-0070). *Why:* one atomic call keeps quotas and limits consistent, and no gateway clock decides a period.
- **D5 Flusher** — active replica only, every 10 s under `pg_try_advisory_xact_lock(1397774156, 2)`, `GREATEST` upserts, epoch checked every 1 s and re-seeded atomically with max(Redis, Postgres), previous periods flushed for 60 s after rollover, migration 00008, lost counts ≤ one interval; tests for repeated and late flushes, data loss and two control planes (ADR-0070). *Why:* absolute writes and a transaction lock make every flush idempotent and exclusive.
- **D6 Hardening carry-over** — per-IP cap 1,024 (IPv6 /64, trusted proxies exempt) in `ConnLimit` with its own metric; boundary 503s explained (ADR-0025) and cut by the deny cache and leases, measured not judged; NULL policy = quota override with its own API resource; the failover test fixed only if it is the ephemeral-port race, with a non-ephemeral port (ADR-0071). *Why:* each item is closed with the least new mechanism.
- **D7 pprof pass** — `BenchmarkServeHTTP` (plain, local, redis, apikey, lease), allocations per request = allocs/op, benchstat over 10 runs before and after, CPU and alloc profiles from benchmarks and from gateway-1 under load, guardrails (no behaviour change, no unsafe, tested pools, one change per commit), no numeric target (ADR-0073 §D7). *Why:* the goal is NFR-01; a fixed allocation target could reward the wrong changes.
- **D8 Bench gate** — nightly at 03:17 UTC; `cmd/benchgate`; comparable = same arch, CPU model, target and setup; nightly and release baselines from gated, valid main results; flag at > 10% and > the baseline's spread; one confirming re-run in the same job; verdicts stored on `bench-results`; before any release only the nightly baseline applies; unit-tested on fixtures (ADR-0073 §D8). *Why:* it is the design's rule plus ADR-0016's target-rate condition, testable offline.
- **D9 NFR load and chaos** — NFR-02 on both runners with the gateway on CPU 0 then 0,2, `loadgen serve` as the no-op upstream, gateway-bound probes and R(2)/R(1) ≥ 1.70; NFR-10 by `chaos keys`, 1M GCRA and 1M sliding-window keys, Δ`used_memory` ÷ Δ`DBSIZE` ≤ 200 B; NFR-11 by a 10-min 2,000-rps soak and gateway `/metrics` deltas; NFR-15 by loadgen's per-tenant mode toggling the noisy tenant every 10 s, median of 30 pair ratios < 1.10; the chaos suite with new Redis-latency and gateway-kill scenarios (ADR-0074). *Why:* each rule measures the requirement and none can fail on a laptop stall alone.
- **D10 kind** — kind v0.33.0 by `go install`, helm v4.3.1 and kubectl by checksummed downloads, node image by digest; 3 nodes, 127.0.0.1:443 → NodePort 30443 only; dependencies with compose's images in three namespaces, Redis with a password, Keycloak HTTPS-only with a `make certs` certificate; `kind load` for local images; about 5.5 GB, compose stopped first; `make kind-up/down/smoke` with a 3-minute smoke test (ADR-0075 §D10). *Why:* pinned, isolated in its own kubeconfig, and within the laptop's memory.
- **D11 Helm chart** — gateway Deployment (3, surge 1, unavailable 0, preStop sleep 10 s, TLS only, admin on the pod IP, probes on it), NodePort edge, NetworkPolicies admitting Prometheus and the node network, kind CIDRs allowed, control plane at 1, migrate hook Job, a fleet certificate with pod-name replica IDs, emptyDir snapshot with a PDB, an optional seed and the CP-07 runbook, hardened pod security, token-less service accounts, Secrets by reference; dashboards, burn-rate and other alerts with runbooks, exemplars; NFR-14 as an in-cluster 300-rps load through a rolling restart with zero failed requests (ADR-0075 §D11). *Why:* it meets every kind-specific box with stock Kubernetes features.
- **D12 Release** — Keep a Changelog, Syft SPDX JSON SBOMs per image and platform plus source, `release.yml` on `v*` tags or a dispatch defaulting to dry run, publishing only from a tag behind a `release` environment, cosign keyless signatures and SBOM attestations, digest pins at release, actions by SHA; in this run only a dry run and a PR (ADR-0076). *Why:* nothing leaves the repository without the user.

**Open items from the M6 review**

- **O1** — the shadow report keeps counting would-denies by policy name; its help text says so (ADR-0071 §O1), as the user chose (option b).
- **O2** — verify-m6 names its policy `m6-limit-<run>` (ADR-0072, superseding ADR-0061's naming), as the user chose.

**Other choices the requirement text leaves open** (each in its ADR): the deny cache's and lease table's caps (65,536); the 1 s lease life; approximate `RateLimit` headers for leased requests; `quota/daily` and `quota/monthly` header items; a quota-only call being fail-open; the epoch value's format; quota usage read from Postgres (up to 10 s old); daily usage rows pruned after 90 days; redis_exporter added for the Redis dashboard and the memory alert; the latency SLO at 0.5 s; no Alertmanager; no `latest` image tag; the chart not published to a registry.

## Consequences

- Later M7 tasks implement these ADRs; a task that finds one wrong writes a new ADR that supersedes the part.
- P19 – P32 change docs/design.md only after the user approves them; T4, T5 and the questions above are for the same checkpoint.
- Carried and not M7 unless a task needs them: no CRL for stream certificates; the Playground and SC-03's wording (M8); a `consumer_id` filter on `listAccessLists` (needs its own ADR); `make verify-m3`'s accepted laptop pass rate.
- New ports: docs/ports.md gets the kind NodePort note and two port-forward ports (13000, 19091) for Grafana and Prometheus on kind.

## Alternatives considered

- **One large M7 ADR** — over 1,500 lines; the limiter, the cluster and the release are reviewed by different questions.
- **Editing the design text now** — requirement text changes only with the user's approval.
- **Reordering perf before the limiter features** — the perf pass would profile a hot path that deny-cache, leasing and quotas then change.
