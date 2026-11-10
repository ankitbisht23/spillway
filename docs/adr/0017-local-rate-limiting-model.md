# 0017. Local rate limiting model

- **Status:** Accepted
- **Date:** 2026-11-10
- **Requirement IDs:** RL-01 – RL-04, RL-07, RL-17, RL-25 – RL-27, RL-31, GW-17, GW-29, SEC-03, SEC-05, OB-01, OB-03, NFR-03

## Context

M2 adds rate limiting that runs entirely in each gateway's memory, plus the static API keys that give limits an identity. The design fixes the algorithms and their maths (Algorithms, Limiter script contract), the limit model (RL-03), the key dimensions (RL-04), the replica division (RL-02, RL-31), the modes (RL-26) and the request pipeline, but leaves open:

- where the code lives and what the `Limiter` interface looks like in Go, and how a concurrency slot is given back;
- how per-key state stays bounded in memory, and what a key is made of, given that raw API keys and header values must never be stored (RL-13's rule for Redis);
- which proto fields carry policies, the replica count and route cost, with their defaults and validation;
- what M2 does with a policy that is not local-only, when Redis limiting only arrives in M3;
- what a key template may contain, and what happens when a request lacks what the template needs;
- how the client IP is found behind proxies (SEC-05), and where static API keys come from and how they are checked (GW-17, SEC-03);
- in which order policies run, what a deny costs the other policies, and how shadow mode records without rejecting;
- how limiter state survives a reload (GW-29);
- which metrics, log fields and responses a decision produces, and how M2's exit check measures it.

## Decision

### Packages and interface (RL-01, RL-02)

- `internal/ratelimit` holds the interface, `Decision` and a `Clock` (`Now() time.Time`) with a real implementation and a fake one for tests that only moves when told to. `internal/ratelimit/local` holds the in-memory GCRA, sliding-window counter, fixed-window and concurrency limiters. `internal/ratelimit/redis` stays as it is until M3.

  ```go
  type Decision struct {
      Allowed    bool
      Remaining  int64         // cost units still available under this limit after this call
      ResetAfter time.Duration // until the limit is back to full (windows: until the window ends)
      RetryAfter time.Duration // denied only: until this request's cost could be admitted; 0 if allowed
  }

  type Limiter interface {
      Allow(ctx context.Context, key string, cost int64) (Decision, error)
  }
  ```

  The concurrency limiter has the same `Allow` plus `Release(key string, cost int64)`. The pipeline calls `Release` exactly once for every request the limiter admitted: at once if another limit denies the request (the refund below), otherwise when the request completes, times out or the client disconnects (RL-02). `Release` never panics and never takes a count below 0.
- `key` is the 16-byte digest of the filled key template (below), passed as a string. Local limiters never return an error and ignore `ctx`; both are there for M3's Redis limiters.
- **Refund.** `Allow` with a negative cost is a refund, as in the script contract: it makes no decision, returns `Allowed: true` with `Remaining` and `ResetAfter` after the refund, and reverses an earlier admission of that cost. GCRA subtracts `|cost|·T` from the TAT; the window algorithms subtract `|cost|` from the current window's count, never below 0 (a refund that crosses a window boundary is lost, which errs towards denying); concurrency does what `Release` does.

### Maths (RL-01, design Limiter script contract)

All arithmetic is in integer nanoseconds of the `Clock`'s Unix time, `t` = now, `c` = cost, and `rate`, `burst`, `period` are the per-replica values (RL-31, below).

- **GCRA.** `T = period / rate`, `τ = T · burst`, `TAT' = max(TAT, t) + c·T`. Allow if `TAT' − t ≤ τ`, and then store `TAT'`; a deny stores nothing. `Remaining = floor((τ − (TAT_s − t)) / T)` and `ResetAfter = TAT_s − t`, where `TAT_s` is the stored TAT after the call (`max(TAT, t)` on a deny). On a deny `RetryAfter = TAT' − τ − t`.
- **Fixed window.** Windows start at multiples of `period` since the Unix epoch: `w = floor(t / period) · period`, `e = t − w`. A count that belongs to an older window is reset to 0. Allow if `count + c ≤ rate`, then `count += c`. `Remaining = rate − count`, `ResetAfter = period − e`, and on a deny `RetryAfter = period − e`.
- **Sliding-window counter.** Windows are aligned the same way; state is `{w, cur, prev}`. When `t` is one window past `w`, `prev = cur, cur = 0`; two or more windows past, both are 0. `estimate = prev · (1 − e/period) + cur`. Allow if `estimate + c ≤ rate`, then `cur += c`. `Remaining = max(0, floor(rate − estimate))` with `estimate` after the call, `ResetAfter = period − e`. On a deny, `RetryAfter` is the earliest time the request would fit if nothing else were admitted: `period·(1 − (rate − c − cur)/prev) − e` when `cur + c ≤ rate`, otherwise `(period − e) + period·(1 − (rate − c)/cur)`, rounded up to a nanosecond.
- **Concurrency.** Allow if `inflight + c ≤ max`, then `inflight += c`. `Remaining = max − inflight`, `ResetAfter = 0`, `RetryAfter = 0`.

### Bounded memory

- Per-key state lives in 64 maps sharded by the key digest, each behind its own mutex, one set per limiter.
- One sweep per registry runs every 10 s and drops idle entries: GCRA entries with `TAT ≤ t`, fixed-window entries whose window has ended, sliding-window entries two windows past `w`. Dropping them changes no decision, since a missing entry behaves exactly like an idle one. A concurrency entry is deleted as soon as its count returns to 0.
- **Key digest.** The filled key template is hashed with SHA-256 and the first 16 bytes are the map key: the same digest as the design's Redis key spec, which M3 encodes as 22 base64url characters. Raw API keys and header values never become map keys.

### Config (RL-03, RL-07, RL-31)

New fields in `api/proto/config.proto`, all additive (`buf breaking` stays green):

```proto
message Config {
  // … existing fields 1–3 …
  RateLimitSettings ratelimit = 4;
  repeated Policy policies = 5;
}

message Route {
  // … existing fields 1–12 …
  // Cost units each request deducts (RL-07); unset means 1.
  int64 cost = 13;
}

message RateLimitSettings {
  // Gateway replicas sharing every limit (RL-31); unset means 1.
  int32 replicas = 1;
}

message Policy {
  string name = 1;
  string level = 2;          // global | tenant | consumer | route
  PolicyMatch match = 3;
  string key_template = 4;
  string algorithm = 5;      // gcra | sliding_window | fixed_window | concurrency
  repeated Limit limits = 6;
  string mode = 7;           // enforce | shadow | off; unset means enforce
  bool local = 8;            // runs in gateway memory only (RL-17)
}

message PolicyMatch {
  repeated string routes = 1;  // route names, or the single entry "*" for every route
  repeated string methods = 2; // empty means every method
}

message Limit {
  int64 rate = 1;     // requests (cost units) per period; for concurrency, the max in flight
  string period = 2;  // Go duration, whole seconds
  int64 burst = 3;    // GCRA only
}
```

Compile rejects the snapshot, and the gateway keeps its last good one, when any of these fails:

| Field | Rule |
| --- | --- |
| `ratelimit.replicas` | 0 (unset) means 1; negative is rejected |
| `routes[].cost` | 0 (unset) means 1; negative or above 1,000,000,000 is rejected |
| `policies[].name` | required, unique, 1–64 characters from `[A-Za-z0-9._-]`, so it is safe as a metric label, a header item name (ADR-0018) and, from M3, a Redis key part |
| `level` | required; one of the four values. In M2 it has no effect at run time; M3 uses it to pick Redis slots (RL-05, RL-13) |
| `match.routes` | required; either exactly `["*"]` or names of existing routes, without duplicates |
| `match.methods` | upper-case method tokens, without duplicates |
| `key_template` | required; see Key templates |
| `algorithm` | required; one of the four values |
| `limits` | at least one. GCRA and window limits: `rate` 1 to 1,000,000,000; `period` a Go duration that is a whole number of seconds, from `1s` to `8760h`, unique within the policy. Concurrency: exactly one limit with `rate` (the max in flight) ≥ 1 and no `period` or `burst` |
| `burst` | GCRA only, rejected for other algorithms. Unset (0) means `rate` when `period ≤ 1m`, and is an error when `period > 1m`; otherwise 1 to 1,000,000,000. `τ = period · burst / rate` must be at most `8760h`, which keeps the nanosecond maths far from overflow |
| `mode` | unset means `enforce`; else one of the three values |
| `local` | must be `true` unless `algorithm` is `concurrency`, which is always local (RL-02). Otherwise compile fails with `policy "<name>": only local policies (local: true) are supported until global Redis policies arrive in M3` |
| cost | for every route a policy matches, the route's cost must not exceed the per-replica capacity of each of the policy's limits: `burst` for GCRA (RL-03), `rate` for the windows, the max for concurrency. A larger cost could never be admitted |

**Per-replica values** (RL-31, RL-02). Configured limits are cluster-wide. Each gateway's limiters use `max(1, floor(rate / replicas))`, `max(1, floor(burst / replicas))` (burst after its default) and `max(1, floor(max / replicas))`, with the period unchanged. The cost check uses these per-replica values.

`failure_mode` and `lease_k` are not added in M2: M3 adds `failure_mode` with the Redis limiters, M7 adds `lease_k` with batched mode.

### Scope: every policy runs in memory (RL-17, RL-31)

M2 has no Redis limiter, so every policy is local and none touches Redis. Policies must say `local: true` (concurrency is local regardless) so that a config written for M2 keeps meaning the same thing once M3 makes non-local policies global. Until then a policy without `local: true` fails compile with the message above rather than silently running per replica.

### Key templates (RL-04)

- A template is literal text with placeholders in braces. Literal text may not contain `{` or `}` outside a placeholder. Placeholders and their values:

  | Placeholder | Value |
  | --- | --- |
  | `{ip}` | the client IP (below), as `netip.Addr.String()` |
  | `{api_key}` | the lower-case hex SHA-256 of the request's API key, never the key |
  | `{consumer}` | the API key's consumer name |
  | `{tenant}` | that consumer's tenant |
  | `{route}` | the matched route's name |
  | `{method}` | the request method as sent |
  | `{header:<Name>}` | the first value of request header `<Name>`, which must be a valid header name |

  Any other placeholder fails compile.
- **Unfilled templates.** When a placeholder cannot be filled for a request (no API key, a consumer without a tenant, a missing or empty header), the policy does not apply to that request: no decision, no metric, no header item. This leaves a gap: until per-route `auth_mode` (GW-20, M5) can require a key, a client can skip a `{consumer}` or `{api_key}` policy by sending no key. Configs should pair such policies with an `{ip}` flood policy that covers anonymous traffic, as the example below does.

### Client IP (SEC-05, RL-04)

- New setting `SPILLWAY_GATEWAY_TRUSTED_PROXIES`: comma-separated CIDRs of proxies in front of the gateway. Default empty, which means `X-Forwarded-For` is ignored. An entry that is not a CIDR stops the gateway at start.
- The TCP peer address is the client IP unless the peer is inside a trusted CIDR. Then the gateway reads every `X-Forwarded-For` field line, joined with commas, and walks the entries right to left, skipping trusted addresses: the first untrusted one is the client IP; if every entry is trusted, the leftmost one is. An entry that is not a bare IP address (ports and brackets included) makes the gateway fall back to the peer address. IPv4-mapped IPv6 addresses (`::ffff:a.b.c.d`) are normalised to IPv4 before matching and before filling `{ip}`.
- What the upstream receives is unchanged: the gateway still replaces `X-Forwarded-For` with the peer address (ADR-0008).

### Static identity (GW-17, SEC-03)

- New setting `SPILLWAY_GATEWAY_IDENTITY`: an optional identity YAML file, loaded and polled the same way as the config file (SHA-256 every 1 s; an invalid file keeps the last good identity, and an invalid file at start stops the gateway). It is parsed (YAML → JSON → protojson) into a new message in `config.proto`, the identity section of a snapshot (ADR-0005), with its own version:

  ```proto
  message Identity {
    int64 version = 1;
    repeated Consumer consumers = 2;
    repeated ApiKey api_keys = 3;
  }

  message Consumer {
    string name = 1;   // required, unique
    string tenant = 2; // optional; empty leaves {tenant} unfilled
  }

  message ApiKey {
    string prefix = 1;   // the key's first 12 bytes; unique
    string hash = 2;     // lower-case hex SHA-256 of the whole key (64 characters)
    string consumer = 3; // an existing consumer's name
  }
  ```

- Keys arrive in the `X-API-Key` request header on every route (per-route auth modes come with M5). No header: the request is anonymous. Otherwise the header must have exactly one value at least 12 bytes long whose first 12 bytes match a key's `prefix` and whose SHA-256 equals that key's `hash`, compared with `crypto/subtle.ConstantTimeCompare`; anything else, including an empty value or two values, is answered 401 by the gateway with `Content-Type: application/json` and body `{"error":"invalid API key"}`. With no identity file, every key is invalid. The header still goes to the upstream unchanged.
- Keys are never logged. The access log's `key_hash` is the first 12 hex characters of the presented key's SHA-256, set whenever the request reached key resolution with a key, valid or not.

### Pipeline (design Request pipeline, RL-26)

1. Route match. Rate limiting runs right after it, before ADR-0011's size checks, so oversized requests count too.
2. Phase 1: policies whose templates use only `{ip}`, `{route}`, `{method}` and `{header:…}`.
3. API key resolution (401 above). A request denied in phase 1 skips it.
4. Phase 2: policies that use `{api_key}`, `{consumer}` or `{tenant}`.
5. Proxy (or the gateway's own answer).

- A policy applies when its mode is not `off`, its `match.routes` covers the route, `match.methods` is empty or holds the method, and its template fills. Within each phase, policies run in config order and every limit of every applying policy is checked, even after a deny, so each policy records a decision and the headers list every enforce limit that applied.
- A request is denied (429) when any enforce-mode limit denies. A denied request consumes nothing: every limit that admitted it in either phase, enforce or shadow, is refunded (or released, for concurrency). A shadow limit never rejects a request and never changes the response; like the script contract's write rule, it keeps its deduction only when the request is allowed and that shadow limit allowed it.
- A request denied in phase 1 is not authenticated, so its answer is 429 even if its key is invalid. A request admitted by phase 1 and then rejected with 401 keeps its phase-1 deductions, so floods of bad keys still hit the IP limits.

### Runtime state (GW-29)

- Limiters live in a registry outside the snapshot, keyed by a stable ID: policy name, algorithm and the limit's period in seconds (`per-consumer|gcra|60`); concurrency limiters by policy name alone. The config watcher reconciles it on each swap, before the new snapshot goes live, as it does upstream pools (ADR-0008):
  - IDs that remain keep their state (TATs, window counts, in-flight counts) and take the new per-replica rate and burst; a change of `rate`, `burst` or `ratelimit.replicas` keeps state.
  - New IDs start empty.
  - Removed IDs are dropped. A request in flight keeps the concurrency limiter it was admitted by and releases into it, so a removed concurrency limit drains instead of breaking; a policy removed and added back starts empty.
- A request still running on the old snapshot uses the registry's current limiter for each ID and skips IDs the registry no longer has.
- Test: a reload under load resets no bucket and no in-flight count.

### Responses, metrics and logs (RL-25 – RL-27, OB-01, OB-03)

- A denial is answered by the gateway with 429, `Content-Type: application/json`, body `{"error":"rate limited"}`, and `Retry-After` plus the `RateLimit` and `RateLimit-Policy` fields of ADR-0018. Allowed responses carry the two fields as well. Only enforce-mode limits appear in the fields, and concurrency limits do not (ADR-0018).
- `gw_requests_total` counts a 429 as `origin="gateway"`, `reason="ratelimited"`, and a 401 as `origin="gateway"`, `reason="none"`. Both are always access-logged, as gateway-answered 4xx (ADR-0013).
- `gw_ratelimit_decisions_total{policy, decision, mode}`: one increment per applying policy per request. `decision` is `deny` if any of the policy's limits denied, else `allow`; `mode` is `enforce` or `shadow`. A shadow would-deny is `decision="deny", mode="shadow"`.
- `gw_ratelimit_check_duration_seconds{backend="local"}`: one observation per phase that evaluated at least one policy, refunds included. Buckets: 10 µs, 25 µs, 50 µs, 100 µs, 250 µs, 500 µs, 1 ms, 2.5 ms, 5 ms, 10 ms, 25 ms, 50 ms, which also cover M3's 5 ms Redis budget.
- The access log's `decisions` field lists every applying policy in evaluation order as `{"policy": "<name>", "decision": "allow|deny", "mode": "enforce|shadow"}`; `key_hash` is as above.

### Example (used by the M2 tests)

Config (the upstream is the compose one):

```yaml
version: 1
ratelimit: { replicas: 1 }
upstreams:
  - name: httpbin
    endpoints:
      - address: upstream-1:8080
      - address: upstream-2:8080
      - address: upstream-3:8080
routes:
  - name: bytes
    match: { path_prefix: /bytes }
    upstream: httpbin
  - name: anything
    match: { path_prefix: /anything }
    upstream: httpbin
    cost: 5
  - name: delay
    match: { path_prefix: /delay }
    upstream: httpbin
  - name: default
    match: { path_prefix: / }
    upstream: httpbin
policies:
  - name: per-ip                     # phase 1: covers anonymous traffic too
    level: global
    match: { routes: ['*'] }
    key_template: '{ip}'
    algorithm: gcra
    limits: [{ rate: 200, period: 1s }]           # burst defaults to 200
    local: true
  - name: per-consumer               # phase 2: skipped for anonymous requests
    level: consumer
    match: { routes: ['*'] }
    key_template: '{consumer}'
    algorithm: gcra
    limits:
      - { rate: 100, period: 1m }                 # burst defaults to 100
      - { rate: 10000, period: 1h, burst: 500 }   # burst required above 1m
    local: true
  - name: tenant-writes
    level: tenant
    match: { routes: [anything], methods: [POST, PUT, PATCH, DELETE] }
    key_template: '{tenant}:{route}'
    algorithm: sliding_window
    limits: [{ rate: 600, period: 1m }]
    local: true
  - name: per-key-daily
    level: consumer
    match: { routes: ['*'] }
    key_template: '{api_key}'
    algorithm: fixed_window
    limits: [{ rate: 100000, period: 24h }]
    local: true
  - name: delay-inflight
    level: route
    match: { routes: [delay] }
    key_template: '{route}'
    algorithm: concurrency
    limits: [{ rate: 20 }]                        # max in flight; always local
  - name: bytes-per-agent
    level: global
    match: { routes: [bytes], methods: [GET] }
    key_template: '{ip}:{header:User-Agent}'
    algorithm: gcra
    limits: [{ rate: 5, period: 1s }]
    mode: shadow
    local: true
  - name: retired
    level: global
    match: { routes: ['*'] }
    key_template: '{method}'
    algorithm: fixed_window
    limits: [{ rate: 10, period: 1s }]
    mode: off                                     # never evaluated
    local: true
```

Identity file. The keys are example values for tests only: `alice-example-key-1` and `bob-example-key-1`; tests generate any other keys at run time.

```yaml
version: 1
consumers:
  - { name: alice, tenant: acme }
  - { name: bob, tenant: acme }
  - { name: carol }                  # no tenant: tenant-writes skips her requests
api_keys:
  - { prefix: alice-exampl, hash: 64571644321bee63cc93bac0fe8dfdb7d1858a642b4bcef984833813e18b6507, consumer: alice }
  - { prefix: bob-example-, hash: d9c03dab37eb8704de0f841f2032d043796278f8060fe1aa845719671e158210, consumer: bob }
```

### Exit check (`make verify-m2`)

`test/verify/m2.sh`, in `m1.sh`'s style, prints each measured number with PASS or FAIL:

- (a) the property tests for every algorithm against its own bound (design Testing, Property) pass;
- (b) shadow never rejects: gateway-1 runs a config whose only policy on the target route is a shadow policy, loadgen drives it well above that policy's limit, and the run shows zero 429s while `gw_ratelimit_decisions_total{mode="shadow", decision="deny"}` rose;
- (c) NFR-03: two loadgen runs at the same fixed rate and duration, one straight to an upstream and one through gateway-1 with local policies active that allow all of the traffic; it prints both p99s and the added p99, PASS if at most 2 ms.

## Consequences

- Limiter decisions in M2 use each gateway's clock; that is fine for per-replica state and changes in M3, where Redis `TIME` decides (RL-12).
- With `ratelimit.replicas: N` each gateway admits about 1/N of a limit, so a load balancer that spreads traffic unevenly lets less through overall than the limit. Small limits round down to 1 per replica and admit up to `replicas` in total. Heartbeats replace the static count in M5 (RL-24).
- A client without a key skips consumer, tenant and key policies until GW-20 (M5) lets a route require a key; the IP flood policy is the guard meanwhile.
- A config written for M3's global policies fails to load in M2 instead of quietly limiting per replica.
- Periods are whole seconds, so sub-second windows are not available; a short burst is expressed with GCRA's `burst` instead.
- An admitted-then-refunded request costs a few map operations; denied requests therefore cost more than allowed ones, which the NFR-03 run does not measure (it allows all traffic).
- The registry, sweep and phase split are the M2 tasks' work; M3 adds `failure_mode`, the Redis limiters behind the same interface and the global path in the pipeline, keeping the policy fields above.

## Alternatives considered

- **Raw filled templates as map keys** — API keys and header values would sit in memory and in any heap dump; the digest also gives M3 the same key bytes.
- **An LRU or TTL cache per limiter** — needs a size to tune and can evict a key that is still limited, letting it start fresh; a sweep of provably idle entries never changes a decision.
- **Running non-local policies per replica in M2** — the same YAML would mean different limits in M2 and M3.
- **Evaluating every policy after API key resolution** — floods with bad keys would never meet the IP limits, and identity-free limits would wait for key hashing.
- **Stopping at the first deny** — the later policies would record no decision and the headers would miss their limits.
- **Checking all limits before writing any, as the Redis script does** — needs a two-step interface across limiters; with local limiters a refund is cheap and keeps `Allow` the one call M3 also implements.
- **Sub-second periods** — `RateLimit-Policy`'s `w` is whole seconds (ADR-0018), and none of the design's limits needs one.
- **The client IP from the leftmost `X-Forwarded-For` entry** — any client can write it.
- **API keys in the config file** — keys are identity and must not roll back with config (ADR-0005).
- **Plain-text 429 and 401 bodies, like the M1 errors** — clients of a limited API parse a JSON error more easily; the other gateway errors keep their M1 form.
