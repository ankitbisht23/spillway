# 0020. Method case in rate limit policies

- **Status:** Accepted
- **Date:** 2026-11-10
- **Requirement IDs:** RL-03, RL-04

## Context

Methods are case-sensitive tokens (RFC 9110 §9.1), and Go's server accepts any token, so `post` reaches the gateway as `post`. ADR-0007 matches route methods exactly and case-sensitively, so a route that lists methods answers `post` 405. A route that lists none proxies it unchanged. ADR-0017 compared a policy's `match.methods` with the method exactly and filled `{method}` with "the request method as sent". A review of M2 found what follows from that: on a route without a method list, `post`, `Post` and `pOST` skipped every POST-only policy, such as ADR-0017's `tenant-writes`, and each spelling got a `{method}` bucket of its own. Many upstream stacks (Django, Werkzeug and Flask among them) upper-case the method and serve `post` as a POST, so a write limit could be bypassed by changing the method's case.

## Decision

This amends ADR-0017's policy matching and its `{method}` placeholder: rate limit policies see the request method upper-cased. `match.methods`, which `Compile` already requires to be upper case, is compared with the upper-cased method, and `{method}` is filled with it. Routing (ADR-0007), the access log, the metrics and the request sent upstream keep the method as the client sent it.

## Consequences

- A method in any case meets the limits of its upper-case spelling and shares its buckets, whatever the upstream makes of it.
- Methods that differ only in case can no longer be limited apart. No standard method has a lower-case meaning, so nothing is lost.
- The gateway still forwards `post` to routes without a method list; whether to refuse methods that are not upper case is a separate decision, not taken here.

## Alternatives considered

- **Answer 400 or 405 to any method that is not all upper case** — closes the same gap and more, but changes which requests the gateway proxies, a routing decision outside M2's rate limiting.
- **Upper-case the method for routing and forwarding too** — would change what upstreams receive and contradict ADR-0007's case-sensitive route matching.
