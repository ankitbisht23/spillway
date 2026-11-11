# Spillway

A horizontally scalable API gateway in Go with a Redis-backed distributed rate limiter, a Postgres-backed control plane and a React admin UI.

Status: M0 (foundations), M1 (proxy core) and M2 (local rate limiting) are done; M3 (distributed rate limiting) is next.

- [Requirements and build plan](docs/design.md)
- [Prerequisites and setup](docs/setup.md)
- [Ports](docs/ports.md)
- [Architecture decision records](docs/adr/)

## Benchmark

NFR-01 baseline from `bench.yml` on `main` at 1310f0c ([Actions run](https://github.com/ankitbisht23/spillway/actions/runs/41817046132)): the gateway pinned to 1 vCPU, plain HTTP/1.1, 1 KB responses from go-httpbin, loadgen at a constant arrival rate (open model). Each figure is the median of 5 runs of 30 s warm-up and 3 min measured, with the range across the runs in brackets ([Benchmark method](docs/design.md#benchmark-method), ADR-0015, ADR-0016).

| Arch | Hardware (GitHub runner, 4 vCPU) | Target rate | Achieved, rps per gateway vCPU | p50 | p99 | p99.9 | Result |
| --- | --- | --- | --- | --- | --- | --- | --- |
| x64 | AMD EPYC 7763, 2 cores × 2 SMT threads | 4,600 rps | 4,600 (4,600 – 4,600) | 0.63 ms (0.58 – 0.64) | 2.68 ms (2.33 – 2.99) | 4.21 ms (3.64 – 8.94) | [results/x64/20261104T105219Z.json](https://github.com/ankitbisht23/spillway/blob/bench-results/results/x64/20261104T105219Z.json) |
| arm64 | Neoverse N2 (`implementer 0x41 part 0xd49`), 4 cores | 8,800 rps | 8,800 (8,764 – 8,800) | 0.90 ms (0.85 – 41.6) | 47.1 ms (3.21 ms – 1.19 s) | 69.1 ms (12.3 ms – 1.20 s) | [results/arm64/20261104T105228Z.json](https://github.com/ankitbisht23/spillway/blob/bench-results/results/arm64/20261104T105228Z.json) |

Capacity: the first run, at 10,000 rps on both runners, saturated the gateway (run sets invalid, latency of a full queue) at 5,369 rps per vCPU on x64 (5,164 – 5,529) and 9,924 on arm64 (9,796 – 9,975): [results/x64/20261104T094901Z.json](https://github.com/ankitbisht23/spillway/blob/bench-results/results/x64/20261104T094901Z.json), [results/arm64/20261104T094904Z.json](https://github.com/ankitbisht23/spillway/blob/bench-results/results/arm64/20261104T094904Z.json).

On x64 the gateway's CPU 0 and loadgen's CPU 1 are SMT threads of one core. On arm64, 8,800 rps is close to that runner's limit: the five runs' p99 were 3.2, 5.3, 47, 537 and 1,193 ms.

## Chaos

Redis failure behaviour (NFR-13, [Chaos scenarios](docs/design.md#chaos-scenarios), ADR-0021, ADR-0025), measured on the dev stack: Apple silicon laptop with 14 CPUs, Docker Desktop, a Redis 8.10.3 cluster of 3 primaries and 3 replicas with `cluster-node-timeout 3000` and `cluster-require-full-coverage no`, the 5 ms Redis budget. Reproduce with `make chaos-redis-outage` and `make chaos-redis-primary`; each prints its checks and always brings the cluster back.

| Scenario | Injected | Measured | Result |
| --- | --- | --- | --- |
| Full outage | `docker pause` of all six nodes while loadgen sends 200 rps per route; 20 s measured | fail_open: 4,000/4,000 allowed. fail_closed: 4,000/4,000 answered 503. degraded (300/s over 3 replicas): 99.5/s admitted on each replica. 28,000/28,000 Redis checks within 7 ms (budget plus 2 ms slack), 276–320 of them timeouts before the breakers opened | PASS in 7 of 9 runs; each FAIL was 1 of 28,000 checks over 7 ms |
| Killed primary | `docker kill` of the primary owning slot 0 while 30 keys (10 per primary) get 20 rps each for 40 s | the surviving primaries' keys: 16,000/16,000 Redis decisions. The killed primary's keys: fail_open decisions only between the kill and their failover, then Redis decisions. Failover window 4.4–6.2 s over 22 runs, median 5.5 s | PASS in 19 of 21 runs; each FAIL was 2–3 of 16,000 surviving-key requests answered by `fail_open` |

The failover window runs from the kill to the gateway's first Redis decision for the killed primary's keys. In a typical run (5.47 s): the gateway's health checks open the dead node's breaker after 0.35 s; the surviving primaries mark it failed after 4.57 s (`cluster-node-timeout` plus the gossip that reaches a majority); its replica wins the election after 5.38 s (a rank-0 delay of 0.5–1 s); and the gateway, which reloads its slot map every 200 ms while a breaker is open, decides in Redis 0.05–0.09 s after that (0.22 s in another run). Almost all of the window is Redis Cluster's own failure detection and election, so a shorter `cluster-node-timeout` is the lever for a shorter window.

Neither rule has any tolerance, so a stall of the Docker Desktop VM can fail a run. In the killed-primary FAILs, gateway-1's access log shows stalls of 5–15 ms in which every in-flight request slowed down at once, Redis-decided ones included; the few calls to a surviving primary that overran the 5 ms budget then failed open, as RL-23 says they should. Each full-outage FAIL was one check over 7 ms among the ~300 calls per run that time out at the 5 ms deadline, which a timer fired more than 2 ms late explains. Run again on a FAIL; on a quiet machine a run usually passes.

## Licence

MIT, see [LICENSE](LICENSE).
