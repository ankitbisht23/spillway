# 0014. Load generator v1: open model, achieved rate, JSON result

- **Status:** Accepted
- **Date:** 2026-11-04
- **Requirement IDs:** NFR-01, NFR-02, NFR-05

## Context

M1 asks for `cmd/loadgen` v1: a constant arrival rate (open model), HDR histograms, JSON results for `bench.yml`, and a failed run when the achieved rate is below 99% of the target, because a saturated generator makes a benchmark invalid (docs/design.md, Benchmark method; ADR-0003). The design says to measure latency without coordinated omission and to record each run's hardware, CPU model included, but not:

- what "achieved" counts: requests sent, answered, or answered successfully;
- what the generator does when requests pile up in flight, or when it falls behind its own schedule;
- where a request's latency starts and ends, and whether failed requests have one;
- the JSON fields, the histogram's range and precision, or how to name a CPU on arm64, whose `/proc/cpuinfo` has no model name;
- how loadgen reaches a TLS listener with the dev certificate, and how it runs in compose.

## Decision

- **Schedule.** Request *i* is due at start + *i*/rate. The generator sleeps until a request is due, sends it in its own goroutine and moves on; it never waits for an answer. When it wakes up late, it sends every overdue request at once.
- **Latency** runs from a request's due time to the last byte of its response, so a stalled target and a late generator both show in it. It goes into an HDR histogram (`github.com/HdrHistogram/hdrhistogram-go` v1.4.0) from 1 µs to 1 h with 3 significant digits; longer latencies count as 1 h. The result has p50, p99, p99.9 and max in microseconds.
- **Warm-up.** Requests due before the warm-up ends are sent at the rate and not recorded. The defaults are the benchmark protocol's 30 s warm-up and 3 min measured.
- **Dropped requests.** A request due while `-max-in-flight` requests (default 10,000) are in flight is dropped, never queued. Once the run's time is up, every request not yet sent is dropped too. After that, loadgen waits for the requests in flight, each bounded by `-timeout` (default 10 s).
- **Achieved rate** = measured requests sent ÷ measured duration. The run is valid when it is at least 99% of the target. Otherwise loadgen still prints the result, with `valid: false`, and exits 1. Bad flags exit 2.
- **Errors.** A request with no response counts under `errors` by kind: `timeout` (the request timeout or a network timeout), `dial` (no connection) or `other`. It has no latency. Every response counts under `status` by class (`2xx`, `5xx` …), whatever its status.
- **Protocol.** `http://` targets use HTTP/1.1; `https://` offers HTTP/2 by ALPN (TLS 1.2 or later), and the result records the protocol of the first response. `-cacert` names a PEM file to trust instead of the system roots, such as the gateway's dev certificate. Connections are kept alive, capped by `-conns` (default 100; requests beyond it wait for a connection, which counts in their latency). Redirects are not followed, and no proxy is used.
- **Result**, one JSON object on stdout:

  | Field | Value |
  | --- | --- |
  | `url`, `protocol` | the target and the protocol used, so TLS runs are labelled |
  | `target_rps`, `achieved_rps`, `valid` | the rates and the 99% verdict |
  | `warmup_s`, `duration_s` | seconds |
  | `requests` | `scheduled`, `sent`, `dropped` in the measured time |
  | `status`, `errors` | counts by status class and by error kind |
  | `latency_us` | `p50`, `p99`, `p99_9`, `max` |
  | `hardware` | `cpu_model`, `goarch`, `num_cpu`, `gomaxprocs` |

  `cpu_model` is the first `model name` in `/proc/cpuinfo`. arm64 kernels list none, so there it is the core's implementer and part codes, such as `implementer 0x41 part 0xd0c` (Arm Neoverse N1). It is empty when the file is missing (macOS). The commit SHA and the timestamp are for `bench.yml` to add (ADR-0003).
- **Image and compose.** The root `Dockerfile` gets a `loadgen` target on the same distroless base (ADR-0006). Compose has a `loadgen` service under the profile `tools`, so `make dev` does not start it. It runs on demand in the compose network (ADR-0004) with `docker compose run --rm --build loadgen -url http://gateway-1:8000/bytes/1024 -rate …`, read-only, with `nofile` 65536 and `deploy/certs` mounted at `/etc/spillway-certs`. CI builds the target for both architectures and runs it as 65532 on a read-only root filesystem.

## Consequences

- A stalled target cannot hide latency: every request due during the stall is in flight and waits it out, and the p99 shows it.
- The in-flight cap and the end of the run turn a saturated or late generator into dropped requests, and so into an invalid run, instead of a burst that the target then has to absorb.
- Go's timers can wake late, by up to about a millisecond on some systems, and that lateness counts in every latency. It is the same for every run on one machine, but it puts a floor under absolute sub-millisecond numbers. M7's added-latency work (NFR-03) will show whether loadgen needs a busy-wait near due times.
- Latency is only for answered requests: a run with many timeouts reports the p99 of the requests that came back, so `bench.yml` has to check `errors` as well as `valid`.
- One goroutine per request in flight; 10,000 in flight cost tens of megabytes.
- `cpu_model` on arm64 names the core, not the chip: two chips with the same core look alike.
- Per-tenant histograms (NFR-15) and the accuracy harness (M3) extend this command later.

## Alternatives considered

- **Closed loop** (each worker waits for its answer) — slow answers lower the request rate and hide latency: coordinated omission.
- **Queueing when the in-flight cap is reached**, as wrk2 effectively does with fixed connections — the queue grows without bound behind a stalled target, and the catch-up burst afterwards is load the schedule never asked for.
- **Achieved rate from answered requests** — mixes target failures into the generator's verdict; errors are reported on their own instead.
- **Recording failed requests at their timeout** — one timeout value repeated would stand for latencies nobody measured.
- **Latency from the moment a request is sent** — a generator that runs late would report the target as faster than it is.
- **vegeta, wrk2 or k6 for benchmarks** — the design dropped third-party HTTP load tools for benchmarks; k6 stays for scenarios.
- **A `time.Ticker` at the request interval** — it drops ticks when the generator is slow, which loses requests silently and hides the lateness; due times computed from the start keep both visible.
- **`lscpu` for the CPU model** — not in the distroless image.
