# 0088. make verify-m7: implementation details

- **Status:** Accepted
- **Date:** 2026-11-26
- **Requirement IDs:** NFR-01 – NFR-15

## Context

ADR-0074 sets the verify-m7 plan: one PASS/FAIL line per NFR row, each row's source, load shape, budget and pass rule, the order and total time of a full run, the stall tolerance, the results JSON and the README block. Later ADRs changed some rows before the verify task:

- **NFR-01** is judged at the per-arch floors (P19, ADR-0080) on any CPU model (ADR-0082). With P19 approved, ADR-0074's table drops the separate NFR-01 dispatch: the NFR-05 run at the floors serves both.
- **NFR-05** is the bench gate's verdict (ADR-0081). `first` is a FAIL (ADR-0074).
- **NFR-02** is an open known limit (ADR-0084 §4). On 2026-11-25 the user decided that verify-m7's NFR-02 line reads the newest scaling results on `bench-results` and prints them honestly. It is never shown as PASS unless the rule is really met. It is left out of the overall exit status only because the user accepted the limit, and that exclusion is printed and written to the README.
- **NFR-14**'s rolling restart runs on kind through `make kind-rollout` (ADR-0086). verify-m5 has the (c6) staleness-alert line (ADR-0087).
- The M7 load and chaos scenarios have make targets (ADR-0083).

This ADR records the choices that ADR-0074 left to the verify task. No pass rule changes.

## Decision

### Entry points

- `make verify-m7` runs `test/verify/m7.sh` (the full run, `all`). `make verify-m7 README=1` also rewrites the README block. `make verify-m7 PARTS="…"` runs a subset (`bench`, `compose`, `kind`, `chaos` or NFR numbers), which is never exit evidence and lists only the rows it ran.
- `test/verify/m7.sh readme <json>` rewrites the README block from a full run's JSON. `test/verify/m7.sh report <dir>` prints the rows and rewrites `<dir>.json` from a run's saved logs and state. The verify task commits the README block from its last full run this way, after the run and without editing the block by hand.
- Each run writes `test/verify/out/m7-<UTC timestamp>.json` and a directory of the same name. The directory holds every step's full log and exit code, `steps.tsv` (time and budget per step), `state.env` (what was run and the bench run) and the `bench-results` files it read. `test/verify/out/` is git-ignored.

### Rows

1. **NFR-01 and NFR-05 come from one gated dispatch**, `gh workflow run bench.yml --ref <branch> -f gate=true`, sent at the start. The run is found by its `run-name` (`bench throughput x64=4600 arm64=6300 gate=true`), head SHA and creation time. `VERIFY_M7_BENCH_RUNS=<id>` reuses a run already dispatched for HEAD; the script checks its head SHA and name first. Results are matched by `run_url`.
   - **NFR-01** per arch reads the first run set, not the re-run. PASS when it is valid, its target is the floor, and its median per gateway vCPU, rounded, is at least the floor (x64 4,600, arm64 6,300 since ADR-0090; 8,800 before), on any CPU model. The line prints each model.
   - **NFR-05** per arch reads the final verdict: the re-run's if the first set was flagged, else the first set's. PASS when that verdict is `pass` and a nightly baseline was compared (ADR-0089 adds one exception for x64). The line prints the comparisons, and P34's cross-model figures when present.
   - If the run stores no result for an arch, or does not finish within 120 minutes of its dispatch, that arch fails both rows.
2. **NFR-02** dispatches nothing. Per arch it reads the newest stored result with `setup.mode == "scaling"`, and judges it as `scaling.jq` and `scaling.sh` did when they stored it.
   - The row is PASS (and counted) only when both arches are valid and pass.
   - Otherwise its verdict is **KNOWN LIMIT** and it is not counted. Each arch is printed as INVALID with its would-be S and its probes that were not gateway-bound, or as FAIL with its S.
   - The output, the JSON (`excluded`) and the README block state that NFR-02 is left out of the exit status only because the user accepted it (ADR-0084 §4).
3. **Rows that share a check** (verify-m3 for NFR-04, NFR-06 at 3 replicas and NFR-13; verify-m5 for NFR-08, NFR-09 and NFR-12) take the PASS/FAIL lines under the check's own `== ` section header. A FAIL under a setup header fails every row of that step. A step that exits non-zero without printing a FAIL line fails its rows. So a stall that fails verify-m3's full-outage line fails NFR-13 only, which is the honest result.
4. **NFR-06** combines verify-m3's accuracy lines with `make accuracy REPLICAS=1` and `REPLICAS=5`. **NFR-07** uses `make accuracy REPLICAS=1|3|5 MODE=batched`. Each line prints the maximum over-admission per pattern and the boundary pattern's 503 share. The 503s are a known limit (ADR-0079) and are not judged.
5. **NFR-14** is one row covering three steps. It passes only if verify-m1's reload checks pass, every `make kind-smoke` check passes, and every `make kind-rollout` check passes.
6. **The chaos suite's three extra scenarios** are rows of their own, counted like any NFR row: Redis +50 ms (`make chaos-redis-latency`), gateway kill −9 (`make chaos-gateway-kill`) and an upstream endpoint answering 5xx (`make verify-m4`). The other chaos scenarios are already inside NFR-12, NFR-13, NFR-14 and NFR-15.
7. **The exit status** is PASS when every counted row is PASS. It is printed as `M7 exit: PASS` or `M7 exit: FAIL (<n> rows)`.

### Running

8. **Preflight** applies only when the bench part runs. It requires all of the following, otherwise NFR-01 and NFR-05 fail with the reason and nothing is dispatched:
   - the tree is clean apart from the three local files ADR-0074 names (`.claude/settings.json`, `docs/m1-code-guide.md`, `dump.rdb`);
   - `gh api user` is the repository's owner;
   - `origin/<branch>` is HEAD.
9. **Budgets** are ADR-0074's, per step: accuracy 4 min per run, the three chaos steps 3 + 3 + 2 min, the kind part 25 min, the full run 150 min. A step over its budget prints `OVER BUDGET`. That is reported, not failed, as ADR-0074 says.
10. **The kind part** first checks for kind's node containers by label (`io.x-k8s.kind.cluster=spillway`). If a cluster already exists, NFR-14 fails with that reason and the cluster is left alone. Otherwise the script:
    - prints that it is stopping compose for memory, then runs `docker compose down` without `-v`;
    - runs `make kind-up`, `kind-smoke`, `kind-rollout` and `kind-down`;
    - runs `make dev`.
11. **Cleanup on every exit:**
    - `deploy/compose/gateway/config.yaml` and `identity.yaml` are put back if they differ from the start;
    - `make kind-down` runs if this run's cluster is still up;
    - after any compose step, gateway-2 … gateway-5 and toxiproxy's `redis-0` … `redis-5` proxies are removed, and `make dev` runs. That recreates gateway-1 in static mode at compose's default trace ratio, as a plain `make dev` leaves it.
12. **No re-run anywhere.** Each full run sends its own dispatch, and a failed line stays failed.

### Tests

`test/verify/m7_test.go` builds run directories from fixture logs and `bench-results` files, runs `m7.sh report` on them and checks every verdict:

- a full passing run with NFR-02 INVALID: KNOWN LIMIT, not counted, with the exclusion note;
- one FAIL in a verify-m3 section failing only its row;
- NFR-05 `first`;
- an invalid arm64 NFR-01 run set;
- no bench run;
- a verify-m5 setup failure;
- batched over-admission above its bound;
- a crashed step;
- kind not brought up;
- a step not run;
- NFR-02 valid and passing (PASS, counted) and valid and failing (still KNOWN LIMIT);
- the README rewrite: idempotent, the rest of the file kept, a partial run refused.

## Measured on the branch (2026-11-26)

Nine full runs of `make verify-m7` on m7-verify, in three sets of three, one after another. None was re-run, and no rule was changed between runs. Every step was within its budget, and each run took 87–89 minutes of its 150. Only the last set is the M7 exit evidence; the first two are listed because each one led to a user decision.

| Set | Commit | Rule change before the set | Runs (UTC start) | Rows that failed | Exit |
| --- | --- | --- | --- | --- | --- |
| A | 067dbd2 | none | 04:29, 05:58, 07:26 | NFR-05 `first` on both arches (all 3); NFR-04 in run 3 | 0 of 3 |
| B | 58e8641 | ADR-0089 (x64 NOT DEMONSTRATED), gated `main` baselines | 10:09, 11:37, 13:06 | run 1 NFR-04 + chaos Redis +50 ms; run 2 NFR-05 arm64; run 3 NFR-01 + NFR-05 arm64 invalid, NFR-13 | 0 of 3 |
| C | 390f3f4 | ADR-0090 (arm64 rate and floor 6,300), gated `main` arm64 baseline at 6,300 | 15:33, 17:01, 18:29 | run 1 NFR-05 arm64 | **2 of 3** |

### Set C: the exit evidence (390f3f4, arm64 at 6,300)

The `main` baseline for arm64 at 6,300 is [run 45846586871](https://github.com/ankitbisht23/spillway/actions/runs/45846586871) (9a2d3ed, `refs/heads/main`, valid, verdict `first`, p99 median 4.207 ms, spread 0.111; x64 on AMD EPYC 9V74 `pass` against main's earlier 9V74 result).

| Run | Bench run | NFR-01 x64 (CPU, p99) | NFR-01 arm64 p99 | NFR-05 arm64 | NFR-05 x64 | Every other counted row | Exit |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1, 15:33 | [45850229009](https://github.com/ankitbisht23/spillway/actions/runs/45850229009) | PASS (AMD EPYC 9V45, 2.261 ms) | PASS, 4.679 ms | **FAIL**: flagged p99 +11.2% (4.679 vs 4.207 ms, above 10% and the baseline spread 0.111), confirming re-run 5.219 ms, +24.1% | NOT DEMONSTRATED (9V45) | PASS | FAIL (1 row) |
| 2, 17:01 | [45864041725](https://github.com/ankitbisht23/spillway/actions/runs/45864041725) | PASS (Intel Xeon 6975P-C, 1.875 ms) | PASS, 4.279 ms | PASS, p99 +1.7% | NOT DEMONSTRATED (6975P-C) | PASS | PASS |
| 3, 18:29 | [45876358123](https://github.com/ankitbisht23/spillway/actions/runs/45876358123) | PASS (Intel Xeon 6975P-C, 1.666 ms) | PASS, 4.395 ms | PASS, p99 +4.5% | NOT DEMONSTRATED (6975P-C) | PASS | PASS |

**Full-run pass rate over the exit runs: 2 of 3.** In runs 2 and 3, NFR-05 was printed NOT DEMONSTRATED and not counted (ADR-0089); NFR-02 was KNOWN LIMIT and not counted in all three (ADR-0084 §4). Main's x64 baselines were on AMD EPYC 9V74, Intel Xeon Platinum 8573C and 8370C; no set C dispatch landed on one of them.

| Row | Run 1 / Run 2 / Run 3 |
| --- | --- |
| NFR-02 (KNOWN LIMIT) | newest scaling results, run 45661905238 (d488f06): INVALID on both arches, would-be S x64 2.013, arm64 1.517 |
| NFR-03 added p99 | 0.042 / 0.134 / 0.209 ms |
| NFR-04 added p99 | 0.305 / 0.338 / 0.344 ms |
| NFR-06 max over-admission | 0 at 1, 3 and 5 replicas |
| NFR-07 max over-admission | 3 / 3 / 4 of 10 at 1 replica; 17 / 13 / 14 of 30 at 3; 3 / 3 / 2 of 50 at 5 |
| NFR-08 p99 | 8 / 9 / 7 ms |
| NFR-09 max | 53 / 55 / 57 ms |
| NFR-10 GCRA | 117.0 / 117.7 / 117.3 B per key at 1M keys |
| NFR-10 sliding window | 156.2 / 156.1 / 156.2 B per key at 1M keys |
| NFR-11 | 0 gateway-caused 5xx of 1,260,000 requests |
| NFR-12 | (c1) 0 failed of 347,909 / 347,903 / 347,930; (c6) alert at +76 / +76 / +90 s |
| NFR-13 | 12 of 12 checks |
| NFR-14 | reload 59,999 of 59,999 (1 not sent) / 60,000 / 60,000 with 29 swaps; kind smoke 21 of 21; rolling restart 0 failed of 72,000, complete in 3 / 3 / 7 s |
| NFR-15 median ratio | 0.413–0.414 / 0.411–0.418 / 0.455–0.457 |
| Chaos | Redis +50 ms 10 of 10, gateway kill 3 of 3, upstream 5xx 8 of 8 |

The boundary pattern's 503 share was 48–61% strict and 39–55% batched (known limit, ADR-0079). One more gated arm64 set at 6,300 exists on 390f3f4: [run 45849990777](https://github.com/ankitbisht23/spillway/actions/runs/45849990777), dispatched by a verify attempt that was stopped before its compose part, `pass` at p99 4.247 ms. It is data, not a verify run.

### Known limit: arm64 p99 varies between runners (NFR-05)

All six gated arm64 sets at 6,300 so far ran on the same recorded CPU model (Neoverse N2) and the same code apart from verify-only files: p99 medians 4.207 (main baseline), 4.247, 4.679, 5.219 (run 1's re-run), 4.279 and 4.395 ms. Throughput was 6,300 in every run. Inside one set, the baseline's run-to-run spread was 0.111; between sets, the p99 median ranged over 24%. The gate's tolerance is the baseline's spread **inside its own run set** (one runner), while each comparison is **across runners**. The confirming re-run runs in the same job, on the same runner, so it confirms the runner as much as the code. A slower runner can therefore flag and confirm a p99 rise that is not a code change, as in run 1: its first set and its re-run were both slower than the baseline's runner (the re-run more so).

On 2026-11-26 the user decided: no rule change; run 1 stays a FAIL in the pass rate; the between-runner variance is recorded as an NFR-05 known limit (here and in the README's Known limits). The gate's design (where its spread comes from) is carried to the M7 review as a proposal for the user, not changed here.

### Set B (58e8641, arm64 at the old 8,800 rate)

| Run | Bench run | Failed rows |
| --- | --- | --- |
| 1, 10:09 | [45798620152](https://github.com/ankitbisht23/spillway/actions/runs/45798620152) | NFR-04: no three valid pairs, 276 Redis errors in one burst of 2,040 ms (over ADR-0034's 1,000 ms); chaos Redis +50 ms: 8 of 10, Redis did not decide again within 5 s after the latency was removed. NFR-05 passed on both arches that run (x64 against main's AMD EPYC 9V74 baseline). |
| 2, 11:37 | [45812246931](https://github.com/ankitbisht23/spillway/actions/runs/45812246931) | NFR-05 arm64: confirmed fail, p99 105 ms then 315 ms on the re-run (baseline 7.9 ms) |
| 3, 13:06 | [45825996442](https://github.com/ankitbisht23/spillway/actions/runs/45825996442) | NFR-01 and NFR-05 arm64: invalid run set (runs reached 8,604 and 8,702 of 8,800); NFR-13: 11 of 12, one decision over the 10 ms full-outage line |

The arm64 failures on the same commit (pass, confirmed fail, invalid) led to ADR-0090. The chaos Redis +50 ms failure did not recur in the six runs after it. NFR-04 and NFR-13 are verify-m3's lines failing on laptop stalls (ADR-0027, ADR-0034); they count as FAIL.

### Set A (067dbd2, before ADR-0089)

| Run | Bench run | NFR-01 (x64 CPU) | NFR-05 | NFR-04 | Every other counted row | Exit |
| --- | --- | --- | --- | --- | --- | --- |
| 1, 04:29 UTC | [45755559828](https://github.com/ankitbisht23/spillway/actions/runs/45755559828) | PASS (AMD EPYC 9V45) | FAIL: `first` on both arches | PASS, 0.477 ms | PASS | FAIL (1 row) |
| 2, 05:58 UTC | [45765073716](https://github.com/ankitbisht23/spillway/actions/runs/45765073716) | PASS (Intel Xeon 6975P-C) | FAIL: `first` on both arches | PASS, 0.273 ms | PASS | FAIL (1 row) |
| 3, 07:26 UTC | [45775387596](https://github.com/ankitbisht23/spillway/actions/runs/45775387596) | PASS (AMD EPYC 7763) | FAIL: `first` on both arches | FAIL: no three valid pairs | PASS | FAIL (2 rows) |

- **NFR-05.** `bench-results` held no gated result from `main`: GitHub started no scheduled nightly at 03:17 UTC on 2026-11-26. Each run's gate had no baseline on any CPU model. This led to the gated `main` dispatches and ADR-0089.
- **NFR-04 in run 3** is verify-m3's own line failing on a laptop stall: eight Redis calls timed out at the 5 ms budget in separate bursts 63 s apart, which ADR-0034's one-burst excuse does not cover.
- Every other row passed in all three runs, with figures in the same ranges as set C (NFR-01 arm64 was then 8,800 rps per vCPU, p99 7.0 / 8.0 / 18.2 ms).

## Consequences

- One command gives every row on one commit. Each full run uses about 20–40 minutes of each runner for NFR-01 and NFR-05.
- An x64 dispatch that lands on a CPU model with no gated `main` night gets `first` (ADR-0081). ADR-0089 prints that as NOT DEMONSTRATED and leaves it out of the exit status; the line prints the model so the reason is visible.
- NFR-05 on arm64 can fail on runner variance alone (see the known limit above), so a full run's exit depends partly on which runners the dispatch gets.
- The README block is generated, never edited by hand, and keeps FAIL and KNOWN LIMIT rows visible.

## Alternatives considered

- **A separate NFR-01 dispatch** — P19 made the floors the gate's rates, so ADR-0074 drops it. A second run set would only cost runner time.
- **A fresh scaling dispatch per run** — the user decided NFR-02 reads the newest result (ADR-0084 §4). A dispatch costs about 45 minutes per arch and cannot change the open limit without a user decision.
- **Counting a step's exit code for every row it feeds** — one stall in verify-m3's outage line would also fail NFR-04 and NFR-06, which it did not measure.
- **README=1 on every run** — the second run's preflight would then fail on a modified README. The `readme` subcommand writes the block after the last run instead.
