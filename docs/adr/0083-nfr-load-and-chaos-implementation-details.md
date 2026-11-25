# 0083. NFR load tests and the chaos suite: implementation details

- **Status:** Accepted
- **Date:** 2026-11-25
- **Requirement IDs:** NFR-02, NFR-05, NFR-10, NFR-11, NFR-13, NFR-15

## Context

ADR-0074 §D9 sets the M7 load tests (NFR-02 scaling on the CI runners, NFR-10 key sweep, NFR-11 soak, NFR-15 noisy neighbour) and the chaos suite (new Redis-latency and gateway-kill scenarios, `make chaos`), with their pass rules, stall tolerance and time budgets. ADR-0068 approved P20 (Benchmark method step 1, applied by this task), P29 (NFR-02 on arm64 only, should x64's SMT layout be what fails it) and P33 (no k6). ADR-0081 proposed P34, an information-only cross-model comparison in the bench gate, which the user approved on 2026-11-25. This ADR records the small choices ADR-0074 left open and P34's form. No pass rule of ADR-0074 is changed; where a check adds a condition, it is listed.

## Decision

### Entry points

| What | Script | Make target | Budget (ADR-0074) |
| --- | --- | --- | --- |
| NFR-02 scaling | `test/load/scaling.sh` | `bench.yml -f mode=scaling` | 60 min per dispatch (job timeout 75) |
| NFR-10 keys | `test/load/keys.sh` (`chaos keys`) | `make nfr-keys` | 8 min |
| NFR-11 soak | `test/load/soak.sh` | `make nfr-soak` | 12 min |
| NFR-15 noisy neighbour | `test/load/noisy.sh` | `make nfr-noisy` | 12 min |
| Redis latency +50 ms | `test/chaos/redis-latency.sh` (`chaos latency`) | `make chaos-redis-latency` | 3 min |
| Gateway `kill -9` | `test/chaos/gateway-kill.sh` (`chaos gwkill`) | `make chaos-gateway-kill` | 3 min |
| Compose chaos suite | — | `make chaos` | about 40 min |

`make verify-m7` (the verify task) runs these targets. `nfr-soak`, `nfr-noisy`, `chaos-redis-latency` and `chaos-gateway-kill` recreate the gateways at the 1% trace ratio through their `dev` prerequisite, as the M1–M3 targets do (ADR-0030, ADR-0074). `make chaos` runs each scenario through its own target, so each gets its own environment, keeps going after a failing scenario and fails at the end if any failed.

### NFR-02 (`scaling.sh`, `bench.yml`)

1. **CPU usage** is read through Docker's stats API (`cpu_stats.cpu_usage.total_usage`, which on cgroup v2 is the container's `cpu.stat` `usage_usec` × 1,000), from 1 s into a probe's measured 30 s to 1 s before its end (a 28 s window, timed on the host), so it works on the runners and on a laptop alike.
2. **No OTLP exporter** in scaling runs: bench.sh's Collector shares the upstream's CPU, which here must be the no-op upstream's alone. The gateway logs 1% of requests, as in bench.sh. Gateway config: `test/load/gateway.yaml`, the upstream answering as `upstream:8080`.
3. **Measured runs** use `-conns 200`, as the probes do. The arch's rate per vCPU r is `BENCH_RATE`, which `bench.yml` sets from the dispatch's rate inputs (defaults 4,600 and 8,800, the NFR-01 floors).
4. **Result**: one JSON per arch with `setup.mode: "scaling"`, `n1` and `n2` (probes with their vCPU readings and gateway-bound flag, P, R, the run set), `scaling` (S), `valid`, `pass` and `invalid_reasons`. It is stored on `bench-results` with `gate.verdict: "ungated"` (`benchgate -ungated`, without its summary), so it never compares with or becomes a throughput baseline. The job summary shows S, R and every probe's vCPUs; the job fails when the result is invalid or S < 1.70.
5. **`run-name`** is `bench <mode> x64=<rate> arm64=<rate> gate=<bool>`; `gate` is forced off in scaling mode.

### NFR-10 (`chaos keys`)

6. Readings cover all six nodes (CLUSTER SHARDS hostnames); replicas' deltas are printed for information. A failing step ends its sweep; the next kind still runs. The test's keys are removed before the first sweep too. **UNLINK in batches of 1,000** is a pipeline of 1,000 one-key `UNLINK`s per batch, since a multi-key `UNLINK` across slots is a CROSSSLOT error in cluster mode. `-steps`, `-max-bytes` and `-max-count-off` exist for development and the integration test (40,000 keys, which allows 1,000 B per key because fixed overheads weigh more at that size); defaults are ADR-0074's.

### loadgen's per-tenant mode (NFR-11, NFR-15)

7. **One transport per tenant**, so one tenant's requests can never queue another's inside loadgen; all tenants share one schedule start, so their slots line up.
8. **Warm-up slots alternate too** (negative slot indices, odd = on): the noisy tenant toggles through the warm-up instead of starting cold in slot 1.
9. **Settling** is min(1 s, slot ÷ 2); `-slot` must be at least 2 s.
10. A toggled tenant's achieved rate is sent per second of the time it was scheduled to send (rate × sent ÷ scheduled), and its 99% rule counts scheduled requests. The top level of a per-tenant result adds the tenants up (target = Σ rates, latencies from all answers, valid when every tenant is).
11. **Pooled p99s**: each tenant also gets `pooled_p99.even_slots` and `.odd_slots` over the settled answers of all its even and all its odd slots: the "pooled on/off p99s" ADR-0074 asks to print (even = the noisy tenant off).
12. **Run-time identities**: `test/load/lib.sh` makes one consumer `c-<T>` of tenant `<T>` and a random 48-hex static key per tenant, writes each key to its own `0600` file in a `0700` temp directory that only loadgen's container mounts (read-only, running as the caller's uid), and swaps `deploy/compose/gateway/identity.yaml` (prefix and SHA-256 only) and `config.yaml` by rename, putting both back on every exit. Keys are never printed or logged; loadgen reads a key file once and its errors name the file, never the content (unit-tested). Config and identity versions: 71 noisy, 72 soak; 73 and 74 are the latency and gateway-kill configs.

### NFR-15 and NFR-11 checks

13. **NFR-15**: pair k is (slot 2k off, slot 2k+1 on); a quiet tenant passes when all 30 pairs have answers in both slots and the median of p99(on) ÷ p99(off) is < 1.10. Added line: every tenant sent ≥ 99% of its schedule (ADR-0074's per-tenant 99% rule as a check).
14. **NFR-11**: the deltas cover the whole run, warm-up included (before and after, as ADR-0074 says). Added lines: every tenant sent ≥ 99% of its schedule (the run was at ADR-0074's load), and gateway-1's `process_start_time_seconds` did not change (a restart would reset the counters).

### Chaos scenarios

15. **Redis latency**: the sender adds and removes the toxic itself through toxiproxy's API (toxic `chaos-latency`, type `latency`, `downstream`, on all six proxies), so the windows come from the actual add and removal times: judged window = [added + 1 s, removed); pre-toxic window = from 5 s after the start to the toxic; recovery = from removed + 5 s to the end. Expected degraded admits = 50 × W + 50 with W the judged window's measured length (19 s nominal). A fail-closed answer is a 503 whose body is `rate limit unavailable`. The override is `test/chaos/latency.compose.yml` (ADR-0074 names it `.yaml`; `.yml` keeps it out of the tests that parse every `test/chaos/*.yaml` as a gateway config). gateway-4 gets `test/chaos/latency.yaml` through the override's mount, so no shared config is swapped; the proxies listen on 26370–26375 inside the compose network (docs/ports.md).
16. **"Every answer carries Spillway-Decision-Time again" and the deny cache.** A deny served from the deny cache (RL-19, ADR-0069) makes no Redis call and so carries no `Spillway-Decision-Time`; the degraded route (100 rps against 50/s) gets such denies whenever Redis is healthy, so the rule as worded would fail on a healthy gateway. The recovery rule therefore counts as a Redis decision an answer with `Spillway-Decision-Time` or a 429 with neither `Spillway-Decision-Time` nor `Spillway-Redis-Error` (a cached deny replays a Redis deny), and requires that no answer carries `Spillway-Redis-Error`. The fail_open and fail_closed routes never deny, so for them the rule is exactly ADR-0074's.
17. **Gateway kill**: gateway-1 … gateway-3 run `test/chaos/gwkill.yaml` (no policy), swapped like the other configs. A failure is no answer, a timeout or a non-2xx. Added lines: a failure from gateway-2 was observed (else the kill did not happen) and the sender sent ≥ 99% of its schedule.

### P34 (NFR-05, cmd/benchgate)

18. With verdict `first`, the gate also compares the run set with the newest result stored before it that is eligible as a baseline (valid, on `main`, verdict pass or first or none) with the same arch, target rate and setup and **another recorded CPU model**; none when either model is unrecorded. It is stored as `gate.cross_model: {info, baseline, cpu_model, comparisons: [{metric, base, value, change}]}` (absent otherwise) and shown in the job summary as an "Information only (P34)" line and table. It has no `flagged` field, never changes the verdict or the exit code, and never makes or blocks a baseline. P34 has no docs/design.md text.

### Design text

19. P20's step-1 text and P33's two rows are applied to docs/design.md. The M7 box "Load tests against every NFR row …" is **not** ticked: NFR-14's rolling restart is measured on kind by the helm task (ADR-0068 maps the box to helm).

## Measured on the branch (2026-11-25)

Each new scenario ran three times on the laptop (Apple M4 Pro, Docker Desktop 14 CPUs, about 8.3 GB) and NFR-02 once on the runners; every run is reported, none was re-run.

| Scenario | Runs | Result |
| --- | --- | --- |
| NFR-10, GCRA keys at 1,000,000 | 3/3 PASS | 118.3, 118.2, 118.9 B per key; every call allowed; Σ Δ DBSIZE = keys created exactly; about 25 s per run |
| NFR-10, sliding-window keys at 1,000,000 | 3/3 PASS | 157.5, 157.0, 157.7 B per key; same |
| NFR-11 soak | 3/3 PASS | 0 gateway-caused 5xx of 1,260,000 requests each (0.0000%); no shed, circuit_open, fail_closed or upstream 5xx; 125,155 / 125,456 / 125,360 429s (tenant D above 600/s); every tenant at 100% of schedule |
| NFR-15 noisy neighbour | 3/3 PASS | median p99(on) ÷ p99(off) over 30 of 30 pairs: B 0.378 / 0.347 / 0.337, C 0.377 / 0.343 / 0.340, D 0.376 / 0.342 / 0.342; pooled p99 noisy off about 6.2–6.6 ms, on about 2.2–2.7 ms |
| Redis latency +50 ms | 3/3 PASS | fail_open 1,900/1,900 2xx; fail_closed 1,899–1,900/1,900 503 fail_closed; degraded 950 admitted (expected 1,000 ± 10%); medians fell under the toxic (−0.03 to −0.38 ms: calls end at the budget, then the breakers open); Redis decides on every answer from +45 s |
| Gateway `kill -9` | 3/3 PASS | 0 failed of about 4,992 on each of gateway-1 and gateway-3; gateway-2: 1 failed (transport), sent before its first observed failure 134–154 ms after the kill request |
| `make chaos` (whole compose suite) | 1/1 PASS | Redis latency, Redis full outage, Redis primary killed, control-plane outage, gateway kill, verify-m4 and NFR-15 (medians 0.350 / 0.349 / 0.347) all PASS |
| NFR-02 scaling ([run 45626436921](https://github.com/ankitbisht23/spillway/actions/runs/45626436921)) | INVALID on both arches (neither PASS nor FAIL) | see below; finished by task nfr-scaling |

NFR-15's ratios are below 1 because the quiet tenants' p99 is *lower* while the noisy tenant sends: at 300 rps the gateway and the Docker Desktop VM idle between requests and pay wake-up latency, which the noisy tenant's 2,000 rps keeps away. The rule (< 1.10) is met by a wide margin; the effect is reported, not corrected.

### NFR-02 on the runners: INVALID, finished by task nfr-scaling

| Arch, CPU model | n | P (2xx/s) | R (rps) | probe vCPU: gateway / loadgen / upstream | gateway-bound |
| --- | --- | --- | --- | --- | --- |
| x64, AMD EPYC 9V74 | 1 | 8,834 | 7,900 | 0.960 / 0.71 / 0.31 (×3 alike) | yes |
| x64, AMD EPYC 9V74 | 2 | 17,186 | 15,400 | 1.70–1.71 / **0.93–0.94** / 0.68 | no: loadgen saturated |
| arm64, Neoverse N2 | 1 | 11,697 | 10,500 | **0.947–0.949** / 0.62 / 0.33 | no: gateway just under 0.95 |
| arm64, Neoverse N2 | 2 | 19,254 | 17,300 | **1.69–1.70** / 0.84 / 0.47 | no: gateway under 1.90 |

Both measured run sets were valid on both arches (5 × 3 min at R, every request 2xx). S would be 1.949 on x64 and 1.648 on arm64, but neither result is valid, so neither passes or fails NFR-02. On x64 the 1-vCPU loadgen saturates before the 2-vCPU gateway does (not the SMT layout, so P29 does not apply); on arm64 neither loadgen nor the upstream reaches 0.90 vCPU, yet the gateway does not reach 0.95 × n; why is not established (candidates: work charged to no container's cgroup, such as the bridge network's softirq processing, or loadgen's single dispatch loop at 57,600 rps offered). The jobs took 40 minutes each (budget 60).

20. **NFR-02 moves to task nfr-scaling (user decision, 2026-11-25).** ADR-0074 says nfr-load stops for the user when NFR-02 cannot be measured as written. The user decided to ship this task with NFR-02 open: [run 45626436921](https://github.com/ankitbisht23/spillway/actions/runs/45626436921) is recorded as INVALID on both arches (x64: loadgen saturated at n = 2, 0.93–0.94 vCPU; arm64: gateway 0.947–0.949 vCPU at n = 1 and 1.69–1.70 at n = 2, under 0.95 × n), so it neither passes nor fails NFR-02, and the separate next task **nfr-scaling** finishes NFR-02. This task delivers `scaling.sh` and `bench.yml -f mode=scaling` as the harness; no pass rule is changed and the run was not repeated.

## Consequences

- One make target per scenario; `make chaos` runs the compose suite in about 40 minutes.
- loadgen and cmd/chaos grow the per-tenant mode, `serve`, `keys`, `latency` and `gwkill`, each unit-tested; `chaos keys` also has an integration test.
- A scaling result is stored on `bench-results` but never compared.
- NFR-02 stays open after this task; nfr-scaling completes it.

## Alternatives considered

- **Reading cgroup files on the host** — not reachable on Docker Desktop; the stats API reports the same counter on both.
- **One shared HTTP transport for all tenants** — the noisy tenant would take the quiet tenants' connections inside loadgen and fake interference.
- **Requiring Spillway-Decision-Time on every answer literally** — fails on a healthy gateway because of the deny cache (16).
