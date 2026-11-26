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

1. **NFR-01 and NFR-05 come from one gated dispatch**, `gh workflow run bench.yml --ref <branch> -f gate=true`, sent at the start. The run is found by its `run-name` (`bench throughput x64=4600 arm64=8800 gate=true`), head SHA and creation time. `VERIFY_M7_BENCH_RUNS=<id>` reuses a run already dispatched for HEAD; the script checks its head SHA and name first. Results are matched by `run_url`.
   - **NFR-01** per arch reads the first run set, not the re-run. PASS when it is valid, its target is the floor, and its median per gateway vCPU, rounded, is at least the floor (x64 4,600, arm64 8,800), on any CPU model. The line prints each model.
   - **NFR-05** per arch reads the final verdict: the re-run's if the first set was flagged, else the first set's. PASS when that verdict is `pass` and a nightly baseline was compared. The line prints the comparisons, and P34's cross-model figures when present.
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

## Consequences

- One command gives every row on one commit. Each full run uses about 20–40 minutes of each runner for NFR-01 and NFR-05.
- An x64 dispatch that lands on a CPU model with no gated `main` night gets `first`, and NFR-05 fails on that run (ADR-0081). The line prints the model so the reason is visible.
- The README block is generated, never edited by hand, and keeps FAIL and KNOWN LIMIT rows visible.

## Alternatives considered

- **A separate NFR-01 dispatch** — P19 made the floors the gate's rates, so ADR-0074 drops it. A second run set would only cost runner time.
- **A fresh scaling dispatch per run** — the user decided NFR-02 reads the newest result (ADR-0084 §4). A dispatch costs about 45 minutes per arch and cannot change the open limit without a user decision.
- **Counting a step's exit code for every row it feeds** — one stall in verify-m3's outage line would also fail NFR-04 and NFR-06, which it did not measure.
- **README=1 on every run** — the second run's preflight would then fail on a modified README. The `readme` subcommand writes the block after the last run instead.
