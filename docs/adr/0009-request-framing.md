# 0009. Request framing: re-frame instead of rejecting Content-Length with Transfer-Encoding

- **Status:** Proposed
- **Date:** 2026-11-04
- **Requirement IDs:** SEC-07

## Context

SEC-07 says that a request with both `Content-Length` and `Transfer-Encoding` is rejected, to prevent request smuggling. Smuggling works when two HTTP parsers on the path disagree about where a request's body ends. The gateway's HTTP/1.1 parser is Go's `net/http` server, and for such a request it:

- treats `Transfer-Encoding: chunked` as the framing and removes `Content-Length` before the handler runs, so the handler cannot tell the request had both;
- answers 400 to several `Content-Length` values that differ;
- answers 501 to any `Transfer-Encoding` other than a single `chunked`, including `gzip, chunked` and a repeated `chunked`.

RFC 9112 §6.3 allows exactly this: a server MAY reject such a message, and an intermediary that forwards it MUST first remove `Content-Length` and use the chunked framing. The gateway then sends the body on its own connection to the upstream with framing it chose itself, so the upstream never sees both headers. The only parsers that read the client's original bytes are Go's and whatever sits in front of the gateway.

Rejecting with 400, as SEC-07 says, would mean reading each request's raw header lines before `net/http` parses them. That needs a wrapper around every connection which parses HTTP/1.1 framing itself, so that it knows where each keep-alive request starts. That is a second HTTP parser in the gateway, the very kind of disagreement smuggling exploits.

## Decision

Proposed: keep Go's behaviour and change SEC-07's text from "conflicting `Content-Length` and `Transfer-Encoding` rejected" to:

> Requests with both `Content-Length` and `Transfer-Encoding` are re-framed: `Transfer-Encoding: chunked` wins, `Content-Length` is dropped and the upstream receives the body with the gateway's own framing (RFC 9112 §6.3); differing `Content-Length` values are rejected with 400 and any `Transfer-Encoding` other than a single `chunked` with 501.

`TestRequestFraming` in `internal/proxy` pins this behaviour, so a Go upgrade that changes it fails the build.

## Consequences

- No extra parser on the hot path, and no extra code to get wrong.
- A client that sends both headers gets an answer instead of a 400. That is harmless when the gateway is the edge. If a proxy in front of the gateway frames by `Content-Length`, the two could disagree; the M7 kind deployment and the demo's Caddy proxy both terminate HTTP themselves and re-frame, so neither forwards the conflicting pair.
- HTTP/2 is unaffected: frames carry the body length, and Go rejects a `Content-Length` that disagrees with them.

## Alternatives considered

- **Connection wrapper that parses raw headers and rejects with 400** — meets the current wording, but adds a second HTTP/1.1 parser that has to track keep-alive and pipelined requests exactly as `net/http` does.
- **A patched or forked `net/http`** — a permanent maintenance cost on every Go release.
