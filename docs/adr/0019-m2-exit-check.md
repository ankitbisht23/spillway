# 0019. The M2 exit check

- **Status:** Accepted
- **Date:** 2026-11-10
- **Requirement IDs:** NFR-03, RL-26, RL-01, RL-02

## Context

ADR-0017 names the three parts of `make verify-m2`: the property tests for every algorithm, shadow mode never rejecting, and NFR-03's added p99 of at most 2 ms with local limits, measured by paired runs straight to an upstream and through gateway-1. It does not settle:

- how gateway-1 gets a config for each check without leaving the committed one changed;
- the rates, durations and policies of the runs, and what makes a run count;
- what the check prints, and which parts CI runs.

## Decision

- **Script.** `test/verify/m2.sh`, with `m1.sh`'s PASS/FAIL lines; `make verify-m2` runs all three parts after `make dev`, and `make verify-m2-shadow` runs only the shadow part.
- **Configs.** Each check's gateway config lives in `test/verify` (`m2-shadow.yaml`, version 21; `m2-latency.yaml`, version 22) and is swapped into gateway-1's mounted `deploy/compose/gateway/config.yaml` by rename, as `make verify-m1` does (ADR-0015). The script waits up to 10 s for `gw_config_version` on the admin port to show the new version, and copies the committed file back on exit, also after a failure or an interrupt.
- **(a) Property tests.** `go test -run '^TestProperty' ./internal/ratelimit/local/` with `-rapid.checks=10000` (rapid's default is 100), one line per algorithm with the number of random cases and its bound.
- **(b) Shadow never rejects.** The only policy is a shadow GCRA limit of 10/s keyed by route. loadgen sends 500 rps for 10 s to `/bytes/1024` through gateway-1. It passes if the run is valid, every request got a 2xx (so no 429), and `gw_ratelimit_decisions_total{policy="verify-shadow", decision="deny", mode="shadow"}` rose during the run.
- **(c) NFR-03.** Two loadgen runs in the compose network (ADR-0004), each 30 s warm-up and 60 s measured at 1,000 rps (`VERIFY_M2_RATE`, `VERIFY_M2_WARMUP` and `VERIFY_M2_DURATION` override them for a quick look): first straight to `upstream-1:8080/bytes/1024`, then to `gateway-1:8000/bytes/1024` with one route to upstream-1 alone, so both runs reach the same upstream. The gateway run has three enforce-mode local policies on the route whose limits are far above the load: GCRA on `{ip}`, sliding window on `{route}:{method}` and concurrency on `{route}`, so every request runs the limiter path, a concurrency release included. loadgen sends no API key, so phase 2 has nothing to evaluate. Each run passes if loadgen calls it valid (at least 99% of the target sent) and every request got a 2xx; the gateway run also needs every policy's allow count to cover every request sent, with no deny. The added p99 is the gateway run's p99 minus the direct run's, PASS at 2 ms or less; p50 is printed alongside. Without two passing runs there is nothing to compare, and the NFR-03 line fails.
- **CI.** The integration job runs `make verify-m2-shadow` (about 15 s). The property tests already run in the `go` job's `make test`. The latency pair stays local: it takes three minutes, and a p99 difference of a few hundred microseconds on a shared runner is noise, not a gate.

## Consequences

- One quick laptop run (Apple silicon, Docker Desktop, the full compose stack up, access log of every request on) measured a direct p99 of 1.44 ms, a gateway p99 of 1.55 ms and an added p99 of 0.10 ms at 1,000 rps; loadgen's timer lateness (ADR-0014) is in both runs and cancels out of the difference.
- Like `make verify-m1`, a run killed with SIGKILL can leave the committed config swapped; `git checkout deploy/compose/gateway/config.yaml` restores it.
- NFR-03 is measured with traffic the limiters allow. Denied requests cost a refund more (ADR-0017) and are not measured here.
- M3's NFR-04 (added p99 with a Redis check) can reuse the same pair with a global policy in the gateway config.

## Alternatives considered

- **A second gateway container with its own config for the checks** — D14 and ADR-0017 measure gateway-1, and another container would need its own settings kept in step with compose.
- **A gitignored copy of the config** — gateway-1 reads the mounted `config.yaml`; another file would mean recreating gateway-1 with other settings (ADR-0015).
- **Higher rates (2,000 rps and more)** — Docker Desktop and the full compose stack share the laptop's CPUs; 1,000 rps is well inside what every dev machine sustains, and NFR-01 measures throughput separately.
- **The whole check in CI** — see CI above.
