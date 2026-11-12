# 0031. The M4 exit check

- **Status:** Accepted
- **Date:** 2026-11-12
- **Requirement IDs:** GW-08, GW-09, GW-12, GW-13, GW-14, OB-02

## Context

M4's exit reads: "with one of three endpoints failing, ≥ 99% of requests succeed within 10 s of the failure, retries stay within budget, and traces show each attempt." The design's chaos table adds a row "Upstream endpoint returns 5xx | fault-injecting mock | Outlier ejection; breaker opens; retries capped by budget". Neither says:

- how the endpoint fails, or how the failure is injected, given that the dev stack has go-httpbin mocks whose only 5xx are per path (`/status/503`), and toxiproxy, which works on TCP only;
- which requests count towards the 99%, over which interval, and what "succeed" means;
- how "within budget" is computed, and from what;
- how a check finds the traces of retried requests, and how long it waits for them;
- how the check survives the laptop's host and VM stalls. Under load the dev laptop stalls every process for 4–9 ms about every 15 s, and occasionally for about 370 ms (ADR-0026, ADR-0027). In M3, a check whose line sat inside the stall's reach failed one run in three.

ADR-0030 gives the mechanisms: P2C, health checks, outlier ejection capped at 50% (1 of 3), attempt-counting breaker, the additive budget `0.2 × Q + 30` per 10 s read from `gw_retries_total{outcome="sent"}` and `gw_upstream_duration_seconds_count{attempt="1"}`, the `upstream attempt` spans, and `Spillway-Attempts` in dev mode.

## Decision

### Host stalls: why none can fail a rule by itself

Every number below was chosen so that a stall of the whole machine, up to the 370 ms measured, cannot produce a failed request, a retry, an ejection, a breaker trip or a missed trace on its own. No rule has a count allowance meant to absorb stalls: each rule holds because a stall creates nothing it counts.

- **Timeouts are at least 2.7× the longest stall.** The route's `connect` and `per_try` are 1 s and `total` 5 s. The health check timeout is 1 s. The sender's client timeout is 10 s, its metric scrapes have 2 s, and Tempo gets 60 s. A stall delays an answer by at most its length, about 0.37 s, on top of go-httpbin's 1–2 ms in the compose network, so no attempt, probe or request reaches a timeout because of one. Rule (a) counts answers, not latency, so a late 2xx is a success.
- **Health checks** mark an endpoint unhealthy after 3 consecutive failed probes, 1 s apart. A stall fails no probe (1 s timeout), and a single stall could touch at most one probe per endpoint.
- **Outlier ejection** needs 5 consecutive 5xx, connect errors, resets or attempt timeouts on one endpoint. The healthy endpoints produce none during a stall, and the 1 s per-try timeout rules out stall-made timeouts. Even a wrong ejection would remove at most 1 of 3 endpoints (the 50% cap).
- **The breaker** needs at least half of the attempts in its 10 s window to fail. The window holds about 5,000 attempts, and a stall adds no failures.
- **Caps.** After a 370 ms stall, the 500 rps sender releases about 185 requests at once. The gateway's caps are 4096 requests in flight and 8192 connections. The upstream's `max_concurrent`, `max_pending` and `max_connections` are 1024 each, and the sender's own cap is 1000 in flight. New connections for the burst dial in milliseconds inside the compose network, against a 1 s connect timeout.
- **The budget** allows at least 30 retries per window from its floor alone, and in fact about `0.2 × 5,000 + 30 ≈ 1,030`. The scenarios need about 5–20 retries. A stall causes no retries (above).
- **Scrapes and windows** (rule b) use the times each scrape was sent and answered. A delayed scrape changes which windows are checked, never whether a window's numbers are right. The coverage rule allows 2 s between scrapes, against a 1 s schedule plus a 0.37 s stall.
- **Traces** (rule c) wait in the gateway's export queue (2048 spans, flushed every second) and then for the Collector's 10 s tail-sampling decision. A stall delays them by its length, against a 60 s deadline.
- **No re-run.** Each scenario runs once per `make verify-m4`. A FAIL is reported as it is, and the script never runs a check again.

### Scenarios

Two scenarios run one after the other, each once, with the same rules: **`5xx`**, where the endpoint answers 503 to everything, and **`refused`**, where it refuses connections. The first is the chaos table's row. The second exercises GW-12's other condition, connect failure, and the reset of requests in flight.

- **Fault-injecting mock.** `chaos mock`, a subcommand of `cmd/chaos` (same image as the M3 sender), runs as the compose service `upstream-fault` under the `harness` profile. It publishes no host port, so docs/ports.md is unchanged. It is read-only, with `nofile` 65536.
  - Traffic on `:8080`: any path answers 200 with 1,024 bytes, keep-alive.
  - Control on `:8081`, compose network only: `POST /fault?mode=5xx|refused|none` answers 204 once the mode is in effect, and `GET /healthz` serves the compose healthcheck.
  - `5xx`: every request on `:8080` (health-check path included) answers 503 with an empty body.
  - `refused`: the mock closes its `:8080` listener and every open connection on it, so new connections are refused by the kernel.
  - `none`: healthy again, with the listener reopened.
  - A killed container was not chosen. On Docker's bridge network, connections to a vanished address wait for ARP to fail (seconds) instead of being refused, and the result depends on Docker.
- **Configs.** `test/verify/m4-5xx.yaml` (version 51, upstream `verify-5xx`) and `test/verify/m4-refused.yaml` (version 52, upstream `verify-refused`). The two upstream names give each scenario fresh registry state (ADR-0030: state is keyed by upstream name and survives reloads), so the first scenario's ejection cannot carry over. Apart from the name and version, both are:

  ```yaml
  version: 51
  upstreams:
    - name: verify-5xx
      endpoints:                       # lb unset: p2c (D3)
        - { address: 'upstream-1:8080' }
        - { address: 'upstream-2:8080' }
        - { address: 'upstream-fault:8080' }
      health_check: { path: /status/200, interval: 1s, timeout: 1s, unhealthy: 3, healthy: 2 }
      # outlier_detection, circuit and retry_budget unset: the defaults (ADR-0030)
  routes:
    - name: verify
      match: { path_prefix: /bytes }
      upstream: verify-5xx
      timeouts: { connect: 1s, per_try: 1s, total: 5s }
      retry: { attempts: 3 }
  ```

  It has no policies, so no rate limit or Redis call is in the way. gateway-1 runs in dev mode (compose), so it sets `Spillway-Attempts`. It samples traces at the compose default ratio of 1, because `make verify-m4` does not export the benchmark ratio (ADR-0030).
- **Sender.** `chaos upstream` (compose service `chaos`, `docker compose run --rm -T chaos upstream -fault <scenario> …`), in the compose network:
  1. It sets the mock to `none`, waits up to 10 s for gateway-1's `gw_config_version` to show the scenario's version, and then up to 10 s for `gw_upstream_endpoint_healthy` to be 1 for all three endpoints. Otherwise the scenario fails as `setup`.
  2. It sends `GET http://gateway-1:8000/bytes/1024` at **500 rps**, open model as loadgen (ADR-0014): due times fixed, overdue requests sent at once, a request due while 1,000 are in flight dropped, HTTP/1.1 keep-alive, 10 s client timeout. It runs for 40 s: 10 s before the fault and 30 s after. Each request carries `traceparent: 00-<random 128-bit trace ID>-<random 64-bit span ID>-01` (sampled) and `X-Request-Id: m4-<scenario>-<run id>-<i>`. For each request it records the due time, send time, answer time, status, `Spillway-Attempts` and trace ID, or the error kind (`timeout`, `dial`, `other`, `dropped`).
  3. At 10 s it takes **t_f = the time just before it sends `POST /fault?mode=<scenario>`**: the earliest the fault can act.
  4. Every 1 s, from 1 s before its first request until the first scrape sent at least 2 s after its last answer (about 43 scrapes, so about 34 windows for rule (b) against its minimum of 30), it scrapes `http://gateway-1:8001/metrics` with a 2 s timeout. For each scrape it records the send and receive times, `R` = `gw_retries_total{upstream=U, outcome="sent"}`, `Q` = `gw_upstream_duration_seconds_count{upstream=U, attempt="1"}`, `X` = `gw_retries_total{upstream=U, outcome="budget_exhausted"}`, every `gw_upstream_endpoint_healthy{upstream=U}` and `gw_circuit_state{upstream=U}`.
  5. After the last answer it sets the mock back to `none` and runs rule (c). It prints a JSON report on stdout and one PASS/FAIL line per rule on stderr, like the M3 sender.

  The fault endpoint's `ip:port` is `upstream-fault` resolved by the sender in the compose network, port 8080.

### Rules (each scenario)

- **(0) The fault took effect.** At least one scrape sent within `[t_f, t_f + 10 s]` shows `gw_upstream_endpoint_healthy{endpoint=<fault ip:port>} = 0`. Without it, the other rules would measure a healthy upstream, so the scenario fails. The line prints the time from t_f to the first such scrape.
- **(a) ≥ 99% succeed within 10 s of the failure.**
  - **Counted:** every request due before `t_f + 10 s` whose answer had not arrived by t_f. That covers the requests in flight when the fault started, everything sent in the 10 s after it, and requests dropped or never answered in that time.
  - **Succeeded:** a 2xx whose body was read in full within the 10 s client timeout.
  - **PASS when** `100 × succeeded ≥ 99 × counted`. At 500 rps that is about 5,000 counted and at most about 50 failures. The run must also be valid: at least 99% of all scheduled requests sent, as loadgen requires (ADR-0014); otherwise (a) fails as `sender saturated`.
  - The line prints counted, succeeded and the share, failures by kind, and the 2xx share before t_f and after `t_f + 10 s` for information.
  - "Within 10 s of the failure" is read as the 10 s window that starts at the failure, the strictest reading. It includes the requests caught in flight, rather than giving the system 10 s to recover first.
- **(b) Retries stay within budget in every rolling 10 s window.** A window is a pair of scrapes `i < j` with `recv_j − send_i ≤ 10 s` and `send_j − recv_i ≥ 8 s`, so that it surely lies inside 10 s and spans at least 8 s. For each scrape `j` the check takes the earliest such `i`. Each window must satisfy
  `5 × (R_j − R_i) ≤ (Q_j − Q_i) + 150`
  which is `retries ≤ 0.2 × requests + 3/s × 10 s`, ADR-0030's formula with the defaults, in integers. Coverage: every scrape must be answered, consecutive scrapes must be sent at most 2 s apart, and at least 30 windows must be checked. Otherwise (b) fails as `coverage`. The line prints the windows checked, the tightest one (retries against allowance), and the totals of `sent` and `budget_exhausted`.
  - In these scenarios retries are expected far below the allowance (tens against about 1,030), so (b) catches retry storms and accounting bugs. The budget actually binding is proven by m4-retries's fake-clock unit tests and its integration test (ADR-0030), not here.
  - The gateway checks its budget only when it grants a retry, over the trailing 10 s. A window that ends later could exceed the formula only if traffic dropped sharply after a burst of retries, and the sender's rate is constant.
- **(c) Traces show each attempt.**
  - **The traces:** the requests sent at or after t_f whose answer carried `Spillway-Attempts ≥ 2`, in send order. The first 20 are checked, or all of them if fewer. Fewer than 5 fails (c), as `too few retried requests`. Five is guaranteed by construction: ejection needs 5 consecutive failures, and each is retried within the floor of the budget.
  - **The query:** for each, `GET http://tempo:3200/api/v2/traces/<trace id>`. Traces not yet complete are asked again every 2 s, until 60 s after the sender's last answer: 10 s for the Collector's decision, 1 s for each export batch, and Tempo's ingest, with margin.
  - **A trace matches** when it holds exactly one `spillway-gateway` server span whose parent span ID is the one the sender sent, and exactly n spans named `upstream attempt` that are its children, where n is the request's `Spillway-Attempts`. Their `spillway.attempt` values are exactly 1…n. Attempts 1…n−1 have status error. Attempt n, when it got a response, has `http.response.status_code` equal to the status the sender got; when it failed without one (the outcome table's connect error, reset or deadline rows), it has status error and the sender got the gateway's 502 or 504.
  - **PASS when** every checked trace matches. The line prints matched against checked, the total attempt spans, and the first mismatch with its trace ID.
  - These traces are kept because the request is sampled (the flag, and ratio 1) and because each has a failed attempt span, which the Collector's `errors` policy keeps (ADR-0030).

### The run

- `test/verify/m4.sh`, in `m3.sh`'s style, via `make verify-m4: dev`. It builds the `chaos` image and starts `upstream-fault` (`docker compose --profile harness up -d --wait upstream-fault`). For each scenario it swaps the config into gateway-1's mounted `config.yaml` (by rename, restored on exit as before, ADR-0019) and runs the sender. It prints, per scenario, lines (0), (a), (b) and (c), plus info lines for the breaker's highest state over the run (expected 0, closed) and the fault endpoint's gauge. It ends with `M4 exit: PASS` or `M4 exit: FAIL (n checks)`, and exits 1 on any FAIL. On exit it sets the mock to `none` and stops `upstream-fault`. A run takes about 3 minutes.
- CI does not run it, as with M3's chaos and latency checks: it waits on Tempo for up to a minute per scenario, and its timing depends on a quiet machine. CI keeps the unit and integration tests of every M4 mechanism.
- m4-verify runs `make verify-m4` at least 3 times on the dev laptop and reports the pass rate per rule in its PR, with one full output. If a rule fails and profiling does not explain it, the run stops for the user. No rule is loosened, and no re-run is added.

### The chaos table's row

The row expects "outlier ejection; breaker opens; retries capped by budget" for an endpoint that returns 5xx. With one of three endpoints failing, ADR-0030's breaker should **not** open: ejection leaves only a handful of failed attempts in a window of thousands. An open breaker would also fail rule (a) for the whole upstream. So the exit check proves ejection (rule 0) and budgeted retries (rule b), and prints the breaker state as information. The breaker opening on an upstream whose endpoints all fail is proven by m4-breaker's integration test, and belongs in M7's chaos suite as an all-endpoints scenario. That reading of the row is recorded among ADR-0030's decisions for the user.

## Consequences

- `make verify-m4` needs the `harness` profile's `upstream-fault`, Tempo and the Collector's tail sampling. It is the first exit check that reads traces.
- `cmd/chaos` grows two subcommands. The existing killed-primary sender stays its default, so `make chaos-redis-primary` is unchanged.
- The 99% rule is checked at one rate and one fault per scenario. A slower failure (an endpoint that hangs) is left to M7's chaos suite. Its timeouts would be per-try timeouts, which the outcome table already covers.
- A FAIL in (b) means the gateway retried more than its formula allows, since the scenario's need is far below the allowance. A FAIL in (c) means a lost or malformed trace, or a retry the gateway did not trace.

## Alternatives considered

- **One scenario (5xx only)** — leaves GW-12's connect-failure path and the reset of requests in flight unexercised at the exit, for about a minute of runtime saved.
- **Toxiproxy for the failure** — it can refuse or reset connections, but cannot answer 5xx, which is the chaos table's row. One mock does both.
- **`docker kill` of an endpoint** — the outcome depends on Docker's ARP timing (connections hang for seconds instead of being refused).
- **A 10 s grace before counting** (99% of requests after `t_f + 10 s`) — the weaker reading. It would ignore exactly the requests the failure hits.
- **Reading attempts from the access log** — needs `docker compose logs` scraping and request-ID matching. The dev-mode header gives the sender the count per request directly, as `Spillway-Decision-Time` does for M3.
- **Exactly 10 s windows for (b)** — scrape times are measured, not exact, so a window is bounded on both sides instead: at most 10 s, at least 8 s.
- **Requiring the breaker to open in this scenario** — contradicts rule (a) with one of three endpoints failing.
- **A count allowance for stall-made failures** — ADR-0027 rejected that for M3. Here the timeouts make stall-made failures impossible, so none is needed.
