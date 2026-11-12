# 0032. Answering shed connections

- **Status:** Accepted
- **Date:** 2026-11-12
- **Requirement IDs:** GW-26

## Context

ADR-0030 says a connection accepted past `SPILLWAY_GATEWAY_MAX_CONNECTIONS` gets a fixed `HTTP/1.1 503` "without reading the request", then a half-close and a drain until EOF or the 1 s deadline. It also asked m4-shedding to measure the memory behind the default caps (8192 connections, 4096 requests in flight) and to lower them if the worst case exceeds 1 GiB.

Built as written, the 503 often reaches the client before the client has sent its request. In compose, at 1000 rps against a connection cap of 64, Go's HTTP/1.1 transport failed 0.3% to 1% of shed connections (52 to 83 per run) with `readLoopPeekFailLocked` / "unsolicited response" instead of showing the 503: a client may treat bytes that arrive before its request as not belonging to it.

## Decision

This adds to ADR-0030's connection cap; everything else there stays.

- **The shed path waits for the request's first byte**, within the same 1 s deadline, then writes the fixed 503 and half-closes and drains as before. It never parses the request. On the TLS listener the first byte is the first decrypted byte, after the HTTP/1.1-only handshake.
- **A connection that sends nothing** within the deadline is closed with no answer. It still counts once in `gw_requests_total{reason="shed"}` and still holds one of the shed path's 256 places until the deadline, as it did before while being drained.
- **Measured memory** (`SPILLWAY_MEASURE_MEMORY=1 go test -run '^TestMemoryBudget$' -v ./internal/proxy`, gateway heap plus goroutine stacks, 1000 clients per case, clients and upstream in a separate process): an idle connection costs about 18 KiB plain, 26 KiB over TLS with HTTP/1.1 and 25 to 36 KiB over HTTP/2; a request in flight, held mid-body, 69 to 85 KiB, or 133 to 191 KiB with a 64 KiB PUT body buffered for retries. The worst case at the defaults (8192 × the largest idle connection + 4096 × the largest request on top of its connection) was 0.85 to 0.88 GiB over 4 valid runs, under 1 GiB, so **the defaults stay 8192 and 4096**. This is live memory; the heap can grow past it with GOGC=100 unless GOMEMLIMIT is set, which the gateway sets from the container's limit (GW-27).

## Consequences

- After the change, 0 of about 56,000 shed connections failed in 3 compose runs; every one showed the 503.
- A shed answer now waits for the client's first request bytes, one round trip later than ADR-0030 described; in compose the client still saw it with a p50 of 0.85 to 0.94 ms.
- A client that connects and stays silent past the cap learns nothing before the 1 s close; it would have had to read the 503 without sending a request, which no HTTP client does.

## Alternatives considered

- **Write the 503 at once, as ADR-0030 says** — the measured client failures above.
- **Parse the request line and headers before answering** — more code and more time per shed connection for no gain: the first byte is enough to know the client has sent its request.
