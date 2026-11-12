# 0029. Methods that are not upper case

- **Status:** Accepted
- **Date:** 2026-11-12
- **Requirement IDs:** GW-01, RL-03, RL-04

## Context

ADR-0007 matches route methods exactly and case-sensitively. ADR-0020 upper-cased the method for rate limit policies only and left open "whether to refuse methods that are not upper case". It also assumed that a route listing methods answers `post` with 405. It does not. The router never answers 405: when a path matches a route whose method list excludes the request's method, the walk falls back to the best prefix route, and with no such route the answer is 404.

The M3 review showed what follows. With a route `login` (`/login`, `[POST]`) and a catch-all route `/`, `post /login` is routed to the catch-all, so every policy scoped to `login` is skipped. The request goes upstream as `post /login`, and Django, Werkzeug and Flask serve it as a POST. Sending `POST`, `POST`, `post`, `post`, `post`, `pOST` to a login route limited to one attempt gave 200, 429, 200, 200, 200, 200: unlimited attempts by changing the method's case.

The review's run left this decision open; the run took it on the user's behalf, as for U3 in ADR-0021, which answers 400 for paths that upstreams read differently from the gateway.

## Decision

This adds to ADR-0007 and supersedes ADR-0020's note that the gateway still forwards methods that are not upper case. ADR-0020's policy rule stays, though it now always sees an upper-case method.

- **Requests.** Before routing, right after the CONNECT and query checks, the gateway answers **400** (`invalid request method`, plain text like the other M1 errors, `origin="gateway"`) to a request whose method is not equal to its upper-cased self, meaning any method with a lower-case letter. Nothing else changes: upper-case methods, standard or not, are routed and forwarded as before.
- **Routes.** A route's `methods` must be upper case. `post` is a compile error (`method "post" is not upper case, such as GET`), since no request could ever match it.

## Consequences

- A method in any other case can no longer reach a broader route than its upper-case spelling would, or reach an upstream at all, so the case trick no longer gets around any limit.
- A client that sends lower-case methods gets 400. No standard method is lower case, and the gateway never forwarded such a method to a route that lists methods, so only routes with no method list lose that traffic.
- Not decided here: a `HEAD` request to a route that lists only `GET` still falls back to a broader route (ADR-0007 has no implicit `HEAD`), and a method that a route excludes still falls back instead of getting 405. Upstreams often serve `HEAD` as `GET`, so `HEAD` can get around a `GET` route's limits when a broader route exists. The options are an implicit `HEAD` for `GET` routes (and in policy matching), a 405 when a path matched but the method did not, or keeping the fallback. That needs the user's decision.

## Alternatives considered

- **Match route methods case-insensitively and forward the method unchanged** — closes the same gap and keeps proxying `post`, but upstreams would still see methods whose meaning depends on the upstream, and it changes ADR-0007's exact matching for every route.
- **Upper-case the method for routing and forwarding** — changes what upstreams receive; ADR-0020 rejected it for that reason.
- **Answer 501 (method not implemented)** — RFC 9110's answer for an unknown method, but the gateway forwards unknown upper-case methods, so the issue here is the spelling, not the method; 400 matches U3.
