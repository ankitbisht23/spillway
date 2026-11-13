# 0034. User decisions after the M4 review

- **Status:** Accepted
- **Date:** 2026-11-13
- **Requirement IDs:** GW-09, GW-12, GW-13, GW-14, NFR-04, NFR-13

## Context

M4 ended (PRs #37–#45) with four questions for the user. On 2026-11-13 the user answered all four:

1. **The breaker and client-induced failures (ADR-0033, "Not decided here").** The breaker counts every 5xx answer as a failure (ADR-0030, "Attempt outcomes"), and there is one breaker per upstream, shared by every route and client. A reviewer showed that 20 requests to a path answering 500 open it for every client, and that one such request per `open_for`, timed by `Retry-After`, keeps it open: the failed half-open probe reopens it. The user chose ADR-0033's option (a): count only the failures that mean the upstream itself is unhealthy.
2. **`make verify-m3` on the dev laptop.** It passed 3 of 6 full runs during M4. Every failure was a host or Docker Desktop VM stall that made Redis calls miss the 5 ms budget, beyond what ADR-0027 tolerates (one Redis error per NFR-04 gateway run; none in the killed-primary test):
   - **NFR-04, main 10487ba, 2026-11-13, gateway run 3:** 439 Redis timeouts and 439 `fail_closed` 503s; the run voided its pair and the check failed with no comparison. Gateway-1's access log in Loki puts them in two places: one at 11:21:12.906Z (latency 6.3 ms, the regular 4–9 ms stall that ADR-0027 describes), then 438 from 11:21:32.541Z (the earliest one's arrival) to 11:21:33.329Z (the latest one's answer), **788 ms**, in two clusters (264 in 274 ms, then 174 in 247 ms, 261 ms apart), latencies 5.0–108.8 ms.
   - **NFR-04, M3 (ADR-0027):** 165 Redis errors within about **370 ms**.
   - **Killed primary, #45's branch:** one failure-mode decision for a key of the killed primary about 5 s before the kill, so outside its failover window; the check failed.

   The user chose to extend ADR-0027: tolerate **one** stall burst, bounded in time, reported and not voiding, in each NFR-04 gateway run and in the killed-primary "failure modes only in window" check. Never a re-run.
3. **D4 wording (ADR-0030).** ADR-0030 proposed new text for GW-12 and GW-13 so that the design describes `retry.non_idempotent`. The user approved it.
4. **D1 and D2 (ADR-0030, ADR-0033).** The user confirmed both as built in PR #45.

## Decision

### 1. The breaker counts gateway errors only (GW-14)

This supersedes the "Outlier and breaker" column of ADR-0030's attempt-outcome table, and settles ADR-0033's open question. ADR-0030's table becomes:

| Outcome | Outlier (GW-09) | Breaker (GW-14) | Retryable (GW-12) | Client answer if it is the last attempt |
| --- | --- | --- | --- | --- |
| Response 1xx–4xx | success | success | no | the response |
| Response 500, 501, 505–599 | failure | **success** | no | the response |
| Response 502, 503, 504 | failure | failure | yes | the response |
| Connect error: refused, unreachable, connect timeout | failure | failure | yes | 502, or 504 for the connect timeout |
| Reset or EOF before the response headers | failure | failure | yes | 502 |
| The attempt's own deadline (per-try, or the total when it is earlier) fires before the headers | failure | failure | yes, if time is left | 504 |
| Client went away; idle timeout (GW-11); route body limit (413) | not counted | not counted | no | as before |
| Gateway refusal: dial-time allow-list (SEC-06), `max_connections`, `max_pending` | not counted | not counted | no | as before |

- **A breaker failure** is exactly a retryable outcome: 502, 503 or 504 from the upstream, a connect error, a reset or EOF before the headers, or the attempt's own deadline. These are the answers of an upstream (or of a proxy in front of it) that cannot serve, Envoy's "gateway error" set.
- **500, 501 and 505–599 count as successes for the breaker**, not as nothing. The upstream answered with a response of its own, which shows it is reachable and serving; the breaker protects against an upstream that cannot answer, not against a path that answers an error. Counting them keeps the breaker's maths as ADR-0030 has it, with one meaning per term: every attempt with an outcome enters the window, `min_requests` counts those attempts, and the error rate is gateway errors over them. A half-open probe answered 500 is a successful probe. Counting them as nothing was the alternative: a probe answered 500 would then neither close nor reopen the breaker, and the window's size would depend on which paths clients call. As successes they can lower the error rate, but so can any 2xx answer: a client gets no lever it did not have.
- **Unchanged:** outlier ejection counts every 5xx (GW-09 says "5xx"), so an endpoint that answers 500 to everything is still ejected; retries are unchanged (GW-12 lists connect failure and 502/503/504); a request's attempt span still records a 5xx as a failed attempt (OB-02); `gw_circuit_state`, the transition log lines and every breaker parameter stay as they are.
- In the code, the outlier detector gets the attempt's failure as before, and the breaker gets it only when the outcome is retryable (`gatewayError` in `internal/proxy/retry.go`).

**Why (recorded at the user's request):** a client that can make the upstream answer 500 (one bad path) could open the per-upstream breaker for every client after `min_requests` attempts, and keep it open with one request per `open_for` timed by `Retry-After`.

### 2. One stall burst tolerated in the M3 exit check (NFR-04, NFR-13)

This supersedes ADR-0027's rule for NFR-04 gateway runs ("at most one Redis error") and extends ADR-0021's killed-primary rule "failure modes only in window". Everything else in ADR-0021, ADR-0025, ADR-0026 and ADR-0027 stays: the 5 ms budget, the full-outage 10 ms line with no count allowance, `fail_closed` in `test/verify/m3-latency.yaml`, the killed-primary rules for surviving primaries' keys, for other answers and for the 10 s failover window, the accuracy rules, and NFR-03's check in `make verify-m2`.

**A burst** is a set of excused failures that all fall within **one window of at most 1,000 ms**, from the earliest one's due time (when the sender scheduled the request) to the latest one's answer. Every excused failure must come from a Redis **timeout**: a conn or script error is never excused. Failures that do not fit in one such window (two bursts, or one longer burst) fail the check as before. A single failure is a burst of one.

**Why 1,000 ms:** the longest measured stall event is the 788 ms one above (two clusters of failures 261 ms apart, which one window covers), the next 370 ms. 1 s is about 1.27 × the longest, the first round number above it. It is the burst's whole extent, so at 1,000 rps it excuses at most about 1,000 requests out of the 90,000 a gateway run sends, warm-up included, and only in one stretch. A gateway that fails Redis calls twice in a run, or for more than a second, still fails.

#### NFR-04 gateway runs (`test/verify/m3.sh`, `test/verify/added-latency.sh`, `cmd/loadgen`)

- **What the check has.** Gateway-1's counters before and after the run, warm-up included, as before: `verify-redis` `allow` (a), `deny` (d) and `fail_closed` (f) decisions, `gw_redis_errors_total` over every kind (e), and now also its `kind="timeout"` series (t). And loadgen's result, which gets a new `failures` object: every request of the run, **warm-up included**, that got no answer or a non-2xx answer, with `count`, `first` (the earliest one's due time), `last` (the latest one's answer, or its give-up time when it got none) and `span_ms` (last − first); it is absent when there are none. Loadgen's other fields keep their meaning (measured requests only).
- **The run's failures are excused, and the run and its pair still count, when all of:** d = 0; a + f ≥ the measured requests sent (as before); t = e, every Redis error a timeout; f = e, each error made one `fail_closed` decision (the config has one policy and one call per request); `failures.count` = f, every failed answer is one of those 503s and each of them reached loadgen; and, when e ≥ 1, `failures.span_ms` ≤ 1,000. Otherwise the run voids its pair as before. The measured part may then have at most f failed answers, as ADR-0027's caller rule already says.
- **Unchanged:** the added-latency comparison (medians of three alternating pairs, PASS when 0 ≤ added p99 ≤ 5 ms), the 99% rate rule, and that a direct run voids on any failed answer.
- **The excused requests stay in the run's latency distribution.** They are answers the client got, and loadgen's histogram has every answer; taking them out would hide what the stall cost. A burst can raise that one run's p99 (the 10487ba run's was 7.6 ms); the comparison takes the median of three gateway runs, so one such run does not move it.
- **What it prints.** The run's line keeps its counts and adds, when a burst was excused, `<n> timeouts in one burst of <span> ms, <first>–<last> UTC, excused (ADR-0034)`; when the run voids for this rule, `not excused:` and the failed condition (for example `failures span 20423 ms > 1000 ms`).

#### Killed primary (`cmd/chaos`)

- **What the check has.** The sender's outcomes: each request's due time, answer time and status, and whether it carries `Spillway-Decision-Time`. To tell a timeout from a conn error per request (the failover's own failures are conn errors, at the same time), dev mode gets a second header: **`Spillway-Redis-Error`**, on a response whose limiter calls failed, lists each failed call's kind in order, comma-separated (`timeout`, `conn` or `script`, as `gw_redis_errors_total` counts them; a call skipped because the budget ran out is `timeout`, as ADR-0021 counts it). Refunds, which never decide a request, are not listed. Like `Spillway-Decision-Time` (ADR-0021), it is sent only when `SPILLWAY_GATEWAY_DEV_MODE` is `true`, and an upstream's copy never passes.
- **Excused:** the failure-mode decisions for the killed primary's keys that fall outside their window (answered before the kill was requested, or sent after the key's window ended) pass the check when every one carries `Spillway-Redis-Error: timeout` and they all fit in one burst (≤ 1,000 ms, flag `-max-burst`). Otherwise the check fails as before.
- **What it prints:** the line keeps its counts and, when there are such decisions, adds `<n> in one burst of timeouts, <from> – <to> ms from the kill, <span> ms, excused (ADR-0034)`, or `not excused:` and why (a non-timeout kind, or the span); the JSON report has `outside_window_burst` with the count, the window and whether it was excused.
- **Unchanged:** failure-mode decisions for keys on the surviving primaries still fail the check, with no tolerance: the user extended only the "failure modes only in window" check. So a stall that also hits a surviving primary's key still fails the run.

**No re-run.** A check that fails is reported as failed; the scripts never run it again.

### 3. GW-12 and GW-13 wording (D4)

The user approved ADR-0030's proposed text. docs/design.md now reads:

- **GW-12** (M4) Retries on connect failure and 502/503/504, idempotent methods only, unless a route opts in for all methods (`retry.non_idempotent`), exponential backoff with jitter.
- **GW-13** (M4) Retry budget per upstream (`retry_budget` on the upstream): retries ≤ 20% of requests over the last 10 s, plus a floor of 3 retries/s; routes set attempts (and may opt non-idempotent methods in) only. Excess retries are dropped, not queued.

No other requirement text changes.

### 4. D1 and D2 confirmed

The user confirmed D1 (`HEAD` matches routes and policies that list `GET`, unless another route on the same path lists `HEAD` itself; ADR-0030, ADR-0033) and D2 (a Redis policy keyed by a client-chosen header logs a warning on every config load, nothing is rejected, the docs say such limits should be local or bounded, and the `used_memory` alert is a note for M7's OB-05; ADR-0030, ADR-0033) as built in PR #45. No code changes.

## Consequences

- A client can no longer open an upstream's breaker for everyone with a path that answers 500, nor hold it open with one request per `open_for`. An upstream that answers 500 to every request no longer trips its breaker either; outlier ejection still ejects its endpoints (up to `max_ejected_percent`), and its 500s still reach the clients.
- One stall burst no longer voids an NFR-04 gateway run or fails the killed-primary window check. Two bursts in one run, a burst longer than 1 s, or any conn or script error still do. On the 10487ba run above, this rule would still fail: its isolated error 19.6 s before the burst makes two bursts.
- Dev-mode responses whose Redis calls failed carry `Spillway-Redis-Error`. Production, with dev mode off, is unchanged.
- loadgen's JSON gains `failures`; nothing else reads it.
- ADR-0021 (killed-primary window rule, extended), ADR-0027 (NFR-04 one-error rule), ADR-0030 (breaker column of the attempt-outcome table; D4's wording, now applied) and ADR-0033 (the open breaker question) are superseded or settled in those parts. Their status lines say so.

## Alternatives considered

- **Breaker: reopen from half-open only when a share of the probes fail (option b)** — a client's 500s would still open the breaker; it only makes holding it open harder.
- **Breaker: a per-route opt-out (option c)** — every operator must find the routes a client can make fail; a forgotten one is the same hole.
- **500s count as nothing for the breaker** — a probe answered 500 would resolve nothing, and the window's size would depend on the mix of paths (above).
- **A burst window of 500 ms or 2 s** — 500 ms is below the 788 ms measured; 2 s excuses twice the requests for no measured need.
- **Placing NFR-04's failures by gateway-1's access log** — it has every 5xx with its time, but reading it means parsing a minute of compose logs per run, and loadgen already sees every answer on the same clock as its schedule.
- **Excusing only killed-primary decisions answered before the kill** — then their kind can be read from the metrics, as nothing else fails before the kill; but most of the run (about 24 of 40 s) comes after the window, where a stall is as likely.
- **Re-running a failed check** — the user ruled it out again.
