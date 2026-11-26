# 0089. NFR-05: x64 NOT DEMONSTRATED on a CPU model with no main baseline

- **Status:** Accepted
- **Date:** 2026-11-26
- **Requirement IDs:** NFR-05

## Context

NFR-05 compares a run only with baselines of the **same recorded CPU model** (design.md, ADR-0073 §D8, ADR-0081). verify-m7 reads the bench gate's final verdict per arch, and ADR-0074 and ADR-0088 count `first` (nothing comparable) as a FAIL.

The first three full verify-m7 runs (2026-11-26, 04:29, 05:58 and 07:26 UTC; ADR-0088) failed NFR-05 with `first` on both arches. `bench-results` held no gated result from `main` because the 03:17 UTC nightly never ran. GitHub's x64 runners rotate between several CPU models: those three runs landed on AMD EPYC 9V45, Intel Xeon 6975P-C and AMD EPYC 7763. arm64 has shown one model, Neoverse N2.

So even with a gated `main` baseline in place, an x64 dispatch lands on a model that has no baseline yet whenever the pool hands it one that no `main` run has met. That run's gate has nothing to compare, and nothing a verify run does can change that. A `first` like this says nothing about whether the code regressed.

The user decided on 2026-11-26:

1. A gated `bench.yml` run on `main` creates the main baselines for both arches (run 45787833386, dispatched 08:59 UTC by the main loop).
2. verify-m7 adds the rule below.

## Decision

verify-m7's NFR-05 line, per arch, keeps ADR-0088's reading (the final verdict: the re-run's when the first set was flagged, otherwise the first set's), with one exception:

- **x64 NOT DEMONSTRATED.** An x64 final verdict of `first` is printed as **NOT DEMONSTRATED** when both of these hold:
  1. no eligible baseline on the runner's CPU model was stored before the result;
  2. at least one other CPU model has one.

  "Eligible baseline" is benchgate's own test (ADR-0081): a result from `refs/heads/main` that is valid, with gate verdict `pass` or `first` or no gate, and comparable apart from the model (same arch, same target rate, same whole `setup` object).

  The line prints the runner's CPU model and the models that do have baselines, for example: `x64 first on CPU AMD EPYC 7763, no main baseline on this CPU model; NOT DEMONSTRATED: main baselines exist only on Intel Xeon 6975P-C (ADR-0089)`. P34's cross-model figures follow when the gate stored them. The script reads the models from the `bench-results` files it already downloads (`main_models` in test/verify/m7.sh).
- **Row verdict.** When x64 is NOT DEMONSTRATED and arm64 passes, the NFR-05 row is `NOT DEMONSTRATED`. It is **not counted** in the exit status, like NFR-02's known limit. The exclusion is printed after the rows and written to the README block ("NFR-05 is not counted: …, ADR-0089"). It is never shown as PASS.
- **Everything else counts as before:**
  - Any x64 verdict against a same-model baseline counts normally: `pass` is PASS, `fail` is FAIL.
  - An x64 `first` counts as FAIL when no CPU model has a main baseline, or when the runner's own model has one (then the `first` comes from something other than the model, such as a changed setup or rate).
  - `invalid` and a missing result are FAIL.
  - arm64 must compare against main's baseline and pass. An arm64 `first`, a confirmed `fail` (flagged, then confirmed by the re-run) or `invalid` is FAIL.
  - When arm64 fails, the row is FAIL and counted. The x64 part still reads NOT DEMONSTRATED in the measured text.

**design.md is unchanged.** NFR-05's row already says "same recorded CPU model only". A run on a model with no baseline therefore has no comparison under the requirement as written. This ADR decides only how the M7 exit evidence treats that case, as ADR-0084 §4 did for NFR-02. No P-item is needed.

## Consequences

- An x64 dispatch that lands on an unseen model no longer fails the M7 exit by itself. NFR-05 is still demonstrated on arm64 against main's baseline. On x64 it is demonstrated whenever the dispatch lands on a model with a main baseline.
- The x64 gap from ADR-0081 stays open: a run on a new model is not compared that night. The nightly and later `main` runs build chains for the models that recur. Each one shrinks the set of models that give NOT DEMONSTRATED.
- verify-m7's exit can now leave out two rows (NFR-02 and, on some runs, NFR-05). Each exclusion is printed with its ADR.
- Tests: `TestReportNFR05` in test/verify/m7_test.go covers each case above from fixture files.

## Alternatives considered

- **Keep `first` as FAIL (ADR-0074).** Whether a full run passes would then depend on which x64 model GitHub assigns, not on the code. The user chose against it.
- **Dispatch until x64 lands on a model with a baseline.** That is a re-run rule, which this run never adds.
- **Count the cross-model comparison (P34) as the verdict.** That changes NFR-05's rule ("same recorded CPU model only") and needs design.md changed. P34 stays information only.
