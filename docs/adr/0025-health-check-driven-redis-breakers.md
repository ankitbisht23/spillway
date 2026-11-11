# 0025. Health-check driven Redis breakers

- **Status:** Accepted
- **Date:** 2026-11-11
- **Requirement IDs:** RL-22, RL-23, NFR-06, NFR-13

## Context

ADR-0021 ("Budget and breakers") opens a Redis node's breaker after **5 consecutive failed request calls of any kind** — timeouts and script errors included — keeps it open for 1 s, then sends the next request call as a half-open probe. Two runs showed that this rule mixes up load with failure:

- **Hot keys.** The accuracy harness (#32) sends 100 requests at once on one key (bursty) or 200 within 20 ms around each second (boundary). Calls at the back of such a burst overrun the 5 ms budget and fail as `timeout`. Five of those in a row open the breaker, and then **every** key on that node is refused for a second and follows its failure mode. The harness's `fail_closed` policy answered 503 to 9–12% of bursty requests and 76–90% of boundary requests. Keys admitted 23–45/s against a 100/s limit in the boundary pattern. The 503s were mostly breaker refusals (`conn`), not timeouts.
- **Recovery.** The half-open probe is a request call. It has to dial and answer inside the request's 5 ms budget. After an outage, a new connection through the test's proxy often took longer than that, so `TestGatewayRedisFailure` once kept a breaker open for about 10 s after the nodes were back.

The user decided (2026-11-11) to replace the request-driven breaker with a health-check driven one: request timeouts never count, broken connections do, and background PINGs open and close breakers. The task also asked why about 100 concurrent calls on one key overrun 5 ms, and to fix any part of that which is client tuning, without changing the 5 ms default budget.

### Why a hot key overruns the budget

Measured on the dev stack: Apple silicon, Docker Desktop with 14 CPUs, Redis 8.10.3 in compose, go-redis v9.23.0.

- **Redis cost per call.** `INFO commandstats` gives 15–16 µs per `EVALSHA` of the limiter script. Main-thread CPU (`INFO cpu`) over 10,000 one-key calls in bursts of 100, minus the idle rate, gives about **50–60 µs of Redis CPU per call**. The rest is socket reads and writes and the event loop inside Docker Desktop's VM. So one primary answers about 17,000–20,000 single calls a second, and a burst of 100 calls on one key, which all go to one primary, needs about 5–6 ms of that primary's one thread. That is the whole budget. A probe process in the compose network sending bursts straight to the client (no gateway, no HTTP) saw 3–9% timeouts at 33 concurrent calls and 33–54% at 100 under a 5 ms deadline. With no deadline, the p99 was 8 ms at 33 and 12.5 ms at 100. A single call takes about 0.75 ms (p50) inside the VM.
- **The boundary pattern is beyond one node's capacity.** 10 keys × 200 requests within 20 ms spread over three primaries is about 670 calls per primary per burst. That is 35–40 ms of single-thread work arriving in 20 ms, so most calls in the burst cannot be answered within 5 ms, whatever the client does. A call that times out has usually been sent already, so Redis still runs it and the queue does not shrink.
- **go-redis routing policies (client).** With routing policies on (the go-redis v9.23.0 default), every cluster call goes through `routeAndRun`. On the first call, that sends `COMMAND` and waits for its reply (hundreds of kB from Redis 8) **inside the request's budget**. On every later call, it takes the command-info cache's mutex, one lock for the whole client. Before this change, the host-run gateway in `TestGatewayRedisFailure` took 4–6.6 ms over its first request, on main and on this branch alike, against the 5 ms budget, and this branch answered it 503 in 3 of 13 runs. The limiter needs neither feature, because each call names its key position and runs on its slot's primary.
- **Pool settings (client), tried and not adopted.** Each timed-out call's connection is closed (a reply may still be pending), so a later burst dials again, and pool waits appear once 5 × `GOMAXPROCS` = 70 turns per node are taken. Changing these settings did not help beyond run-to-run noise. Over three runs at 100 concurrent calls, `MinIdleConns` 16 or 64 gave 27–46% and 42–50% timeouts, against 33–54% without. In 10-second harness runs at 3 replicas, `PoolSize` 16 or 32 left the boundary pattern at 54–82% admitted, the same spread as the default. Turning off `HELLO` and `CLIENT SETINFO` (RESP2, `DisableIdentity`) made no difference either.

## Decision

The breaker rule of ADR-0021 is replaced as follows. The rest of ADR-0021's "Budget and breakers" section stays: one budget per request, no resend, breakers keyed by the address `MasterForKey` names, and refused calls failing as `conn` (no `breaker_open` kind).

### What a request call counts

Each request call (and refund) that the breaker let through ends in one of three ways:

- **Replied.** Redis answered: a decision, or any error reply, including script errors (`ERR`, `OOM`, `WRONGTYPE`) and cluster-state replies (`CLUSTERDOWN`, `READONLY`, …). The node's broken-connection count goes back to 0. A script error still counts in `gw_redis_errors_total{kind="script"}`, and its policies follow their failure modes, but it never opens a breaker.
- **Broken.** No reply because the connection failed: a dial error, a reset, EOF or a broken pipe. That is a `conn` error other than a pool timeout, a closed client or the Redis 7 refusal (RL-30). **3 consecutive** broken calls open the node's breaker.
- **Timed out.** The deadline passed before a reply. The call is neither counted nor does it reset the count. This also covers calls the proxy never sent because the budget was spent. A timeout says how long a call waited, not that the node is down; the health checks find a node that stopped answering.

### Health checks

- The client runs one background loop. Every **100 ms** it sends one `PING` to every node it knows, all at once, each with a **50 ms** timeout, outside any request budget. Each node has its own go-redis client for this: one connection, no retries, dial, read and write timeouts of 50 ms, and the same address-map dialer (ADR-0004). So a PING never queues behind request traffic in the request pool.
- The nodes it knows are every node the warm-up visits, primaries and replicas (ADR-0024), plus every primary a request call is routed to. Replicas are checked so that a replica promoted by a failover already has a closed breaker with a known state. A node held open for running Redis older than 7 (RL-30) is not PINGed and stays held.
- **3 consecutive missed PINGs** (no `PONG` within 50 ms, for any reason) open the node's breaker. A node that stops answering is open **250–350 ms** later. The fake-clock test measures 320 ms for a node that pauses 30 ms into a round, and `TestHealthChecks` measured about 350 ms on the system clock.
- **Open:** every call to the node fails at once as `conn` without being sent. Time alone never closes it, and no request is sent as a probe. **The first answered PING closes it** and resets its broken-connection count. Missed PINGs and broken calls are counted separately. An answered PING resets only the missed count, and a reply resets only the broken count.
- **Refill on close.** When a breaker closes, the client opens one request connection to that node in the background: a `PING` through the node's request pool, with the warm-up's 5 s timeout (ADR-0024), outside any budget. During an outage the timed-out calls closed the node's pooled connections. Without the refill, the first call after recovery dials, sends `HELLO` and runs the script all inside its 5 ms budget. Through `TestGatewayRedisFailure`'s proxies that often overran: 5 of 19 runs answered the first degraded request after the outage from the degraded limiter.
- **Slot map.** Opening a breaker reloads the slot map, as before. In addition, every health check round reloads it while any breaker is open (go-redis runs at most one reload per 200 ms). This replaces ADR-0021's reload on each failed probe. After a failover, the slot reaches the promoted replica within about 200 ms of the election, not within up to 1 s. The killed-primary window in ADR-0021 ("Chaos") is then `cluster-node-timeout` (3 s) plus the election (about 1 s) plus up to 0.2 s.
- Logs: warn `redis limiter: circuit breaker open` with `node` and either `failures=3` or `missed_pings=3` and `since_first_miss`; info `redis limiter: circuit breaker closed` with `node` and `open_for`. Times come from the client's `ratelimit.Clock`, so the unit tests drive every transition on the fake clock.
- The 503's `Retry-After: 1` stays. It is no longer "the breaker's open time", since breakers have none. It is the shortest whole number of seconds, and longer than a health check takes to find a node back.

### Client settings

- `ClusterOptions.DisableRoutingPolicies: true`: no `COMMAND` fetch inside a request's budget and no client-wide lock per call. A unit test checks that the fake node never receives `COMMAND`. go-redis marks the option experimental. If a later version removes it, the build breaks and the upgrade has to deal with it. If the option stays but stops working, that test fails.
- `PoolSize`, `PoolTimeout` and `MinIdleConns` stay at go-redis's defaults, as measured above.

### Timeout classification

A failed call is a `timeout` when its error is a timeout **or its context's deadline has passed when the error is read**, even if the context's own timer has not yet marked it done. A socket deadline taken from the context can fire first. This applies to calls and to slot-map loads (`MasterForKey`). Before, an unwarmed first call that failed with `i/o timeout` while loading the slot map could be counted as `conn`. That made `TestWarm` flaky. With 12 parallel `-race` processes running it 20 times each, 18 of 240 runs failed on main and 0 of 240 after the change.

### Results

`make accuracy REPLICAS=3` (60 s per pattern, 10 keys at 2× a 100/s limit, burst 100, 3 gateways; same laptop as #32). The "before" rows are #32's runs at 3 replicas.

| Run | Pattern | 2xx | 429 | 503 | Admitted per key | Max over-admission |
| --- | --- | --- | --- | --- | --- | --- |
| before (#32) | bursty | 58018 | 49412 | 12570 | 96.7/s | 0 |
| before (#32 re-run) | bursty | 59006 | 50322 | 10672 | 98.3/s | 0 |
| after | bursty | 59755 | 58953 | 1292 | 99.6/s | 0 |
| after (re-run) | bursty | 59770 | 58830 | 1400 | 99.6/s | 0 |
| before (#32) | boundary | 26992 | 1922 | 91086 | 45.0/s | 0 |
| before (#32 re-run) | boundary | 23832 | 1039 | 95129 | 39.7/s | 0 |
| after | boundary | 39228 | 8809 | 71963 | 65.4/s | 0 |
| after (re-run) | boundary | 44889 | 12043 | 63068 | 74.8/s | 0 |

Uniform is unchanged: 60990 / 59010 / 0 at 101.7/s, and 60986 / 59009 / 5 at 101.6/s in the re-run. Every remaining 503 is a `timeout` (`conn` 0). Bursty 503s fall by about 90%, to about 1%. Boundary 503s fall by a quarter to a third, but 53–60% of boundary requests still get 503. The pattern offers each primary about twice what it can answer within 5 ms in this VM (above), so those calls time out whatever the breaker does. A hot key that bursts past one node's single-call throughput needs batching (one script call for many requests on the key, M7's batched mode), not breaker or pool changes. NFR-06's rule is unaffected: over-admission stays 0, and 503s are reported, not judged (ADR-0021).

## Consequences

- A burst on one key costs only the calls that overrun the budget. Other keys on the node keep their Redis decisions.
- A node that hangs costs each request routed to it up to one budget until the PINGs open its breaker, 250–350 ms after it stopped. Before, five failed calls opened it, which was sooner under heavy traffic and later under light traffic. The full-outage rule "within one budget" still holds, because every call is bounded by the budget. A node whose connections break (killed, refusing) opens after three calls, as fast as traffic finds it.
- Recovery no longer depends on a request getting through inside its budget. With the refill, `TestGatewayRedisFailure` passed 20 of 20 runs: Redis decided again 44–71 ms after the outage ended, and the first request was always decided by Redis.
- Each gateway holds one more connection per node and sends 10 PINGs a second to each node. With 6 nodes and 100 gateways, that is 1,000 PINGs a second per node, a negligible load for Redis.
- A node that leaves the cluster for good keeps being PINGed, and while its breaker is open the slot map is reloaded about every 200 ms, until the gateway restarts. That costs one `CLUSTER SLOTS` and one failed dial per round. Forgetting nodes would need the slot map's node list, which go-redis only exposes through calls that reload it.
- The breaker no longer reacts to script errors. A node that answers every call with `OOM` stays closed, and its policies follow their failure modes call by call, which is the right outcome for each call and costs no budget.

## Alternatives considered

- **Count timeouts but raise the threshold or add a rate window.** Any timeout count mixes load with failure, and a bigger hot key would still trip it.
- **Probe with a request call, but with its own longer timeout.** The request would wait for the probe, or the probe would be an extra write that deducts. A PING is free and runs outside any request.
- **PING through the request pool's connections.** During a burst the PING would wait for a pool turn behind the very calls that overrun, and miss.
- **Use go-redis's own node health** (the cluster client's failing flag): internal and not exposed.
- **Tune `PoolSize`, `MinIdleConns`, RESP2 without `HELLO`/identity.** No gain beyond run-to-run noise (Context). The cost per call sits in Redis.
- **Batch concurrent calls on one key now.** This is the real fix for hot keys, but it is M7's batched mode and changes the call contract.
