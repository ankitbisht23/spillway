# 0074. NFR load tests, the chaos suite and make verify-m7

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** NFR-01 – NFR-15, OB-05

## Context

M7's exit (`make verify-m7`) is "every NFR row (NFR-01 – NFR-15) meets its target, with the measured result in the README, or an ADR re-baselines it". Nine rows already have checks from M1–M6 (`make verify-m1` … `verify-m6`); NFR-02, NFR-07, NFR-10, NFR-11, NFR-14 (rolling restart) and NFR-15 are first measured now, NFR-01 and NFR-05 get M7's form (ADR-0073), and box 5 asks for a chaos suite. Constraints:

- The laptop (Apple M4 Pro, Docker Desktop with 14 CPUs and about 8.3 GB) stalls: usually 4–9 ms about every 15 s, sometimes 100–820 ms (ADR-0026, ADR-0027, ADR-0034). `make verify-m3` fails some runs because of them, which the user accepted. New checks must not fail on a stall alone, and no check is ever re-run.
- kind and the full compose stack do not fit in 8.3 GB together (ADR-0075).
- `cmd/loadgen` has no per-tenant histograms (NFR-15 needs them) and one URL per run.

## Decision

### D9. The new load tests

#### NFR-02 scaling (CI runners)

- `bench.yml` gains a dispatch input `mode` (`throughput`, the default, or `scaling`) and a `run-name` naming the mode, rates and gate (so verify-m7 can find its runs). `mode: scaling` runs `test/load/scaling.sh` instead of `bench.sh` on both runners, 75-minute timeout; its result goes to `bench-results` like any other with `setup.mode: "scaling"` (never comparable to throughput results, ADR-0073).
- **Layout** (the design's: gateway 1 then 2 vCPUs, loadgen 1, upstream 1, no Redis): gateway `--cpuset-cpus 0`, then `0,2`; loadgen `1`; upstream `3`. On the x64 runner (2 cores × 2 threads, siblings 0-1 and 2-3) the gateway's two vCPUs are then on two different cores, each sharing it with a busy sibling, as its one vCPU does in the 1-vCPU run; putting them on 0-1 would make the "2 vCPUs" one core and measure SMT, not scaling. arm64 has no SMT, so the layout is neutral there.
- **No-op upstream.** go-httpbin costs about 70% of the gateway's CPU per request (ADR-0015), so one upstream vCPU could not answer a 2-vCPU gateway. `cmd/loadgen` gains a subcommand `loadgen serve -addr :8080 -bytes 1024`: a `net/http` server answering every request with 1,024 bytes from a fixed buffer, no logging; scaling runs use it as the upstream (the image already exists, so no new image).
- **Method** for n = 1, then n = 2: (1) **probe**: 3 loadgen runs of 10 s warm-up + 30 s at target `3 × r_arch × n` (r_arch = 4,600 x64, 8,800 arm64), `-conns 200`; P(n) = the median of their 2xx answers per second; during each probe the script reads each container's cgroup `cpu.stat` `usage_usec`; (2) **measured**: one run set of 5 runs of 30 s + 3 min at R(n) = 0.9 × P(n) rounded down to 100 (ADR-0016's 90% rule).
- **Valid** when both measured run sets are valid (ADR-0015) and every probe was **gateway-bound**: the gateway used ≥ 0.95 × n vCPU and loadgen and the upstream each ≤ 0.90 vCPU. A probe that is not gateway-bound measured loadgen or the upstream, not the gateway.
- **Pass:** valid, and S = R(2) ÷ R(1) ≥ **1.70** (within 15% of linear), on **both** arches (the design names no arch, so the stricter reading). Run time about 45 minutes per arch, in parallel.
- If loadgen or the upstream cannot keep up on one vCPU (the probes say so), NFR-02 cannot be measured as written on a 4-vCPU runner; nfr-load then stops for the user with the numbers. P29 offers, ahead of time, to judge NFR-02 on arm64 only should x64's SMT pairs be what fails it.

#### NFR-10 limiter keys (laptop compose)

- `chaos keys` (new subcommand, in the compose network) creates keys with the real limiter client and script (`rlredis.Client`, ADR-0021) as tenant-level keys `rl:v1:{t:<n>}:9001:300000:<digest>` with numeric tenant IDs 1 … 1,000,000 (managed-mode shape; policy ID 9001 is reserved for this test), one GCRA limit `{rate: 1, period: 5m, burst: 1}` (TTL 10 min). It calls the script with a 5 s timeout per call and at most 256 calls in flight.
- **Sweep:** read `used_memory` (`INFO memory`) and `DBSIZE` of the three primaries; create keys in steps to 250,000, 500,000 and 1,000,000, reading both after each step (2 s after the last call). Per-key memory at a step = Σ primaries Δ`used_memory` ÷ Σ primaries Δ`DBSIZE`; the step also checks Δ`DBSIZE` equals the keys created (exactly). Then the same sweep with sliding-window keys (`algorithm: sliding_window`, `{rate: 1, period: 5m}`, policy 9002). Replicas' Δ is printed for information.
- **Pass:** at 1,000,000 keys, ≤ **200 B** per key for GCRA keys **and** for sliding-window keys; key counts exact. Afterwards `SCAN MATCH rl:v1:*:900[12]:*` with `UNLINK` in batches of 1,000 on each primary (never `KEYS`). Compose Redis has `maxmemory 256mb` per node and `noeviction`; the estimate is about 35 MB per primary per sweep. About 8 minutes.

#### NFR-11 availability (laptop compose)

- **Soak:** gateway-1 with `test/verify/m7-soak.yaml` (three upstream endpoints; static API keys for four tenants of five consumers each; a local per-IP policy; a tenant-level Redis GCRA policy, `degraded`, 600/s per tenant; a consumer-level Redis sliding-window policy, `fail_open`; route timeouts `per_try: 2s`, `total: 5s`). loadgen sends `/bytes/1024` for 4 tenants at 500 rps each (2,000 rps), one of them at twice its tenant limit so 429s occur, 30 s warm-up + 10 min.
- **SLI** (the design's): gateway-caused 5xx = `gw_requests_total{origin="gateway", code_class="5xx"}` with `reason` not in `shed`, `circuit_open`, `fail_closed`, over all `gw_requests_total`, as deltas of gateway-1's `/metrics` read on the admin port before and after the run (scrape-independent).
- **Pass:** ≤ **0.1%**. The deliberate 5xx (`shed`, `circuit_open`, `fail_closed`) and upstream 5xx are printed separately. About 12 minutes.

#### NFR-15 noisy neighbour (laptop compose)

- **loadgen per-tenant mode:** `-tenant name=<name>,rate=<rps>,key-file=<path>[,toggle]` (repeatable; replaces `-rate` when present) sends each tenant's requests at its own rate with `X-API-Key` read once from the file (never printed or logged), and `-slot <duration>` (default 10 s) cuts the measured time into slots; a tenant with `toggle` sends only in odd slots (slot 0 off). The JSON gets `tenants.<name>`: achieved rate, status counts, p50/p99/p99.9, and per slot `{index, on, requests, p99}` from a per-slot HDR histogram; requests due in a slot's first second are left out of that slot's histogram (settling). loadgen's 99% rule applies per tenant over its scheduled requests.
- **Run:** gateway-1 with `test/verify/m7-noisy.yaml` (four tenants A–D, one consumer and static key each, made at run time by the script into a temp directory mounted into the loadgen container and removed on exit; a tenant-level Redis GCRA policy 200/s, burst 200, `fail_open`). Quiet tenants B, C, D at 100 rps each, all the time; noisy tenant A at **2,000 rps (10× its limit)** in odd slots. 30 s warm-up + 10 min = 60 slots = 30 (off, on) pairs.
- **Pass:** for each quiet tenant, the **median over the 30 pairs of p99(on slot) ÷ p99(off slot) < 1.10**. The pooled on/off p99s are printed too. About 12 minutes.

#### The chaos suite

| Scenario (design's Chaos scenarios) | Where | Script, target | Rules |
| --- | --- | --- | --- |
| Redis latency +50 ms | compose | new `test/chaos/redis-latency.sh`, `make chaos-redis-latency` | below |
| Redis full outage | compose | `test/chaos/redis-outage.sh` (in `test/verify/m3.sh`) | ADR-0021, ADR-0027 |
| Redis primary killed | compose | `test/chaos/redis-primary.sh` (in `m3.sh`) | ADR-0021, ADR-0034 |
| Control plane down 10 min | compose | `test/verify/m5.sh` rule (c) | ADR-0042, ADR-0052; plus (c6) below |
| Gateway `kill -9` under load | compose | new `test/chaos/gateway-kill.sh`, `make chaos-gateway-kill` | below |
| Upstream endpoint returns 5xx | compose | `test/verify/m4.sh` | ADR-0031 |
| Rolling restart under load | kind | NFR-14, ADR-0075 | zero failed requests |
| Noisy tenant at 10× limit | compose | NFR-15 above | p99 within 10% |

`make chaos` runs the compose scenarios one after another: `chaos-redis-latency`, `chaos-redis-outage`, `chaos-redis-primary`, `chaos-cp-outage`, `chaos-gateway-kill`, `verify-m4` and the NFR-15 run (about 40 minutes).

- **Redis latency +50 ms.** gateway-4 is recreated from a test-only compose override (`test/chaos/latency.compose.yaml`) with `SPILLWAY_REDIS_ADDR_MAP` mapping `redis-N:6379` to toxiproxy proxies `toxiproxy:2637N` (created through toxiproxy's API; the proxied ports stay inside the compose network, docs/ports.md), and `test/chaos/latency.yaml`: three routes, each with one Redis policy — `fail_open`, `fail_closed`, and `degraded` at 50/s, burst 50. The sender (`chaos latency`) drives each route at 100 rps for 60 s; from +20 s to +40 s a 50 ms `latency` toxic sits on all six proxies. Rules, over the toxic window minus its first second: fail_open ≥ 99.9% 2xx; fail_closed 100% 503 `fail_closed`; degraded admits within ±10% of 50 × 19 + 50 requests; each route's **median** latency ≤ its pre-toxic median + 10 ms (budget 5 ms + ADR-0027's 5 ms slack: calls end at the budget, not 50 ms later); and from +45 s every answer carries `Spillway-Decision-Time` again (Redis decides within 5 s of the toxic's removal). gateway-4 is removed afterwards.
- **Gateway `kill -9`.** `chaos gwkill` sends 300 rps round robin to gateway-1 … gateway-3 (static mode, one transport per gateway, 10 s timeout) for 40 s; at +20 s the script runs `docker kill -s KILL gateway-2`. The sender ejects a gateway at its first transport error, as a passive-checking load balancer would. Rules: zero failed requests to gateway-1 and gateway-3; every failed request went to gateway-2 and was sent before the sender's first observed failure from it. gateway-2 is removed afterwards (it belongs to the `replicas` profile).
- **(c6) Staleness alert.** The task that adds OB-05's alert rules (ADR-0075) adds a line to `m5.sh` rule (c): during the outage, compose Prometheus's `ALERTS{alertname="SpillwayConfigSyncStale", alertstate="firing"}` exists for gateway-1 before the outage ends (the design's "config-sync staleness alert fires").

### The verify-m7 plan

`make verify-m7` runs `test/verify/m7.sh`, PASS/FAIL lines in the style of `m1.sh` … `m6.sh`, every measured number printed, exit 1 if any line fails, no line ever re-run. `m7.sh <parts>` runs a subset (`bench`, `compose`, `kind`, or NFR numbers such as `10 11 15`) for development; only `all` (the default) is the exit evidence.

| NFR | Where | How verify-m7 gets the number | Load shape | Budget | Pass rule |
| --- | --- | --- | --- | --- | --- |
| NFR-01 | CI x64 + arm64 | dispatches `gh workflow run bench.yml --ref <branch> -f rate_x64=10000 -f rate_arm64=10000` at start, waits with `gh run watch`, reads the run's two result files from `origin/bench-results` (matched by `run_url`) | 5 × (30 s + 3 min) at 10,000 rps per arch, gateway 1 vCPU | 30 min (in parallel) | per arch: run set valid and median achieved, rounded, ≥ 10,000 rps per vCPU (ADR-0073 D1); with P19 approved, valid at 4,600 / 8,800 (then this dispatch is dropped and the NFR-05 run serves both) |
| NFR-02 | CI x64 + arm64 | dispatches `bench.yml -f mode=scaling`, reads its two results | probes + 5 × (30 s + 3 min) at R(1) and R(2) | 60 min (in parallel) | per arch: valid, gateway-bound probes, R(2)/R(1) ≥ 1.70 |
| NFR-03 | laptop compose | runs `test/verify/m2.sh all` (= `make verify-m2`), reads its NFR-03 line | ADR-0019/0021: alternating direct and gateway runs | 12 min | added p99 ≤ 2 ms |
| NFR-04 | laptop compose | runs `test/verify/m3.sh` (= `make verify-m3`), reads its NFR-04 line | ADR-0021: three alternating pairs at 1,000 rps, Redis policy | in m3's 15 min | 0 ≤ added p99 ≤ 5 ms, ADR-0027/0034 tolerance |
| NFR-05 | CI x64 + arm64 | dispatches `bench.yml -f gate=true` on HEAD, reads the verdicts (re-run included) | default rates 4,600 / 8,800, 5 runs, + 1 confirming set if flagged | 100 min (in parallel) | per arch: the final verdict, after the confirming re-run when the first set was flagged, is `pass`, compared against at least the nightly baseline (and the release baseline when one exists); `first` (nothing comparable), `invalid` or `fail` is FAIL |
| NFR-06 | laptop compose | `m3.sh`'s accuracy line (3 replicas), plus `test/accuracy/run.sh 1` and `run.sh 5` | 10 keys at 2× 100/s, burst 100; uniform, bursty, boundary, 60 s each | m3 + 8 min | zero over-admission (GCRA strict bound) at 1, 3 and 5 replicas |
| NFR-07 | laptop compose | `test/accuracy/run.sh <n> batched`, n = 1, 3, 5 (ADR-0069) | same, `lease_k: 10` | 12 min | over-admission ≤ n × 10 = 10, 30, 50 |
| NFR-08 | laptop compose, 5 managed gateways | runs `test/verify/m5.sh all` (= `make verify-m5`), rule (a) | 60 publishes | in m5's 18 min | p99 < 5 s (ADR-0042, ADR-0052) |
| NFR-09 | same | `m5.sh` rule (b) | 20 revocations | in m5 | each rejected on all 5 replicas < 5 s |
| NFR-10 | laptop compose | `m7.sh` step: `chaos keys` | 1M GCRA keys, then 1M sliding-window keys | 8 min | ≤ 200 B per key at 1M, both kinds; key counts exact |
| NFR-11 | laptop compose | `m7.sh` step: soak + gateway-1 `/metrics` deltas | 2,000 rps (4 tenants), 30 s + 10 min | 12 min | gateway-caused 5xx ≤ 0.1% of requests |
| NFR-12 | laptop compose | `m5.sh` rule (c) (+ (c6) once the alert rules exist) | 500 rps, control plane and Postgres down 600 s | in m5 | ADR-0042/0052 rules: zero failed requests, config frozen, recovery ≤ 60 s |
| NFR-13 | laptop compose | `m3.sh` chaos lines | ADR-0021: full outage, killed primary | in m3 | ADR-0021/0027/0034 rules |
| NFR-14 | laptop compose (reload) + kind (rolling restart) | `test/verify/m1.sh reload` (= `make verify-m1-reload`); then `make kind-up` and `test/kind/rollout.sh` (ADR-0075) | reload: 1,000 rps 60 s, 29 reloads; rollout: 300 rps HTTPS for 4 min, `kubectl --context kind-spillway -n spillway rollout restart deployment/spillway-gateway` at +60 s | 2 min + 25 min (kind up to down) | reload: every request 2xx, ≥ 25 reloads (ADR-0015); rollout: zero failed requests (transport error, timeout or non-2xx, warm-up included), ≥ 99% of target sent, all three pods replaced and Ready within 5 min |
| NFR-15 | laptop compose | `m7.sh` step: loadgen per-tenant mode | quiet 3 × 100 rps, noisy 2,000 rps toggled every 10 s, 30 s + 10 min | 12 min | each quiet tenant: median over 30 pairs of p99 on ÷ off < 1.10 |

The chaos suite's extra lines (Redis latency, gateway kill, upstream 5xx via `test/verify/m4.sh`) run in the compose part too (about 8 minutes) and count like any line.

**Order and total time of one full run.** Preflight (clean tree apart from the three local files, `gh auth status` shows ankitbisht23, HEAD on `origin`; otherwise the bench lines fail with the reason): 1 min. Dispatch the three bench runs (`VERIFY_M7_BENCH_RUNS=<ids>` reuses runs already dispatched for HEAD instead). Compose part, in this order, `make dev` first: m1 reload (2), m2 (12), m3 (15), accuracy strict 1 and 5 (8), batched 1/3/5 (12), m5 (18), NFR-10 (8), NFR-11 (12), NFR-15 (12), chaos latency, gateway kill, m4 (8): about **107 min**. Kind part: `docker compose down` (no `-v`), `make kind-up`, `make kind-smoke`, the rollout check, `make kind-down`, `make dev`: about **25 min**. Then wait for the bench runs if still going (normally finished; a 120-minute cap from dispatch, then FAIL "bench run not finished"). **Total ≈ 2 h 15 min; budget 2 h 30 min.** It needs kind *not* to be up at start, and brings compose down for the kind part itself, bringing it back after (ADR-0075's memory plan).

**Results and the README.** `m7.sh` writes the run's numbers and verdicts as JSON to `test/verify/out/m7-<UTC timestamp>.json` (`test/verify/out/` is gitignored). `make verify-m7 README=1` also rewrites the block between `<!-- verify-m7:begin -->` and `<!-- verify-m7:end -->` in `README.md` from that JSON: one table row per NFR with target, measured value, where, the date, commit and PASS/FAIL, and links to the bench result files. A FAIL row is written as a FAIL, never hidden. The verify task commits the block from the last of its full runs, by PR (as ADR-0003 asks for benchmark numbers).

**Leftovers.** On every exit `m7.sh` restores `deploy/compose/gateway/config.yaml`, removes gateway-2 … gateway-5 and any test override containers, puts gateways back in static mode, removes its temp key files, deletes its toxiproxy proxies, runs `make kind-down` if it brought kind up, and leaves `make dev`'s stack running.

### Stall tolerance

Measured stalls: usually 4–9 ms about every 15 s; sometimes 100–820 ms, a few per long run (ADR-0027, ADR-0034). There is **no re-run rule** anywhere in verify-m7: a failed line is reported failed. NFR-05's confirming re-run is part of the requirement itself (a flagged nightly result re-runs once), runs on GitHub's runners, and is not a re-run of a failed verify check. Per rule, why a stall alone cannot fail it:

- **NFR-01, NFR-02, NFR-05** run on GitHub's runners, not on the laptop; runner noise is handled by the design's own median of 5 runs and spread.
- **NFR-03, NFR-04, NFR-13, NFR-06 at 3 replicas** are `make verify-m2` and `make verify-m3` unchanged, with ADR-0021/0027/0034's tolerances (medians of three pairs; one stall burst ≤ 1,000 ms excused in NFR-04 gateway runs and in the killed primary's window check). **Exception, stated honestly:** the full outage's 10 ms line and the killed-primary rule for surviving primaries' keys can still fail on a stall, which is why `make verify-m3` fails some runs on this laptop; the user accepted that pass rate, and verify-m7 inherits it without a new tolerance.
- **NFR-06 at 1 and 5, NFR-07** judge admitted counts against the algorithm's bound on Redis `TIME` (strict) or on lease grant time plus monotonic elapsed (batched): a stall delays requests but cannot make Redis admit more than its bound, and lease timestamps are real service times for which ADR-0069's bound holds. Timeout 503s from a stall are reported, not judged.
- **NFR-08, NFR-09, NFR-12** keep ADR-0042/0052's rules: 5 s thresholds and a 60 s recovery against stalls of at most 0.82 s.
- **NFR-10** measures memory, which a stall does not change; calls have 5 s timeouts (> 0.82 s), so a stall slows the sweep and fails no call, and counts are checked exactly.
- **NFR-11:** route timeouts of 2 s per try and 5 s total exceed the longest stall, so a stall cannot turn into a gateway 504; a stall at 2,000 rps piles at most about 1,640 requests in flight, under the 4,096 in-flight cap (and shed answers are excluded anyway); Redis-related failures are `fail_open` or `degraded`, never 5xx. And 0.1% of 1.2M requests is 1,200.
- **NFR-14:** the client timeout is 10 s and loadgen schedules late requests instead of dropping them below 10,000 in flight; at 300 rps a 0.82 s stall leaves about 250 in flight. Achieved rate over 4 minutes is unaffected.
- **NFR-15:** an 820 ms stall overlaps at most two 10 s slots, so at most two of the 30 pairs; the median of 30 ratios ignores up to 14 bad pairs. The regular 4–9 ms stalls fall on on and off slots alike (their 15 s period is not a multiple of the 20 s pair).
- **Redis latency chaos:** medians (a stall affects far fewer than half the requests of a 19 s window) and admitted counts over the whole window, not per-request maxima.
- **Gateway kill:** judged by order (sent before the first observed failure) and a 10 s timeout, not by time thresholds.
- **(c6):** the alert fires after 60 s of staleness in a 600 s outage.

### Time budgets of the NFR runs

As in the table; outside verify-m7, each M7 task runs its own scenario at least three times within: NFR-02 60 min per dispatch, NFR-07 12 min per full set, NFR-10 8 min, NFR-11 12 min, NFR-15 12 min, chaos latency and kill 3 min each, kind rollout 25 min including kind up and down, a full verify-m7 2 h 30 min. A run over its budget is reported as such.

## Consequences

- One command measures all fifteen rows, about 2¼ hours, three times for the M7 exit; the README's table comes from it.
- verify-m7's full-run pass rate cannot exceed verify-m3's on this laptop, since it includes verify-m3's checks unchanged; the verify task reports the rate.
- loadgen grows a per-tenant mode and a `serve` subcommand; cmd/chaos grows `keys`, `latency` and `gwkill`.
- NFR-02 may not be measurable as written on a 4-vCPU runner; the probes will say so, and the user decides (P29).

## Alternatives considered

- **NFR-15 as two long phases (quiet, then noisy)** — one stall in the noisy phase fails it, and drift between phases counts as interference.
- **NFR-11 over the whole verify-m7 run** — the chaos and outage checks inject deliberate failures; a dedicated soak measures availability in normal operation.
- **NFR-10 through the gateway** — 1M keys at the gateway's rate takes longer and measures the same script writes.
- **Running kind and compose together** — does not fit in 8.3 GB (ADR-0075).
- **A k6 script for the soak and noisy tests** — loadgen already has the open model, HDR histograms and the JSON verify reads; k6 would be a second load tool for the same job.
