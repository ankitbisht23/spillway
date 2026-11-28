# 0106. make verify-m8: implementation details

- **Status:** Accepted
- **Date:** 2026-11-28
- **Requirement IDs:** SC-01 – SC-03, SC-08, SC-09, SEC-05

## Context

ADR-0098 settles `make verify-m8`: its parts (G0, up, K, D1 – D7, S, end with G1), every check's rule, the controlled load (D25), the line format, the result paths, the trap and the stall tolerance; ADR-0101 §13 and ADR-0102 §1 and §14 amend d6.3, D4's policy level and d6.1. The scenario checks already exist as `demo-traffic run --scenario=dN --check` (ADR-0102 §12) and the share checks as `make demo-share-test` (ADR-0097 §4, ADR-0105). Building the script (task verify, ADR-0093) left where the G and K checks run, how their few open details are measured, and how the script turns each part's output into one verdict. Accepted ADRs are not edited, so the choices are recorded here. No rule of ADR-0098 changes.

## Decision

1. **Where each check runs.** g1 – g5 and k1 – k4 are `demo-traffic verify guards --phase G0|G1` and `demo-traffic verify keys` (`cmd/demo-traffic/verify.go`), run like the D checks in a one-off container of the `demo-traffic` service with `--no-deps`, inside the compose network, with the run's directory mounted at `/out` for their JSON reports; on the dev stack the one-off is the only demo container and `--rm` removes it. g6 (`make demo-fault-redis ON=1` exactly as a user runs it) and g7 (containers, `make verify-m0-stack`) are `test/verify/m8.sh`'s own, since they run make targets and look at Docker. Every HTTP call of the G and K checks has a 15 s timeout.
2. **A build step first.** G0 runs before `make demo` builds anything, so the script builds the `demo-traffic` image first (`docker compose … build demo-traffic`; the `demo-seed` one-shot shares it). If the build fails, every check that needs the image prints `FAIL … setup:` and the end part still runs.
3. **Check IDs.** G0's and G1's checks print as `G0.g1` … `G0.g6` and `G1.g1` … `G1.g7`, so the verdict's list of failed checks names the phase; every other ID is ADR-0098's. `make demo-share-test`'s lines (`share-check: s4 PASS: …`) print as `PASS s4 share: …`; its IDs are ADR-0105's s1 – s8, s9a – s9c, s10 – s13; s10 prints six lines (each restored setting, then the clean-up) and s11 two, each counted (73 check lines in a passing run).
4. **g2's body** is a valid custom run: `{"kind": "custom", "tenant": "pro-co", "route": "catalog", "rate": 10, "duration_s": 5}`. g3 and g4 use a random UUID. g5 sends both switches `{"active": true}` without and with a token and reads toxiproxy's `GET /proxies` once: no toxic whose name starts `demo-` (the switches' toxics are `demo-cut-up`, `demo-cut-down`, `demo-slow`), and in G1 no proxy whose name starts `demo-`.
5. **k1 – k4 details.** All keys are `pro-co-mobile`'s. k1 compares the answer's own `created_at` and `expires_at`. k2 passes when the answer is 422 with code `validation_failed` and a detail's `path` (or the message) names `expires_at`. k3 takes `expires_at` from the issue answer (the control plane's clock, which the gateways share in the Docker VM), applies the key on each replica with demo-traffic's existing 100 ms / 10 s probe (`waitApplied`, on `catalog`), then probes `catalog` on all three replicas at once every 100 ms until `expires_at + 5 s` (about 30 probes per replica past the 2 s margin). A probe sent before `expires_at − 1 s` that got no answer counts as a failure, although the rule names only answered probes (stricter, never looser), and each replica needs at least one judged probe on each side. k4 requires exactly the seed's three tenants with their plans and `consumer-plan` limits (free 10/1s burst 20, pro 100/1s burst 200, enterprise 1000/1s burst 1000; a unit test checks these against `deploy/demo/seed.yaml` and `deploy/demo/config.yaml`) and both faults `false` (`null` fails). K's keys are not revoked: they expire within 30 minutes, and the next run's `make demo-reset` drops them with the database.
6. **From part output to checks** (`lines_of` in m8.sh). A part's check lines are its `PASS`/`FAIL` lines; each check the part should have printed and did not prints `FAIL <id> setup: no result (step <part> exit <code>; <log>)`; a part that exits non-zero without any FAIL line prints `FAIL <part> exit: …`; a part that cannot run (no image, or `make demo` / `make demo-reset` failed) prints each of its checks as `FAIL <id> setup: <reason>`; a run that stops early prints the checks of every part not run as `FAIL <id> setup: not run`. The verdict counts every line. The build, up and down steps have no checks; a non-zero exit of one is printed as a `note:` line, and its consequences fail the checks after it (K – S, or G1's g5 and g7).
7. **Output.** The terminal gets a `==` header per part with the time since the start, the check lines as each part ends, then the setup notes, the duration with the commit and the verdict last. Each part's full output (the scenarios' viewer lines and summaries, compose's lines) goes only to `test/verify/out/m8-<UTC timestamp>/<part>.log`. `test/verify/out/m8-<UTC timestamp>.json` holds the start, end, commit, branch, duration, verdict, every step's exit code and time, every check line split into part, ID, verdict and text, and each part's own JSON report (`reports.d5` …) with its measured values. `test/verify/m8.sh report <dir>` prints the lines and writes the JSON again from a run's directory (tested on fixtures in `test/verify/m8_test.go`).
8. **The trap acts only on what is left.** On an exit before the end it runs `make demo-share-stop` if a share is active, `make demo-fault-clear` if the demo control plane runs and `make demo-down` if any demo container exists, logging to `cleanup.log`. A run that reaches its end has already run `make demo-down` and checked G1, so the trap does nothing more there (no second `make dev`).
9. **Nothing else changes for the run.** No tracked file is written (the demo runs in managed mode on its own database; the share test's files live in the ignored `deploy/demo/run/` and s10 checks it is empty), the gateways keep compose's defaults, and `make verify-m8` stays out of CI (ADR-0098).
10. **No generated README block.** Unlike verify-m7 (ADR-0074, ADR-0088), ADR-0098 gives verify-m8 no README output, so `m8.sh` has no `readme` mode and the verify task writes no `verify-m8:begin` block. The README belongs to the readme task (SC-06, ADR-0099 §6), which can cite the run below.

## Measured on the branch (2026-11-28)

### make verify-m8: 3 of 3 full runs PASS

Each run: 73 of 73 checks PASS, on the laptop (Apple M4 Pro, Docker Desktop 14 CPUs, 8.3 GB), within ADR-0098's budget. Run 1 was on 0fe92f8, runs 2 and 3 on 160099a (160099a changes only comments, the fixture test and this ADR). Results in the ignored `test/verify/out/m8-<stamp>.json`.

| | Run 1 | Run 2 | Run 3 |
|---|---|---|---|
| Stamp, duration | 20261128T040858Z, 17 min 44 s | 20261128T042732Z, 17 min 36 s | 20261128T044530Z, 17 min 47 s |
| k3 key expiry (each replica) | applied 0.11 s; 139 before, 30 × 401 after | same | same |
| d1.3 free held | 300 × 2xx (10.0/s) of 900 | same | same |
| d2.1 quiet p99 on ÷ off (pro, enterprise) | 0.721, 0.758 | 0.702, 0.695 | 0.821, 0.805 |
| d2.3 noisy, lowest on slot 429 share | 97.62% | 97.58% | 97.60% |
| d3.1 most per 1 s / 10 s / 1 min window | 201/300, 1199/1200, 6197/6200 | 201/300, 1199/1200, 6199/6200 | 201/300, 1194/1200, 6193/6200 |
| d4.1 applied (shadow, promote, rollback) | 0.11, 0.11, 0.11 s | 0.11, 0.11, 0.01 s | 0.11, 0.11, 0.01 s |
| d4.4 enterprise-co-web 429 share | 64.29% | 64.29% | 64.29% |
| d5.1 catalog 2xx in the cut | 100.00% of 2000 | 100.00% of 2001 | 100.00% of 2001 |
| d5.3 orders per replica (limit ÷ 3) | 32.9/s each | 32.9/s each | 32.9/s each |
| d5.5 Redis checks ≤ 10 ms | 100.00% on each replica | same | same |
| d6.2 breaker opened (gw-1, 2, 3) | 6.80, 6.80, 5.80 s | 5.80, 5.80, 5.80 s | 6.80, 6.80, 6.80 s |
| d7.1 leaked key on top | 1.00 s | 1.01 s | 5.00 s |
| d7.2 block on every replica, 10 rounds | max 0.05 s | max 0.05 s | max 0.05 s |

### The other exits, once each on 160099a

- `make verify-m1`, `verify-m2`, `verify-m4`, `verify-m5`, `verify-m6`: PASS. NFR-03 added p99 0.121 ms; NFR-12's staleness alert at +86 s.
- `make verify-m3`: **FAIL**, 1 check: the killed primary's surviving primaries' keys had 15,999 Redis decisions and 1 failure-mode answer of 16,000 (rule: none), in a 19 ms timeout burst at +6.4 s after the kill. NFR-04 PASS (0.345 ms added).
- `make verify-m7` (run m7-20261128T060405Z, 87 min 36 s, budget 150): **FAIL, 3 rows**; every other row PASS, NFR-02 the accepted known limit.
  - **NFR-05:** bench [run 46114900991](https://github.com/ankitbisht23/spillway/actions/runs/46114900991): x64 landed on Intel Xeon 6975P-C, a model with no `main` baseline (NOT DEMONSTRATED); arm64 flagged against the 3,171 µs nightly (+31%, confirming re-run +40.9%), so `fail`.
  - **NFR-13:** surviving primaries' keys 15,987 Redis decisions, 13 failure-mode of 16,000, in a 72.6 ms burst at +14 s.
  - **Chaos Redis +50 ms:** 1,900 of 1,901 fail_closed answers 503, 1 answered 2xx.

### The user's decisions (2026-11-28)

1. **NFR-05: new baseline, then "Exclude outlier".** The arm64 3,171 µs nightly (`results/arm64/20261127T121534Z.json`) is on the committed exclusion list (ADR-0107, #116). The gated bench on `main` caaf832 ([run 46128589731](https://github.com/ankitbisht23/spillway/actions/runs/46128589731)) passed and its run sets are the new baselines: x64 AMD EPYC 9V74 p99 1.619 ms (spread 53.1%), arm64 6,300 rps p99 2.719 ms (spread 8.7%). The comparison the user asked for, on m8-verify 2a6c553 ([run 46130134450](https://github.com/ankitbisht23/spillway/actions/runs/46130134450)), passed on both arches: x64 on AMD EPYC 9V74 (same model, so demonstrated) p99 1.398 ms (−13.7%), arm64 6,300 rps p99 2.827 ms (+4.0%). **NFR-05 is recorded as PASS by that comparison.** The verify-m7 row above stays a FAIL in this record.
2. **Chaos Redis +50 ms: "Keep the rule".** A reported FAIL. It is a timing edge: `cmd/chaos/latency.go` stamps the toxic-off time after it has removed all six toxics, so an answer from a node whose toxic was removed first can land inside the judged window. No code or rule change.
3. **NFR-13 and verify-m3's surviving primaries: "Mac constraint".** Reported FAILs under the known Docker Desktop for Mac stall constraint, like NFR-04 at M7's exit (README, Known limits). No rule change, no re-run.

### Known limit: TestGatewayPerIPCap and Docker Desktop's port forwarder

`make test-integration`'s TestGatewayPerIPCap can fail on the laptop with connection resets. Diagnosis: Docker Desktop's host port forwarder (`com.docker.backend`) resets unpaced bursts of host dials. 1,100 sequential host dials (about 8,000/s) were reset in 4 of 12 bursts, at dials 542–989. With 1 ms between dials 5 of 5 bursts were fine, and from inside the compose network 3 of 3. The host's listen-queue overflow counter stayed 0. The gateway's per-IP counters moved +76 closed and 0 shed on good bursts and did not move on reset bursts, so the resets never reached the gateway. Not a gateway defect; the test is unchanged. On this branch (2a6c553 plus this ADR), `make test-integration` failed only this test, at dial 1,086 of 1,100 ("connection reset by peer"); run alone afterwards for information, it failed 1 of 3 times (dial 742) and passed 2 of 3.

## Consequences

- `make verify-m8` judges every ADR-0098 check, prints each with its measured value, and leaves the stack as `make dev` does on every exit.
- The G and K checks are unit-tested against fakes (a broken guard, a demo toxic or proxy left, a gateway that ignores expiry, the seed changed), and the script's verdict logic on fixtures (a FAIL line, a missing line, a crashed part, a part that could not run, an interrupted run).
- If a later task adds a check to a scenario, its ID must be added to `ids_of` in m8.sh, or the run prints it but cannot notice it missing.

## Alternatives considered

- **g1 – g5 and k1 – k4 in bash with curl** — gateway-2 and gateway-3 have no host port and the service token's secret must not reach the host shell; the one-off already has both, and Go code can be tested against fakes.
- **Printing every part's full output** — the D2 viewer lines alone are over 80 lines; the PR would carry noise instead of the measured numbers, which the logs keep.
- **Unqualified `g1` … `g7` in both phases** — a FAIL list "g5" would not say whether the guard failed before or after the demo.
