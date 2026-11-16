# Spillway

A horizontally scalable API gateway in Go with a Redis-backed distributed rate limiter, a Postgres-backed control plane and a React admin UI.

Status: M0 (foundations), M1 (proxy core), M2 (local rate limiting), M3 (distributed rate limiting), M4 (resilience), M5 (control plane and auth) and M6 (admin UI) are done; M7 (performance and hardening) is next.

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

Redis failure behaviour (NFR-13, [Chaos scenarios](docs/design.md#chaos-scenarios), ADR-0021, ADR-0025, ADR-0027), measured on the dev stack: Apple silicon laptop with 14 CPUs, Docker Desktop, a Redis 8.10.3 cluster of 3 primaries and 3 replicas with `cluster-node-timeout 3000` and `cluster-require-full-coverage no`, the 5 ms Redis budget. Reproduce with `make chaos-redis-outage` and `make chaos-redis-primary`; each prints its checks and always brings the cluster back.

| Scenario | Injected | Measured | Result |
| --- | --- | --- | --- |
| Full outage | `docker pause` of all six nodes while loadgen sends 200 rps per route; 20 s measured | fail_open: 4,000/4,000 allowed. fail_closed: 4,000/4,000 answered 503. degraded (300/s over 3 replicas): 99.5/s admitted on each replica. 28,000/28,000 Redis checks within 10 ms (budget plus 5 ms slack), 269–335 of them timeouts before the breakers opened | PASS in 2 of 3 runs on 8a459b7 plus the M3 review fixes; the FAIL was 1 of 28,000 checks over 10 ms. Under ADR-0021's earlier 7 ms line it passed 7 of 9 runs, each FAIL 1 check over 7 ms |
| Killed primary | `docker kill` of the primary owning slot 0 while 30 keys (10 per primary) get 20 rps each for 40 s | the surviving primaries' keys: 16,000/16,000 Redis decisions. The killed primary's keys: fail_open decisions only between the kill and their failover, then Redis decisions. Failover window 4.4–6.2 s over 22 runs, median 5.5 s | PASS in 19 of 21 runs; each FAIL was 2–3 of 16,000 surviving-key requests answered by `fail_open` |

The failover window runs from the kill to the gateway's first Redis decision for the killed primary's keys. In a typical run (5.47 s): the gateway's health checks open the dead node's breaker after 0.35 s; the surviving primaries mark it failed after 4.57 s (`cluster-node-timeout` plus the gossip that reaches a majority); its replica wins the election after 5.38 s (a rank-0 delay of 0.5–1 s); and the gateway, which reloads its slot map every 200 ms while a breaker is open, decides in Redis 0.05–0.09 s after that (0.22 s in another run). Almost all of the window is Redis Cluster's own failure detection and election, so a shorter `cluster-node-timeout` is the lever for a shorter window.

Stalls of the host and of the Docker Desktop VM can still fail a run. The full-outage line is 10 ms, the 5 ms budget plus a 5 ms slack for those stalls (ADR-0027), with no count allowance: one check over 10 ms fails the run; the script also prints how many checks ended within 5 ms and within 7 ms. The killed-primary rules have no tolerance. In the killed-primary FAILs, gateway-1's access log shows stalls of 5–15 ms in which every in-flight request slowed down at once, Redis-decided ones included; the few calls to a surviving primary that overran the 5 ms budget then failed open, as RL-23 says they should. A check that fails is reported as failed; the scripts never run it again (ADR-0027).

Upstream endpoint failures (GW-08, GW-09, GW-12, GW-13, OB-02, [Chaos scenarios](docs/design.md#chaos-scenarios), ADR-0030, ADR-0031), measured on the same laptop with `make verify-m4`: gateway-1 with one upstream of three endpoints (`p2c`, health checks every 1 s, default outlier detection, breaker and retry budget, 3 attempts per request), 500 rps for 40 s, the fault on the third endpoint after 10 s.

| Scenario | Injected | Measured | Result |
| --- | --- | --- | --- |
| Endpoint returns 5xx | `upstream-fault` answers 503 to everything | the endpoint out of rotation within the first scrape after the fault (1.0 s); 5,001/5,001 requests in the 10 s from the fault answered 2xx; 5 retries, all within budget (930 allowed in the tightest window); each retried request's trace in Tempo with one span per attempt; the breaker stays closed | PASS in 6 of 6 runs on the M4 exit branch |
| Endpoint refuses connections | `upstream-fault` closes its listener and connections | the same, with connect errors retried: 5,001/5,001 answered 2xx, 5 retries | PASS in 6 of 6 runs |

With one endpoint of three failing, outlier ejection after 5 consecutive failures removes it, and each of those failed attempts is retried on another endpoint, so no client sees the failure. The breaker opens only when most attempts to an upstream fail, which one bad endpoint of three never causes (ADR-0031).

Control-plane propagation and outage (NFR-08, NFR-09, NFR-12, CP-07, [Chaos scenarios](docs/design.md#chaos-scenarios), ADR-0042, ADR-0052), measured on the same laptop with `make verify-m5`: gateway-1 … gateway-5 in managed mode against the compose control plane over mTLS, each check run once per run.

| Check | Injected | Measured | Result |
| --- | --- | --- | --- |
| Publish reaches every replica (NFR-08) | 60 publishes, each a route edit, validate and publish | from `published_at` to the last of the five replicas' first apply heartbeat, both on Postgres's clock: p50 3–4 ms, p99 (the maximum at N = 60) 6–44 ms, against 5 s | PASS in 7 of 7 runs |
| Revoked key rejected everywhere (NFR-09) | 20 keys, each issued, seen working on all five, then revoked | from just before the revoke request to the last replica's first 401, probes every 50 ms: p50 51–52 ms, max 54–59 ms, against 5 s; the identity path itself (revocation to the last apply heartbeat, Postgres clock) p50 3 ms, max 5–9 ms | PASS in 7 of 7 runs |
| Control plane down for 10 minutes (NFR-12) | `docker compose stop` of the control plane and Postgres for 600 s under 500 rps over the five gateways; gateway-3 restarted at +300 s | 347,774–347,899 requests, all 2xx; every scrape in the outage at the versions from before it, ready, with the sync age growing (603–630 s at the end); gateway-3 ready from its on-disk snapshot 5.0 s after its restart; after the control plane was back, all five live again in 8.1 s and synced within 11.3–11.5 s | PASS in 6 of 6 runs with the reconnect fix; the first run, before it, failed only its recovery rule (below) |

The first full run found a real defect: three gateways had not reconnected 60 s after the control plane came back. While the control plane is stopped its name does not resolve, and gRPC's default DNS resolver retries with its own backoff of up to 120 s, so after 10 minutes a gateway could stay away for 2 minutes. Managed gateways now resolve the control plane's name on every dial and reconnect within the 5 s backoff (ADR-0052). Every timeout in the check is at least 4× the 820 ms worst stall measured on this laptop, and no check is ever run again (ADR-0042).

## Admin UI

The M6 exit (UI-01, UI-02, NFR-08, ADR-0061), measured on the same laptop with `make verify-m6`: gateway-1 … gateway-5 in managed mode; Playwright in Chromium drives the control plane's embedded UI as `test-admin` after a real Keycloak login, while an independent probe in the compose network sends a heavy client (200 rps) and a light one (10 rps) through all five gateways. Each stage is judged over a 20 s window starting 1 s after the last of the five gateways reported applying it.

| Check | Done in the UI | Measured | Result |
| --- | --- | --- | --- |
| A publish reaches every replica, from the click (NFR-08) | 23 publishes: 20 route edits in the Routes screen, then the shadow, enforce and rollback publishes below | click to the publish answer (host clock) plus `published_at` to the last of the five apply heartbeats (Postgres clock): p50 51–53 ms, p99 (the maximum at N = 23) 54–71 ms, against 5 s; the UI part at most 53 ms, the propagation part at most 27 ms | PASS in 7 of 7 runs |
| Shadow | policy `m6-limit` (local GCRA, 50/s with a burst of 50, keyed by a request header) created in the Policy editor in shadow mode, published from the diff with a note | 4,000/4,000 heavy and 200/200 light requests answered 200, by all five gateways | PASS in 7 of 7 runs |
| Reviewed | its shadow report over the last 15 minutes | the report shows would-denies; the heavy client's key 3,638–3,683 would-denies, the light client's key absent | PASS in 7 of 7 runs |
| Enforced | promoted from the report, published from the diff with a note | 3,000 of 4,000 heavy requests (75.0 %) answered 429 with `"m6-limit";r=0` and `Retry-After`, the rest 200; light 200/200 | PASS in 7 of 7 runs |
| Rolled back | History, roll back to the version before the policy, with a note | 4,000/4,000 heavy and 200/200 light requests answered 200 | PASS in 7 of 7 runs |
| The whole run | 21,608–21,676 requests over 103 s; the browser watched throughout | only 200s and 429s, and every 429 between the enforce publish and the rollback's apply; zero console errors; no token or key in Web Storage, cookies, or any of the 204–205 request URLs, 78 navigations and 74 history entries | PASS in 7 of 7 runs |

A run takes about 2.5 minutes. Every Playwright timeout is at least 12× the 820 ms worst stall measured on this laptop; there are no retries, and no check is ever run again (ADR-0061). The CI `e2e` job runs the main flows (login, publish with diff, shadow → promote → rollback, issue/rotate/revoke a key, drain an endpoint, roles, live screens) against the compose stack with static gateways on every PR.

## Licence

MIT, see [LICENSE](LICENSE).
