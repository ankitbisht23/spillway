# 0003. Benchmark results on a `bench-results` branch

- **Status:** Accepted
- **Date:** 2026-11-02
- **Requirement IDs:** NFR-01, NFR-05

## Context

Benchmarks run on GitHub Actions (see docs/design.md, Benchmark method). NFR-01 needs a recorded throughput baseline from M1; NFR-05 needs a regression gate that compares each run with the previous baseline from M7. Both need result history that a workflow can write to. `main` is protected by a ruleset that requires a PR, so a scheduled job cannot commit there. Runs happen on two architectures, x64 (`ubuntu-latest`) and arm64 (`ubuntu-24.04-arm`), whose numbers are not comparable with each other; hosted runners of one architecture can also land on different CPU models. A gate that only compares with last night’s baseline lets a run of small, sub-threshold drops add up unnoticed.

## Decision

- `.github/workflows/bench.yml` writes one JSON file per run to the `bench-results` branch at `results/<arch>/<UTC-timestamp>.json`, with `<arch>` `x64` or `arm64`. The branch is unprotected (the ruleset targets `main` only) and the workflow creates it on its first run. Each file records the commit SHA and the runner's hardware, including the CPU model.
- Each architecture has its own baseline: the newest result for that architecture from a passing run on `main`. The gate compares a run only with the baseline of the same architecture.
- The gate also compares each run against the last release’s baseline (the newest passing result for the commit of the last release tag), which catches slow creep that no single night shows. Comparisons are made only between results with the same recorded CPU model; when no baseline with that model exists, the run is stored without a comparison and becomes that model’s first baseline. A run whose load generator missed 99% of its target rate is invalid: stored, marked failed, never compared.
- The gate flags a run when the median of its 5 runs shows a > 10% drop in throughput or rise in p99 and the change is larger than the baseline's run-to-run spread; a flagged run re-runs once and fails only if the re-run confirms it, because shared runners can swing close to 10% on their own (docs/design.md, Benchmark method). A failing run is stored and marked as failed, so a regression never becomes the next baseline.
- README benchmark tables are updated by an ordinary PR that cites the result files, never by the workflow.
- M1: `bench.yml` with a manual trigger only; the first run records the baseline (NFR-01). M7: nightly schedule and the regression gate (NFR-05).

## Consequences

- Result history lives in git: browsable, diffable and never expiring. Each run adds a few kilobytes per architecture.
- A change of runner CPU model resets comparisons for that model; until a release has run on it, only the nightly baseline applies.
- The bench job needs `contents: write`, so it runs only from the manual trigger and the schedule, never on `pull_request` events.
- `bench-results` can be rewritten by anyone with write access. Acceptable for a solo repository; agent sessions cannot force-push (`.claude/settings.json`).

## Alternatives considered

- **Workflow artifacts only** — they expire (90 days at most), so there is no long-term history or stable baseline to compare against.
- **Bot PRs into `main`** — a PR to review or auto-merge every night: noise in the PR list and in `main`'s history.
- **Committing to `main`** — blocked by the ruleset; a bypass would need a privileged token and would weaken protection for everything else.
