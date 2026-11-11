# 0024. Redis limiter warm-up

- **Status:** Accepted
- **Date:** 2026-11-11
- **Requirement IDs:** RL-16, RL-22, RL-23, RL-30, GW-24, NFR-06, NFR-13

## Context

ADR-0021 loads nothing at start: the first call to each primary loads the slot map, dials, runs `INFO server` for the Redis 7 check (RL-30), sends `EVALSHA`, gets `NOSCRIPT` and sends `EVAL`. That is four or five round trips plus a TCP handshake, all inside the request's 5 ms budget (RL-22), and inside Docker Desktop's VM or across zones it often overruns. With m3-failure's failure modes the overrun is no longer harmless: right after a gateway starts, a `fail_closed` policy answers 503 and a `degraded` one falls back to its in-memory limiter, which would also pollute the accuracy harness (every admission must carry a Redis decision) and the chaos runs. Readiness must still never depend on Redis (GW-24), and a sent call must still never be resent (RL-16).

## Decision

The gateway warms the limiter's client up in the background, superseding ADR-0021's "Nothing is loaded at start"; the budget, the breakers and the failure modes are unchanged.

- `(*redis.Client).Warm(ctx, retry)` makes passes over the cluster: load the slot map, then on **every node, primaries and replicas**, open a connection (it stays in that node's pool), run the Redis 7 check and `SCRIPT LOAD` the limiter script. A node below Redis 7 is skipped: the check holds its breaker open for good, as ADR-0021 says. Each pass has a 5 s timeout; a pass that fails is tried again every `retry` until one succeeds, then `Warm` returns.
- `cmd/gateway` starts `go client.Warm(ctx, time.Second)` right after creating the client. The admin server's readiness does not look at it (GW-24). The first failed pass is logged at warn, success at info (`redis limiter warmed`, with the node count), nothing in between.
- Warming replicas too means a replica promoted by a failover has already passed the version check and holds the script, so the first call after the slot map moves to it is one `EVALSHA`. A node that restarts empty still gets the script through ADR-0021's `EVAL` fallback on its first call.
- Nothing is warmed on a snapshot apply: the client, its seeds and its nodes come from the gateway's settings, never from a snapshot (ADR-0008).
- Warm-up commands go to the node clients directly, never through the breakers, and none of them deducts anything: `INFO` and `SCRIPT LOAD` are safe to repeat, so the retry does not touch RL-16.

## Consequences

- A gateway that started while Redis was up decides its first request in Redis within the budget; the unit test `TestWarm` (a fake node whose connections and slot map take 50 ms) and the integration test `TestGatewayRedisFailure` (a host-run gateway's first request, with a `fail_closed` policy and the default budget) show it. On the compose stack gateway-1 logs `redis limiter warmed` about 7 ms after `starting`.
- A gateway started during an outage keeps trying every second; until a pass succeeds its requests follow their failure modes, as before.
- Requests that arrive in the few milliseconds before the first pass finishes can still overrun the budget; the M3 integration test keeps its wait for that.
- One more idle connection per replica node per gateway.

## Alternatives considered

- **Exclude connection setup from the budget** — the budget is what bounds a request's wait on Redis (RL-22, NFR-13); a dial during an outage would then hang a request until the dial timeout.
- **Make readiness wait for the warm-up** — breaks GW-24: a Redis outage would take every gateway out of rotation, including those whose policies are `fail_open`.
- **Warm on every snapshot apply** — the snapshot cannot change the cluster, so it would only repeat the start-up pass.
- **Warm primaries only** — the first call after a failover would again pay the version check and `EVAL` inside the budget, during the window the chaos test measures.
