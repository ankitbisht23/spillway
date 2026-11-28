# 0107. Bench gate: a committed exclusion list for outlier baselines

- **Status:** Accepted
- **Date:** 2026-11-28
- **Requirement IDs:** NFR-05

## Context

- **The outlier.** The arm64 nightly on `main` at 6f780de ([run 45997114752](https://github.com/ankitbisht23/spillway/actions/runs/45997114752), scheduled, started 12:15 UTC on 2026-11-27) stored `results/arm64/20261127T121534Z.json`: valid, verdict `pass`, 6,300 rps, p99 median **3.171 ms** with a run-to-run spread of **28.4%**, on Neoverse N2 (`implementer 0x41 part 0xd49`). The gated arm64 run set on `main` before it (a69b856, [run 45911766126](https://github.com/ankitbisht23/spillway/actions/runs/45911766126), `results/arm64/20261126T233501Z.json`) had 3.973 ms (spread 22.8%). Every arm64 run set stored after it, on the same CPU model, target rate and setup, had 4.17–4.47 ms: m8-verify 160099a 4.167 and 4.467 ms, and `main` d3a734b ([run 46122432047](https://github.com/ankitbisht23/spillway/actions/runs/46122432047)) 4.375 ms, flagged at +38.0%, with a confirming re-run of 4.339 ms at **+36.8%**, so verdict `fail`. Throughput was 6,300 rps in all of them.
- **Same gateway request path.** Between 6f780de and d3a734b the gateway's request path did not change: the only change under `internal/proxy` is an exported `TrustedProxies.Trusts` wrapper used by the control plane, and the rest is control-plane, demo and UI code. `cmd/loadgen`'s generator moved to `internal/loadgen` and was generalised for `cmd/demo-traffic` (ADR-0096 §1). Earlier arm64 run sets at 6,300 measured with the old generator ranged over 3.867–5.219 ms (ADR-0088's known limit and the m7-review-fixes run set), so the later 4.17–4.47 ms sit inside that range. The 3.171 ms is the value outside it.
- **Why it blocks the gate.** The nightly baseline is the newest eligible result (ADR-0003, ADR-0081), and only passing gated runs on `main` become eligible. So every later arm64 run set is compared with 3.171 ms, and it is flagged unless its p99 is at most 3.171 × (1 + 0.284) ≈ **4.07 ms**. A flagged or failed run set never becomes a baseline. Every arm64 gate stays blocked until a runner happens to land at or below about 4.07 ms, and no run set since the outlier has.
- **Why this happens.** ADR-0088 records that arm64 p99 varies between runners: the gate's tolerance is the spread inside one run set on one runner, while comparisons cross runners. The deferred NFR-05 gate-design item (ADR-0091 item 15) is about that rule. It **stays deferred**: this ADR does not change the rule.
- **The user's decision** (2026-11-28, during M8's verify step, "Exclude outlier (Recommended)"): add a committed exclusion list to the bench gate so that a named stored run set can never be a baseline, and list this run set on it with its reason.

## Decision

1. **The list.** `test/load/bench-baseline-exclusions.json`, next to `bench.sh`, holds `{"exclusions": [...]}`. Each entry has `arch` (`x64` or `arm64`), `path` (the stored name, `results/<arch>/<YYYYMMDDTHHMMSSZ>.json`), `reason` and `adr` (`ADR-NNNN`, the record that justifies it). Its first entry is `results/arm64/20261127T121534Z.json`, reason as in the Context, `ADR-0107`. An entry is added or removed only by a PR with an ADR, like any other change to the gate.
2. **What it changes.** `cmd/benchgate -exclusions <file>` treats a listed run set as not eligible. So it is never the nightly baseline, the release baseline or the P34 information-only baseline (ADR-0081, ADR-0083), and never one of the "other CPU models" the summary names for `first`. The newest eligible run set that is not listed is used instead. Nothing else changes: the 10% and spread rule, the one confirming re-run, ADR-0003's "only passing gated runs on `main` become baselines", and the stored files on `bench-results`. That branch is not edited. The excluded file stays there with its `pass` verdict.
3. **The job summary and the stored result.** When the gate skips a listed run set that it would otherwise have considered (stored before the candidate, eligible apart from the list, and comparable with the candidate, or, for verdict `first`, comparable apart from its CPU model), the summary prints one line per run set: `Skipped excluded run set <path>, commit <sha>, <timestamp>: on the committed exclusion list (<ADR>), so never a baseline. Reason: <reason>`. The stored `gate` field lists those paths in a new optional `excluded` array, which is left out when empty. Results without a skipped run set are stored exactly as before.
4. **Fail loudly.** A list that cannot be read, is not JSON, has trailing data, has no `exclusions` array or an unknown key, an entry whose path is not a stored name of its arch, an empty reason, a malformed ADR id or a duplicate path, or an entry **of the candidate's arch** that names no stored result, is bad input. benchgate exits 2, bench.yml's gate step fails, and the run set is not stored (ADR-0081 choice 7). A typo can therefore not leave the outlier as a baseline unnoticed. Entries of the other arch are checked for form only, because each job loads only its own arch's history. `-exclusions` together with `-ungated` is also bad input. Without `-exclusions` the gate behaves exactly as before.
5. **Who passes it.** `bench.yml` passes `-exclusions test/load/bench-baseline-exclusions.json` to the gate step (when gated) and to the re-run step, and `make bench-gate` does the same. A unit test parses the committed list and checks that each entry's ADR file exists.
6. **Left as they are.** `make verify-m1`'s NFR-01 baseline lookup and `make verify-m7`'s list of CPU models with `main` baselines read `bench-results` with their own `jq` filters (ADR-0081 choice 12, ADR-0089). Neither is the gate: the excluded run set's throughput (6,300 rps) is a valid NFR-01 measurement, and arm64 has one CPU model. So they do not read the list.

### Proposed wording (P55, for the user; docs/design.md is not changed by this task)

- **(P55) Benchmark method, step 4:** after "the baseline is the newest result for the same arch from a passing run on `main`", add: "except a run set listed, with its reason and ADR, in the committed exclusion list `test/load/bench-baseline-exclusions.json` (ADR-0107)".

## Consequences

- The next gated arm64 run on `main` is compared with a69b856's run set (3.973 ms, spread 22.8%), so it is flagged only above about 4.88 ms. Gated again offline against the current `bench-results` with the list, d3a734b's two arm64 run sets get `pass` (p99 +10.1% and +9.2%, within the 22.8% spread). Without the list they are flagged at +38.0% and +36.8%. The x64 newest result is unchanged (`pass`, nothing skipped).
- If a later nightly passes at an outlying low p99 again, the same block can recur. Listing it needs a new PR and ADR. The gate-design item in ADR-0091 item 15 (where the tolerance comes from) is the lasting fix and stays deferred for the user.
- Deleting or rewriting `bench-results` so that a listed arch's file is gone makes that arch's gate fail with exit 2 until the entry is removed. This is intended: the list must name real run sets.

## Alternatives considered

- **Rewrite the excluded file's verdict on `bench-results`** (for example to `ungated`): this edits stored history by hand, nothing in `main` records why, and the user ruled out editing `bench-results`.
- **Wait for a run at or below about 4.07 ms:** none of the four arm64 run sets since the outlier came close, so arm64 NFR-05 could stay blocked indefinitely.
- **Change the gate rule** (median of the last N baselines, cross-runner tolerance, and similar): that is the deferred NFR-05 gate-design item, a rule change the user has not decided.
- **Silently skip unknown entries:** a mistyped path would leave the outlier as the baseline with no sign of it.
