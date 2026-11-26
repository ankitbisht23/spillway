# 0090. arm64 bench rate and NFR-01 floor re-baselined to 6,300

- **Status:** Accepted
- **Date:** 2026-11-26
- **Requirement IDs:** NFR-01, NFR-05

## Context

`bench.yml` runs one target rate per architecture. On arm64 (`ubuntu-24.04-arm`) that rate, 8,800 rps per gateway vCPU, is both NFR-01's floor (ADR-0080) and the load the NFR-05 regression gate compares (ADR-0073, ADR-0081). ADR-0016 set it at about 90% of M1's slowest saturated run (0.9 × 9,796).

On 2026-11-26 the arm64 runners were not one machine's worth of capacity. Every arm64 run set at 8,800 that day, on the same recorded CPU model (Neoverse N2, `implementer 0x41 part 0xd49`):

| Time (UTC) | Commit | Verdict | Achieved rps (median) | p99 median |
| --- | --- | --- | --- | --- |
| 08:59 | 0e88093 (main) | invalid | 7,938 (7,836 – 8,039) | 1,309 ms |
| 09:48 | 0e88093 (main) | first | 8,800 | 7.9 ms |
| 10:09 | 58e8641 (m7-verify) | pass | 8,800 | 8.4 ms |
| 11:37 | 58e8641 (m7-verify) | fail (p99 +1,230%) | 8,800 | 105 ms |
| 11:56 | 58e8641, confirming re-run | fail | 8,800 | 315 ms |
| 12:30 | 0e88093 (main) | pass | 8,800 | 8.8 ms |
| 13:07 | 58e8641 (m7-verify) | invalid | 8,800 (8,604 – 8,800) | 352 ms |

58e8641 changes no gateway code against 0e88093 (verify script, its tests, ADRs and the README), yet got pass, fail and invalid. When the runner is slower, the open-loop load at 8,800 queues: throughput holds or falls a little and p99 rises 10–150×. The gate then reports a regression or an invalid run set that is the runner's, not the code's, and verify-m7 fails NFR-01 and NFR-05 on it.

Capacity runs the same day (`bench.yml -f rate_x64=20000 -f rate_arm64=20000` on 58e8641, information only; such runs are always invalid because they cannot reach 99% of 20,000):

| Run | arm64 median (min – max) | x64 median, CPU model |
| --- | --- | --- |
| 45839786856 | 10,166 (9,806 – 10,214) | 6,080, AMD EPYC 7763 |
| 45839798366 | 10,155 (9,596 – 10,512) | 6,359, AMD EPYC 7763 |
| 45839811106 | 11,033 (10,609 – 11,198) | 17,147, Intel Xeon 6975P-C |

The slowest arm64 runner measured that day is the 08:59 run set: it saturated at 7,938 rps median. Earlier records: 9,924 (M1), 7,070 (c490e68, before the perf pass), 10,889 (8573118, after it).

## Decision

**User decision, 2026-11-26** ("Lower both"): the arm64 rate drops to 80% of the slowest arm64 runner measured, and it stays one rate for NFR-01's floor and the NFR-05 gate.

- The arm64 rate is 0.8 × 7,938 = 6,350, rounded down to the hundred: **6,300**. It is `bench.yml`'s `rate_arm64` default and the scheduled run's rate.
- NFR-01's Target in docs/design.md reads "≥ 4,600 rps per gateway vCPU on x64 (`ubuntu-latest`) and ≥ 6,300 on arm64 (`ubuntu-24.04-arm`, re-baselined from 8,800 to 80% of the slowest runner measured, ADR-0090), …"; the rest of the row is unchanged. `make verify-m1` and `make verify-m7` print 6,300 as the arm64 floor.
- x64 is unchanged at 4,600, about 76% of the slowest x64 runner measured (6,080).
- The gate compares only results with the same target rate (ADR-0073 §D8), so arm64 results at 8,800 are never compared with results at 6,300. A gated run on main at 6,300 becomes the new arm64 baseline (`first`); the 8,800 results stay on `bench-results` as history.
- No other rule changes: the 99% validity rule, the 10% and spread thresholds, the confirming re-run, and ADR-0089's x64 rule stay as they are.

## Consequences

- arm64 runs at about 57–62% of the typical runner's capacity and 80% of the slowest seen, so a slow runner no longer turns into a queue. A real regression of more than 10% in throughput or p99 still shows: throughput is fixed by the offered rate, so a slower gateway shows as higher p99.
- NFR-01's arm64 promise is weaker: 6,300 instead of 8,800 rps per vCPU. The capacity table in the README still records what the runners reach (10,155–11,033).
- NFR-02 scaling dispatches use the same input as the rate per vCPU: their probes offer 3 × 6,300 × n, which still saturates the gateway at n = 1 and n = 2 (capacity about 10–11k per vCPU), so NFR-02's method is unaffected.
- If a runner slower than 7,938 appears, this ADR's numbers are the reference for a further re-baseline, which is the user's decision.

## Alternatives considered

- **Gate rate only, NFR-01 floor stays 8,800** — the floor would still be invalid on slow runners, as the 13:07 run set was. Not chosen by the user.
- **Treat a p99 spike with flat throughput as invalid** — changes the gate's rule and hides real latency regressions.
- **Larger or self-hosted arm64 runners** — costs money and changes the measured setup.
- **Ship verify-m7 with the honest failures** — leaves NFR-01 and NFR-05 failing on runner noise in every exit run.
