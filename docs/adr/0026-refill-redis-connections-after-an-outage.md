# 0026. Refill Redis connections after an outage

- **Status:** Accepted
- **Date:** 2026-11-11
- **Requirement IDs:** RL-22, NFR-13

## Context

ADR-0025 refills a node's request pool when its breaker closes, with **one** connection. `make verify-m3` runs the full-outage chaos test and then the killed-primary one on the same gateways. When m3-chaos (#34) measured both, the killed-primary test failed in 2 of 3 runs that came right after a full outage, and in 0 of 8 runs on their own. Each failure was 2–3 of 16,000 requests for keys on the surviving primaries that got a failure-mode answer. The full-outage test failed in 2 of 9 runs, each time with 1 of 28,000 Redis checks over 7 ms. m3-verify measured these causes on the same laptop (Apple M4 Pro, 14 CPUs, Docker Desktop):

- **Pools after an outage.** `CLIENT LIST` on each node, read right after `make chaos-redis-outage`, showed one request connection from gateway-1 per node: the refill's. At the loads the dev stack sends, a primary's pool settles at more than that. At 200 calls/s per primary (the killed-primary test) it holds 2. At 1,000 calls/s on one primary (NFR-04) it holds 4. Until the pool grows back, a call that finds no idle connection dials one inside its 5 ms budget. In the killed-primary run that followed an outage, one primary's pool grew from 1 to 2 connections, so at least one call dialed inside its budget.
- **The cost of a dial inside the VM.** A probe in the compose network timed a new go-redis connection (DNS, TCP, `HELLO`, `CLIENT SETINFO`) plus one `PING` against redis-1, 300 times: p50 1.09 ms, p99 4.62 ms, max 5.16 ms. A `PING` on a connection that is already open took p50 0.09 ms, p99 0.56 ms. So a call that has to dial loses about a fifth of its budget to the dial at the median, and nearly all of it at the p99.
- **Stalls of the whole machine.** A probe container that sleeps 1 ms in a loop woke more than 2 ms late about once a second while the stack was idle, by up to 4–7 ms. It woke up to 45 ms late while docker paused, killed or started containers. Under load the worst stalls repeat about every 15.06 s and last 4–9 ms. They hit every process at the same instant: in a 120 s run at 1,000 rps through gateway-1, six of the seven moments when gateway-1 answered requests in over 4 ms matched a late wake-up of the probe in another container to the millisecond. A native macOS build of the probe saw some of the same stalls, at the same instants, outside the VM. So they come from the host and the VM, not from the gateway. Calls that time out in the full-outage test end about 0.5–1.5 ms after the deadline: Go's netpoller waits in whole milliseconds, and the VM adds its own wake-up delay. One stall over about 1 ms in the ~400 ms before the breakers open is enough to fail the 7 ms check. A stall during a Redis call that would otherwise take 1 ms makes it miss its 5 ms budget.

Run order matters only because of the pools: the full-outage test leaves them at one connection per node, and the killed-primary test is the next load.

## Decision

- When a node's breaker closes, the client opens **`refillConns` = 4** request connections to it, outside any budget, in the background as before. It takes 4 dedicated connections from the node's pool with `Client.Conn()`, sends a `PING` on each at once, which dials and initialises them, and then closes them, which hands them back to the pool. The 5 s timeout is the warm-up's, as before (ADR-0024).
- 4 is the largest pool the dev stack's loads keep per primary (measured above). It is a constant. Every gateway holds 4 connections per node after an outage, as it did before the outage under the same load.
- The rest of ADR-0025 stays. The warm-up still opens one connection per node (ADR-0024). The budget, the 2 ms chaos slack and every chaos pass rule stay as they are.

## Consequences

- In the killed-primary run that follows a full outage, every request connection to a surviving primary was older than the run: no call dialed inside its budget.
- `TestClientBreaker` checks that closing a breaker opens `refillConns` connections, and that as many concurrent calls after it dial none.
- After an outage, a gateway holds 4 connections per node rather than 1, so with 6 nodes and 100 gateways, Redis holds about 1,800 more idle connections than before.
- A pool that needs more than 4 connections still grows inside request budgets. So does a pool after a gateway starts, since the warm-up opens one. A pool that grew beyond 4 before an outage is not restored to that size.
- The host and VM stalls remain. They can still put a timed-out call over the 7 ms line in the full-outage test, or a Redis call past its budget in any test. The pass rates in the M3 exit PR are measured with them. Raising the slack or changing a pass rule is the user's decision, not this ADR's.

## Alternatives considered

- **go-redis `MinIdleConns`** — keeps idle connections at all times, not only after an outage. But go-redis dials them without initialising them, so the first call on each still sends `HELLO` inside its budget. ADR-0025 also measured no gain from it for hot keys.
- **Refill to the pool's size before the breaker opened** — by the time a breaker opens, the timeouts have already closed connections and background dials are still pending, so go-redis's pool stats no longer show the size before the outage. Tracking a high-water mark per node would need the request pools in the health check loop. That is more code for loads the constant already covers.
- **Running the killed-primary test before the full-outage test** — hides the dials instead of removing them. The chaos runs should not depend on their order.
- **Opening several connections in the warm-up too** — the same hazard after a gateway starts. But no exit check measures it, and ADR-0024's one connection per node stays until a measurement asks for more.
