# 0016. Per-architecture benchmark rates and CPU topology

- **Status:** Accepted
- **Date:** 2026-11-04
- **Requirement IDs:** NFR-01, NFR-05

## Context

ADR-0015 gives `bench.yml` one target rate for both architectures, the number input `rate` (default 10,000 rps). Its first run on `main` (commit 7e404da, Actions run 41810064213) saturated the gateway on both runners. Both run sets are stored on `bench-results` and marked invalid, because a run set is valid only if every run sends at least 99% of its target (all 5 runs missed it on x64, one on arm64):

| Arch | CPU | Achieved rps, median (min – max of 5) | p50, median | p99, median |
| --- | --- | --- | --- | --- |
| x64 | AMD EPYC 7763 | 5,369 (5,164 – 5,529) | 1.86 s | 1.98 s |
| arm64 | Neoverse N2 (`implementer 0x41 part 0xd49`) | 9,924 (9,796 – 9,975) | 1.00 s | 1.04 s |

These latencies are a full queue, not the gateway's service time: loadgen keeps at most 10,000 requests in flight (ADR-0014), and 10,000 divided by the achieved rate is about the p50 on each arch (1.86 s and 1.01 s). A baseline taken at saturation would record that queue, and its runs miss the 99% rule anyway. The two runners differ by almost 2×, so one rate either saturates x64 or leaves arm64 at about half its capacity.

Why x64 is so much slower is not known. One guess: GitHub's 4-vCPU x64 runner is 2 cores with 2 SMT threads each, so the gateway on CPU 0 shares a physical core with loadgen on CPU 1, while the arm64 runner has one thread per core. A result records the CPU model, the CPU count and the kernel, but not which CPUs share a core, so the stored data can neither confirm nor rule this out.

## Decision

- **Rates.** This amends ADR-0015's Workflow decision: `bench.yml` takes two number inputs in place of `rate`: `rate_x64` (default 4,600) and `rate_arm64` (default 8,800). The matrix lists each runner with its rate (`ubuntu-latest` with `rate_x64`, `ubuntu-24.04-arm` with `rate_arm64`), and each job passes its own as `BENCH_RATE`. The defaults are about 90% of the slowest run on each arch above (0.9 × 5,164 and 0.9 × 9,796, rounded down to the hundred), so every run stays below saturation and its latency is that of a busy gateway, not of a queue at its cap.
- **Topology.** `test/load/bench.sh` adds `hardware.topology` to its result: one `{cpu, core, socket, thread_siblings}` per logical CPU, from `/sys/devices/system/cpu/cpu*/topology/` (`core_id`, `physical_package_id`, `thread_siblings_list`, the last as the kernel's list, such as `"0-1"`), sorted by CPU. It reads the sysfs of the machine that runs the script, which on the GitHub runners is the Docker host. Without sysfs, as on macOS, whose Docker runs in a VM, `topology` is `null` and the run goes on. CI's short `bench.sh` run checks that the topology lists every CPU.
- **`make verify-m1`** prints each baseline's target rate next to its achieved rate, since below saturation the two are about equal.
- Everything else in ADR-0015 stands: the CPU layout, 5 runs of 30 s + 3 min, the validity rule, `bench.sh`'s own `BENCH_RATE` default of 10,000 for laptops, and the push to `bench-results`.

## Consequences

- A valid baseline per arch becomes possible on today's runners. Its `rps_per_vcpu` is then the rate the gateway sustained at the recorded latency, about the target, not its peak; the peak per arch is the saturated run set above. The M1 exit PR proposes NFR-01's re-baselined number from both, since NFR-01's 10k rps per vCPU is an initial target (docs/design.md, Non-functional requirements); this ADR does not change NFR-01.
- Results at different targets measure different things. M7's regression gate (NFR-05) has to compare results with the same target rate as well as the same CPU model; at a fixed rate below capacity a slower gateway shows as a higher p99, or as an invalid run once capacity falls under the target, not as a lower achieved rate.
- The defaults fit today's gateway and runners. If capacity moves, for example when M7 makes the gateway faster or GitHub changes the runners' CPUs, a dispatch can override the rates, and new defaults come with a new ADR backed by a saturating run.
- The topology shows from the next result whether CPU 0 and CPU 1 are SMT siblings on x64. If they are, the gateway shares its core with loadgen there; a 4-vCPU runner with 2 cores has no spare core to give it, so changing the layout is left to M7, whose scaling runs (NFR-02) need a new one anyway, with its own ADR.
- Each result grows by one small object per CPU.

## Alternatives considered

- **One rate for both, at 90% of x64 (4,600)** — arm64 would run at under half its capacity, and its latency would say little about a busy gateway.
- **Finding each runner's capacity in the job** (a ramp or a search before the 5 runs) — a longer job and more script, and the target would move between dispatches, so results would be harder to compare; M7 can revisit this for the regression gate.
- **Moving the gateway off CPU 0 now** — the SMT guess is not confirmed, and on 2 cores × 2 threads every layout still puts two of the three containers on one core.
- **`lscpu -p` or `lscpu -J` for the topology** — the CSV needs parsing, and the JSON's value types have changed between util-linux versions; sysfs is what `lscpu` reads.
