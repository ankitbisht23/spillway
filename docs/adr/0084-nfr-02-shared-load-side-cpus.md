# 0084. NFR-02: loadgen and the upstream share CPUs 1 and 3

- **Status:** Accepted
- **Date:** 2026-11-25
- **Requirement IDs:** NFR-02

## Context

ADR-0074 §D9 measures NFR-02 on the 4-vCPU CI runners with the gateway on CPU 0, then CPUs 0 and 2, loadgen alone on CPU 1 and the no-op upstream (`loadgen serve`) alone on CPU 3. A result is valid only when every probe is gateway-bound: the gateway at ≥ 0.95 × n vCPU and loadgen and the upstream at ≤ 0.90 vCPU each. [Run 45626436921](https://github.com/ankitbisht23/spillway/actions/runs/45626436921) was INVALID on both arches (ADR-0083, item 20):

- **x64 (AMD EPYC 9V74):** at n = 2, loadgen used 0.93–0.94 vCPU (limit 0.90) and the gateway 1.70–1.71 (needs 1.90). R(1) 7,900, R(2) 15,400, S 1.949.
- **arm64 (Neoverse N2):** the gateway used 0.947–0.949 vCPU at n = 1 (needs 0.95) and 1.69–1.70 at n = 2 (needs 1.90), while loadgen (0.62 / 0.84) and the upstream (0.33 / 0.47) were far from their limits. R(1) 10,500, R(2) 17,300, S 1.648. The cause was not established.

On 2026-11-25 the user decided ("Share CPUs, profile arm64"): loadgen and the upstream share CPUs 1 and 3; a probe is gateway-bound when the gateway uses ≥ 0.95 × n vCPU and loadgen and the upstream together ≤ 1.80 vCPU; the pass rule stays R(2) ÷ R(1) ≥ 1.70 per arch. Before the one measured dispatch, find out with profiles why the arm64 gateway does not use its CPUs, and fix the cause only with evidence.

## Decision

### 1. Layout and validity rule (user decision)

Gateway on CPU 0, then CPUs 0 and 2 (unchanged). loadgen and `loadgen serve` both run with `--cpuset-cpus 1,3`. A result is valid when both measured run sets are valid (ADR-0015) and every probe is gateway-bound: **gateway ≥ 0.95 × n vCPU and loadgen + upstream ≤ 1.80 vCPU combined**. Pass: valid and S = R(2) ÷ R(1) ≥ 1.70 on each arch. Nothing else in NFR-02 or in any other rule changes. On x64 (siblings 0-1 and 2-3) the gateway's CPUs still sit on two different cores, each shared with a load-side sibling, as before.

**docs/design.md, Benchmark method, step 1** (applied by this task, as the user decided):

- Current: "…scaling runs (NFR-02) use gateway 1 then 2 vCPUs (CPU 0, then CPUs 0 and 2, so the two never share an SMT core), loadgen 1, a no-op upstream (`loadgen serve`) on 1 and no Redis, and are valid only if the gateway, not loadgen or the upstream, saturates its CPUs."
- New: "…scaling runs (NFR-02) use gateway 1 then 2 vCPUs (CPU 0, then CPUs 0 and 2, so the two never share an SMT core), loadgen and a no-op upstream (`loadgen serve`) sharing the other 2 (CPUs 1 and 3) and no Redis, and are valid only if the gateway, not loadgen or the upstream, saturates its CPUs."

**ADR-0074 §D9, Layout and Valid** (superseded here; ADR-0074 itself is not edited):

- Current: "gateway `--cpuset-cpus 0`, then `0,2`; loadgen `1`; upstream `3`." and "every probe was **gateway-bound**: the gateway used ≥ 0.95 × n vCPU and loadgen and the upstream each ≤ 0.90 vCPU."
- New: "gateway `--cpuset-cpus 0`, then `0,2`; loadgen and upstream both `1,3`." and "every probe was **gateway-bound**: the gateway used ≥ 0.95 × n vCPU and loadgen and the upstream together ≤ 1.80 vCPU."

**(P35, proposed, not applied)** The NFR table's NFR-02 row still says "gateway on 1 then 2 vCPUs, loadgen 1, upstream 1, no Redis". Proposed: "gateway on 1 then 2 vCPUs, loadgen and upstream sharing 2, no Redis". The user's decision named only step 1 and ADR-0074's rule, so the row is left for the user.

The rule lives in `test/load/scaling.jq` (`gateway_bound`, `not_bound_reason`), which `scaling.sh` includes and `test/load/scaling_test.go` tests at and around both limits. Results record `rule.gateway_bound.loadgen_plus_upstream_max_vcpu: 1.80`, `setup.cpus.loadgen` and `.upstream` `"1,3"`, and the job summary prints loadgen + upstream for each probe.

### 2. Profiling: what holds the gateway back

Method: laptop probes (Apple M4 Pro, Docker Desktop, exploratory only, ADR-0016) with per-container cgroup CPU, per-CPU `/proc/stat`, the gateway's thread `schedstat` (run time and run-queue wait), a CPU profile, a 2-s execution trace and goroutine snapshots; then one exploratory dispatch on the runners, [run 45659287508](https://github.com/ankitbisht23/spillway/actions/runs/45659287508) ("EXPLORATORY diagnostics, not a result"; it stopped before the gate, so nothing was stored), which ran each variant twice on both arches with the new layout: n = 1 and n = 2 at 3 × r × n offered, 8 s warm-up + 24 s, readings over the middle 12 s. The temporary script and workflow change were reverted on the branch.

| Runner probe (2 reps each) | arm64 Neoverse N2: 2xx/s; vCPU gateway / load side | x64 EPYC 7763: 2xx/s; vCPU gateway / load side |
| --- | --- | --- |
| n=1, loadgen default in-flight (10,000) | 11,477 / 11,451; 0.975 / 0.978; 1.49 / 1.51 | 5,142 / 5,117; 0.996 / 0.997; 1.15 / 1.16 |
| n=1, in-flight 400 | 11,954 / 11,346; **1.000 / 1.000**; 1.41 / 1.41 | 5,256 / 5,154; 0.999 / 1.000; 1.15 / 1.13 |
| n=2, in-flight 10,000 | 18,864 / 18,417; 1.746 / 1.747; 1.84 / 1.84 | 11,169 / 10,611; 1.831 / 1.821; 1.75 / 1.74 |
| n=2, in-flight 400 | 19,762 / 19,814; **1.840 / 1.848**; 1.78 / 1.79 | 11,440 / 11,310; **1.857 / 1.890**; 1.74 / 1.76 |
| n=2, in-flight 800, `-conns 400` | 18,529 / 19,562; 1.882 / 1.867; 1.83 / 1.86 | 10,663 / 10,537; 1.879 / 1.838; 1.74 / 1.68 |
| n=2, in-flight 400, gateway `GOMAXPROCS=4` (diagnostic) | 19,757 / 19,465; 1.906 / 1.900; 1.76 / 1.77 | 11,800 / 11,692; 1.921 / 1.920; 1.75 / 1.74 |
| n=2, old layout (loadgen 1, upstream 3), in-flight 10,000 | 17,156 / 16,628; 1.718 / 1.705; 0.86 + 0.51 | 10,621 / 10,599; 1.743 / 1.796; 0.92 + 0.69 |

Findings:

1. **The probe's queue inside loadgen cost the gateway its last few percent (fixed).** A probe offers 3 × the gateway's capacity, so loadgen's 10,000-request in-flight limit filled and stayed full: 10,000 goroutines waiting for one of 200 connections, p50 0.5–2 s. Loadgen then turns connections around more slowly (scheduling and GC over 10,000 goroutines), and the gateway waits for requests. With at most 400 in flight (2 × `-conns`; the rest are dropped, as loadgen already does past its limit) the arm64 gateway reached **1.000 vCPU at n = 1 on every probe** (0.975–0.978 before; 0.947–0.949 in run 45626436921 with the old layout), the n = 2 gateway rose from 1.75 to 1.84–1.85 vCPU, throughput rose 3–5% and the load side used less CPU. On the laptop the same change took the n = 1 gateway from 0.967 to 0.999 vCPU. P still means "2xx answers per second through 200 connections"; only the excess's fate changes (dropped instead of queued). `scaling.sh` now passes `-max-in-flight 400` to probes only; measured runs keep loadgen's default, so that a short stall queues rather than drops their requests (ADR-0026/0034).
2. **Not the cause at n = 2:** connections (`-conns 400` gave the same gateway vCPU and fewer answers, on the laptop `-conns 800` too); GC (laptop: `GOGC=800` cut GCs from about 68/s to 7/s and lowered the gateway's vCPU, with +3% answers and *more* idle); the per-IP connection cap's `ConnState` mutex, the only contended application lock in the trace's sync profile (laptop: a build without it measured the same); the upstream pool (no `MaxConnsPerHost`; 57–100 upstream round trips in flight in laptop goroutine snapshots, out of 200 connections); steal time (0 on every CPU); the load side (idle 7–11% per CPU on the runners at n = 2).
3. **What remains at n = 2 is the Go scheduler on two CPUs.** With in-flight 400 the gateway's CPUs 0 and 2 were each idle 5–6% (runner `/proc/stat`) while its threads accumulated 0.12–0.15 s of run-queue wait per second (`schedstat`); the laptop trace showed both Ps idle in 1,000+ short gaps a second (10 µs – 1 ms). More Ps than CPUs fills the gaps (`GOMAXPROCS=4`: 1.90–1.92 vCPU, +2–3% answers), so the gateway is not waiting for work that does not exist; with two Ps, a P that runs out of goroutines parks and is woken a little late when the next network events arrive. The gateway sets no `GOMAXPROCS` (Go ≥ 1.25 derives it from the cpuset and cgroup, cmd/gateway/memlimit.go), and choosing another value is a runtime behaviour change of the gateway, so it is **not** applied here; it is reported to the user as an option, with the numbers above.

The arm64 scaling efficiency itself is also low: gateway CPU per answer rises from about 84–88 µs at n = 1 to about 93 µs at n = 2, so S on arm64 sits near 1.70 even when the probes are gateway-bound.

### 3. The measured dispatch

One `bench.yml -f mode=scaling` dispatch on the branch after these changes, both arches, budget 60 minutes per arch (ADR-0074), judged by the rule above and never re-run. Its numbers are in the PR description and in the follow-up section below.

## Consequences

- The x64 n = 2 loadgen saturation of run 45626436921 cannot recur as such: the load side has two CPUs, and the runners' load side stayed at ≤ 1.76 vCPU on x64. On arm64 the load side sits at 1.78–1.79 vCPU at n = 2, close to the 1.80 limit.
- The exploratory numbers predict that with the default `GOMAXPROCS` the n = 2 gateway reaches 1.84–1.89 vCPU, under 1.90; if the measured dispatch confirms it, NFR-02 stays open and the user decides (see the PR).
- `test/load/scaling.jq` is the one place the probe rule is written; verify-m7's NFR-02 line (task verify) uses it, or the result's `valid` and `pass`, which it decides.

## Alternatives considered

- **Keep `-max-in-flight 10,000` in probes** — it measures loadgen's queue management, not the gateway, and cost the arm64 gateway 2–3% of a vCPU.
- **More connections** — no more gateway CPU and fewer answers on both runners.
- **`GOMAXPROCS` above the cpuset size for the gateway** — fills the idle gaps on the runners, but changes the gateway's runtime configuration away from its default; a user decision, not a harness detail.
- **Moving the gateway off CPU 0 or using other CPUs** — the user kept the gateway's CPUs unchanged.
