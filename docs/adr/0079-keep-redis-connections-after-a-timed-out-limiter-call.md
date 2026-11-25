# 0079. Keep Redis connections after a timed-out limiter call

- **Status:** Proposed (the user decides whether it ships: see "The open question")
- **Date:** 2026-11-25
- **Requirement IDs:** RL-22, RL-23, NFR-06, NFR-07

## Context

ADR-0078 measured the accuracy test's boundary pattern at 39–72 % 503s (ADR-0071 D6b's threshold is 10 %) and found connection churn amplifying it. During the boundary minute the primaries accepted 5,461–20,869 new connections, against 215–782 in the bursty minute. Their main-thread CPU per executed call was 96–98 µs, against 31–45 µs bursty. A primary that spends about 100 µs per call answers about 50 calls within one 5 ms budget, against about 630 offered to it in each 20 ms burst. The user decided (2026-11-24) to fix the churn first: a limiter call that runs out of its budget must keep its connection. What a client sees, the failure modes and the budget stay as they are. If the fix lowers the 503s and regresses nothing, it ships even if the share stays above 10 %. In that case this ADR records the residual as a known limit of the 5 ms budget under synchronized bursts (ADR-0078's option (a)).

Where the churn comes from, in go-redis v9.23.0 with the limiter's `ContextTimeoutEnabled: true` (ADR-0021):

- **The reply read.** The socket's read deadline is the request's deadline. A call that times out ends its read with `i/o timeout`, and go-redis treats any read error as a bad connection (`isBadConn` with `allowTimeout` false). It closes the connection, because a reply may still be on its way. The next call that finds no idle connection dials.
- **The handshake.** go-redis dials in the background and hands the new connection to the waiting call, which then sends `HELLO` and `CLIENT SETINFO` under that call's deadline. A handshake that overruns the budget closes the new connection too. ADR-0078 counted 4,184–15,062 `HELLO`s per primary in the boundary minute.
- **The dial** is not part of the churn. In v9.23.0 the pool dials under its own context, `DialTimeout` 5 s per attempt and up to 5 attempts. A dial whose caller has given up still puts its connection in the pool.

## Decision

### The mechanism

- The limiter's cluster client (`internal/ratelimit/redis`) no longer gives the request's deadline to its sockets: `ContextTimeoutEnabled` is off. Its sockets' read and write deadlines are the **hard timeout** instead (`ReadTimeout` and `WriteTimeout`).
- `Call` and `Refund` run each call on a goroutine of its own. The caller gets its answer when the call finishes or when its context ends, whichever comes first. If the context ends first, the answer is a `timeout` error, as before. Its `Node` is the primary when the call had found it by then, and its `Err` is the context's error. The gateway applies the failure modes to it exactly as before (RL-23). Neither the budget, nor the 503 and its `Retry-After`, nor `gw_redis_errors_total` changes.
- The goroutine carries the call on with the request's context:
  - **Waiting for a connection still ends with the context.** go-redis's pool turn and its wait for a dial both follow `ctx.Done()`. So a call that has no connection when the budget ends is still never sent.
  - **A connection that the call holds keeps it to the end.** The script call is written and its reply read under the hard timeout. If the call took a new connection, its handshake is too. The late reply is read off, decoded and discarded, and the connection goes back to the pool.
  - **No script call is sent after the context ends.** `send` checks the context after the version check (an `INFO` on a node's first call), before `EVALSHA`. It checks again before the `EVAL` that follows `NOSCRIPT` (RL-16). One case is left: a call whose connection was handed over just before the deadline, and whose handshake ends after it. Its `EVALSHA` goes out after the budget. That is what a call that was sent but answered late already does today (ADR-0025): Redis runs it, and it may deduct for a request that got its failure-mode answer. It can under-admit, never over-admit. It can happen at most once per new connection.
  - **The breakers count the call when it ends** (ADR-0025). A late reply counts as replied and resets the node's broken-connection count. A late failure is classified with the expired context, so it is a `timeout` and counts as nothing, as before.

### Hard timeout, pool and dial

- **Hard timeout: 1 s**, a constant (`hardTimeout`). It bounds every read and write on a request connection, a new connection's handshake included. Before, they were bounded by the request's deadline, or by go-redis's 5 s default outside requests. Why 1 s:
  - It is above the longest host or Docker Desktop stall seen on the dev laptop (820 ms, ADR-0034), so a stall does not close connections.
  - It is two orders of magnitude above a queued call in the boundary pattern (tens of milliseconds).
  - It is short enough that a node that stopped answering frees every connection within about a second. The health checks open its breaker within 250–350 ms (ADR-0025), and after that no call is sent to it. So a stuck node holds at most the pool's connections per gateway, each for at most 1 s.
- The same deadlines now bound the warm-up's, refill's and slot map's reads on this client, at 1 s per read. Their own contexts (5 s, ADR-0024 and ADR-0026) still bound each pass.
- **Pool size** is unchanged: go-redis's default for a cluster, 5 × `GOMAXPROCS` per node (ADR-0025). A call holds its pool turn until its goroutine hands the connection back, so the pool can never hold more connections than that. Calls beyond it wait for a turn until their context ends and are not sent. `PoolTimeout` follows go-redis's default, now `ReadTimeout` + 1 s = 2 s, but a request's wait ends with its context first. `MinIdleConns` stays 0 (ADR-0026).
- **Dial** is unchanged: go-redis dials in the background (`DialTimeout` 5 s per attempt, up to 5 attempts, at most `PoolSize` dials at once), and a dial whose caller has given up puts its connection in the pool. Only the handshake moved from the request's deadline to the hard timeout (above).
- **Scope: the limiter's cluster client only.** The health checks' per-node clients keep the request-style deadline (`ContextTimeoutEnabled`, 50 ms), because a PING that overruns must count as a miss. The control plane's quota flusher (`internal/controlplane/quotaflush`), `cmd/chaos` and the tests' clients have no 5 ms budget, so they do not churn. They are left alone.
- **Cost:** one goroutine, one channel and one small allocation per limiter call.

### Results

`make accuracy REPLICAS=3`, three runs per mode on main 4487160 (before) and on this branch (after), on the same laptop and stack as ADR-0078 (Apple silicon, Docker Desktop with 14 CPUs, gateways at the 1 % trace ratio). A sampler held one connection to each primary and read `INFO stats cpu commandstats` once a second. A window runs from the end of the previous pattern to the end of its own, as the harness's summary lines show. "New connections" is the increase of `total_connections_received` summed over the three primaries. "Script calls" is the `EVALSHA` and `EVAL` calls they executed. "CPU per call" is main-thread CPU (`used_cpu_sys_main_thread` + `used_cpu_user_main_thread`) over script calls, per primary.

Boundary pattern:

| Run | 2xx | 429 | 503 | 503 share | Admitted per key | Redis calls per admitted | Max over-admission | New connections | Script calls | CPU per call | p99 decision |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| strict, before 1 | 40078 | 10791 | 69131 | 57.6 % | 66.8/s | 2.84 | 0 | 37,361 | 49,493 | 86–93 µs | 74.8 ms |
| strict, before 2 | 37676 | 9021 | 73303 | 61.1 % | 62.8/s | 3.04 | 0 | 37,556 | 45,382 | 90–108 µs | 83.2 ms |
| strict, before 3 | 44168 | 9308 | 66524 | 55.4 % | 73.6/s | 2.61 | 0 | 38,991 | 54,942 | 81–84 µs | 69.1 ms |
| strict, after 1 | 31727 | 40953 | 47320 | 39.4 % | 52.9/s | 3.05 | 0 | 502 | 87,994 | 41–45 µs | 65.7 ms |
| strict, after 2 | 31422 | 40379 | 48199 | 40.2 % | 52.4/s | 3.10 | 0 | 306 | 87,647 | 41–48 µs | 67.9 ms |
| strict, after 3 | 32183 | 38401 | 49416 | 41.2 % | 53.6/s | 3.09 | 0 | 306 | 90,025 | 40–47 µs | 60.9 ms |
| batched, before 1 | 49600 | 23908 | 46492 | 38.7 % | 82.7/s | 1.55 | 17 of 30 | 25,555 | 34,672 | 97–138 µs | 114.7 ms |
| batched, before 2 | 50977 | 29255 | 39768 | 33.1 % | 85.0/s | 1.45 | 17 of 30 | 22,871 | 38,598 | 98–124 µs | 166.1 ms |
| batched, before 3 | 49383 | 20874 | 49743 | 41.5 % | 82.3/s | 1.62 | 20 of 30 | 28,194 | 34,063 | 108–109 µs | 175.3 ms |
| batched, after 1 | 35768 | 45486 | 38746 | 32.3 % | 59.6/s | 2.17 | 0 of 30 | 306 | 69,588 | 45–50 µs | 64.9 ms |
| batched, after 2 | 39110 | 47094 | 33796 | 28.2 % | 65.2/s | 1.84 | 4 of 30 | 306 | 63,478 | 44–53 µs | 62.7 ms |
| batched, after 3 | 34810 | 47659 | 37531 | 31.3 % | 58.0/s | 2.14 | 6 of 30 | 306 | 66,664 | 48–54 µs | 64.3 ms |

Uniform and bursty patterns:

| | Uniform 503 | Bursty 2xx | Bursty 503 | Bursty new connections | Bursty CPU per call |
| --- | --- | --- | --- | --- | --- |
| strict, before | 15 / 3 / 0 | 60201 / 60117 / 59953 | 675 / 778 / 1477 | 1,008–1,790 | 29–45 µs |
| strict, after | 3 / 5 / 4 | 59973 / 59642 / 59916 | 997 / 1581 / 1037 | 275–282 | 29–44 µs (*) |
| batched, before | 32 / 5 / 46 | 60116 / 60064 / 60013 | 513 / 287 / 488 | 601–794 | 32–38 µs |
| batched, after | 3 / 0 / 1 | 60033 / 60014 / 59831 | 479 / 422 / 626 | 281–283 | 32–40 µs |

(*) Without one primary in strict run 1 that executed only 257 calls in the bursty minute, which gives 757 µs per call: idle time divided by almost no calls.

Every run of both modes passed NFR-06 and NFR-07 (12 of 12): over-admission was 0 in strict mode, and at most 6 of 30 in batched mode after, against 17–20 before. Every 503 is a `timeout` (`conn` 0, `script` 0).

What changed:

- **The churn is gone.** In the boundary minute, the primaries accepted about 300 new connections instead of 23,000–39,000. They are the pools growing to the burst's size once. Main-thread CPU per call fell to 40–54 µs, from 81–138 µs, which is close to the bursty minute's. The primaries executed about 1.8 times as many script calls (strict 88,000–90,000 against 45,000–55,000).
- **503s fell by about a third.** Strict went from 55–61 % to 39–41 %, and batched from 33–42 % to 28–32 %. Uniform and bursty are unchanged within run-to-run noise.
- **2xx fell too, and 429s rose.** Strict 2xx went from 37,676–44,168 to 31,422–32,183, and admitted per key from 63–74/s to 52–54/s. Batched went from 49,383–50,977 to 34,810–39,110. 429s went up three to four times. Why: in strict mode, Redis admits about the same number per key in each run (about 100/s plus the burst, so about 61,000 over the minute). The 2xx are the admissions whose reply came back within the budget. The rest are admissions for requests that already had their failure-mode answer, a 503 under the harness's `fail_closed` policy. Keeping the connections lets about 1.8 times as many calls reach Redis. The queue per primary stays longer than 5 ms, so more of the admissions land after their requests' budgets: about 29,000 of the 61,000 after, against 17,000–23,000 before. A later request on the key then finds the tokens spent, and gets a 429.
- **Latency:** the boundary p99 decision latency fell (strict 61–68 ms from 69–83 ms, batched 63–65 ms from 115–175 ms). Every 503 still comes at the budget.

### The open question

The fix does what it was meant to do: no churn, about half the CPU per call, a third fewer 503s, no over-admission, and fewer batched over-admissions. But in the boundary pattern a client also gets about 20–30 % fewer 2xx, and three to four times as many 429s, because more of the tokens go to requests that were already answered 503. NFR-06 bounds over-admission, not under-admission, so it still passes. But the user's outcome rule asks for no regression in accuracy, and fewer admissions under the limit is a client-visible change. This ADR therefore stays Proposed, and the user decides:

1. **Ship it as it is.** Fewer 503s and no churn are worth fewer 2xx under synchronized overload. The residual 503s (28–41 %) are then documented as a known limit of the 5 ms Redis budget under synchronized bursts (ADR-0078's option (a)), and this ADR is accepted with that paragraph.
2. **Ship it with a refund of late admissions.** When a late reply says the script admitted, and the request was answered by a failure mode that did not let it through (`fail_closed`, or a degraded deny), the gateway refunds the call as ADR-0021/ADR-0028 refund a request that did not proceed. That is one more Redis call, made at the worst moment, and a change to RL-23's flow, so it needs its own decision.
3. **Do not ship.** Keep closing timed-out connections (ADR-0025), and record the boundary 503s as ADR-0078's option (a).

## Consequences

- If shipped: a timed-out call holds its connection until its reply comes or the hard timeout passes, at most 1 s. A gateway holds at most 5 × `GOMAXPROCS` connections per node, as before. In the boundary minute each pool grew once to the burst's size and stayed there.
- Redis runs every script call that was sent, including those whose requests already had their answer (ADR-0025 said so for the calls that were sent before; now there are more of them). They can only under-admit.
- Each limiter call costs one more goroutine and one small allocation. The perf task (ADR-0073) measures the gateway with it.
- A node that stops answering releases each connection within 1 s instead of within one budget. Its breaker still opens within 250–350 ms (ADR-0025), and calls still get their answer at the budget.

## Alternatives considered

- **Keep `ContextTimeoutEnabled` and pass go-redis a context whose `Deadline` is hidden but whose `Done` is the request's.** Same effect. But it relies on go-redis reading `Deadline` only for socket deadlines, which go-redis does not promise. `ContextTimeoutEnabled` off is the documented switch.
- **A per-command read timeout.** go-redis has one for blocking commands, but it is unexported, and with `ContextTimeoutEnabled` the earlier of the context's deadline and the timeout wins anyway.
- **Re-open closed connections faster** (`MinIdleConns`, a larger `PoolSize`, a refill after timeouts). Each dial and handshake still costs Redis main-thread time, which is the amplifier ADR-0078 measured. ADR-0025 and ADR-0026 measured no gain from the pool settings.
- **go-redis autopipelining or one connection per node with pipelined calls.** It would change how calls queue and fail, and the call contract (one call, one reply, no resend: RL-16). It is a much larger change than the churn needs.
- **Single-flight reservations with waiters (close to RL-20)**, which ADR-0078 lists. That fixes the number of calls that arrive together, not the churn. It needs its own decision.
