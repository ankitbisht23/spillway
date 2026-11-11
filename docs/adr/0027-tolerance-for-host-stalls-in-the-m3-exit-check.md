# 0027. Tolerance for host stalls in the M3 exit check

- **Status:** Accepted
- **Date:** 2026-11-12
- **Requirement IDs:** NFR-04, NFR-13

## Context

ADR-0026 removed the dials that made the killed-primary test fail after a full outage, and measured what is left: stalls of the host and of Docker Desktop's VM. Under load the worst repeat about every 15.06 s and last 4–9 ms. They hit every container at the same instant, the gateway included, and a call that times out ends 0.5–1.5 ms after its deadline. With them, `make verify-m3` passed 1 of 3 full runs on the dev laptop (Apple M4 Pro, 14 CPUs), and each FAIL was a single outlier:

- **Full outage.** 27,999 of 28,000 Redis checks within 7 ms. The line is ADR-0021's 5 ms budget plus a 2 ms slack, so one stall of more than about 1 ms while calls are timing out, in the ~400 ms before the breakers open, fails it.
- **NFR-04.** In gateway runs 2 and 3, one Redis call each missed its 5 ms budget, which gave one `fail_closed` 503 per run. ADR-0021 voids a run with any non-2xx answer or any Redis error, so the run voided its pair and the check failed with no latency comparison, although the medians were within the target.
- **Killed primary** passed 15 of 15 after ADR-0026.

The stalls come from the machine, not from the gateway, and Spillway cannot remove them. The requirement texts stay as they are: NFR-13's "within one Redis time budget" and NFR-04's p99 ≤ 5 ms. What changes is how much of the dev laptop's noise the exit check tolerates. The user chose a small tolerance for both checks, no re-run of a failed check, and no other change.

## Decision

This supersedes ADR-0021's full-outage "within one budget" rule and, for NFR-04's gateway runs only, its rule that a Redis error or a non-2xx answer voids a run. Everything else in ADR-0021, ADR-0025 and ADR-0026 stays.

- **Full outage, within one budget.** Every Redis check made during the run, across gateway-1 … gateway-3, must fall in the `le="0.01"` bucket of `gw_ratelimit_check_duration_seconds{backend="redis"}`: the 5 ms budget plus a **5 ms slack**, up from 2 ms. 10 ms covers the measured stalls (up to 9 ms) on a call that would otherwise end at the deadline. The bucket exists already, so the histogram does not change. There is still no count allowance: one check over 10 ms fails. The line also prints how many checks ended within 5 ms and within 7 ms, so a run that needed the slack shows it.
- **NFR-04 gateway runs.** A gateway run may have **at most one** Redis error and its one `fail_closed` 503. The run's line reports both; the run and its pair still count, and the 503 is not a failed answer. Two or more Redis errors, a `fail_closed` decision without a Redis error, any deny, or any other non-2xx answer still void the run as before. The added-latency comparison is unchanged: medians of three alternating pairs, PASS when 0 ≤ added p99 ≤ 5 ms. The excused 503 stays in the run's latency distribution: it is one request of 60,000, below the p99.
- **Unchanged.** The 5 ms budget; `fail_closed` in `test/verify/m3-latency.yaml`; the killed-primary rules (no failure-mode answer for keys on surviving primaries, a failover window of at most 10 s, the 2 ms slack); the accuracy rules; NFR-03's check in `make verify-m2`, which has no Redis call and keeps "any non-2xx voids the run".
- **No re-run.** A check that fails is reported as failed; the scripts never run it again.

In the scripts: `test/chaos/redis-outage.sh` reads the `le="0.01"` bucket. In `test/verify/added-latency.sh` the caller's `judge` prints the number of failed answers it accounts for after its verdict, and a run passes with at most that many; `m2.sh`'s judge accounts for none, and `m3.sh`'s for the `fail_closed` decisions of a run with at most one Redis error.

## Consequences

- A stall during the full outage no longer fails NFR-13 unless it puts a check more than 5 ms past its budget. A real regression that makes timed-out calls overrun the budget by more than that still fails it.
- A single stall during an NFR-04 gateway run no longer hides the latency comparison. A gateway that fails Redis calls more than once a minute at 1,000 rps still fails it.
- The integration test in `test/integration/gateway_failure_test.go` keeps its own 7 ms line: it is not the exit check and runs in CI, where it passes.
- `make verify-m3`'s pass rate under these rules is measured and recorded in the M3 exit PR.

## Alternatives considered

- **A count allowance in the full-outage check (say, 1 in 10,000 over 7 ms)** — lets a regression that adds a few slow calls pass, and the number would be as arbitrary as the slack. The user chose a line that every call must meet.
- **Re-running a failed check once** — doubles the time of a FAIL and hides a failure that repeats one run in two. Not chosen.
- **A 15 ms or 25 ms line** — the next buckets. Wider than the measured stalls need.
- **Making `m3-latency.yaml` `fail_open`** — a failed call would then be a fast local answer inside the measured latency instead of a visible 503. ADR-0021 chose `fail_closed` for that reason.
