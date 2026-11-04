# 0015. Benchmark workflow and the M1 exit check

- **Status:** Accepted
- **Date:** 2026-11-04
- **Requirement IDs:** NFR-01, NFR-14

## Context

M1 ends with `bench.yml` (manual trigger) recording the first throughput baseline (NFR-01) and `make verify-m1` showing that a config reload under load drops no requests (NFR-14, reload part). The design's Benchmark method and ADR-0003 settle the runners, the CPU pinning, the 5 × (30 s + 3 min) protocol and the `bench-results` branch; ADR-0014 settles loadgen and its per-run JSON. They do not settle:

- where the run logic lives, so that a laptop can run it too;
- which upstream answers, whether it limits the gateway on 1 vCPU, and what NFR-01's "no-op upstream" means;
- the gateway's settings during a run, access logging above all;
- what a result file holds, how the median and the spread are computed, and when a run set is invalid;
- how two matrix jobs write to one branch;
- how `make verify-m1` drives reloads under load, counts them, and finds the baseline.

## Decision

- **Script.** `test/load/bench.sh` (the design's repo layout keeps load scripts in `test/load/`) runs a whole run set and prints its result as JSON on stdout, progress on stderr; `bench.yml` and a laptop run the same script. It builds the root Dockerfile's `gateway` and `loadgen` targets and `deploy/compose/upstream`, and runs them in a Docker network of their own, not in the compose stack, whose other services would compete for the CPUs. Docker picks the network's subnet, which is the gateway's whole allow-list (SEC-06).
- **Layout.** Gateway `--cpuset-cpus 0`, loadgen `1-2`, upstream `3`, each with `nofile` 65536; gateway and loadgen read-only. Go sizes `GOMAXPROCS` from the CPU set: 1 for the gateway, 2 for loadgen. No Redis until M3 needs it. The gateway serves plain HTTP on `:8000` (SEC-01) with `test/load/gateway.yaml` (one route, one upstream) and no OTLP endpoint.
- **Run set.** Target `BENCH_RATE` (default 10,000 rps) on `http://gateway:8000/bytes/1024` (1 KB responses). Each of the 5 runs is a fresh loadgen container with ADR-0014's defaults: 30 s warm-up, 3 min measured, 100 connections. The gateway and upstream stay up across the runs. `BENCH_RUNS`, `BENCH_WARMUP` and `BENCH_DURATION` shorten a laptop or CI smoke run.
- **Upstream.** go-httpbin (ADR-0006) with `-log-level OFF`; NFR-01's "no-op upstream" is read as one that does no more than return the bytes. A quick run on an Apple M4 Pro (Docker Desktop, the same pinning) measured, from `docker stats`:

  | Setup | Rate | Gateway CPU | go-httpbin CPU |
  | --- | --- | --- | --- |
  | go-httpbin alone, request log on | tops out near 25k rps | – | – |
  | go-httpbin alone, request log off | 30k rps | – | 89% |
  | through the gateway (access log 1%), go-httpbin log on | 20k rps | 99% | 83% |
  | through the gateway (access log 1%), go-httpbin log off | 20k rps | 89% | 63% |

  With its log off, go-httpbin spends about 70% of the gateway's CPU per request, so the gateway saturates first and no separate no-op upstream is needed.
- **Access log.** `SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE=0.01`. In the same quick run, logging every request cost the gateway about 40% more CPU per request (95% of its vCPU at 15k rps, against 89% at 20k rps with 1%), and the writes also load Docker's unpinned log driver. Denials and 5xx are still always logged (ADR-0013). The rate is recorded in each result, so a change to it shows.
- **Result**, one JSON object per run set:

  | Field | Value |
  | --- | --- |
  | `timestamp` | start of the run set, UTC, RFC 3339 |
  | `commit`, `ref`, `run_url` | the commit measured, its ref (`refs/heads/main` for a baseline) and the Actions run (empty on a laptop) |
  | `arch` | `x64` or `arm64`, from Docker's architecture |
  | `valid`, `invalid_reasons` | the verdict and, for an invalid set, why |
  | `target_rps` | the target rate |
  | `rps_per_vcpu` | the median achieved rate ÷ the gateway's vCPUs (1) |
  | `achieved_rps`; `latency_us.p50`, `.p99`, `.p99_9` | each as `median`, `min`, `max` and `spread` across the runs, where spread = (max − min) ÷ median |
  | `setup` | gateway vCPUs, the three CPU sets, URL, protocol, warm-up, duration, access-log sample rate |
  | `hardware` | `cpu_model` from loadgen (ADR-0014); `cpus`, `memory_bytes`, `os`, `kernel` of the Docker host |
  | `runs` | every run's loadgen result |

- **Validity.** A run set is valid only if every run is valid by loadgen (at least 99% of the target sent), no request went unanswered and every answer was 2xx; ADR-0014 leaves the error check to `bench.yml`. An invalid set is stored like a valid one and then fails its job.
- **Workflow.** `bench.yml`: `workflow_dispatch` with a number input `rate` (default 10000); matrix `ubuntu-latest` and `ubuntu-24.04-arm` with `fail-fast: false`; `contents: write`; 45-minute timeout. Each job commits its file to `bench-results` as `results/<arch>/<timestamp>.json`, the timestamp in ISO 8601 basic format (`20261104T101500Z`, no colons, so the branch checks out on any OS). It commits with git plumbing on top of the fetched branch, or as a new root commit when the branch is missing, and retries up to 5 times when the other job pushed first. It never force-pushes.
- **`make verify-m1`** (`test/verify/m1.sh`, with `make verify-m0`'s PASS/FAIL lines):
  - *Reload under load* (NFR-14). loadgen in the compose network sends 1,000 rps to gateway-1's `/bytes/1024` for 60 s with no warm-up. Every 2 s (29 times, the last 2 s before the load ends) the mounted `deploy/compose/gateway/config.yaml` is replaced by rename, alternately with `test/verify/m1-reload.yaml`, whose version, upstream name and endpoints differ, so each swap compiles new routes and replaces the upstream pool (GW-29). It passes if loadgen's run is valid and every request got a 2xx, and if gateway-1 logged `config loaded` at least 25 times since the load started. The committed file is copied back on exit, also after a failure or an interrupt.
  - *Baseline* (NFR-01). It fetches `origin/bench-results` and takes, per arch, the newest result with `valid: true` and `ref: refs/heads/main`. It prints that result's rps per gateway vCPU next to the 10k target (for information only) and its median p99, and passes when both arches have one. Until the M1 exit PR it fails as "pending", the one allowed M1 exception.
  - `make verify-m1-reload` runs only the reload part. CI's integration job runs it on every PR (about 70 s), followed by one 5 s run of `bench.sh`.

## Consequences

- One script for laptop, CI smoke and `bench.yml`. A laptop result records its hardware but is never stored; only `bench.yml` pushes.
- Docker's own processes are not pinned, so they and the runner's agent still share the four vCPUs; the median of 5 and the recorded spread absorb some of that.
- `bench-results` grows by one small file per arch per dispatch.
- `make verify-m1` changes a tracked file while it runs. A run killed with SIGKILL can leave it changed; `git checkout deploy/compose/gateway/config.yaml` restores it.
- go-httpbin needs about 70% of the gateway's CPU per request. If M7 makes the gateway much faster, the upstream may become the limit, and a no-op upstream comes back as a question.
- M7 adds the nightly schedule and the regression gate (NFR-05) on this file format, and the 2-vCPU scaling runs (NFR-02) change `setup.gateway_vcpus`.

## Alternatives considered

- **Benchmarking inside the compose stack** — Redis, Keycloak, Grafana and the rest share the CPUs, and pinning there would mean bench-only overrides for every service.
- **A no-op upstream as a loadgen subcommand** — not needed while go-httpbin with its log off costs less per request than the gateway; one more server to maintain.
- **Logging every request during benchmarks** — measures stdout and Docker's log driver as much as the gateway.
- **Committing results from a worktree of `bench-results`** — more state to clean up between push retries than plumbing.
- **Counting reloads from `gw_config_version`** — a gauge sampled from outside cannot count swaps between two values; the gateway logs one `config loaded` per swap.
- **Swapping a gitignored copy of the config** — gateway-1 reads the mounted `config.yaml`; another file would mean recreating gateway-1 with other settings.
- **All of `make verify-m1` in CI** — the baseline part fails until the exit PR and depends on `bench-results`, not on the PR under test.
