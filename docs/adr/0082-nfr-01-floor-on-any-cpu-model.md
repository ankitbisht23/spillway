# 0082. NFR-01's floor on any CPU model

- **Status:** Accepted
- **Date:** 2026-11-25
- **Requirement IDs:** NFR-01, NFR-05

## Context

- **ADR-0080** applied P19: NFR-01's Target became "≥ 4,600 rps per gateway vCPU on x64 (`ubuntu-latest`) and ≥ 8,800 on arm64 (`ubuntu-24.04-arm`), each on its baseline's recorded CPU model, …", so an arch passes only with a valid run set at its floor on the CPU model its baseline recorded.
- **GitHub's x64 `ubuntu-latest` pool rotates CPU models** (ADR-0080, ADR-0081): the x64 baseline is on an AMD EPYC 7763; an AMD EPYC 9V45 and an Intel Xeon Platinum 8370C were seen on 2026-11-25. A job cannot choose its model, so under the current text an x64 NFR-01 run that lands on another model can neither pass nor fail: it does not count, and NFR-01 waits for the pool to hand out the baseline's model again.
- The phrase exists for comparisons: two results are only comparable on the same CPU model. That is NFR-05's rule (its row says "same recorded CPU model only", and step 4 of Benchmark method "Comparisons are made only between results with the same recorded CPU model, target rate and setup"); NFR-01 is a floor, not a comparison.
- **User decision, 2026-11-25** ("Any model, floor met"): NFR-01's floor passes on an arch with a valid run set at the floor (x64 4,600, arm64 8,800) on whatever CPU model the runner lands on, and the result records that model. "Each on its baseline's recorded CPU model" applies to regression comparisons only (NFR-05 and the bench gate's same-model rule), not to the floor. No other rule changes.

## Decision

docs/design.md, NFR table, NFR-01's Target:

- **Current:** "≥ 4,600 rps per gateway vCPU on x64 (`ubuntu-latest`) and ≥ 8,800 on arm64 (`ubuntu-24.04-arm`), each on its baseline's recorded CPU model, 1 KB responses, no-op upstream (re-baselined from M1's runs, ADR-0016)"
- **New:** "≥ 4,600 rps per gateway vCPU on x64 (`ubuntu-latest`) and ≥ 8,800 on arm64 (`ubuntu-24.04-arm`), on whatever CPU model the runner has, which the result records (ADR-0082), 1 KB responses, no-op upstream (re-baselined from M1's runs, ADR-0016)"

NFR-01 passes on an arch when a valid run set (ADR-0015) at that arch's floor exists for the commit, on any CPU model; both arches must pass. The result's `hardware.cpu_model` is printed with the verdict. NFR-05's row and step 4 of Benchmark method keep the same-CPU-model rule unchanged; they already state it, so no text moves there.

What follows for the other M7 tasks:

- **verify-m7's NFR-01 line** (the verify task, ADR-0074's table) uses this rule: per arch, PASS when the run set it reads is valid and its median achieved rate, rounded, is at least the floor, whatever its CPU model; the line prints each arch's CPU model.
- **`make verify-m1`** (a valid baseline exists, ADR-0080) and **cmd/benchgate** (NFR-05, ADR-0081) are unchanged.

## Consequences

- An x64 NFR-01 run counts on any model the pool hands out, so NFR-01 no longer depends on which model a job lands on.
- A floor met on a faster model than the baseline's says less about the baseline's model; the recorded model lets a reader tell, and the regression gate still compares like with like.
- ADR-0080's flag that no after-pass x64 result existed on the 7763 no longer blocks NFR-01.

## Alternatives considered

- **Keep the text** — NFR-01 on x64 depends on the pool's draw; a run on another model proves nothing either way.
- **Per-model floors** — needs a baseline per model before any run on it can pass, the same gap as NFR-05's `first`; the user chose one floor per arch.
