# 0011. Route transforms, size limits and timeouts

- **Status:** Accepted; stalled request bodies superseded by 0033
- **Date:** 2026-11-04
- **Requirement IDs:** GW-02, GW-03, GW-11, GW-28, SEC-07

## Context

GW-02 asks for prefix stripping, path rewrites and request and response header changes per route; GW-03 for body and header size limits answered with 413 and 431; GW-11 for connect, per-try, overall and idle timeouts. None of them says:

- how a stripped or added prefix meets ADR-0007's rule that the upstream gets the cleaned path;
- in which order header changes apply, or which headers they may touch, given that SEC-07 makes the gateway the owner of hop-by-hop and `X-Forwarded-*` headers;
- what counts towards a header limit, and how a body without `Content-Length` is limited;
- what each timeout covers, what an unset one means, or how they fit GW-28's 60 s write timeout.

Go's transport adds two constraints. It dials with the request context's values but not its cancellation, so a connection being opened outlives a cancelled request. And it does not return from a cancelled request until a pending read of the request body returns.

## Decision

- **Route fields**, all optional:

  | Field | Meaning |
  | --- | --- |
  | `strip_prefix` | leading path segments removed before forwarding, e.g. `/v1` |
  | `add_prefix` | path put in front after stripping, e.g. `/api` |
  | `request_headers`, `response_headers` | `set`, `add` (lists of `{name, value}`) and `remove` (names) |
  | `max_body_bytes` | 413 above it; 0 means no limit |
  | `max_header_bytes` | 431 above it; 0 means the server's 64 KiB, and it can't be more |
  | `timeouts` | `connect`, `per_try`, `total`, `idle`, as Go durations (`200ms`, `3s`) |

- **Path rewrite** (GW-02). `strip_prefix` must be whole leading segments of the route's `match.path` or `match.path_prefix`, `:param` segments included (`/t/:tenant`), and is not `/`. At request time that many segments come off the front of the cleaned path, then `add_prefix`, a clean path not ending in `/`, goes on; an empty result is `/`. With `path_prefix: /v1/orders`:

  | Rewrite | Request | Upstream gets |
  | --- | --- | --- |
  | `strip_prefix: /v1` | `/v1/orders/42` | `/orders/42` |
  | `strip_prefix: /v1/orders` | `/v1/orders`, `/v1/orders/` | `/` |
  | `strip_prefix: /v1`, `add_prefix: /api` | `/v1/orders/` | `/api/orders/` |

  The result depends only on the cleaned path and the config, so it is clean too, and ADR-0007's guarantee holds: nothing the client sends can make the upstream see a path other than the one routed on, rewritten by that route.
- **Header transforms** (GW-02) apply `remove`, then `set` (replacing every value), then `add` (keeping existing values); names are case-insensitive. Request transforms run after the gateway has removed hop-by-hop headers and set `X-Forwarded-*`. Response transforms run on upstream responses after hop-by-hop headers are removed; the gateway's own answers (404, 413, 431, 502–504) are not transformed. Compile rejects:
  - names that would undo SEC-07 or the framing: the hop-by-hop headers `Connection`, `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding` and `Upgrade`, plus `Host`, `Content-Length`, `Forwarded` (which the gateway removes, ADR-0008) and every `X-Forwarded-*`;
  - names that are not HTTP tokens, and values with control characters other than tab, so a value can't inject a header.
- **Size limits** (GW-03), checked before anything reaches the upstream:
  - Headers: each field counts as `Name: value\r\n`, `Host` included, the request line not; above `max_header_bytes`, 431.
  - Body with a `Content-Length` above `max_body_bytes`: 413 without reading it.
  - Body of unknown length (chunked, or HTTP/2 without `content-length`): counted as it streams upstream through `http.MaxBytesReader`; past the limit the upstream request is aborted and the client gets 413. The `ResponseWriter` is not passed to `MaxBytesReader`, because the transport reads the body on its own goroutine and must not touch it.
- **Timeouts** (GW-11):

  | Timeout | Default | Covers |
  | --- | --- | --- |
  | `connect` | 5 s (ADR-0008's value, now per route) | opening one upstream connection |
  | `per_try` | `total` | one upstream attempt |
  | `total` | 15 s; at most 59 s | the whole request, from routing to the last byte of the response body |
  | `idle` | none | the time without a body byte moving in either direction: read from the client's request body or from the upstream's response body |

  - `total` stays at most 59 s so a 504 still fits inside the 60 s write timeout (GW-28); a test checks the two constants agree. The other timeouts have no upper bound, but `total` bounds them all.
  - Until retries arrive (GW-12, M4) a request has one attempt, so `per_try` and `total` bound the same span and the earlier wins.
  - `connect` travels in the request context to the registry's dialer, which applies it to the dial, since the transport keeps context values for a dial but not the request's deadline.
  - A timeout before the response headers answers 504. After them the response is cut: the HTTP/1.1 connection is closed, or the HTTP/2 stream reset.
  - On a `101 Switching Protocols` the idle timer stops, because wrapping the upgraded connection would make it unwritable; `total` still bounds it.

## Consequences

- Every route now has a 15 s overall timeout. A route that needs longer sets `total` up to 59 s; anything longer-lived, such as server-sent events or WebSockets (GW-05, Stretch), needs a requirement that raises the write timeout per route.
- A client that stops sending its request body has its upstream request aborted when `idle` or `total` fires, freeing the upstream at once. The client itself is answered only when it sends more or the server's 60 s read timeout (GW-28) closes the connection, because Go's transport waits for the pending body read. Slow clients stay GW-28's job.
- An oversized chunked body reaches the upstream up to the limit, then the connection is closed; the upstream never gets more.
- `per_try` behaves exactly like `total` until M4 adds retries; the config already has its final shape.
- A transform that names a gateway-owned header fails the whole config, and the gateway keeps its last good snapshot.

## Alternatives considered

- **Strip the literal prefix string at request time** — fails for prefixes with `:param` segments, and needs its own boundary rule; counting segments reuses the router's.
- **Regex path rewrites** (Envoy's `regex_rewrite`) — more than GW-02 asks for, and the forwarded path gets harder to reason about against ADR-0007.
- **A default body limit**, such as nginx's 1 MiB — existing uploads would start failing without any config change; routes opt in.
- **408 when `idle` fires before the response headers**, as Envoy does — every other upstream timeout answers 504, and in practice an idle stream before the headers is a silent upstream.
- **`http.MaxBytesReader` with the `ResponseWriter`** — on overflow it marks the response from the transport's goroutine while the handler may be writing headers, a data race.
- **Transforms on any header** — a transform could put back `Connection` or rewrite `X-Forwarded-For`, undoing SEC-07.
