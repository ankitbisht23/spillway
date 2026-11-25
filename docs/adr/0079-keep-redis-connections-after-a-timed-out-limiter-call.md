# 0079. Keep Redis connections after a timed-out limiter call

- **Status:** Rejected (2026-11-25): the boundary 503s are accepted as a known limit instead (option (a), below)
- **Date:** 2026-11-25
- **Requirement IDs:** RL-16, RL-22, RL-23, NFR-06, NFR-07

## Context

ADR-0078 measured the accuracy test's boundary pattern at 39–72 % 503s (ADR-0071 D6b's threshold is 10 %) and found connection churn amplifying it. During the boundary minute the primaries accepted 5,461–20,869 new connections, against 215–782 in the bursty minute. Their main-thread CPU per executed call was 96–98 µs, against 31–45 µs bursty. A primary that spends about 100 µs per call answers about 50 calls within one 5 ms budget, against about 630 offered to it in each 20 ms burst.

The user decided twice:

1. **2026-11-24: fix the churn first.** A limiter call that runs out of its budget keeps its connection. What a client sees, the failure modes and the budget stay as they are. If that lowers the 503s and regresses nothing, it ships even if the share stays above 10 %, and the residual is recorded as a known limit (option (a)).
2. **2026-11-25: add a refund of late admissions.** The first build cut the 503s and the churn, but boundary 2xx fell 20–30 % and 429s rose three to four times. Calls that Redis ran after their request already had its failure-mode answer still spent tokens. Rule for the second build: ship only if boundary 2xx is at least main's lowest run (strict 37,676, batched 49,383) and nothing regresses. Otherwise, fall back: do not merge the connection change, and record main's boundary 503s as option (a).

Both builds are below. The second missed the 2xx rule in all six runs, so the fallback applies.

## Decision

**Option (a).** The limiter keeps its current connection handling: `ContextTimeoutEnabled: true`, and a call that times out closes its connection (ADR-0021, ADR-0025). Nothing in this ADR's mechanism ships, and the code is as on main. The boundary pattern's 503s are accepted as a **known limit of the 5 ms Redis budget under synchronized bursts**:

- **The pattern.** In `make accuracy`'s boundary pattern, each key's requests arrive in 20 ms bursts at window boundaries, and the harness's policy is `fail_closed`. On the dev laptop (Docker Desktop, 14 CPUs, three primaries), main 4487160 answered 55–61 % of the boundary requests 503 in strict mode and 33–42 % in batched mode (three runs each, table below). Every 503 is a `timeout`, at the budget.
- **Why it is a limit and not a defect.** Each burst offers a primary several times the single-key script calls it can answer within 5 ms (ADR-0025, ADR-0078). Neither M7 feature cuts the number of calls that arrive together (ADR-0078). Keeping the connections lets more of those calls reach Redis, but then fewer of them are answered within the budget (below). The budget (RL-22), the failure modes (RL-23) and the no-retry rule (RL-16) all stay as they are.
- **What still holds.** Over-admission: 0 in strict mode and at most 20 of 30 in batched mode, so NFR-06 and NFR-07 pass in every run. Uniform has 0–46 503s and bursty 287–1,477 (at most 1.2 %).
- **D6b's 10 % threshold** stays a reported number, not a pass rule (ADR-0021, ADR-0071). The boundary 503s are reported as this known limit, and they are no longer an open item.
- A real Redis deployment, or a pattern without synchronized bursts, has more headroom. A future change that reduces the calls per burst (for example single-flight reservations with waiters, close to RL-20) would need its own decision and its own measurement against these rows.

## What was tried

### Build 1: keep the connection

- The limiter's cluster client stopped giving the request's deadline to its sockets (`ContextTimeoutEnabled` off). Its reads and writes were bounded by a **hard timeout of 1 s** instead (`ReadTimeout`, `WriteTimeout`). 1 s is above the longest stall seen on the laptop (820 ms, ADR-0034) and short enough for a stuck node to free its connections within about a second.
- `Call` and `Refund` ran each call on a goroutine of its own and returned a `timeout` at `ctx.Done()`, exactly as before. The goroutine finished the call: the late reply was read off and discarded, and the connection went back to the pool. Waiting for a pool turn or a dial still ended with the context, and no script call was sent after the context ended.
- Pool size was unchanged (go-redis's 5 × `GOMAXPROCS` per node, ADR-0025), as were the dial settings and `MinIdleConns` 0 (ADR-0026). The scope was the limiter's cluster client only. The health checks kept their 50 ms request-style deadline.

### Build 2: also refund late admissions (RL-23 flow)

- A call still in flight at its budget returned its `timeout` with a `Late` accessor that waited for the call's reply.
- When the request ended without being let through (a `fail_closed` 503 or a degraded limiter's 429: the paths that already refund what the request holds), each late call went to one background goroutine. If the late reply admitted, the goroutine reversed what the script wrote, per limit by `granted_i`: the cost for strict and shadow limits, the whole grant g for a batched reservation, and the cost counted for a quota. It refunded as of the reply's `now_us`, through the existing Refund path, under one 1 s context. It never retried, and it counted failures in a new `gw_ratelimit_late_refunds_total{result}` and in `gw_ratelimit_refund_failures_total`.
- A request that was let through (`fail_open`, or a degraded admit) kept its late admission. A late call was never among the request's answered calls, so it could not be refunded twice.
- Unit tests covered: a late admission refunded after a fail_closed 503 and after a degraded deny; a 2xx never refunded; a denied or failed late reply not refunded; a failed refund counted and not retried; a late lease grant refunded whole; no double refund. An integration test on the compose cluster covered a late reservation's `Granted` refunded exactly. All of them passed (integration 3 of 3). They are not on main, since the code was reverted.

## Results

`make accuracy REPLICAS=3`, three runs per mode and build, on the same laptop and stack as ADR-0078 (Apple silicon, Docker Desktop with 14 CPUs, gateways at the 1 % trace ratio). "Before" is main 4487160. A sampler held one connection to each primary and read `INFO stats cpu commandstats` once a second. A window runs from the end of the previous pattern to the end of its own, as the harness's summary lines show.

- **New connections** is the increase of `total_connections_received`, summed over the three primaries.
- **Script calls** is the `EVALSHA` and `EVAL` calls the primaries executed; build 2's include its refunds.
- **CPU per call** is main-thread CPU (`used_cpu_sys_main_thread` + `used_cpu_user_main_thread`) over script calls, per primary.
- **Late refunds** is `gw_ratelimit_late_refunds_total{result="refunded"}` over the whole run, from Prometheus. It is a lower bound, because the replicas' last increments before their removal may be missed. No late refund failed in any run.

Boundary pattern:

| Run | 2xx | 429 | 503 | 503 share | Admitted per key | Redis calls per admitted | Max over-admission | New connections | Script calls | CPU per call | p99 decision | Late refunds |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| strict, main 1 | 40078 | 10791 | 69131 | 57.6 % | 66.8/s | 2.84 | 0 | 37,361 | 49,493 | 86–93 µs | 74.8 ms | — |
| strict, main 2 | 37676 | 9021 | 73303 | 61.1 % | 62.8/s | 3.04 | 0 | 37,556 | 45,382 | 90–108 µs | 83.2 ms | — |
| strict, main 3 | 44168 | 9308 | 66524 | 55.4 % | 73.6/s | 2.61 | 0 | 38,991 | 54,942 | 81–84 µs | 69.1 ms | — |
| strict, build 1, 1 | 31727 | 40953 | 47320 | 39.4 % | 52.9/s | 3.05 | 0 | 502 | 87,994 | 41–45 µs | 65.7 ms | — |
| strict, build 1, 2 | 31422 | 40379 | 48199 | 40.2 % | 52.4/s | 3.10 | 0 | 306 | 87,647 | 41–48 µs | 67.9 ms | — |
| strict, build 1, 3 | 32183 | 38401 | 49416 | 41.2 % | 53.6/s | 3.09 | 0 | 306 | 90,025 | 40–47 µs | 60.9 ms | — |
| strict, build 2, 1 | 32299 | 24706 | 62995 | 52.5 % | 53.8/s | 3.32 | 0 | 412 | 117,685 | 32–50 µs | 64.8 ms | 33,784 |
| strict, build 2, 2 | 34914 | 27992 | 57094 | 47.6 % | 58.2/s | 3.02 | 0 | 306 | 115,706 | 33–50 µs | 80.5 ms | 30,316 |
| strict, build 2, 3 | 29725 | 23157 | 67118 | 55.9 % | 49.5/s | 3.66 | 0 | 305 | 119,004 | 35–51 µs | 75.9 ms | 35,488 |
| batched, main 1 | 49600 | 23908 | 46492 | 38.7 % | 82.7/s | 1.55 | 17 of 30 | 25,555 | 34,672 | 97–138 µs | 114.7 ms | — |
| batched, main 2 | 50977 | 29255 | 39768 | 33.1 % | 85.0/s | 1.45 | 17 of 30 | 22,871 | 38,598 | 98–124 µs | 166.1 ms | — |
| batched, main 3 | 49383 | 20874 | 49743 | 41.5 % | 82.3/s | 1.62 | 20 of 30 | 28,194 | 34,063 | 108–109 µs | 175.3 ms | — |
| batched, build 1, 1 | 35768 | 45486 | 38746 | 32.3 % | 59.6/s | 2.17 | 0 of 30 | 306 | 69,588 | 45–50 µs | 64.9 ms | — |
| batched, build 1, 2 | 39110 | 47094 | 33796 | 28.2 % | 65.2/s | 1.84 | 4 of 30 | 306 | 63,478 | 44–53 µs | 62.7 ms | — |
| batched, build 1, 3 | 34810 | 47659 | 37531 | 31.3 % | 58.0/s | 2.14 | 6 of 30 | 306 | 66,664 | 48–54 µs | 64.3 ms | — |
| batched, build 2, 1 | 42694 | 31734 | 45572 | 38.0 % | 71.2/s | 1.86 | 8 of 30 | 280 | 80,035 | 36–96 µs (*) | 78.8 ms | 19,780 |
| batched, build 2, 2 | 42492 | 35062 | 42446 | 35.4 % | 70.8/s | 1.80 | 6 of 30 | 306 | 81,721 | 44–64 µs | 95.9 ms | 19,382 |
| batched, build 2, 3 | 44105 | 34782 | 41113 | 34.3 % | 73.5/s | 1.74 | 5 of 30 | 303 | 81,534 | 41–61 µs | 66.8 ms | 18,305 |

(*) 96 µs is a primary that executed only 7,261 calls in that window; the other two were at 36 and 50 µs.

Uniform and bursty patterns (per run, 1 / 2 / 3):

| | Uniform 503 | Bursty 2xx | Bursty 503 | Bursty new connections | Bursty CPU per call |
| --- | --- | --- | --- | --- | --- |
| strict, main | 15 / 3 / 0 | 60201 / 60117 / 59953 | 675 / 778 / 1477 | 1,008–1,790 | 29–45 µs |
| strict, build 1 | 3 / 5 / 4 | 59973 / 59642 / 59916 | 997 / 1581 / 1037 | 275–282 | 29–44 µs |
| strict, build 2 | 3 / 6 / 15 | 60076 / 60359 / 60239 | 1398 / 574 / 1014 | 278–378 | 29–44 µs |
| batched, main | 32 / 5 / 46 | 60116 / 60064 / 60013 | 513 / 287 / 488 | 601–794 | 32–38 µs |
| batched, build 1 | 3 / 0 / 1 | 60033 / 60014 / 59831 | 479 / 422 / 626 | 281–283 | 32–40 µs |
| batched, build 2 | 19 / 29 / 7 | 60034 / 60152 / 60222 | 1041 / 862 / 682 | 274–278 | 30–51 µs |

Every run of every build passed NFR-06 and NFR-07 (18 of 18). Every 503 was a `timeout` (`conn` 0, `script` 0).

Other checks on build 1 (not repeated on build 2, which failed the 2xx rule first):

- `make verify-m6`: 1 of 1 PASS.
- `make verify-m3`: 1 run, FAIL on one check. NFR-04's third gateway run voided its pair with 2 timeouts 3.3 s apart. Its accuracy and both chaos tests passed. verify-m3 failing some runs on this laptop was already accepted.
- `make chaos-redis-outage`: 4 of 4 PASS. `make chaos-redis-primary`: 6 of 7 PASS. The one failure was 1 of 16,000 requests on a surviving primary's key, answered after 7 ms.

### Why neither build ships

- **Build 1** removed the churn: about 300 new connections in the boundary minute instead of 23,000–39,000, and CPU per call at 40–54 µs instead of 81–138 µs. It cut the 503 share by about a third. But more calls reached Redis (about 1.8 times as many), the queue per primary stayed longer than 5 ms, and more admissions landed after their requests had been answered 503. So 2xx fell 20–30 % and 429s rose three to four times. Under-admission is client-visible even though NFR-06 bounds only over-admission.
- **Build 2** refunded 30,000–35,000 late admissions per strict run and 18,000–20,000 per batched run, with no failures. Even so, boundary 2xx stayed below main's lowest run in all six runs. Strict was 29,725–34,914 against 37,676, and batched 42,492–44,105 against 49,383. The 503 share went back up, to 48–56 % strict (main 55–61 %) and 34–38 % batched (main 33–42 %). Why:
  - The 2xx are bounded by how many calls Redis answers within 5 ms, not by tokens.
  - The refunds are one more Redis call each, made during the bursts. Script calls in the boundary minute rose to 116,000–119,000 strict, from 88,000–90,000 in build 1, so the queues grew again.
  - A refund lands after its late reply plus its own wait in the queue, so the next burst's in-time calls often still find the tokens spent: 429s stayed 2.3–3.1 times main's in strict mode and 1.2–1.7 times in batched mode (run for run).
- **Main** closes a timed-out call's connection, which acts as crude admission control: calls without a connection fail at their budget without reaching Redis, so fewer calls queue and more of those sent are answered in time.

## Consequences

- No code change: the limiter, its connection handling and RL-16's `ContextTimeoutEnabled: true` stay as on main. No metric is added, and no design text changes. The P-items that build 2 would have needed (RL-16's wording and a `gw_ratelimit_late_refunds_total` row) fall away.
- The boundary 503s are a documented known limit (Decision). Later accuracy runs report them as before. ADR-0071 D6b's open item is closed by this ADR.
- The rows above are the baseline for any later attempt to reduce calls per burst.

## Alternatives considered

- **Ship build 1** (fewer 503s, no churn, but 20–30 % fewer 2xx and 3–4× the 429s under synchronized bursts). Not taken: the user's second decision asked for 2xx at main's level.
- **Ship build 2.** Not taken: 2xx stayed below main's lowest run in every run, and the user's rule for that case is the fallback.
- **Keep `ContextTimeoutEnabled` and hide the deadline from go-redis** (a context whose `Deadline` is hidden but whose `Done` is the request's). Same behaviour as build 1, with a reliance on go-redis internals. It was not tried, because build 1 already measured the behaviour.
- **Re-open closed connections faster** (`MinIdleConns`, a larger `PoolSize`). Each dial and handshake costs Redis main-thread time, which is the amplifier ADR-0078 measured, and ADR-0025 and ADR-0026 measured no gain.
- **Autopipelining or one pipelined connection per node.** This would change the call contract (one call, one reply, no resend: RL-16), which is a much larger change.
- **Single-flight reservations with waiters (close to RL-20).** This would cut the number of calls per burst, which is what both builds show is needed. It needs its own decision.
