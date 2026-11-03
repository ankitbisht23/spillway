# 0007. Route matching: precedence and path rules

- **Status:** Accepted
- **Date:** 2026-11-03
- **Requirement IDs:** GW-01, GW-02, CP-03

## Context

GW-01 asks for matching on host, method, path (exact, prefix, `:param`) and header predicates through a radix tree, with explicit priority breaking ties. It does not say what counts as a tie: whether an exact path beats a longer prefix, whether a host-specific route beats a more specific path without a host, or where method and header predicates rank. Prefix matching also needs a boundary rule (`/v1/orders` against `/v1/ordersx`), and paths need a canonical form: if the gateway routed on `/admin/../public` but forwarded it unchanged, an upstream could resolve it to a path that a different route guards. The router has to answer the same way however routes are listed, and in O(path length) rather than a scan over every route.

## Decision

- **Tree.** `internal/router` keeps one radix tree per host plus one for routes without a host. Each edge is a whole path segment; a node has static children and one `:param` child, and static children are tried first.
- **Precedence**, highest first:
  1. A route with the request's host beats a route without a host. Hosts match exactly and case-insensitively, ignoring the port and a trailing dot. When no route for that host matches, routes without a host are tried.
  2. An exact path beats any prefix.
  3. Between exact paths, the first segment where they differ decides: a static segment beats a `:param`.
  4. Between prefixes, the longer one (in segments) wins; at equal length, rule 3 applies.
  5. More predicates win. A method list counts as one predicate, and each header predicate counts as one.
  6. Higher `priority` wins.
  7. The route listed first wins.
- **Paths.** Route paths must be clean: they start with `/` and contain no `//`, `.` or `..` segments. Prefixes match whole segments, so `/v1/orders` matches `/v1/orders`, `/v1/orders/` and `/v1/orders/42` but not `/v1/ordersx`; a prefix must not end in `/`, except `/` itself, which matches everything. Exact paths are strict about a trailing slash. A `:name` segment matches any non-empty segment; names are unique within a route.
- **Canonical request path.** The gateway routes on `router.CleanPath(URL.Path)`, the decoded path with duplicate slashes and `.` and `..` segments removed and a trailing slash kept, and forwards that same path upstream (M1 proxy task), so routing and upstream always see the same path.
- **Header predicates** match when any value of the header equals `exact` or fully matches `regex` (anchored, RE2). All of a route's predicates must match. The `Host` header is matched through the route's host, never through a predicate.
- **Methods** match exactly and case-sensitively, with no implicit `HEAD` for `GET`.

## Consequences

- One depth-first walk, static before parameter, meets candidates in precedence order: the first exact match found is the answer, and prefixes only need the deepest one. The M1 benchmark matches against 1,000 routes in about 100 ns with no allocations.
- The router is tested against a linear-scan reference that applies the rules above literally, on random and fuzzed route sets.
- Two routes with the same host, path and predicates both compile; the later one is unreachable unless its priority is higher. Warning about shadowed routes belongs in CP-03 validation (M5).
- No wildcard hosts, no regex paths and no header-presence predicates. Each would be a new rule in this order and needs an ADR that supersedes this one.

## Alternatives considered

- **Priority first, specificity second** — an operator could not add a specific route without re-numbering broader ones; the requirement calls priority a tie-breaker.
- **Character-level radix tree (httprouter style)** — compresses static prefixes slightly better, but segment boundaries and `:param` segments become special cases inside edges; with whole-segment edges they fall out of the structure.
- **Envoy-style virtual hosts with no fallback** — a host with routes would answer 404 for paths that a host-less route serves, which surprises more than it protects.
- **Rejecting unclean request paths with 400** — safe, but breaks clients that send `//` by accident; cleaning gives the same guarantee because the cleaned path is what gets forwarded.
