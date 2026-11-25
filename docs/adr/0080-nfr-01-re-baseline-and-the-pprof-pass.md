# 0080. NFR-01 re-baseline (P19) and the pprof pass

- **Status:** Accepted
- **Date:** 2026-11-25
- **Requirement IDs:** NFR-01, NFR-03, NFR-04, OB-07

## Context

- **P19** (ADR-0068) proposed re-baselining NFR-01 from "≥ 10k rps per gateway vCPU" to the per-arch floors that the M1 runs support (ADR-0016's rates: x64 4,600, arm64 8,800). The user approved it on 2026-11-24 ("Yes, re-baseline"). ADR-0068 says the perf task applies it with a re-baselining ADR, as M7's exit allows; this is that ADR. The re-baseline is the user's decision, not this task's.
- **ADR-0073 §D1** made NFR-01's pass rule conditional on P19: with P19, "a valid run set at the arch's floor (x64 4,600, arm64 8,800)", and the bench gate's nightly run at the same rates serves both NFR-01 and NFR-05.
- **ADR-0073 §D7** sets the pprof pass's method: `BenchmarkServeHTTP` (plain, local, redis, apikey, lease), benchstat over 10 runs before and after with compose stopped, CPU and allocation profiles from the benchmarks and from gateway-1 under load, one optimisation per commit with its delta, and guardrails (no behaviour change, no `unsafe`, a tested `sync.Pool` only).
- **Today's capacity on main** (c490e68, `bench.yml -f rate_x64=20000 -f rate_arm64=20000`, run 45573396662): x64 (AMD EPYC 7763) 5,208 rps median (4,974–5,291, spread 6.1%), arm64 (`implementer 0x41 part 0xd49`, Neoverse N2) 7,070 (7,049–7,130, spread 1.1%). M1's saturated run set (7e404da) was 5,369 and 9,924. So x64 kept its capacity through M2–M7 while arm64 lost 29% of it, on the same recorded CPU model, and arm64's capacity on main is below its 8,800 floor.

## Decision

### P19 applied

docs/design.md takes P19's text unchanged: the Overview's targets say "per-vCPU throughput floors per CI architecture (NFR-01)", and NFR-01's Target is "≥ 4,600 rps per gateway vCPU on x64 (`ubuntu-latest`) and ≥ 8,800 on arm64 (`ubuntu-24.04-arm`), each on its baseline's recorded CPU model, 1 KB responses, no-op upstream (re-baselined from M1's runs, ADR-0016)". NFR-01 passes on an arch when a valid run set (ADR-0015) at that arch's floor exists for the commit on its baseline's CPU model; both arches must pass. `bench.yml`'s default rates are already the floors, so the NFR-01 dispatch is `gh workflow run bench.yml --ref <branch>` with no inputs; the capacity dispatch stays `-f rate_x64=20000 -f rate_arm64=20000` (information only). `make verify-m1` prints the arch's floor as the NFR-01 target instead of 10000; its pass rule (a valid baseline exists) is unchanged. No floor is raised here: the measured capacity may support higher floors later, and the user decides those.

### The pprof pass

What the profiles showed (gateway-1 at 4,000 rps of `/bytes/1024` for 60 s, 30 s profiles from the admin port, OB-07; and `BenchmarkServeHTTP` with `-memprofilerate 1`):

- `httputil.ReverseProxy.copyBuffer` was 58% of the bytes the live gateway allocated: without a `BufferPool` the proxy makes a 32 KB buffer for every response, which the runtime zeroes and the GC then scans and frees (GC mark workers were 14% of the live CPU profile).
- Header names in a case other than `http.CanonicalHeaderKey`'s (`RateLimit-Policy`, `RateLimit`, `X-API-Key`, and OTel's lower-case `traceparent`/`tracestate`) made a canonical copy of the name on every `Set`, `Get` or `Values`.
- The Redis phase made its budget-spent error before every call, and its deadline context (and timer) also for requests that their leases served without a call.
- The rest is net/http's own server, transport and header parsing, context and closure allocations the proxy needs per request, and syscalls (24% of the live CPU profile) and futex wake-ups (15%).

Changes, one commit each, with their benchstat deltas in the commit messages:

1. **Reused copy buffers.** `ReverseProxy.BufferPool` is a `sync.Pool` of 32 KB buffers (the proxy's own size, so no behaviour change). The proxy writes only the bytes each read returned before it gives the buffer back; `TestCopyBuffersNotCarried` fills pooled buffers with other bytes, then checks bodies longer than a buffer, short ones, ones sent in small flushed pieces, and 16 concurrent clients with their own bytes and lengths: every body carries only its own bytes. Headers, keys, identity and decisions never go through the pool.
2. **Canonical header names** for the RateLimit fields and the API key header (the names sent are unchanged), and `telemetry.TraceID` looks up `Traceparent` directly, skipping the propagator when it is absent. `TestHeaderNamesCanonical` guards the names.
3. **The budget-spent error** is made only for a call the budget left no time for.
4. **The Redis calls' context** is made for the first call sent, with the same deadline from the start of the phase.
5. **`traceCarrier`**: the W3C fields are looked up by their canonical names on extract and inject (`TestTraceCarrier` shows it reads and writes what `HeaderCarrier` does). The benchmark gateway traces at a 1% head ratio (D6), so this applies to all of its requests.

Tried and dropped: keeping pooled buffers as `*[32768]byte` to save the 24-byte slice header that `Put` allocates. It saved one allocation per request but showed no CPU gain in two A/B runs (+0.5 to +0.9% sec/op, within run-order noise) and needs a slice-to-array conversion that gosec flags (G602).

Not changed: net/http's internals; the closures and per-attempt objects the proxy needs per request (merging them into one allocation would restructure the handler for a few allocations); the unsampled span's own allocations (ADR-0030's budget); limits, timeouts, budgets, buffer caps and the benchmark's access-log and trace rates.

### Benchmark details (small choices)

- Each case is a real `Handler` serving GET `/` to a loopback upstream answering 1 KB, through `httptest.NewRecorder`, so the per-request cost of the recorder and the request clone is in every number (the same before and after). The access log is JSON to `io.Discard` at a 1% sample, as in `bench.sh`; tracing is off (`BenchmarkHandler` keeps the traced cases).
- `redis` is a route-level `{ip}` Redis policy, not a tenant-level one: a static gateway has no tenant, so a `{tenant}` policy would not apply. `apikey` is the tenant-level policy, on a managed snapshot with a managed key. Limits are high enough that nothing is denied.
- `stubRedis` now grants a batched limit its `lease_k` (1 for others), so `lease` (`lease_k` 1,000) is served from its lease; its `redis-calls/op` (≈ 0.001) shows it.
- benchstat is `golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e` in `make tools` (a commit more than six weeks old rather than the newest, eight days old). `*.pprof` is git-ignored.

### Results

`BenchmarkServeHTTP`, benchstat count 10, base c490e68 (plus the benchmark) vs head, Apple M4 Pro, compose stopped:

| Case | sec/op | B/op | allocs/op |
| --- | --- | --- | --- |
| plain | 66.25 µs → 60.67 µs (−8.4%) | 44.58 KiB → 12.70 KiB (−71.5%) | 140 → 138 (−1.4%) |
| local | 68.37 µs → 62.39 µs (−8.8%) | 45.79 KiB → 13.85 KiB (−69.8%) | 170 → 164 (−3.5%) |
| redis | 69.82 µs → 63.65 µs (−8.8%) | 46.19 KiB → 14.21 KiB (−69.2%) | 182 → 175 (−3.8%) |
| apikey | 70.37 µs → 64.97 µs (−7.7%) | 46.74 KiB → 14.76 KiB (−68.4%) | 191 → 184 (−3.7%) |
| lease | 68.91 µs → 61.95 µs (−10.1%) | 46.10 KiB → 13.88 KiB (−69.9%) | 177 → 166 (−6.2%) |

Every change is p = 0.000. `BenchmarkHandler` with tracing on: 176 → 174 allocs/op at ratios 0 and 0.01 (the carrier commit).

Laptop capacity (exploratory, `test/load/bench.sh` layout, `BENCH_RATE=40000 BENCH_RUNS=3 BENCH_WARMUP=10s BENCH_DURATION=60s`, compose stopped, base and head alternated, Apple M4 Pro, results invalid by design since they saturate):

| Round | Base c490e68 | Head | Change |
| --- | --- | --- | --- |
| 1 | 21,080 rps per gateway vCPU (21,000–21,240), p99 528 ms | 27,951 (27,781–28,500), p99 402 ms | +32.6% |
| 2 | 20,450 (20,443–20,580), p99 535 ms | 28,245 (27,580–28,569), p99 389 ms | +38.1% |

The runner numbers after the pass are measured by the branch's `bench.yml` dispatches (ADR-0073 §D1) and reported in the PR.

## Consequences

- NFR-01 is judged against the per-arch floors from now on: by the perf task's branch dispatch, the bench gate's nightly runs (ADR-0073 §D8) and verify-m7.
- arm64's capacity on main (7,070) is below its 8,800 floor. NFR-01 on arm64 passes only if the pass's gain carries over to the runner; if it does not, the perf task stops as blocked with the numbers and the user decides (more work, or a lower arm64 floor backed by a saturating run, ADR-0016). Where arm64 lost 29% since M1 while x64 lost 3% is not known: the runner, or code from M2–M7 whose cost differs by arch. A capacity run of an M1 commit on today's runner would tell them apart; it needs a branch or tag at that commit.
- The copy-buffer pool keeps up to a few 32 KB buffers per P between GCs instead of allocating one per response.

## Alternatives considered

- **Keeping 10k** — the user approved P19.
- **Raising the floors now from today's capacity** — the user decides floors; this task only reports capacity.
- **A pool for the per-request objects (limits, recorder, attempts)** — each would need a no-carry-over test for keys, identity and decisions, for a few allocations next to net/http's ~100.
- **A larger copy buffer** — changes how the proxy reads, which D7 rules out.
