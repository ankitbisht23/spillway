# 0081. Bench gate: implementation details and CPU-model rotation

- **Status:** Accepted
- **Date:** 2026-11-25
- **Requirement IDs:** NFR-05, NFR-01

## Context

- **ADR-0073 §D8** sets the NFR-05 gate: `bench.yml` nightly at 03:17 UTC with the gate, `workflow_dispatch` with a `gate` input (default false), `cmd/benchgate`, comparable results (same arch, `hardware.cpu_model`, `target_rps` and `setup`), nightly and release baselines from gated, valid results on `main` (or M1-era results with no `gate` field), the > 10% and > spread rule, one confirming re-run in the same job, the stored `gate` field, a 100-minute timeout, and fixture tests. ADR-0003 sets where results live and that branch results never become baselines. This ADR records the small choices D8 left open and reports what the x64 runners' CPU-model rotation means for NFR-05.
- **ADR-0030** (M4, accepted): "M7's regression gate compares only results with the same `trace_sample_ratio`." D8's list of setup fields (gateway vCPUs, CPU sets, URL, protocol, warm-up, duration, access-log rate) omits it; `setup` in every result since m4-tracing carries it.
- **ADR-0080's flag:** GitHub's x64 `ubuntu-latest` pool rotates CPU models. The x64 M1 baseline and main's capacity run on 2026-11-25 were on the AMD EPYC 7763; the perf branch's runs the same day landed on an AMD EPYC 9V45 and an Intel Xeon Platinum 8370C. The x64 image also moved from Ubuntu 24.04.5 to 26.04.1. arm64 (`ubuntu-24.04-arm`) has shown one model so far (Neoverse N2, `implementer 0x41 part 0xd49`).
- **P20** (ADR-0068, approved at the checkpoint) gives step 4 of docs/design.md's Benchmark method its new text; this task applies it.

## Decision

### Implemented as D8 states

No rule of D8 is changed. `cmd/benchgate` (`gate.go` the rules, `summary.go` the job summary, `main.go` flags and files) with `make bench-gate FILE=<result.json>`; `bench.yml` gains the schedule, the `gate` input, the gate and re-run steps, and the 100-minute timeout. docs/design.md takes P20's step-4 text and the M7 box is ticked.

### Small choices

1. **Setup** is compared as the whole `setup` object (deep equality, absent fields included), so `trace_sample_ratio` counts as ADR-0030 requires and any field bench.sh adds later counts too. Consequence: the M1 baselines (1310f0c, measured before m4-tracing, no `trace_sample_ratio`) compare with no result measured since, so the **first gated nightly on `main` gets `first` on each arch** and starts the chain.
2. **Ungated results carry `gate: {"verdict": "ungated", …}`** (no comparisons, null baselines). D8 says ungated dispatches never become baselines, while results with no `gate` field (the M1 era) may; the marker keeps the two apart. Every result that `bench.yml` stores from now on has a `gate` field.
3. **Only results stored before the candidate** (by stored file name, which is its UTC timestamp) can be its baselines, so a stored result gated again locally is never its own baseline and never compared with later ones. In CI the candidate is always the newest, so this changes nothing there.
4. **An unrecorded CPU model** (`""`, as on a laptop without `/proc/cpuinfo`) compares with nothing: verdict `first`.
5. **Comparisons** are stored as `{against: "nightly"|"release", metric: "achieved_rps_median"|"p99_us_median", base, value, change, spread, flagged}` with `change = (value − base) ÷ base`, so a throughput drop is negative. The boundary is strict: exactly 10% is not flagged.
6. **The re-run** is judged against the same baselines because the job fetches `bench-results` once, before the first run set. `-rerun-of result.json` records the first set's stored name in `rerun_of`; a confirmed regression (`fail`) also rewrites the first set's verdict to `fail`. A re-run that is invalid gets `invalid` and fails the job; the first set stays `flagged` (never a baseline). A re-run whose `bench.sh` crashes leaves the first set stored as `flagged` and the job failed.
7. **Bad input** (exit 2: a corrupt stored result, a missing file, bad flags) fails the job before the push, so the run set is not stored. Storing it without a verdict would make it look like an M1-era baseline; a corrupt file on `bench-results` must be fixed by hand.
8. **The release tag** is the newest `v*` tag by `sort -V` from `git ls-remote --tags origin`, with its peeled commit for an annotated tag. GNU `sort -V` places a pre-release such as `v1.1.0-rc1` after `v1.1.0`; ADR-0076 defines only `v1.0.0`, so this is noted, not handled.
9. **Fetching** `bench-results` and the tags needs no token (the repository is public); the write token stays in the push step. A network error fails the step; only a missing branch means an empty history.
10. **Stored files** keep every field bench.sh wrote plus `gate`; top-level keys come out in name order (Go's `encoding/json`), nested values unchanged.
11. **The job summary** (also printed to the log) names the run's file, commit, ref, target and CPU model; then either "Compared on CPU model: **⟨model⟩**" or "Compared on CPU model: none. **first**", plus the CPU models that do have baselines for the same arch, target and setup; the nightly and release baselines (or why the release one is not available); the comparison table; the verdict; and whether the run set becomes a baseline.
12. **`make verify-m1`**'s baseline lookup follows D8's eligibility (valid, on `main`, verdict `pass` or `first` or no `gate` field), so a flagged or failed nightly is never shown as the NFR-01 baseline.

### CPU-model rotation and NFR-05 coverage

D8 compares only results with the same recorded CPU model, and GitHub picks the x64 model per job. What follows from the rule as written:

- **Start.** Because of choice 1, the first gated nightly on `main` is `first` on both arches whatever its model, and becomes that model's baseline.
- **x64 nights without a comparison.** A nightly on a model that no earlier gated `main` night had gets `first`: stored, job passes, and on `main` it becomes that model's first baseline. With three models seen, at least three x64 nights pass before every model has a chain, more if the pool is uneven or adds models. A model's nightly baseline can be days old when the next night lands on it; that still catches a step regression, but compares across several days of commits.
- **A regression that lands on a `first` night is not caught that night, and is absorbed.** It becomes that model's baseline, so later nights on the same model compare against the regressed numbers. It is caught only by another model's chain whose baseline predates it, or by the release baseline when the release's run used that model.
- **The release baseline** exists only for the CPU models on which a gated `main` run of the release commit landed; after a release, nights on other models have the nightly comparison only.
- **The runner image** (24.04.5 → 26.04.1) is not part of comparability (D8 compares model, target and setup, not OS or kernel). An image change on the same model is compared and may flag or clear a run by itself; the stored `hardware.os` and `hardware.kernel` let a reader tell.
- **verify-m7's NFR-05 line** (ADR-0074) counts `first` as FAIL; a dispatch that lands on a model without a `main` baseline fails that line, and the line prints the model.
- **arm64** has shown one model, so after its first night every arm64 night is compared.

**P34 (proposal for the user, not applied).** To narrow the gap without changing the pass rule: when a gated run gets `first`, the job summary and the stored `gate` also show an **information-only** comparison against the newest eligible baseline of the same arch, target and setup on another CPU model (never flagged, never failing, never making or blocking a baseline). Alternatives for the user: keep D8 as it is (nights on new models are uncompared until they recur), or a rule change, such as "a `first` result on `main` becomes a baseline only after a second gated run on the same model passes against it", which would delay new chains by a night per model but stop a regression from being absorbed. Each of the latter changes NFR-05's rule and needs the user.

## Consequences

- From the night after merge, `bench.yml` runs at 03:17 UTC on `main`, gates both arches and stores two to four files a night on `bench-results`.
- The first night is `first` on both arches; arm64 is compared from the second night, x64 once each recurring model has had a night.
- A flagged night costs one more run set (about 20 minutes) on that runner.
- `make bench-gate FILE=…` gives the same verdict and summary locally, against `origin/bench-results` and the newest local `v*` tag.

## Alternatives considered

- **Comparing only D8's listed setup fields** — would compare traced results with the untraced M1 baselines, against ADR-0030.
- **Ungated results without a `gate` field** — they would look like M1-era results and could become baselines, against D8.
- **Storing a run set whose gate errored, without a verdict** — same problem as above.
- **Re-fetching `bench-results` before the re-run** — the other arch's push could not change this arch's baselines, but one fetch makes "the same baselines" hold by construction.
- **The gate in shell and jq** — rejected by D8; Go is unit-tested.
