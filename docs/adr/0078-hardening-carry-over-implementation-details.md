# 0078. Hardening carry-over: implementation details and the boundary 503s

- **Status:** Accepted
- **Date:** 2026-11-25
- **Requirement IDs:** GW-26, GW-28, SEC-05, NFR-06, NFR-07, RL-18, RL-19, RL-22, UI-01, CP-22

## Context

ADR-0071 (D6, O1) and ADR-0072 (O2) settle the hardening carry-over. Building it left a few choices they do not make, one wording they assume that does not exist, and two items whose outcome they leave to measurement: the accuracy test's boundary 503s (D6b) and `TestControlPlaneFailover`'s "address already in use" (D6d). This ADR records the choices and the measurements. It changes no decision of ADR-0071 or ADR-0072.

## Decision

### Per-client-IP connection cap (D6a)

- **Default when unset.** `SPILLWAY_GATEWAY_MAX_CONNECTIONS_PER_IP` unset is the smaller of 1,024 and `SPILLWAY_GATEWAY_MAX_CONNECTIONS`, so a deployment that only lowers the global cap (for example to 100) still starts; a value that is set must be a positive integer no larger than the global cap, or the gateway does not start (`SPILLWAY_GATEWAY_MAX_CONNECTIONS_PER_IP: 1025 is above SPILLWAY_GATEWAY_MAX_CONNECTIONS (1024)`).
- **Bookkeeping.** Each address with open connections has an entry (its count and its own waiting list, longest-waiting first) under `ConnLimit`'s existing mutex; the entry is deleted when its count reaches 0. A waiting connection is on both the global list and its address's list. The address's place is taken before the global one; a connection then shed at the global cap gives its address's place back at once. A closed connection is marked released and never rejoins a waiting list, even if the server reports it idle after the close.
- **Logs.** Two new lines, each at most once a second and with the count only: info `closing waiting connections at the per-address connection cap` and warn `shedding connections at the per-address connection cap`, both with `max_connections_per_ip`. The global cap's lines are unchanged.
- **Integration test.** `TestGatewayPerIPCap` uses compose's gateway-1: every host connection reaches it from Docker's one forwarding address (172.29.0.1 on this laptop's compose network), so 1,100 idle host connections exercise the default cap of 1,024, and loadgen in the compose network is the second address. It dials one connection at a time, since Docker Desktop's forwarding reset some of 20 concurrent dials, and first waits for three 200s from the host, since the forwarding lags a gateway that make dev has just recreated.
- **Load tools.** `chaos m6 probe` and the `chaos cp-outage` sender, whose in-flight limits are 2,000 and whose transports had no `MaxConnsPerHost`, get 512 connections per gateway. loadgen (`-conns` 100), `cmd/accuracy` (1,000 per gateway) and the other `cmd/chaos` senders (1,000) already stay below 1,024 and are unchanged. With the cap in place, one strict and one batched `make accuracy REPLICAS=3` and three `make verify-m6` runs left `gw_connections_capped_total` at 0 on every gateway (Prometheus), and their boundary 503 shares (56.8 % strict, 43.0 % batched) were within the spread of main's runs below.

### The boundary 503s (D6b)

`make accuracy REPLICAS=3` on main 7ade15a (deny cache #86, leasing #87, quotas #88; gateways at the 1% trace ratio; same laptop as ADR-0025), three runs per mode plus one profiled run each:

| Run | Pattern | 2xx | 429 | 503 | 503 share | Admitted per key | Redis calls per admitted | Max over-admission |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| ADR-0025 after | boundary | 39228 | 8809 | 71963 | 60.0 % | 65.4/s | — | 0 |
| ADR-0025 re-run | boundary | 44889 | 12043 | 63068 | 52.6 % | 74.8/s | — | 0 |
| strict 1 | boundary | 39242 | 11142 | 69616 | 58.0 % | 65.4/s | 2.90 | 0 |
| strict 2 | boundary | 30320 | 3584 | 86096 | 71.7 % | 50.5/s | 3.90 | 0 |
| strict 3 | boundary | 40308 | 9603 | 70089 | 58.4 % | 67.2/s | 2.85 | 0 |
| strict, profiled | boundary | 40915 | 8372 | 70713 | 58.9 % | 68.2/s | 2.81 | 0 |
| batched 1 (`lease_k` 10) | boundary | 49630 | 23469 | 46901 | 39.1 % | 82.7/s | 1.54 | 17 of 30 |
| batched 2 | boundary | 49010 | 24182 | 46808 | 39.0 % | 81.7/s | 1.58 | 18 of 30 |
| batched 3 | boundary | 47537 | 22860 | 49603 | 41.3 % | 79.2/s | 1.68 | 10 of 30 |
| batched, profiled | boundary | 45051 | 16679 | 58270 | 48.6 % | 75.1/s | 1.89 | 18 of 30 |

Every run passed NFR-06 / NFR-07 (over-admission 0 strict, at most 18 of 30 batched). Uniform had 0–19 503s and bursty 766–1,412 (0.6–1.2 %) in every run. Every 503 is a `timeout`. The boundary pattern's 503 share is above ADR-0071's 10 % threshold in both modes, so, as D6b says, it is reported to the user as an open item and nothing else is changed.

Why the M7 features did not cut it (from the reports and the profiled runs):

- **The deny cache** takes a deny only after a Redis reply. In the boundary pattern each key's 200 requests arrive within 20 ms, the denied half right after the admitted half, all while the first calls are still queued, so it answered only 4–6 % of the strict boundary requests locally (113,637–118,149 of 120,000 still called Redis).
- **Leasing** allows one reservation in flight per key and replica; requests meanwhile are checked strictly. In the boundary minute 68,163–77,033 of about 100,000 policy checks took that `inflight` path, 25,244–28,373 were served from leases, and grants were mostly `granted`/`partial` around 3,000. Redis calls per admitted request fell from about 2.9 to 1.5–1.9, but the calls still arrive in the same 20 ms bursts.
- **Where the budget goes.** In the profiled strict minute, the three primaries executed 49,549 `EVALSHA` (13,462 / 17,021 / 19,066) against 115,160 requests that called Redis: most calls that failed were never sent, their 5 ms budget spent waiting for a pool connection or a new connection's dial and handshake. gateway-1's Redis check durations were ≤ 5 ms for 24.5 % and ≤ 7 ms for 55 % of its calls. gateway-1 used 15 % of one core (CPU profile, 30 s); it is not the bottleneck.
- **Connection churn amplifies it.** Each timed-out call closes its connection (ADR-0025), so the next burst re-dials inside request budgets. In the boundary minute the primaries accepted 5,461 / 11,176 / 20,869 new connections (`HELLO` 4,184 / 8,328 / 15,062), against 215 / 782 / 758 in the bursty minute, and their main-thread CPU per executed call was 96–98 µs, against 31–45 µs in the bursty minute (the script itself: 25–33 µs). Batched mode showed the same (4,407–18,909 new connections, 113–147 µs per call). A primary that spends about 100 µs per call answers about 50 calls within one 5 ms budget, against about 630 offered to it in each 20 ms burst.

So ADR-0025's capacity explanation still holds, with churn as a measured amplifier: the pattern offers each primary several times what it can answer within the budget, and neither M7 feature reduces the number of calls that arrive together.

### Shadow report help text (O1)

The shadow report screen has no "?" popover; the help text ADR-0060 refers to is the muted paragraph under the heading ("Counts are by its published name …"). ADR-0071's text is added as a second muted paragraph after it, verbatim, and Vitest checks it.

### verify-m6's per-run policy name (O2)

`m6-limit-<run>` with m6.sh's ten-digit run ID is 19 characters (ADR-0072 says 20); either is within the policy-name rule. `chaos m6` builds it from `-run-id`, whose default (when run by hand) is the start time in base 36. The README's M6 results table keeps the name its recorded runs used, `m6-limit`; the readme task rewrites that section.

### `TestControlPlaneFailover` (D6d)

20 runs (`go test -tags integration -run TestControlPlaneFailover -count 20 ./test/integration/`) all passed, with no "address already in use", and the logs of the seven failed CI runs on main and task branches since M5 contain none for this test. The one bind failure found, on m7-leasing (run 45463228343), was `TestControlPlaneOutage`'s gateway at 127.0.0.1:33839, the `freeAddr` duplicate that #87 fixed. Without a failing log there is no evidence of which bind fails, so, as D6d requires, nothing is changed. The race D6d describes remains possible in principle; if the failure is seen again, its log decides.

## Consequences

- One address holds at most 1,024 of a replica's connections by default; every load tool in the repository stays at or below 1,000 per gateway, so no exit check measures the cap.
- The boundary 503s remain an open item for the user: accepting and documenting them, single-flight reservations with waiters on the in-flight call (close to RL-20), or another fix such as keeping a connection after a timed-out call (draining its late reply) to stop the churn, each needing its own decision and ADR. NFR-06 and NFR-07 are unaffected: over-admission stays within bounds and 503s are reported, not judged (ADR-0021).

## Alternatives considered

- **Requiring `SPILLWAY_GATEWAY_MAX_CONNECTIONS_PER_IP` to be set whenever the global cap is below 1,024** — breaks existing settings that only lower the global cap.
- **A popover for O1's text** — a new UI element for one paragraph; the screen's help text is its muted paragraphs.
- **Moving the failover test's address out of the ephemeral range without a failing log** — D6d allows the fix only once the race is shown.
