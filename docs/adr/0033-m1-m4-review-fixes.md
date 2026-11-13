# 0033. M1–M4 review fixes

- **Status:** Accepted; the breaker question left open is decided in [ADR-0034](0034-user-decisions-after-the-m4-review.md)
- **Date:** 2026-11-13
- **Requirement IDs:** GW-01, GW-07, GW-11, GW-12, GW-13, GW-14, GW-26, GW-28, OB-02, RL-03, RL-04

## Context

A review of M1–M4 on main (badcc13) found behaviour that the M4 ADRs either did not foresee or described differently from what the code did. Most findings were bugs fixed against the ADRs' own text (a breaker outcome from an earlier state, the access log's trace ID on shed requests, flaky tests). The ones below change or add to a decision, so they are recorded here:

- **Waiting connections pin the connection cap.** `SPILLWAY_GATEWAY_MAX_CONNECTIONS` (ADR-0030, ADR-0032) counts a connection from accept to close. An idle keep-alive connection lasts the server's 120 s idle timeout, and a silent new one its 10 s read-header timeout (GW-28). A 429 keeps the connection open. So one client holding about 8192 idle connections, or reopening about 820 silent ones a second, got every other client shed. In M7 the gateway is the edge, with no ingress in front.
- **A stalled request body holds its places.** ADR-0011 noted that when a client stops sending its body, Go's transport waits for the body read it started, so the handler returns only at the server's 60 s read timeout. M4 added shared places that this holds: the request's in-flight place (GW-26) and its place in the upstream's `max_concurrent` (GW-14). About 17 stalled uploads a second kept an upstream's default 1024 places taken.
- **A retry with nowhere to go.** When a DNS refresh left an upstream with no address between attempts, ADR-0030's order of checks had already charged the budget and counted `gw_retries_total{outcome="sent"}`, the previous answer had been discarded, and the client got a 502 instead of that answer (ADR-0008 answers 503 when there is no address).
- **Round robin with retries.** ADR-0030 has round robin turn the upstream's one counter modulo the candidates. A retry's candidates exclude the endpoints it tried, so retries moved the first attempts' turn: with one of three endpoints failing, it got half of the first attempts and the third endpoint none.
- **D1** (ADR-0030) adds `HEAD` to every route that lists `GET` and not `HEAD`. Under ADR-0007, two routes with the same host, path and predicates both compile and the first listed wins, so a route listing `HEAD` alone after a `GET` route on the same path would lose its `HEAD` requests to it.
- **ADR-0031, rule (c)** says that 5 retried requests are "guaranteed by construction". PR #44 found an edge it missed (below).

## Decision

### Connection cap: a waiting connection gives way (GW-26, GW-28)

This adds to ADR-0030's connection cap and ADR-0032; everything else there stays.

- A connection is **waiting** while it has no request in progress: from accept until its first request starts, and between requests on a keep-alive connection. On HTTP/2, while it has no open stream. The traffic server's `http.Server.ConnState` hook (`ConnLimit.ConnState`) keeps a list of waiting connections in the order they started waiting (`StateNew` and `StateIdle` add one; `StateActive`, `StateHijacked` and `StateClosed` remove it).
- **At the cap**, a new connection takes the place of the connection that has waited longest, which is closed at once (no `GOAWAY` or `Connection: close`, since it has no request to carry one). Only when no connection is waiting, so that every connection has a request in progress, is the new one shed as ADR-0030 and ADR-0032 say.
- An info log line `closing waiting connections at the connection cap`, with `connections` (the count since the last line) and `max_connections`, goes out at most once a second. No metric is added: the connection was not refused.
- RFC 9112 §9.5 lets a server close an idle connection at any time, and clients retry a request that raced with such a close when it is safe (Go's transport does for idempotent requests). nginx does the same when it runs low on connections (it reuses keep-alive connections). A request that arrives on a connection just as it is closed can fail; that needs the cap to be reached at that moment.
- A flood of new connections still sheds when every connection has a request in progress, and a client opening connections faster than legitimate clients send their first request can still crowd them out. A per-client-IP connection cap would bound that. It is not added now; M7 (the gateway at the edge) is where it would be weighed.

### Stalled request bodies (GW-11, GW-14, GW-26)

This supersedes ADR-0011's consequence that a client which stops sending its body is answered only at the server's read timeout.

- When the route's `total` or `idle` timeout ends a request whose body is still arriving (it has a body, and no read of it has returned an error or EOF), the gateway sets the client connection's read deadline to now (`http.ResponseController.SetReadDeadline`; on HTTP/2, the stream's). The pending body read fails at once, the transport's write loop ends, and the handler returns, freeing the in-flight place, the `max_concurrent` place and any concurrency-limit places. The client gets the 504 if it is still reading.
- The hook is removed before the handler returns, so it never touches a connection's next request. Requests without a body pay nothing.

### Retries: the endpoint is picked before the budget (GW-12, GW-13)

This amends ADR-0030's "Order of checks for attempt n + 1". Between check 3 (the breaker) and check 4 (the budget), the retry's endpoint is picked (preferring one not yet tried). If the upstream has no address, the retry is dropped like any other: the client gets attempt n's answer, nothing is charged or counted, and the server span records `spillway.retry_dropped="no_address"`. The endpoint is therefore chosen before the backoff, not after it; an endpoint ejected during the backoff (at most 250 ms) still gets that one attempt, as a request already holding an endpoint does on a reload (ADR-0030).

### Round robin: retries turn their own counter (GW-07)

This amends ADR-0030's `round_robin`. First attempts turn the upstream's counter modulo the candidates, as before. Retries turn a second counter of the upstream's, modulo their candidates. First attempts then get exactly their share whatever the retries do, and a failing endpoint's retries are spread over the others in turn.

### D1: a route that lists HEAD keeps it

D1 is implemented as ADR-0030 records it, with one guard: a `GET` route does not get `HEAD` added when another route with the same host and the same `path` or `path_prefix` lists `HEAD` itself. Policies always get it, since every matching policy applies.

### D2

Implemented as ADR-0030 records it: a warning per Redis policy (not `local`, mode not `off`) whose key template has a `{header:<Name>}`, logged on every config load; docs/setup.md says such limits should be local or keyed by a header the client cannot vary freely. Nothing is rejected. The Redis `used_memory / maxmemory` alert stays a note for M7's alert rules (OB-05).

### Shed requests' trace ID (OB-02)

As ADR-0030 already requires, every access-log line has a trace ID when tracing is on. A request shed by the in-flight cap gets no span, so when it brings no valid `traceparent`, its line gets a random trace ID that no span uses.

### A note on ADR-0031, rule (c)

ADR-0031 says rule (c) always has at least 5 retried requests "by construction": ejection needs 5 consecutive failures, and each is retried within the budget's floor. That missed an edge. A request sent within microseconds before the fault time t_f could reach the mock after the fault and be one of the 5 failures, while (c) counts only requests sent at or after t_f; 4 of 23 runs of the 5xx scenario failed with `too few retried requests: 4`. PR #44 fixed it in the sender: it starts the fault itself, between two requests, at least 1.5 ms after the last request it sent (at most 3/4 of the request interval), so no request sent before t_f can reach the mock after the fault. No rule changed. The guarantee holds with that sender rule, not by construction alone.

### Not decided here: client-induced failures and the breaker

The review also found that the breaker counts every 5xx answer (ADR-0030, "Attempt outcomes"), and it is shared by every route and client of the upstream. A client that can make the upstream answer 500 (one bad path) opens it for everyone after `min_requests` attempts, and one such request per `open_for`, timed by `Retry-After`, keeps it open: the failed half-open probe reopens it. GW-14 does not say what an error is, so this is ADR-0030's choice, and changing it changes what the breaker protects against. The options, for the user to decide: (a) count only failures that point at the upstream's health for the breaker (connect errors, resets, the attempt's own timeout, and 502, 503 and 504, not 500, 501 or 505–599), as Envoy's gateway-error mode does; (b) reopen from half-open only when a share of the probes fail, not one; (c) a per-route opt-out of breaker counting. Until then the rule stays as ADR-0030 has it. Outlier ejection's "5xx" comes from GW-09's text and is not part of the question.

## Consequences

- One client can no longer get every other client's connections shed with idle or silent connections; idle clients reconnect when the gateway is at its connection cap.
- A stalled upload frees its places at the route's timeout instead of after 60 s.
- `gw_retries_total{outcome="sent"}` counts only retries that went upstream, and the budget only charges those.
- A config with a `HEAD`-only route after a `GET` route on the same path keeps its behaviour.
- Redis policies keyed by a header log a warning on every load, the dev config's two included.
- ADR-0007 (no implicit `HEAD`), ADR-0011 (stalled bodies), ADR-0030 (round robin, order of retry checks, connection cap at capacity) and ADR-0032 (when a connection past the cap is shed) are superseded in those parts. Their status lines say so.

## Alternatives considered

- **Shorter idle and first-request timeouts near the cap** — still lets a client hold the cap for the shortened time, and needs a threshold to tune.
- **A per-client-IP connection cap now** — needs the client IP before the request (no `X-Forwarded-For` yet), a new setting and its default; deferred to M7.
- **Closing the request body when total fires** — `Close` waits for the pending `Read` on the same body, so it does not unblock it.
- **Picking the retry's endpoint after the backoff and, with none, returning attempt n's answer** — the budget would already have been charged; picking before the budget keeps the metrics exactly what was sent.
- **A random untried endpoint for round-robin retries** — fair on average, but not deterministic; a second counter keeps round robin exact.
- **D1 without the guard** — silently takes `HEAD` from a route written for it.
