# 0013. Request IDs, access logs and request metrics

- **Status:** Accepted; trace context passthrough and the access log's trace ID source superseded by [ADR-0030](0030-resilience-model.md)
- **Date:** 2026-11-04
- **Requirement IDs:** GW-02, GW-22, OB-01, OB-03

## Context

GW-22 asks for `X-Request-Id` to be generated or propagated and for W3C `traceparent` to be propagated. OB-03 asks for a JSON access log line per request, sampled except for denials and 5xx, on stdout and through OTLP to the Collector and Loki. OB-01 asks for `gw_requests_total`, `gw_request_duration_seconds` and `gw_upstream_duration_seconds` with `reason` and `tenant` labels, keeping consumers, keys and raw paths out of labels and capping tenants at the top 50 by traffic. None of them says:

- which client request IDs to trust, what a new one looks like, or whether a route transform may change it;
- which fields a line has, what "sampled" means, or which answers count as denials;
- how the gateway reaches the Collector, or what happens when the Collector is down;
- what each label value is in M1, before rate limiting (M2) and tenants (M5) exist;
- what an upstream attempt's duration covers;
- how "top 50 by traffic" is measured, and what happens to a tenant that drops out.

## Decision

- **Settings**, from the environment like ADR-0008's:

  | Variable | Meaning | Default |
  | --- | --- | --- |
  | `SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE` | share of the sampled requests (below) that are logged, 0 to 1 | `1` |
  | `SPILLWAY_GATEWAY_OTLP_ENDPOINT` | the OTel Collector as `http://host:port` (plaintext) or `https://host:port` (TLS); logs go there over OTLP/gRPC as well as to stdout | none: stdout only |

  Compose sets `http://otel-collector:4317`. The exporter still reads the standard `OTEL_EXPORTER_OTLP_*` variables for headers, timeout, certificate and compression; the endpoint comes only from the setting.
- **Request ID** (GW-22). The gateway keeps the client's `X-Request-Id` if the client sent exactly one value of 1 to 128 characters from `[A-Za-z0-9._-]`; otherwise it makes a new one with `crypto/rand.Text` (26 base32 characters, 128 random bits). The ID goes to the upstream and back on every response, the gateway's own answers included, replacing any value the upstream sent. Route transforms may not name `X-Request-Id`; it joins ADR-0011's list of gateway-owned headers. Process log lines about a request (an upstream failure, no upstream address) carry its `request_id`.
- **Trace context** (GW-22). `traceparent` and `tracestate` go upstream unchanged, valid or not. If `traceparent` is valid by W3C Trace Context (parsed with OpenTelemetry's propagator), its trace ID goes in the access log. The gateway starts no spans until OB-02 (M4).
- **Access log** (OB-03). One line per request, written when the request ends, with message `access` at level INFO:

  | Field | Value |
  | --- | --- |
  | `request_id` | the request ID |
  | `trace_id` | from a valid `traceparent`, else empty |
  | `route` | the matched route's name, or `none` |
  | `method` | the request method as sent |
  | `status` | the status sent to the client; 499 if the client left before an answer |
  | `latency_ms` | from receiving the request to the end of the response, in milliseconds |
  | `bytes_in`, `bytes_out` | request body bytes read from the client, response body bytes written to it |
  | `upstream` | the upstream's name, or empty if none was chosen |
  | `decisions` | `[]` until rate limiting (RL-27, M2) |
  | `key_hash` | empty until API keys (SEC-03, M2) |

  Lines carry no path, query, headers or client address, which can hold personal data or tokens.
- **Sampling** (OB-03). Every 5xx is logged, and so is every 4xx the gateway answered itself (404, 405, 413, 431, 499; 429 from M2): these are the denials. Every other request is logged if its request ID falls within the sample rate: the first 8 bytes of the ID's SHA-256, as a fraction of 2^64, below the rate. The decision depends only on the ID, so every replica, and every hop that keeps the ID, decides alike, and a lower rate logs a subset of what a higher one logs.
- **OTLP** (OB-03). The gateway's one `slog` logger writes JSON to stdout and, when the endpoint is set, through the OpenTelemetry slog bridge to an OTLP/gRPC log exporter, so process logs reach Loki too. Records are batched (export every second, queue of 2048) and dropped when the queue is full, so a slow or missing Collector never holds up a request. The resource carries `service.name=spillway-gateway`, `service.version` and `service.instance.id` (the host name, which tells replicas apart). Export errors go to stdout only, never back into the queue. On shutdown the queue gets up to 5 s to flush. The Collector's existing `logs` pipeline forwards to Loki's OTLP endpoint, where attributes become structured metadata: `{service_name="spillway-gateway"} | request_id="…"` finds a request. Modules: `go.opentelemetry.io/otel`, `otel/sdk` and `otel/sdk/log` v1.47.0, `otlploggrpc` v0.23.0, `contrib/bridges/otelslog` v0.21.0.
- **Metrics** (OB-01), on the admin port:

  | Metric | Labels |
  | --- | --- |
  | `gw_requests_total` | `route`, `method`, `code_class`, `origin`, `reason`, `tenant` |
  | `gw_request_duration_seconds` | `route`, `tenant` |
  | `gw_upstream_duration_seconds` | `upstream`, `attempt` |

  - `route` is the route's name, or `none` when no route matched; a route may not be named `none`.
  - `method` is one of RFC 9110's eight methods or `PATCH`, else `other`.
  - `code_class` is `1xx` to `5xx`, or `other` for codes an upstream sends above 599.
  - `origin` is `upstream` when the upstream's response was passed on, else `gateway`.
  - `reason` is `upstream` when the gateway answered because the upstream failed or had no address (502, 503, 504), else `none`. The other values in the design's table arrive with their features.
  - `tenant` is `none` for every request until tenants exist (M5); see the cap below.
  - `attempt` is `1` until retries (GW-12, M4).
  - An upstream attempt's duration runs from sending the request to receiving the response headers, or to the attempt failing, so it does not depend on how fast the client reads.
  - Both histograms use the buckets 0.5 ms, 1 ms, 2.5 ms, 5 ms, 10 ms, 25 ms, 50 ms, 100 ms, 250 ms, 0.5 s, 1 s, 2.5 s, 5 s, 10 s, 30 s and 60 s: fine below NFR-03's 2 ms, and up to the 59 s longest route timeout (GW-11).
- **Tenant cap** (OB-01). Each minute, the 50 tenants with the most requests in that minute (ties by name) keep their own label for the next minute, and every other tenant reports as `other`. Until a minute closes, free places go to tenants as they arrive. A tenant that loses its place loses its series in both metrics at once, so tenant churn cannot grow the series count. `none` is not a tenant and is never capped. Labelling and observing a request happen under the same lock as the minute's re-ranking, so a re-ranking can't delete a series between them; requests without a tenant skip the lock.

## Consequences

- Every request the handler answers is recorded, including a response cut off mid-body (with the bytes written before the cut) and an upgraded connection (logged when it closes, as a 101, with the bytes before the upgrade). Requests that Go's server rejects before the handler (a malformed request, headers over 64 KiB, the framing errors of ADR-0009) and failed TLS handshakes get neither a line nor a metric.
- A client choosing its own request IDs chooses whether its sampled requests are logged; its denials and 5xx are always logged.
- A scan of unrouted paths is logged in full, since every 404 the gateway answers is a denial.
- Loki drops empty attributes, so `trace_id` and `key_hash` are absent there rather than empty; stdout keeps them.
- Series stay bounded: per route and tenant about 10 methods × 6 classes × 3 origin and reason pairs for the counter, and 19 series per histogram (17 buckets, sum and count). Tenants multiply the first two metrics by at most 52 (`none`, `other` and 50 tenants).
- From M5 every request with a tenant takes one lock to label and observe; the M7 benchmarks show whether it needs sharding.
- The gateway now depends on gRPC and the OpenTelemetry SDK; M5's gRPC config stream needed gRPC anyway.
- Traces (OB-02, M4) reuse `SPILLWAY_GATEWAY_OTLP_ENDPOINT`, and their trace ID replaces the parsed one in the access log.

## Alternatives considered

- **UUIDv4 request IDs** — the standard library has no generator; `crypto/rand.Text` gives the same randomness in fewer characters, all within the allowed set.
- **Trusting any client request ID** — a long or odd value would end up in logs and upstream headers unchecked.
- **Random sampling** — replicas and hops would disagree about a request, and tests would not be repeatable.
- **Sampling denials and 5xx too** — the requests worth investigating would go missing.
- **Logging the path and query** — they can carry personal data and tokens, which logs must never hold.
- **A separate logger or file for access logs** — `msg` already separates them, and one pipeline keeps process and access logs together in Loki.
- **OTLP over HTTP** — the Collector's gRPC port 4317 is the dev default, and gRPC arrives with the M5 config stream regardless.
- **The standard `OTEL_EXPORTER_OTLP_ENDPOINT` variable** — gateway settings are `SPILLWAY_GATEWAY_*` (ADR-0008), and exporting stays off unless asked for.
- **The OpenTelemetry metrics SDK** — Prometheus scrapes the admin port (OB-07); the Prometheus client is already there.
- **Native histograms** — they need scrape and dashboard changes; fixed buckets work everywhere.
- **Upstream duration to the last body byte** — it would include the client's read speed.
- **First-come tenant labels** — not "top by traffic", and a tenant that left would hold its place for good.
- **Keeping the series of tenants that lost their place** — every tenant that ever ranked would keep its series.
- **Status 200 or 0 for a client that left** — 200 counts it as a success, 0 is no status; 499 is what nginx logs and dashboards already know.
