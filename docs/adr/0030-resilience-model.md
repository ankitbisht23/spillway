# 0030. Resilience model

- **Status:** Accepted; connection-cap answer timing superseded by 0032; connection cap at capacity, round-robin retries and the order of retry checks superseded by 0033; the breaker column of the attempt-outcome table superseded by 0034; the `authn` span's results extended by [0049](0049-gateway-jwt-and-auth-modes-details.md)
- **Date:** 2026-11-12
- **Requirement IDs:** GW-07, GW-08, GW-09, GW-11, GW-12, GW-13, GW-14, GW-15, GW-22, GW-23, GW-26, GW-29, OB-01, OB-02, OB-03, NFR-03, NFR-04, NFR-11

## Context

M4 asks for load balancing (GW-07), active health checks (GW-08), outlier ejection (GW-09), retries (GW-12) within a per-upstream budget (GW-13), a per-upstream circuit breaker (GW-14), deadline propagation (GW-15), graceful shutdown (GW-23), load shedding (GW-26) and traces (OB-02). The requirement texts leave open:

- the config: field names, defaults, what an unset block means, and validation;
- which state lives where, keyed by what, and what a reload does to it (GW-29);
- what counts as a failure, and whether health checks and outlier ejection combine;
- what happens when every endpoint is out;
- the consistent-hash construction and what "a header or key" is;
- which outcomes are retried, what happens to request bodies, the backoff, and the exact budget formula, including whether GW-13's "plus a floor of 3 retries/s" adds to the 20% or is a minimum;
- the deadline header's name and value, and whether a client may send it;
- breaker states, what an error is (attempts or requests), what the caps count and how the breaker meets retries and ejection;
- the shutdown order and its periods, the shedding caps, how a connection cap sheds without queueing in `accept`, and what happens on TLS;
- span names, attributes and sampling, and how the sampling keeps NFR-03 and NFR-04 measurable;
- the order of the M4 tasks and what the gateway does between them.

Today (main at 36a5965): `internal/proxy`'s `Registry` holds one pool per upstream name (an `http.Transport` and the resolved `ip:port` of every endpoint) and picks round-robin (ADR-0008). A request makes one attempt bounded by `min(per_try, total)` (ADR-0011). `serve()` fails `/readyz` on SIGTERM and gives `srv.Shutdown` 10 s (ADR-0010). `traceparent` goes upstream unchanged and the gateway starts no spans (ADR-0013).

The design's config format (docs/design.md, "Config format") already names some of the fields: `lb: p2c`, `health_check: { path, interval, unhealthy, healthy }`, `circuit: { max_concurrent, error_rate, window, min_requests, open_for, half_open_probes }`, `retry_budget: { ratio, min_per_sec }` on the upstream, and `retry: { attempts }` on the route. The `upstreams` and `routes` tables use the same names. The run's recommended design suggested other names (`load_balancing`, `circuit_breaker`, `retries`). This ADR keeps the design's names, so the format, the Postgres columns (M5) and the config stay one vocabulary. It adds fields only where the design has none.

The user's decisions for this run are D3–D6, and D1–D2 are carried over from M3 ("Decisions taken for the user"). The laptop's host stalls (4–9 ms about every 15 s, occasionally about 370 ms; ADR-0026, ADR-0027) shape every timeout below that an exit check depends on. ADR-0031 has the exit check.

## Decision

### Config (proto, additive)

Fields are added to `api/proto/config.proto` in the task that uses them (see "Tasks"). All of them are additive, so `buf breaking` passes. As elsewhere in the proto, durations are Go duration strings, enum-like values are lower-case strings, and an unset number (0) means the default. A negative number, or a value outside the range below, rejects the whole snapshot with the error shown, prefixed by `upstream "<name>": ` or `route "<name>": `, as the existing compile errors are.

```proto
message Upstream {
  string name = 1;
  repeated Endpoint endpoints = 2;
  string lb = 3;                          // round_robin | p2c | consistent_hash; unset = p2c (GW-07)
  string hash_on = 4;                     // consistent_hash only, and required with it: a key template
  HealthCheck health_check = 5;           // unset = no active checks (GW-08)
  OutlierDetection outlier_detection = 6; // unset = the defaults (GW-09)
  CircuitBreaker circuit = 7;             // unset = the defaults (GW-14)
  RetryBudget retry_budget = 8;           // unset = the defaults (GW-13)
}
message HealthCheck      { string path = 1; string interval = 2; string timeout = 3; int32 unhealthy = 4; int32 healthy = 5; }
message OutlierDetection { int32 consecutive_failures = 1; string base_ejection = 2; string max_ejection = 3; int32 max_ejected_percent = 4; }
message CircuitBreaker   { int32 max_connections = 1; int32 max_pending = 2; int32 max_concurrent = 3; double error_rate = 4;
                           string window = 5; int32 min_requests = 6; string open_for = 7; int32 half_open_probes = 8; }
message RetryBudget      { double ratio = 1; double min_per_sec = 2; }
message Route { ...; Retry retry = 14; }
message Retry            { int32 attempts = 1; bool non_idempotent = 2; }
```

| Field | Default | Valid | Error when invalid |
| --- | --- | --- | --- |
| `lb` | `p2c` | `round_robin`, `p2c`, `consistent_hash` | `lb "x": want round_robin, p2c or consistent_hash` |
| `hash_on` | none | a key template (ADR-0017's syntax and placeholders) | `hash_on is required with lb consistent_hash`; `hash_on is only for lb consistent_hash`; the key template's own errors |
| `health_check.path` | required when the block is set | starts with `/`; no space, `#` or control character; may carry a `?query` | `health_check.path "x": want a path such as /healthz` |
| `health_check.interval` | `5s` | 100 ms to 1 h | `health_check.interval: "x" is not a duration from 100ms to 1h` |
| `health_check.timeout` | `1s`, or `interval` when that is shorter | > 0 and ≤ `interval` | `health_check.timeout: want more than 0 and at most interval` |
| `health_check.unhealthy`, `.healthy` | 3, 2 | 1 to 100 | `health_check.unhealthy: want 1 to 100` |
| `outlier_detection.consecutive_failures` | 5 | 1 to 1000 | `outlier_detection.consecutive_failures: want 1 to 1000` |
| `outlier_detection.base_ejection` | `30s` | 1 s to 1 h | `outlier_detection.base_ejection: …` |
| `outlier_detection.max_ejection` | `300s` | ≥ `base_ejection`, ≤ 24 h | `outlier_detection.max_ejection: want at least base_ejection` |
| `outlier_detection.max_ejected_percent` | 50 | 1 to 100 | `outlier_detection.max_ejected_percent: want 1 to 100` |
| `circuit.max_connections`, `.max_pending`, `.max_concurrent` | 1024 each | 1 to 1,000,000 | `circuit.max_pending: want 1 to 1000000` |
| `circuit.error_rate` | 0.5 | > 0 and ≤ 1 | `circuit.error_rate: want more than 0 and at most 1` |
| `circuit.window` | `10s` | 1 s to 10 min | `circuit.window: …` |
| `circuit.min_requests` | 20 | 1 to 1,000,000 | `circuit.min_requests: …` |
| `circuit.open_for` | `10s` | 100 ms to 10 min | `circuit.open_for: …` |
| `circuit.half_open_probes` | 5 | 1 to 1000 | `circuit.half_open_probes: …` |
| `retry_budget.ratio` | 0.2 | > 0 and ≤ 1 | `retry_budget.ratio: want more than 0 and at most 1` |
| `retry_budget.min_per_sec` | 3 | > 0 and ≤ 10,000 | `retry_budget.min_per_sec: …` |
| `retry.attempts` (route) | 1 (no retry) | 1 to 5, the first attempt included | `retry.attempts: want 1 to 5` |
| `retry.non_idempotent` (route) | false | | |

- Every upstream has outlier detection, a breaker and a retry budget, with the defaults when the block is unset. There is no switch to turn them off: no requirement asks for one, and a block can loosen them (`error_rate: 1`, `consecutive_failures: 1000`). Active health checks are the one opt-in, because they need a path only the operator knows.
- The retry budget's window is GW-13's fixed 10 s, not a field.
- Compiled forms in `internal/config`: `Upstream` gains `LB string`, `HashOn *KeyTemplate`, `HealthCheck *HealthCheck` (nil = none), `Outlier OutlierDetection`, `Circuit CircuitBreaker`, `RetryBudget RetryBudget`, all with defaults filled in. `Route` gains `Retry Retry{Attempts int; NonIdempotent bool}`.
- The design's example also shows an endpoint `weight`. No M4 requirement asks for weights, so the proto does not add it. The `outlier_detection` block needs a column next to the design's `upstreams` columns in M5.

### Settings (env, ADR-0008)

| Variable | Meaning | Default |
| --- | --- | --- |
| `SPILLWAY_GATEWAY_DRAIN` | GW-23: how long the gateway keeps serving after SIGTERM, with `/readyz` failing | `5s` (`0s` allowed) |
| `SPILLWAY_GATEWAY_SHUTDOWN_GRACE` | GW-23: after the drain, how long in-flight requests get to finish | `15s` |
| `SPILLWAY_GATEWAY_MAX_CONNECTIONS` | GW-26: open client connections across the traffic listeners | `8192` |
| `SPILLWAY_GATEWAY_MAX_IN_FLIGHT` | GW-26: requests being handled at once | `4096` |
| `SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO` | OB-02: head-sampling ratio for traces without a parent decision, 0 to 1 | `1` |

Errors follow `settings.go`'s style, e.g. `SPILLWAY_GATEWAY_DRAIN: "x" is not a duration of 0 or more`. Tracing is on exactly when `SPILLWAY_GATEWAY_OTLP_ENDPOINT` is set, and spans go to that endpoint over OTLP/gRPC, like the logs (ADR-0013).

Compose sets `SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO: ${SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO:-1}` and `stop_grace_period: 30s` on the gateways (see "Graceful shutdown" and "Sampling").

### Packages and registries (GW-29)

- **`internal/resilience`** (new; docs/design.md's repo layout already names it): the state machines, with an injected `Clock` (`interface{ Now() time.Time }`; `ratelimit.FakeClock` satisfies it) and no knowledge of HTTP:
  - `Window`: rolling success and failure counts over a duration, in a fixed number of buckets. `NewWindow(d time.Duration, buckets int)`, `Add(now time.Time, failed bool)`, `Counts(now time.Time) (total, failed int64)`, `Reset()`.
  - `Budget`: GW-13 (below). `Request(now)`, `TryRetry(now time.Time, ratio, minPerSec float64) bool`.
  - `Breaker`: GW-14's state machine and its `max_concurrent` cap (below). `Acquire(now time.Time, s *config.CircuitBreaker) (*Pass, error)`; `Pass.Record(now time.Time, failed bool)` once per attempt; `Pass.MayRetry() bool`; `Pass.Done()`. Errors: `ErrOpen` (with `RetryAfter`) and `ErrOverloaded`. `State() State`.
  - `Outlier`: one endpoint's GW-09 state. `Record(now time.Time, failed bool, s *config.OutlierDetection, mayEject func() bool) (ejectedFor time.Duration)`, `Ejected(now time.Time) bool`.
  - `Gate`: a counter with a cap that never waits. `TryAcquire(limit int64) bool`, `Release()`, `InUse() int64`. GW-26's caps and the breaker's caps use it.
- **`internal/proxy`'s `Registry`** grows from pools to upstream state. It is still keyed by upstream name, still read without locks on every request, and still reconciled by `Apply` before a snapshot goes live:

  ```go
  type upstream struct {
      name      string
      pool      *pool                         // transport and resolved addresses, as today
      settings  atomic.Pointer[config.Upstream]
      endpoints atomic.Pointer[[]*endpoint]   // published after each resolve
      breaker   *resilience.Breaker
      budget    *resilience.Budget
      conns     resilience.Gate               // max_connections
      pending   resilience.Gate               // max_pending
      next      atomic.Uint64                 // round robin
  }
  type endpoint struct {
      addr     string        // resolved ip:port: the endpoint's identity
      host     string        // the configured address it came from, e.g. upstream-1:8080
      healthy  atomic.Bool   // active health; true without health checks
      outlier  resilience.Outlier
      inFlight atomic.Int64  // attempts sent and not finished (p2c)
      stop     context.CancelFunc // its prober, if any
  }
  ```

- **Keys.** An upstream's state is keyed by its name, and an endpoint's by the pair (upstream name, resolved `ip:port`), the address the pool already sends to. A configured name that resolves to three addresses is three endpoints.
- **Reconcile** in `Apply`, and after every DNS refresh for that upstream's endpoints:
  - An upstream that remains keeps its state object: breaker state and window, budget window, gates and round-robin counter. Its new settings are stored in place.
  - An endpoint whose (upstream, address) remains keeps its object: health, outlier state and in-flight count.
  - A new endpoint starts healthy and not ejected, with a prober if the upstream has health checks.
  - A removed endpoint's prober stops, and its metric series is deleted. A request already holding it finishes on it.
  - A removed upstream's state is dropped, as its pool is today. A request on an older snapshot that names it gets fresh state built from its snapshot, which the next `Apply` drops (ADR-0008's rule, extended).
- **Settings change in place.** A reload never resets a counter, with one exception. Thresholds, ratios, durations and caps are read from `settings` on each use. A prober picks up a new interval, timeout or path at its next probe. Adding `health_check` starts probers, and the endpoints stay healthy until probes say otherwise. Removing it stops them and marks every endpoint healthy. The exception is a changed `circuit.window`: it rebuilds the breaker's `Window` empty, because buckets of the old width cannot be re-cut. The breaker's state and its in-flight count stay.
- **Test** (GW-29): under steady load, a reload that changes settings but keeps the upstreams and endpoints keeps every endpoint's `healthy`, outlier state and in-flight count, the breaker's state and window, the budget's window, and the gates' counts, all compared by value before and after `Apply`. A reload that renames an endpoint drops only that endpoint.

### Health checks (GW-08)

- **One prober per endpoint**, a goroutine started by the registry, outside any request. The first probe goes out after a random delay within one `interval`, so that probes do not line up across endpoints and replicas. After that, one probe goes out per `interval`, measured from the start of the previous probe. `timeout ≤ interval`, so probes never overlap.
- **The probe:** `GET <path>` to the endpoint's `ip:port`, with `Host` set to the configured address (`upstream-1:8080`), `User-Agent: spillway-health-check`, and its own `timeout` covering connect to the end of the response headers. It reads at most 4 KiB of the body, then closes it. Probes share one `http.Transport` per registry (one idle connection per endpoint, the same allow-list dialer, ADR-0008). It is not the request pool, so probes never queue behind traffic and never count against `circuit` caps (ADR-0025 made the same choice for Redis).
- **Result:** a 2xx or 3xx is a success. Any other status, a connect error, a reset or the timeout is a failure.
- **State:** a new endpoint starts healthy, so a reload or a DNS change never black-holes traffic while the first probes run. After `unhealthy` consecutive failures it is unhealthy, and after `healthy` consecutive successes it is healthy again. The counts persist across settings changes. Logs: warn `upstream endpoint unhealthy` and info `upstream endpoint healthy`, each with `upstream`, `endpoint` and the last failure's reason, never the response body.

### Outlier ejection (GW-09)

- **Counted per attempt**, on the endpoint the attempt went to, using the attempt outcome table below: a failure is a 5xx response, a connect error, a reset before the response headers, or the attempt's own deadline firing. Anything else with a response is a success and resets the count. Outcomes that are the gateway's own doing or the client's are not counted at all.
- **Ejection:** the `consecutive_failures`-th failure in a row ejects the endpoint, if the cap allows. The count then goes back to 0, whether or not the endpoint was ejected.
- **Cap:** at most `floor(n × max_ejected_percent / 100)` endpoints of an upstream are ejected at once, where `n` is its current number of endpoints (resolved addresses). With the default 50%, that is 0 of 1 (a single endpoint is never ejected, since nothing could replace it), 1 of 2, 1 of 3 and 2 of 4.
- **Duration:** `base_ejection × k`, capped at `max_ejection`. `k` is the number of times the endpoint has been ejected. It decays by 1 for each full `base_ejection` the endpoint has spent back in rotation without being ejected, worked out lazily at the next ejection: `k = max(0, k − floor(time back ÷ base_ejection)) + 1`.
- **Return:** when its time is up (checked lazily at selection), the endpoint is selectable again, with a failure count of 0.
- **Logs:** warn `upstream endpoint ejected` with `upstream`, `endpoint`, `for` and `times` (k). Info `upstream endpoint ejection skipped: max_ejected_percent reached` when the cap holds an ejection back.

### Selection and when nothing is left

- **Selectable** = healthy (active checks) **and** not ejected. The two states are independent. A passing health check does not end an ejection, and an ejection does not change health. An endpoint that is down for good is usually held out by both.
- **Panic mode:** when no endpoint is selectable, selection ignores health and uses every endpoint that is not ejected. With the default cap at least `n − floor(n/2)` endpoints are never ejected, so that set is never empty while the upstream has an address; only `max_ejected_percent: 100` can eject them all, and then panic mode uses every endpoint. Logs: warn `upstream has no healthy endpoint; ignoring health checks` once per entry into panic, and info when it ends. With no resolved address at all, the gateway answers 503 as today (ADR-0008).
- Panic mode lets a health check that is wrong everywhere (a bad path, a probe network split) cost nothing. An upstream that is really down fails its attempts, and the breaker opens on the error rate and answers 503 fast.
- `gw_upstream_endpoint_healthy{upstream, endpoint}` is 1 while the endpoint is selectable and 0 while it is unhealthy or ejected. `endpoint` is the `ip:port`. The series is deleted when the endpoint goes.

### Load balancing (GW-07)

Each algorithm chooses among the candidates: the selectable endpoints (or the panic set) that this request has not tried yet, or all of them if it has tried every one.

- **`round_robin`:** the upstream's counter, `next.Add(1) − 1`, modulo the number of candidates.
- **`p2c`** (default, D3): least request with the power of two choices. Two distinct candidates are drawn uniformly at random (`math/rand/v2`), and the one with fewer attempts in flight wins, with ties going to the first drawn. A single candidate is taken as it is. An endpoint's in-flight count runs from sending an attempt until its response body is closed, or the attempt fails.
- **`consistent_hash`:** rendezvous (highest random weight) hashing. `hash_on` is a key template, filled as a policy's would be (`{header:X-Cart-Id}`, `{api_key}`, `{consumer}`, `{ip}` …; `{api_key}` is the key's hash, never the key). For each candidate the score is `xxhash64(filled + "\x00" + addr)` (`github.com/cespare/xxhash/v2`, already in the module graph through go-redis and the Prometheus client, now required directly), and the highest score wins. When the template cannot be filled (the header is missing), that request falls back to `p2c`. Every replica computes the same scores, so a key lands on the same endpoint across replicas. When an endpoint leaves the candidates, only its keys move, each to its next-highest endpoint, and they move back when it returns.
  - **Test:** 10,000 keys over 5 endpoints. Removing one moves exactly that endpoint's keys and no other key. Adding it back restores the first mapping. No endpoint holds more than 1.3× its fair share. A retry of a key takes its second-ranked endpoint.
  - Rendezvous hashing needs no ring to rebuild on reconcile and costs one hash per candidate. Upstreams hold tens of endpoints, so it is cheap.

### Attempt outcomes

Every attempt ends in exactly one row of this table. The table is shared by outlier ejection, the breaker and retries.

| Outcome | Outlier and breaker | Retryable (GW-12) | Client answer if it is the last attempt |
| --- | --- | --- | --- |
| Response 1xx–4xx | success | no | the response |
| Response 500, 501, 505–599 | failure | no | the response |
| Response 502, 503, 504 | failure | yes | the response |
| Connect error: refused, unreachable, connect timeout | failure | yes | 502, or 504 for the connect timeout (as today) |
| Reset or EOF before the response headers | failure | yes | 502 |
| The attempt's own deadline (per-try, or the total when it is earlier) fires before the headers | failure | yes, if time is left | 504 |
| Client went away; idle timeout (GW-11); route body limit (413) | not counted | no | as today |
| Gateway refusal: dial-time allow-list (SEC-06), `max_connections`, `max_pending` | not counted | no | 502 for the allow-list (as today); 503 `circuit_open` for the caps |

An outcome is fixed when the response headers arrive. A failure while the body streams (a cut response) is not counted and is never retried.

### Retries (GW-12, GW-13)

- **Who retries:** a route with `retry.attempts > 1`, for idempotent methods only: `GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE`, `TRACE` (RFC 9110 §9.2.2). With `retry.non_idempotent: true` every method is retried (D4). The conditions are those in the table above, the same for every route (GW-12, "routes set attempts only").
- **Bodies:** a request with no body (`ContentLength == 0`, `http.NoBody`) can always be retried. Otherwise, when the route and method could retry, the gateway reads up to **64 KiB** of the body before the first attempt, through the existing size-limit and idle wrappers, so 413 and the idle timeout behave as today. If the body ends within 64 KiB, it is kept and sent again on each retry. If not, or if `Content-Length` is above 64 KiB (then nothing is read early), the request streams as today and gets one attempt. Bodies on routes or methods that cannot retry are never buffered.
- **No retry after a byte:** retry decisions are made when an attempt's response headers arrive and before anything is written to the client, so a retry never follows a byte sent to the client.
- **Order of checks for attempt n + 1**, after attempt n ended retryable:
  1. attempts are left (`n < attempts`), the method and body allow it, and the request context is still live;
  2. the backoff is drawn: uniform in `[0, min(250 ms, 25 ms × 2^(n−1)))` (full jitter), so before attempt 2 it is below 25 ms, before 3 below 50 ms, before 4 below 100 ms and before 5 below 200 ms. The retry is dropped if the backoff plus 25 ms would reach the total deadline: an attempt with less time than that rarely answers, and it would replace a real upstream answer with a 504;
  3. the breaker allows it (`Pass.MayRetry()`: closed, and not a half-open probe);
  4. the budget grants it (`TryRetry`), which counts `gw_retries_total{outcome="sent"}`; a refusal counts `gw_retries_total{outcome="budget_exhausted"}`.

  If any check fails, no retry is sent and the client gets attempt n's answer: the upstream's response, or the gateway's 502 or 504. A dropped retry is never queued. Then the gateway sleeps the backoff (cancelled by the request context) and sends attempt n + 1 to an endpoint chosen as above, preferring one not yet tried.
- **Budget formula** (`resilience.Budget`, per upstream). A retry is granted at time s when

  `R(s) + 1 ≤ ratio × Q(s) + min_per_sec × 10`

  where `Q(s)` counts the upstream's first attempts that ended, and `R(s)` the retries granted, over the last 10 s: 100 buckets of 100 ms, the current one included, so the window covers between 9.9 and 10 s before s. The floor **adds** to the ratio (GW-13: "plus a floor"): with the defaults, `0.2 × Q + 30` retries per 10 s. A first attempt is counted in `Q` when it ends, at the same moment `gw_upstream_duration_seconds{attempt="1"}` observes it. A retry is counted in `R` when granted, at the same moment `gw_retries_total{outcome="sent"}` counts it. So the metrics show exactly what the budget saw (ADR-0031 checks it from them). Retries never count in `Q`.
- **Attempt metric:** every attempt observes `gw_upstream_duration_seconds{upstream, attempt}` with `attempt` from `"1"` to `"5"`, over the same span as today (ADR-0013).

### Deadline propagation (GW-15)

- Every attempt carries **`Spillway-Deadline-Ms`**: the whole milliseconds left until the attempt's own deadline, `min(total deadline, now + per_try)`, read just before the request is written and rounded down, minimum 1.
- Any `Spillway-Deadline-Ms` from the client is removed before forwarding and never read. A client could otherwise make upstreams give up early or late. The gateway's own total is the route's (GW-11). The header joins ADR-0011's gateway-owned headers, so a transform naming it fails compile.

### Circuit breaker (GW-14)

- **Per upstream**, in the registry (`resilience.Breaker`), checked at request pipeline step 9: after the limiters, before endpoint selection. `Acquire` admits a request, or refuses it with `ErrOpen` or `ErrOverloaded`.
- **What counts:** every attempt's outcome (table above) is recorded in the breaker's `Window` of `window` (10 buckets of `window ÷ 10`, so it covers between 0.9 and 1× `window`). Attempts count, not requests: the breaker measures how the upstream answers, and outlier ejection already keeps one bad endpoint's failures down to a handful (ADR-0031 relies on this). Retries add their own attempts.
- **Closed → open:** after recording a failure, the breaker opens when the window holds at least `min_requests` attempts and `failed ÷ total ≥ error_rate`.
- **Open:** every request gets 503 at once, with origin `gateway`, reason `circuit_open`, body `{"error":"upstream circuit open"}` and `Retry-After` set to the whole seconds until half-open, rounded up, minimum 1. Nothing is sent upstream.
- **Open → half-open:** the first request after `open_for` has passed (checked lazily) moves the breaker to half-open.
- **Half-open:** the first `half_open_probes` requests are admitted as probes. Each probe gets exactly one attempt and no retries. Other requests get the open answer with `Retry-After: 1`. When all `half_open_probes` probes succeed, the breaker closes with an empty window. When any probe fails, it opens again for another `open_for`. A probe that ends without an outcome (the client left, or a gateway refusal) gives its place to the next request.
- **Concurrency caps,** checked in this order:
  - `max_concurrent`: requests admitted to the upstream and not yet finished, from `Acquire` to the end of the response body. One request holds one place across its retries.
  - `max_pending`: attempts waiting for a connection, counted from `httptrace` `GetConn` to `GotConn`. At `GetConn` past the cap, the attempt's context is cancelled with cause `errPendingLimit`.
  - `max_connections`: open upstream connections of the upstream's request pool, counted by the dialer and released when the connection closes. A dial past the cap fails at once with `errConnLimit`.

  Each cap answers 503 at once, with origin `gateway`, reason `circuit_open` (a deliberate refusal, which NFR-11 excludes), body `{"error":"upstream overloaded"}` and `Retry-After: 1`. A cap refusal is not an upstream failure (table above). With HTTP/1.1 upstreams, an attempt holds one connection, so `max_connections` also bounds the attempts in flight. Probes are outside all three caps.
- **Retries and ejection:** a retry needs the breaker closed (checks above). When the breaker opens mid-request, the request's next retry is dropped and the client gets the last attempt's answer. Outlier ejection works below the breaker: an ejected endpoint gets no attempts, so it adds no failures.
- `gw_circuit_state{upstream}`: 0 closed, 1 open, 2 half-open, set on every transition and deleted with the upstream. Logs: warn `upstream circuit open` with `upstream`, `error_rate`, `requests`; info `upstream circuit half-open` and `upstream circuit closed`.
- **Tests** on the fake clock, one per transition: closed stays closed below `min_requests` even at 100% failures; it opens at exactly `min_requests` and `error_rate`; open refuses with the right `Retry-After`; open → half-open after `open_for`; exactly `half_open_probes` admitted; all succeed → closed with an empty window; one fails → open; a cancelled probe frees its place; window buckets expire; a changed window resets the counts, not the state. Plus each cap, and `MayRetry` in each state.

### Graceful shutdown (GW-23)

This supersedes ADR-0010's "Shutdown" bullet. In order:

1. SIGTERM or SIGINT arrives. `/readyz` answers 503 from now on, as today.
2. **Drain** for `SPILLWAY_GATEWAY_DRAIN` (default 5 s). The traffic listeners keep accepting and serving normally, so a load balancer that has not yet seen readiness fail loses nothing. `srv.SetKeepAlivesEnabled(false)` makes each HTTP/1.1 response close its connection, so clients reconnect, and once readiness has taken effect they reach other replicas.
3. **Stop accepting** and call `srv.Shutdown` with `SPILLWAY_GATEWAY_SHUTDOWN_GRACE` (default 15 s). This closes the listeners and idle connections, sends HTTP/2 `GOAWAY`, and waits for in-flight requests. When the grace runs out, `srv.Close()` cuts what is left, and the shutdown log line says how many requests were cut.
4. Stop the health probers, the DNS refresh and the config, identity and certificate watchers (cancel their context).
5. Flush traces and logs together, within one 5 s timeout (the tracer and logger providers' `Shutdown`).
6. Close the admin server and exit 0.

- 5 s of drain is more than two of compose's 2 s healthcheck intervals, and two of the 2 s readiness periods M7's chart will use. 15 s of grace equals the default route `total` (ADR-0011), so requests on default routes always finish; a route with a longer `total` may be cut. The whole shutdown takes at most 5 + 15 + 5 = 25 s. Compose's `stop_grace_period: 30s` and Kubernetes's default `terminationGracePeriodSeconds: 30` fit it, with M7's preStop delay counted in.
- **Test** (cmd/gateway, short periods set through the env): after the signal, `/readyz` is 503 at once. During the drain, a new connection is accepted and its request answered 200 with `Connection: close`. After the drain, a new connection is refused. A request in flight across the drain finishes within the grace. One that outlasts the grace is cut, and `serve` returns. Probers stop after `Shutdown`, and the exporters' `Shutdown` is called.

### Load shedding (GW-26)

The admin port is exempt from both caps: it is a separate server.

- **In-flight cap.** The traffic handler's first step, before routing, is `Gate.TryAcquire(MAX_IN_FLIGHT)`. On failure it answers 503 at once, with `Retry-After: 1`, body `{"error":"overloaded"}`, origin `gateway`, reason `shed`, route `none`. It is recorded like any request: a 5xx, so always access-logged. It gets no span. HTTP/2 streams count one each. The place is released when the handler returns, so a 101-upgraded connection holds its place until it closes.
- **Connection cap.** A `net.Listener` wrapper counts connections from `Accept` to `Close`, across both traffic listeners together. Upgraded (hijacked) connections count until they close. A connection accepted past `MAX_CONNECTIONS` is never handed to `http.Server` and never queued:
  - It goes to a shed path with its own cap of **256** connections at once; past that, it is closed at once with `SO_LINGER 0` (a reset).
  - In the shed path, with a 1 s deadline for everything: on the TLS listener, a TLS handshake with a clone of the server's `tls.Config` whose ALPN offers only `http/1.1`. Then the gateway writes a fixed `HTTP/1.1 503 Service Unavailable` with `Retry-After: 1`, `Connection: close`, `Content-Type: application/json` and `{"error":"overloaded"}`, without reading the request. It half-closes, drains until EOF or the deadline (so the response is not lost to a reset), and closes.
  - Each such connection counts `gw_requests_total{route="none", method="other", code_class="5xx", origin="gateway", reason="shed", tenant="none"}`. It gets no access-log line, since no request was read. A process log warn `shedding connections` with the count since the last line goes out at most once a second.
- **Defaults and their budget.** File descriptors: at most 8192 client connections, plus at most 4096 busy upstream connections, plus idle upstream connections (512 per address), plus probes. That fits compose's `nofile` 65536 with room to spare. Memory, as an estimate for m4-shedding to measure: an idle client connection costs about 16 KiB (plain) to 48 KiB (TLS), and a request in flight about 64 KiB (handler goroutine, `ReverseProxy`'s 32 KiB copy buffer, upstream connection buffers), plus a retry body of up to 64 KiB. The worst case at the defaults is about 8192 × 48 KiB + 4096 × 128 KiB ≈ 0.9 GiB, reached only when every connection is TLS and every request in flight carries a 64 KiB retryable body. m4-shedding measures heap per idle connection and per request in flight and records the numbers. If the measured worst case at these defaults is above 1 GiB, it lowers the defaults to fit 1 GiB and records the new values in its PR.
- A stall of the whole machine (about 370 ms) at the exit checks' rates brings at most a few hundred requests at once (1000 rps × 0.37 s), far below both caps.

### Tracing (OB-02)

- **Provider** (`internal/telemetry`, next to `otlp.go`): `NewTracerProvider(ctx, endpoint, service, version string, ratio float64) (trace.TracerProvider, func(context.Context) error, error)`. It uses `go.opentelemetry.io/otel/sdk/trace` (in `otel/sdk` v1.47.0) and the exporter `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` v1.47.0 with `otlptrace` v1.47.0, pinned exactly. It has the same resource as the logs (ADR-0013). A `BatchSpanProcessor` exports every 1 s, in batches of 512, from a queue of 2048 that drops when full, so tracing never holds up a request. The sampler is `ParentBased(TraceIDRatioBased(ratio))`. Without an OTLP endpoint the gateway uses the no-op tracer and passes `traceparent` through unchanged, as in M1.
- **Propagation.** This supersedes ADR-0013's "trace context" bullet. W3C Trace Context only (`propagation.TraceContext`). A valid client `traceparent` is continued: the server span is its child, and `tracestate` is kept. An invalid or missing one starts a new trace. Each upstream attempt sends a `traceparent` naming the attempt span as the parent, with the sampled flag of the decision, so upstream spans nest under the attempt. The access log's `trace_id` is the gateway's trace ID, which with tracing on is present on every line, sampled or not.
- **Spans.** Paths, queries, headers and client addresses are never attributes, as in the access log (ADR-0013).

  | Span | Kind | When | Attributes |
  | --- | --- | --- | --- |
  | `<METHOD> <route>`, or `<METHOD>` when no route matched (`HTTP` for methods outside the metric's list) | server | every request past the shedding gate | `http.request.method`, `http.route` (route name), `http.response.status_code`, `network.protocol.version`, `spillway.request_id`, `spillway.upstream`, `spillway.origin`, `spillway.reason`, `spillway.attempts`, `spillway.retry_dropped` (`budget_exhausted`, `deadline`, `breaker`) when a wanted retry was not sent. Status error for a 5xx, with `error.type` set to the status |
  | `authn` | internal | when API-key resolution runs | `spillway.authn.result`: `anonymous`, `consumer` or `invalid` |
  | `ratelimit local` | internal | each local phase with at least one applying policy: before and after authentication | `spillway.ratelimit.phase` (`pre_auth`, `post_auth`), `spillway.ratelimit.policies` (count), `spillway.ratelimit.decision` (`allow`, `deny`) |
  | `ratelimit redis` | client | each Redis script call, refunds included | `spillway.ratelimit.slot` (`tenant`, `global`, `key`), `spillway.ratelimit.refund` (bool), `spillway.ratelimit.result` (`allow`, `deny`, `timeout`, `conn`, `script`) |
  | `upstream attempt` | client | each attempt, from send to response headers or failure (the span of `gw_upstream_duration_seconds`) | `spillway.upstream`, `spillway.endpoint` (`ip:port`), `server.address`, `server.port`, `spillway.attempt` (1–5), `spillway.backoff_ms` and `spillway.retry_reason` (the previous attempt's outcome: `connect_error`, `reset`, `timeout`, `502`, `503`, `504`) on attempts ≥ 2, `http.request.method`, `http.response.status_code` when there is a response. Status error for a failure in the outcome table, with `error.type` (`connect_error`, `reset`, `timeout` or the status) |

  "Each limiter check" means one span per Redis call, since each is a network round trip with its own outcome, and one per local phase, since local checks take microseconds and a span per policy would cost more than the check.
- **Unsampled requests** get non-recording spans: no attributes are set (each call site checks `span.IsRecording()`) and nothing is exported. m4-tracing adds `BenchmarkHandler` with tracing off, at ratio 0 and at ratio 1, and records ns/op and allocs/op in its PR.
- **Dev-mode attempt header.** With `SPILLWAY_GATEWAY_DEV_MODE=true` (ADR-0021), every response that went through at least one attempt carries `Spillway-Attempts: <n>`, replacing any value the upstream sent. Like `Spillway-Decision-Time`, it exists for the exit check (ADR-0031) and is never set in production. The access log gains an `attempts` field (0 when none was made) in every mode.

### Sampling (OB-02, D6)

- **Dev and demo:** the gateway samples 100% (compose default ratio 1). The Collector tail-samples in its `traces` pipeline (`receivers: [otlp]`, `processors: [tail_sampling, batch]`, `exporters: [otlp_grpc/tempo]`) with `decision_wait: 10s`, `num_traces: 100000`, `expected_new_traces_per_sec: 1000`, sampled and non-sampled decision caches of 100,000, so late spans follow their trace's decision. The policies, any of which keeps a trace:
  - `errors`: `status_code`, `[ERROR]`: every trace with a 5xx or a failed attempt, so every retried request;
  - `slow`: `latency`, `threshold_ms: 500`: requests slower than half a second, a hundred times NFR-04's 5 ms and far above a healthy dev request;
  - `rest`: `probabilistic`, `sampling_percentage: 10`: a tenth of everything else.

  The demo (M8) uses the same Collector config. A request longer than `decision_wait` is decided on the spans that ended within it.
- **Benchmarks:** a fixed 1% head ratio and no tail sampling (D6). `test/load/bench.sh` runs an OTel Collector next to the gateway (same image, `deploy/compose/otel-collector/bench.yaml`: OTLP in, `batch`, the `nop` exporter), pinned to the upstream's CPU. The gateway gets `SPILLWAY_GATEWAY_OTLP_ENDPOINT` and `SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO=0.01`, and the result JSON records `trace_sample_ratio`. The benchmark then pays for real span export. This also turns on OTLP log export, at the benchmark's 1% access-log sample (ADR-0015). Results from before m4-tracing were measured without either, and the PR shows a quick before/after.
- **Exit checks that measure latency** run the gateways at the benchmark ratio. `make verify-m1`, `verify-m2`, `verify-m3`, `accuracy`, `chaos-redis-outage` and `chaos-redis-primary` export `SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO=0.01` (target-specific in the Makefile). Their `dev` prerequisite then recreates the gateways at 1%. `make dev` alone, and `make verify-m4`, recreate them at the default of 1. m4-tracing runs `make verify-m2` and `make verify-m3` once each and records that NFR-03 and NFR-04 still pass. A failure there blocks the task; it does not change a rule.
- A client can set the sampled flag and so force tracing of its requests. The exporter's bounded, dropping queue caps what that costs, and the Collector decides what is kept.

### Gateway answers added in M4

| Situation | Status | Origin, reason | Body | `Retry-After` |
| --- | --- | --- | --- | --- |
| In-flight or connection cap (GW-26) | 503 | gateway, `shed` | `{"error":"overloaded"}` | 1 |
| Breaker open or half-open full (GW-14) | 503 | gateway, `circuit_open` | `{"error":"upstream circuit open"}` | seconds until half-open, ≥ 1 |
| `max_concurrent`, `max_pending`, `max_connections` | 503 | gateway, `circuit_open` | `{"error":"upstream overloaded"}` | 1 |
| Every attempt failed, or a retry dropped | the last attempt's answer | upstream, or gateway `upstream` | as today | none |

ADR-0008's table stays for everything else. `Retry-After: 1` follows ADR-0021's choice: the shortest whole number of seconds.

### Metrics (design table)

`gw_retries_total{upstream, outcome}` (`sent`, `budget_exhausted`), `gw_circuit_state{upstream}`, `gw_upstream_endpoint_healthy{upstream, endpoint}`, `gw_upstream_duration_seconds{upstream, attempt}` with `attempt` 1–5, and `gw_requests_total`'s `shed` and `circuit_open` reasons. No other metric is added: transitions are in the logs and the per-endpoint gauge. Series stay bounded by upstreams × endpoints.

### Tasks and interim behaviour

| Task (branch) | Builds | Gateway behaviour once merged |
| --- | --- | --- |
| m4-design-adrs | this ADR and ADR-0031 | unchanged |
| m4-health | proto and compile for `health_check` and `outlier_detection`; `internal/resilience` `Outlier`; the registry's upstream and endpoint state and reconcile, with the GW-29 test; probers; selection by selectable endpoints and panic mode; `gw_upstream_endpoint_healthy`. Outcomes are recorded from the one attempt | health checks and ejection work; selection is round robin over selectable endpoints |
| m4-balancing | proto `lb`, `hash_on`; per-endpoint in-flight counts; `round_robin`, `p2c` (now the default), `consistent_hash` with the remapping test | as this ADR, one attempt per request |
| m4-retries | proto `retry`, `retry_budget`; the attempt loop, body buffering, backoff, `Budget` with fake-clock tests and an integration test in which the budget binds; `Spillway-Deadline-Ms`; `gw_retries_total`; the `attempt` label; the access log's `attempts`; `Spillway-Attempts` in dev mode. Ticks the "Retries with backoff …" box | retries without a breaker check |
| m4-breaker | proto `circuit`; `Breaker`, its caps and the 503s; `gw_circuit_state`; `MayRetry` in the retry loop; graceful shutdown (GW-23) with its settings and compose's `stop_grace_period`. Ticks the "Active health checks …" box (the breaker state completes its registries) and the "Circuit breaker …; graceful shutdown" box | as this ADR, without shedding and spans |
| m4-shedding | `MAX_CONNECTIONS`, `MAX_IN_FLIGHT`, the listener wrapper and the shed path (plain and TLS), the memory measurement. Ticks the "Connection and in-flight caps …" box | as this ADR, without spans |
| m4-tracing | the provider, spans, propagation, the ratio setting, the Collector's tail sampling, the bench Collector, the Makefile ratio for the latency checks, `BenchmarkHandler`. Ticks the "OTel tracing …" box | as this ADR |
| m4-verify | ADR-0031: the compose service `upstream-fault`, `chaos upstream` and `chaos mock`, `test/verify/m4.sh` and its configs, `make verify-m4`, the measured pass rate | as this ADR |
| m4-review-fixes | D1, D2 (below) and review findings | as this ADR, with D1 and D2 |

Each task's tests run under `make test`; anything touching proxying, compose or Redis also runs `make test-integration`. The GW-29 reload test grows with each task's state.

### Example (used by the M4 tests)

```yaml
version: 50
upstreams:
  - name: orders
    lb: p2c                                    # the default, written out
    endpoints:
      - { address: 'upstream-1:8080' }
      - { address: 'upstream-2:8080' }
      - { address: 'upstream-3:8080' }
    health_check: { path: /status/200, interval: 5s, timeout: 1s, unhealthy: 3, healthy: 2 }
    outlier_detection: { consecutive_failures: 5, base_ejection: 30s, max_ejection: 300s, max_ejected_percent: 50 }
    circuit:
      max_connections: 1024
      max_pending: 1024
      max_concurrent: 1024
      error_rate: 0.5
      window: 10s
      min_requests: 20
      open_for: 30s
      half_open_probes: 5
    retry_budget: { ratio: 0.2, min_per_sec: 3 }
  - name: carts
    lb: consistent_hash
    hash_on: '{header:X-Cart-Id}'               # requests without it fall back to p2c
    endpoints:
      - { address: 'upstream-1:8080' }
      - { address: 'upstream-2:8080' }
      - { address: 'upstream-3:8080' }
routes:
  - name: orders-read
    match: { path_prefix: /bytes }
    upstream: orders
    timeouts: { connect: 250ms, per_try: 1s, total: 3s }
    retry: { attempts: 3 }                     # GET, HEAD, OPTIONS, PUT, DELETE, TRACE only
  - name: orders-write
    match: { path_prefix: /anything/orders, methods: [POST] }
    upstream: orders
    retry: { attempts: 2, non_idempotent: true } # D4: POST retried, the route's choice
  - name: carts
    match: { path_prefix: /anything/carts }
    upstream: carts
```

### Decisions taken for the user (review)

The user left M4's decision block unfilled. The run adopted the user's own suggested examples for D3–D6. For D1 and D2, carried over from M3, this ADR proposes a pick. Each is recorded here for the user to confirm or change.

- **D3. Default load balancing: `p2c`** (least request with two random choices). It tracks load without a global scan, sends less to a slow endpoint than round robin does, and is Envoy's and the design example's choice. One weakness: an endpoint that fails fast has few requests in flight and so attracts traffic. Outlier ejection removes it after `consecutive_failures` attempts, which is why ejection is on by default.
- **D4. Retry eligibility:** idempotent methods only (`GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE`, `TRACE`) unless the route sets `retry.non_idempotent: true`. **This goes beyond GW-12's text ("idempotent methods only") and GW-13's "routes set attempts only".** Proposed wording for docs/design.md, not applied by this PR: GW-12 "…idempotent methods only, unless a route opts in for all methods (`retry.non_idempotent`), …"; GW-13 "…routes set attempts (and may opt non-idempotent methods in) only…". **Needs the user's confirmation of the text change.** Until then the field exists, defaults to false, and the design's text describes the default behaviour exactly.
- **D5. Load-shedding response:** 503, `Retry-After: 1`, `{"error":"overloaded"}`, origin `gateway`, reason `shed`; on TLS past the connection cap, a handshake limited to HTTP/1.1, then the same response. 1 s is the shortest whole value. Shedding is meant to end within seconds, and fail-closed uses the same value (ADR-0021).
- **D6. Benchmark sampling:** a fixed 1% head ratio, no tail sampling, with a real Collector sink so that export costs are measured.
- **D1. HEAD on a GET-only route** falls through to a catch-all route and skips that route's limits (M3 review). **Proposed: (a) HEAD matches GET.** At compile, every route match and every policy match whose `methods` lists `GET` and not `HEAD` gets `HEAD` added; `{method}` keys keep the real method. Rationale: RFC 9110 defines HEAD as GET without the body, upstreams serve it that way, and it closes the limit bypass without a new status code. (b) 405 would need a "path matched, method didn't" result from the router that the catch-all route makes ambiguous. (c) keeps the bypass. **Needs the user's confirmation; implemented in m4-review-fixes.**
- **D2. Client-chosen Redis keys vs `noeviction`** (M3 review). **Proposed:** at compile, a non-local policy whose key template has a client-chosen dimension (`{header:<Name>}`) logs warn `policy keys come from a client-chosen header; distinct values can fill a noeviction Redis node, after which the policy follows its failure mode`, with `policy`, once per snapshot load. docs/design.md's key template text and the README say that such limits should be local (where U1's 100,000-key cap bounds them) or keyed by a header the client cannot vary freely. A Redis `used_memory / maxmemory` alert is noted for M7's alert rules (OB-05). The config is not rejected, since that would break valid configs whose header is set by a trusted proxy. `{ip}` is not flagged: it is bounded by real addresses, and by /64s for IPv6 (U2). **Needs the user's confirmation; implemented in m4-review-fixes.**
- **Retry-budget floor: additive.** `0.2 × requests + 3/s × 10 s` per 10 s window, not `max(0.2 × requests, 30)`. GW-13 says "plus a floor", and Finagle's `RetryBudget`, with the same shape (a 10 s window, a percentage and a per-second minimum), adds its reserve to the percentage the same way. The two differ by at most 30 retries per 10 s, and only on busy upstreams.
- **Panic mode** when no endpoint is healthy (above) instead of a 503 "no healthy endpoint": a health check that is wrong everywhere costs nothing, and a real outage is still answered fast by the breaker.
- **Breaker counts attempts, not requests,** and every upstream has a breaker and outlier detection with defaults, without an off switch. Only active health checks are opt-in.
- **Config names follow docs/design.md** (`lb`, `circuit`, `retry`, `health_check.unhealthy` and `.healthy`, `retry_budget.ratio` and `.min_per_sec`), not the run's suggested names. New fields: `hash_on`, `health_check.timeout`, `outlier_detection`, `circuit.max_connections` and `.max_pending`, `retry.non_idempotent`.
- **Defaults not set by a requirement:** `consecutive_failures` 5, `base_ejection` 30 s, `max_ejection` 300 s (Envoy's), `max_ejected_percent` 50 with floor rounding; `error_rate` 0.5, `window` 10 s, `open_for` 10 s; caps 1024 (Envoy's); health checks every 5 s with a 1 s timeout, 3 to fail and 2 to recover; at most 5 attempts; backoff from 25 ms up to 250 ms with full jitter; 64 KiB retry bodies; drain 5 s; grace 15 s; 8192 connections and 4096 requests in flight; tail sampling at 500 ms and 10%.
- **Exit-check choices** (ADR-0031): "within 10 s of the failure" means the 10 s window that starts at the failure, counting the requests in flight at that moment; two scenarios (an endpoint answering 503, an endpoint refusing connections) through a fault-injecting mock; 500 rps; retries checked against the formula in every 8–10 s window between metric scrapes; the first 20 retried requests' traces read from Tempo. The chaos table's "breaker opens" is read as an all-endpoints-failing case: with one of three endpoints failing the breaker should stay closed (an open breaker would fail the 99% rule), so the exit check prints its state, and m4-breaker's integration test and M7's chaos suite prove the opening.

## Consequences

- Every existing config keeps working. What changes for it: `p2c` instead of round robin (from m4-balancing), outlier ejection and the breaker with defaults, and a new gateway-owned header that transforms may not name (`Spillway-Deadline-Ms`). No route retries until it sets `retry.attempts`.
- Requests with a body on retrying routes are read up to 64 KiB before the first attempt, which adds that read to their latency. Larger bodies stream as before, without retries.
- One upstream returning 5xx for every request trips its breaker within a few hundred milliseconds under load. Upstreams whose 5xx are ordinary application errors at a high rate should set a higher `error_rate`.
- A single-endpoint upstream is never ejected (the cap rounds down to 0). Its protection is the breaker.
- Shutdown now takes at least the drain, 5 s by default, so `docker compose stop` and restarts of the gateways take about 5 s longer.
- Bench results gain span and log export cost from m4-tracing. M7's regression gate compares only results with the same `trace_sample_ratio`.
- Health checks add one request per endpoint per interval per replica (with 100 replicas and the 5 s default, 20 probes a second per endpoint).
- ADR-0008 (endpoint selection), ADR-0010 (shutdown) and ADR-0013 (trace context passthrough and the access log's `trace_id` source) are superseded in those parts. Their status lines say so.

## Alternatives considered

- **The run's suggested names** (`load_balancing.algorithm`, `circuit_breaker`, `retries`) — docs/design.md's config format, and the M5 tables, already use `lb`, `circuit`, `retry` and `retry_budget`, and a second vocabulary would need design changes for nothing.
- **A ring (Ketama) or Maglev for consistent hashing** — both need a table rebuilt on every endpoint or health change, and Maglev's remapping on removal is only near-minimal. Rendezvous is minimal by construction, has no state, and is O(n) for small n.
- **Hashing on a header name only** (`hash_on: header:X`) — GW-07 says "a header or key". Reusing the limiter's key templates covers both, plus composites, and needs no new parser.
- **A 503 "no healthy endpoint" when health checks fail everywhere** — turns a probe misconfiguration into an outage. Panic mode plus the breaker answers a real outage just as fast.
- **Envoy's "at least one host may be ejected" rounding** — would eject the only endpoint of a one-endpoint upstream.
- **Counting requests (after retries) in the breaker** — retries that succeed would hide an upstream whose attempts fail at a high rate, and a half-open probe with retries would not test one answer.
- **Retry budget as `max(ratio × requests, floor)`** — GW-13 says "plus".
- **An exact sliding log for the budget** — 100 buckets of 100 ms are within 1% of the window, and the metrics show what was decided.
- **Retrying 500** — GW-12 lists 502/503/504, and a 500 is usually deterministic.
- **Buffering whole request bodies** — unbounded memory. 64 KiB covers typical JSON writes.
- **Trusting a client's `Spillway-Deadline-Ms`** — lets clients shorten or stretch upstream work. A trusted-proxy variant can come later if a requirement asks for it.
- **Accept-and-close (a reset) for the connection cap everywhere** — fast, but gives no 503 or `Retry-After`, which GW-26 asks for. It is still the fallback past 256 shed connections.
- **Not accepting past the cap** (leaving connections in the kernel backlog) — that is the queueing GW-26 rules out.
- **Head sampling at the Collector, or tail sampling in the gateway** — OB-02 puts 100% at the gateway and tail sampling in the Collector for dev, and a fixed head ratio for benchmarks.
- **Ignoring the client's sampled flag** — the client controls the trace ID, and so `TraceIDRatioBased`'s outcome, anyway. ParentBased keeps upstream traces whole, and the bounded queue caps the cost.
- **Separate health and outlier metrics** — the design's table has one gauge. It says whether the endpoint takes traffic, and the logs say why.
