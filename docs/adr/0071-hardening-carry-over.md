# 0071. Hardening carry-over

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** GW-26, SEC-05, NFR-06, NFR-11, RL-08, RL-10, CP-03, UI-01, UI-06

## Context

Earlier milestones left four items for M7 (ADR-0035, ADR-0056), and the M6 review left one open question (O1):

1. **A per-client-IP connection cap.** ADR-0033 noted that a client opening connections faster than legitimate clients send their first request can still crowd them out of GW-26's global cap (8,192 by default), and deferred a per-IP cap to M7, where the gateway is the edge.
2. **The accuracy test's boundary 503s.** ADR-0025 measured `make accuracy REPLICAS=3` with 63,068–71,963 timeout 503s out of 120,000 in the boundary pattern (53–60% of its requests): the pattern offers each Redis primary about twice the single-key calls it can answer within the 5 ms budget in Docker Desktop's VM, so calls time out whatever the breaker does, and the harness's fail-closed policy answers 503. Over-admission stayed 0; 503s are reported, not judged (ADR-0021).
3. **`overrides.policy_id` nullable** for quota overrides (RL-10; the M5 CHECK says "until M7").
4. **`TestControlPlaneFailover` "address already in use"**, seen intermittently in M5/M6 runs; to be fixed only if it is clearly a test port bug.
5. **O1 (M6 review).** The shadow report counts would-denies from Prometheus by policy **name** (`gw_ratelimit_decisions_total{policy}`, OB-01 keeps IDs out of labels), so a new policy that reuses a deleted one's name shows the old policy's counts within the window, while its key table (from heavy-hitter windows, by policy ID) does not. The user chose option (b): keep the behaviour and document it.

## Decision

### D6a. Per-client-IP connection cap (GW-26)

- **Setting:** `SPILLWAY_GATEWAY_MAX_CONNECTIONS_PER_IP`, a positive integer, default **1,024** (one eighth of the default global cap, so one address can hold at most 12.5% of a replica's connections), and at most `SPILLWAY_GATEWAY_MAX_CONNECTIONS` (start fails otherwise). Process setting, like the global cap, not snapshot config; Helm value `gateway.maxConnectionsPerIP`.
- **Key:** the TCP peer address at `Accept`, before TLS and before any request: an IPv4 address, or for IPv6 its /64 (one client usually owns a /64). `X-Forwarded-For` is never read here (no request yet). Peers inside `SPILLWAY_GATEWAY_TRUSTED_PROXIES` (SEC-05) are **exempt**, since a trusted proxy carries many clients.
- **Where:** in `ConnLimit` (`internal/proxy/shed.go`), beside the global count: a map from key to open count under the existing mutex, entries deleted at zero. A new connection whose address is at its cap first closes that address's longest-waiting connection without a request in progress (ADR-0033's rule, applied per address); if every one of that address's connections has a request in progress, the new connection takes the existing shed path: a fixed 503 with `Retry-After: 1` within `shedDeadline`, or a reset past `maxShedding` (ADR-0032). The global cap is checked as today after the per-address one.
- **Metric:** `gw_connections_capped_total{cap,action}`, `cap` ∈ `global`, `per_ip`, `action` ∈ `closed_waiting`, `shed`. Shed answers keep `reason="shed"` on `gw_requests_total` (excluded from NFR-11, deliberate). Logs at most once a second, with the count, never the address.
- **Tests:** unit (cap reached: waiting connection closed; all busy: shed 503; trusted proxy exempt; IPv6 /64 grouping; counts released on close and on a shed); one integration test with 1,100 idle connections from one address against gateway-1 (cap 1,024) while a second client still gets 200.
- **Test tools stay under the cap.** Every load tool sends from one container address: loadgen (`-conns`, default 100), `cmd/accuracy`, the `cmd/chaos` senders and verify-m6's probe. hardening-carryover checks each one's connection limit per gateway, and any that can open more than 1,024 connections to one gateway (for example a transport without `MaxConnsPerHost` during the boundary pattern's bursts) gets an explicit limit of 512 per gateway, so no exit check starts measuring the cap.
- On kind, NodePort traffic is SNATed to node addresses (`externalTrafficPolicy: Cluster`, ADR-0075), so the per-IP cap sees node IPs there; in-cluster clients keep their pod IP. Documented in the chart's values.

### D6b. The accuracy test's boundary 503s

- **Cause (as ADR-0025 measured):** not a gateway defect. In the boundary pattern each key's requests arrive in bursts at window boundaries, so one primary gets more single-key script calls in a few milliseconds than it can answer within the 5 ms budget in the Docker Desktop VM; the late calls time out and the fail-closed harness policy answers 503. A dedicated host or a real Redis deployment has more headroom, but the pattern is legitimate and the gateway should not need it.
- **Fix:** fewer calls per burst, from M7's own features, with no change to budgets, breakers or the harness: the deny cache (ADR-0069) answers the denied half of each burst locally after its first deny, and batched mode serves most admissions from leases. No retry, no larger budget (RL-22 stays 5 ms).
- **Measured, not judged:** hardening-carryover runs `make accuracy REPLICAS=3` (strict) and `make accuracy REPLICAS=3 MODE=batched`, three times each, on main after deny-cache and leasing, and records per pattern the 2xx / 429 / 503 counts, the admitted rate per key and the Redis calls per request, next to ADR-0025's rows. NFR-06 and NFR-07 judge over-admission only; 503s stay reported, not judged (ADR-0021). If strict-mode boundary 503s are still above 10% of that pattern's requests, the task reports the numbers to the user as an open item instead of changing anything else.

### D6c. `overrides.policy_id` NULL: a quota override

- **Migration:** in ADR-0070's `00008_quotas.sql` (built by the quotas task, which needs it first): the M5 CHECK is replaced by `overrides_kind` (a policy override has a policy and no quotas; a quota override has quotas, no policy and `limits = '[]'`), with a partial unique index for one standing quota override per consumer.
- **Meaning of NULL:** "this override replaces the consumer's quotas, not a policy's limits". Nothing else may be NULL-policy; validation (CP-03) still requires that a policy override's policy exists and is a consumer- or route-level policy (RL-08), and a policy still used by an override cannot be removed.
- **API:** the NULL-policy rows are their own resource, `/api/v1/quota-overrides` (ADR-0070). `/api/v1/overrides` lists, reads and accepts only policy overrides (its handlers filter `policy_id IS NOT NULL`, and an `id` of a quota override answers 404 there). Making `Override.policy_id` optional or nullable in place would change a required response property, which `oasdiff` reports as breaking (ERR) and `make lint-api` rejects; a separate resource is additive.
- **Identity stream:** one `Override` message type, `policy_id` 0 for a quota override (proto3 has no NULL), `quotas` set; gateways route it to the quota resolution and never to `limitsOf`.
- **UI:** a consumer's Overrides section lists quota overrides as "Quota override" rows (daily, monthly, window) beside policy overrides; the Throttling screen's "add override" (ADR-0062) still creates policy overrides only.

### D6d. `TestControlPlaneFailover` "address already in use"

- `freeAddr` (`test/integration/gateway_failure_test.go`) listens on `127.0.0.1:0`, closes the listener and returns the address. The failover test binds that one gRPC address three times (a, then b after a's kill, then a2 after b's exit), and between a holder's exit and the next bind the port is free; the OS can then hand the same ephemeral port to any other `:0` listener (other tests in the package, other processes), which makes the next bind fail with "address already in use".
- hardening-carryover first runs the test 20 times (`go test -tags integration -run TestControlPlaneFailover -count 20 ./test/integration/`) and reads, from the failing run's log, which bind fails.
- **Fix, only if it is this race:** the failover test's shared gRPC address comes from a new helper that picks a free port **outside the OS's ephemeral range** (20000–29999, below Linux's 32768 and macOS's 49152 starts), so no `:0` listener can be given it while it is free. REST and admin addresses keep `freeAddr` (each is bound once). No retry of anything is added. One commit of its own. If the failing bind is instead the hand-over itself (the standby binding while the killed process's socket lingers), that is product behaviour: the task records the evidence for the user and changes nothing.

### O1. Shadow report counts by policy name

- **Kept as is** (the user's option b). The shadow report screen's help text (ADR-0063's "?" popover) gains: "Would-deny counts come from Prometheus by policy name. A policy that reuses the name of a deleted policy shows that policy's counts until they leave the window. The top keys come from heavy-hitter windows by policy ID and are this policy's only." The REST answer's existing coverage fields are unchanged.
- Implemented in hardening-carryover with a Vitest check that the text is shown.

## Consequences

- One client address can no longer take a replica's whole connection budget; clients behind one NAT share 1,024 connections per replica, which a deployment can raise.
- The boundary 503s should fall sharply with M7's features; whether strict mode reaches a small share on this laptop is measured and reported, not promised.
- Quota overrides have their own endpoints; the design's Admin API table gains a row (P25).
- The shadow report's by-name counts stay, now documented.

## Alternatives considered

- **Per-IP cap keyed by `X-Forwarded-For`** — there is no request at `Accept`; a header-based cap would come after the connection is already held.
- **A per-IP connection rate instead of a count** — needs a clock and a window per address; a count bounds what is held, which is what crowds others out.
- **Raising the accuracy harness's budget or adding replicas of Redis** — hides the gateway's behaviour under the pattern instead of reducing its calls.
- **`Override.policy_id` nullable with an `oasdiff` exception** — needs `api/oasdiff-ignore.txt` and the user's approval for a breaking change to a v1 response; the separate resource needs neither. The user can still prefer it at the checkpoint.
- **Shadow counts by policy ID (O1 option a)** — needs policy IDs as metric labels or a new per-ID counter; the user chose (b).
