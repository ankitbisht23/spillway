# 0073. Performance pass, NFR-01 measurement and the benchmark gate

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** NFR-01, NFR-05, OB-07

## Context

- **NFR-01** targets ≥ 10k rps per gateway vCPU, 1 KB responses, no-op upstream, on the CI benchmark (manual from M1, nightly from M7). The M1 baseline (ADR-0016, `bench-results`) is far from it on x64 and just under it on arm64. Saturated run sets (commit 7e404da): x64 (AMD EPYC 7763) 5,369 rps median (5,164–5,529), arm64 (Neoverse N2) 9,924 (9,796–9,975). Valid baselines (1310f0c) at the per-arch rates ADR-0016 set: x64 4,600 rps, p99 2.675 ms (spread 0.25); arm64 8,800 rps, p99 47.1 ms (spread 25.3: one run's p99 was 1.19 s, close to capacity). The x64 result's topology confirms ADR-0016's guess: CPUs 0 and 1 are SMT siblings of one core (`thread_siblings` `0-1`), so the gateway (CPU 0) shares its core with loadgen (CPU 1). The M1 exit PR (#15) proposed per-arch floors of 4,600 (x64) and 8,800 (arm64); docs/design.md still says 10k, and only the user can re-baseline.
- **The pprof pass** (M7 box 3) is to cut hot-path allocations, without a stated target.
- **NFR-05** asks for no confirmed regression against the same-arch baseline or the last release's baseline, same recorded CPU model only: a median throughput drop or p99 rise > 10% that also exceeds the baseline's run-to-run spread, confirmed by one re-run, from a nightly gate (Benchmark method step 4, ADR-0003). ADR-0016 added that results must also share the target rate. No release exists yet.

## Decision

### D1. How NFR-01 is measured in M7, and against what

- **Runs.** `bench.yml` on both runners (x64 `ubuntu-latest`, arm64 `ubuntu-24.04-arm`), unchanged layout (gateway CPU 0, loadgen 1-2, upstream 3, no Redis; plain HTTP; access log 1%), unchanged protocol: one run set per arch per dispatch = 5 runs of 30 s warm-up + 3 min (ADR-0015). The layout stays although CPU 0 shares a core with loadgen on x64: it is the layout of every stored result, so it keeps comparisons valid, and on a 2-core × 2-thread runner every layout puts two containers on one core (ADR-0016).
- **Valid** means ADR-0015's rule: every run sent ≥ 99% of the target, no request unanswered, every answer 2xx. Each result records `hardware.cpu_model` and the topology (ADR-0016).
- **NFR-01 passes on an arch** when a valid run set at target **10,000** rps exists for the commit and its median achieved rate, rounded to a whole rps, is ≥ 10,000 per gateway vCPU (the gateway has 1). Both arches must pass. An invalid run set at 10,000 is a FAIL whose line prints the achieved rate.
- **Capacity** (information only, never a pass rule): a run set at target 20,000 on both runners saturates the gateway; its median achieved rate is the capacity. It is stored as an invalid run (never compared, never a baseline).
- **The perf task** dispatches, on its own branch after its changes, one capacity dispatch and one NFR-01 dispatch (`-f rate_x64=10000 -f rate_arm64=10000`), and one capacity dispatch on main at its start for the "before" numbers on today's runner CPUs; it reports both, with each result's CPU model. Results on a branch carry that ref, so they never become baselines (ADR-0003).
- **What verify-m7 checks today:** the 10,000 target of docs/design.md. On today's numbers x64 fails unless the perf pass nearly doubles its throughput. The M1 re-baseline proposal goes to the user as **P19**; if the user approves P19, NFR-01's rule becomes "a valid run set at the arch's floor (x64 4,600, arm64 8,800)", and the bench-gate's nightly run, at the same rates, serves both NFR-01 and NFR-05. Neither the perf task nor verify-m7 re-baselines: if NFR-01 still fails after the perf pass, the perf task stops as blocked with the numbers (the run's standing rule for an NFR missed after profiling), and the user decides P19 or more work.

### D7. The pprof-driven pass

- **Benchmarks.** New `BenchmarkServeHTTP` in `internal/proxy` with sub-benchmarks on the real handler and a loopback upstream answering 1 KB: `plain` (one route, no policy), `local` (two local GCRA policies), `redis` (one tenant-level Redis policy against an in-process fake `Redis` that allows), `apikey` (a managed key resolved, plus `redis`), `lease` (a batched policy served from its lease, ADR-0069). **Allocations per request** = `allocs/op` (and `B/op`) of each sub-benchmark, run as `go test -run '^$' -bench '^BenchmarkServeHTTP$' -benchmem -count 10 ./internal/proxy/`.
- **Comparison.** benchstat (`golang.org/x/perf/cmd/benchstat`, added to `make tools` at an exact pseudo-version, as the Makefile's other tools) between 10 runs on the task's base commit and 10 on its head, on the same laptop with compose stopped: `sec/op`, `B/op`, `allocs/op`, with benchstat's own significance test.
- **Profiles.** CPU (`-cpuprofile`) and allocations (`-memprofile` with `-memprofilerate 1`) of `plain` and `redis`; plus live profiles from gateway-1 at 4,000 rps of `/bytes/1024` for 60 s (loadgen in compose): `/debug/pprof/profile?seconds=30` and `/debug/pprof/allocs` from the admin port (OB-07). Profile files are not committed (`*.pprof` added to `.gitignore`); the PR shows `go tool pprof -top` tables before and after.
- **Reporting.** The PR description carries the benchstat tables, the pprof top tables and D1's capacity numbers before and after; the README's performance section gets the after numbers (readme task).
- **Guardrails.** No behaviour change: every test passes unchanged (unit, property, fuzz, integration, `make verify-m1-reload`, and one `make accuracy REPLICAS=3` with zero over-admission). No `unsafe`, `go:linkname` or reflection tricks. A `sync.Pool` is allowed only with a test showing that a reused object never carries a previous request's headers, keys, identity or decisions. No change of limits, timeouts, budgets, buffer caps that change behaviour, or the benchmark's access-log rate. One optimisation per commit, each citing its benchstat delta. No numeric target for allocations: the goal is NFR-01, measured by D1.

### D8. The NFR-05 nightly gate

- **Schedule.** `bench.yml` gains `schedule: - cron: '17 3 * * *'` (03:17 UTC daily, off the hour, when GitHub's queues are shorter). A scheduled run uses `main`, the default rates (4,600 and 8,800) and the gate. `workflow_dispatch` keeps `rate_x64` and `rate_arm64` and gains `gate` (boolean, default false). GitHub pauses schedules after 60 days without repository activity; docs/setup.md already says to re-enable it.
- **The gate**, `cmd/benchgate` (Go, so it is unit-tested): `benchgate -results <dir> -candidate result.json [-release-tag <tag> -release-commit <sha>]` reads a checkout of `bench-results`, prints its verdict as JSON and exits 0 (pass, first), 1 (flagged or failed) or 2 (bad input). After `bench.sh`, the job runs it when gating, with the newest `v*` tag (by version sort) and its commit when one exists.
- **Comparable results** for a candidate: same `arch`, same `hardware.cpu_model`, same `target_rps`, same `setup` (gateway vCPUs, CPU sets, URL, protocol, warm-up, duration, access-log rate).
- **Baselines.** *Nightly baseline:* the newest comparable result from a run on `refs/heads/main` that is valid and whose `gate.verdict` is `pass` or `first`, or that has no `gate` field (the M1-era results). *Release baseline:* the newest such result whose `commit` is the last release tag's commit. Before any release exists, or when no comparable result exists for that commit, the release comparison is "not available" and only the nightly baseline applies.
- **Rule** (per baseline B, candidate C, medians and B's spread = (max − min) ÷ median of its 5 runs, ADR-0015): throughput drop `d = (B − C) / B` of `achieved_rps.median`, p99 rise `r = (C − B) / B` of `latency_us.p99.median`. **Flagged** when `d > 0.10 and d > B.achieved_rps.spread`, or `r > 0.10 and r > B.latency_us.p99.spread`, against either baseline. At a fixed target a throughput loss mostly shows as an invalid run, which fails on its own (ADR-0003).
- **Verdicts.** `invalid` (the run set is invalid: stored, job fails, never compared, never a baseline); `first` (no comparable baseline: stored, job passes, becomes that CPU model's first baseline if on main); `pass`; `flagged` → **one confirming re-run** in the same job (another full run set on the same runner), gated against the same baselines: flagged again → both stored as `fail` and the job fails; not flagged → the first stays `flagged` (never a baseline) and the re-run is `pass` (a baseline if on main). This re-run is NFR-05's own rule, not a re-run of a failed check.
- **Stored.** Each run set's JSON gets `gate: {verdict, baseline, release_baseline, release_tag, comparisons: [{against, metric, base, value, change, spread, flagged}], rerun_of}` (file names for the baselines, null when absent) and is pushed to `bench-results` as today, the re-run as its own file. `bench.yml`'s timeout becomes 100 minutes (two run sets and builds).
- **Who becomes a baseline:** only gated, valid results on `main` with verdict `pass` or `first`. Ungated dispatches (verify-m7's NFR-01 run at 10,000, capacity runs, branch runs) never do.
- **Testing the gate:** unit tests of `cmd/benchgate` on fixture files under `cmd/benchgate/testdata/`: pass; a drop or rise over 10% but within the spread (pass); over both (flagged); the re-run confirming (fail) and clearing (pass, eligible); another CPU model or target rate (first); an invalid candidate; a release baseline found, missing, and before any tag; legacy results without `gate`. `make bench-gate FILE=<result.json>` runs it locally against `origin/bench-results`. The bench-gate task also dispatches `bench.yml -f gate=true` once on its branch and shows the stored verdicts (branch results, never baselines).

## Consequences

- NFR-01 is judged against the design's 10k until the user decides P19; on today's x64 numbers verify-m7's NFR-01 line will fail unless the perf pass nearly doubles throughput.
- The arm64 baseline's p99 spread (25×) makes its p99 comparison almost blind until a calmer baseline lands; the capacity runs show whether 8,800 is still too close to arm64's capacity, and new default rates need their own ADR backed by a saturating run (ADR-0016).
- `bench-results` grows by two to four small files a night.
- A runner CPU model change starts a new baseline chain (verdict `first`) on its first night.

## Alternatives considered

- **Measuring NFR-01 at saturation** — a saturated run is invalid by ADR-0015's rule, and its latency is a queue's; a valid run at the target is the requirement's own form.
- **Moving the gateway to its own core on x64** — every 4-thread layout shares a core somewhere, and it would orphan every stored baseline.
- **A shell/jq gate in the workflow** — hard to unit-test; a small Go command is.
- **Comparing against the previous night only** — misses slow creep; the design asks for the release baseline too.
- **Re-running every night twice for stability** — doubles runner time; the requirement asks for one confirming re-run only when flagged.
