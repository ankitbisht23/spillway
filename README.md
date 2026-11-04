# Spillway

A horizontally scalable API gateway in Go with a Redis-backed distributed rate limiter, a Postgres-backed control plane and a React admin UI.

Status: design complete, build starting at M0.

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

## Licence

MIT, see [LICENSE](LICENSE).
