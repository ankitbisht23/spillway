# 0018. RateLimit headers

- **Status:** Accepted
- **Date:** 2026-11-10
- **Requirement IDs:** RL-25, RL-26

## Context

RL-25 asks for `RateLimit` and `RateLimit-Policy` headers per the IETF httpapi draft on 429s and allowed responses, with `Retry-After` in whole seconds (rounded up, minimum 1), and for an M2 ADR that pins one revision of the draft with exact syntax and test vectors. The draft has changed shape across revisions (early ones used `RateLimit-Limit`, `-Remaining` and `-Reset` fields), so "per the draft" means nothing until a revision is named. The draft also leaves to the server which policies to list, how to name them, and how its algorithms' state maps to the fields. ADR-0017 fixes the limiter `Decision` and the policy model these headers report.

## Decision

- **Pinned revision.** [draft-ietf-httpapi-ratelimit-headers-11](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers-11), "RateLimit header fields for HTTP", 23 May 2026: the latest revision on the IETF datatracker on 2026-11-10, an active Internet-Draft of the HTTPAPI working group. A later revision changes these headers only through a new ADR.
- **What the draft defines** (both fields are Structured Field Lists, RFC 9651, and may not appear in trailers):
  - `RateLimit-Policy`: a non-empty List of quota policy Items. The Item value is a String naming the policy. Parameters: `q` (required, quota in quota units, non-negative Integer), `qu` (quota unit, default requests), `w` (window, positive Integer seconds), `pk` (partition key, Byte Sequence).
  - `RateLimit`: a List of service limit Items, each naming a quota policy. Parameters: `r` (required, available quota, non-negative Integer), `t` (seconds within which the client can use no more than `r`, non-negative Integer, delay-seconds like `Retry-After`), `pk`.
  - Servers may send both on any status, 429 included. `Retry-After` takes precedence over `t`.
- **Items.** One item per limit of every applying enforce-mode policy (ADR-0017), in evaluation order: phase-1 policies in config order, then phase-2 policies in config order, and each policy's limits in config order. Shadow and off policies never appear. Concurrency policies do not appear either: they have no window and no quota spent over time, so `q` and `w` would mislead; a concurrency denial is still visible through the 429 and `Retry-After`.
- **Item names.** A String: the policy's name when the policy has one limit, otherwise `<policy>/<period in seconds>s` (`"per-consumer/60s"`). Policy names are `[A-Za-z0-9._-]` and periods are unique within a policy (ADR-0017), so names are unique within a response and need no escaping.
- **Parameters.** Only these, in this order; `qu` and `pk` are never sent (the unit is the route's cost unit, which is the request unless a route sets `cost`; the partition key would expose key material):

  | Field | Parameter | Value |
  | --- | --- | --- |
  | `RateLimit-Policy` | `q` | the limit's per-replica `rate` (ADR-0017, RL-31); for GCRA this is the sustained rate, so `r` can exceed `q` when `burst > rate` |
  | `RateLimit-Policy` | `w` | the limit's `period` in seconds (always whole, ADR-0017) |
  | `RateLimit` | `r` | the `Decision`'s `Remaining` |
  | `RateLimit` | `t` | the `Decision`'s `ResetAfter` in seconds, rounded up (0 stays 0) |

  The decision used is the one that ends the request's dealings with that limit: for an allowed request, the limit's own decision; for a denied request, the refund's decision for each limit that admitted it (so `r` and `t` describe the state the client actually has), and the deny decision for each limit that denied it.
- **Serialisation.** Each field is one field line, its items joined by `", "` as RFC 9651 serialises a List: `"<name>";q=<q>;w=<w>` and `"<name>";r=<r>;t=<t>`. Both fields are set on every response, gateway answers included (429, 401, 413, 431, 502–504), to a request for which at least one item exists, replacing any `RateLimit` or `RateLimit-Policy` the upstream sent; they are set after ADR-0011's response transforms, so transforms cannot change them. With no items, neither field is set and the upstream's pass through. Go's `http.Header` writes the names in canonical form (`Ratelimit`, `Ratelimit-Policy`); field names are case-insensitive.
- **`Retry-After`** (429 only). Whole seconds: the largest `RetryAfter` among the denying enforce limits, concurrency included, rounded up to a whole second, minimum 1. Never an HTTP date.

### Test vectors

Times are seconds after an instant `t0`; `t0` is 2026-11-10T12:00:00.250Z where a fixed window needs one. Unless a vector says otherwise, `ratelimit.replicas` is 1, the config and identity are ADR-0017's example, and the request is `GET /bytes/1` with cost 1. Each vector gives the state before the request, the decisions and the exact header lines.

1. **One policy, allowed, sub-second reset.** A config whose only policy is `per-ip`, GCRA `{ rate: 10, period: 1s }` (burst 10, `T` = 100 ms, `τ` = 1 s). The IP's first request at `t0`: `TAT'` = t0 + 0.1 s, allowed, `Remaining` 9, `ResetAfter` 100 ms, rounded up to 1.

   ```
   RateLimit-Policy: "per-ip";q=10;w=1
   RateLimit: "per-ip";r=9;t=1
   ```

2. **One policy, denied.** Same config; ten requests from the IP were admitted at `t0`, so `TAT` = t0 + 1 s. An eleventh at `t0`: `TAT'` = t0 + 1.1 s, and 1.1 s > `τ`, so it is denied: `RetryAfter` 100 ms (rounded up, then the minimum 1), `Remaining` 0, `ResetAfter` 1 s.

   ```
   HTTP/1.1 429 Too Many Requests
   Retry-After: 1
   RateLimit-Policy: "per-ip";q=10;w=1
   RateLimit: "per-ip";r=0;t=1
   ```

3. **Several policies and limits, allowed.** Alice's (`X-API-Key: alice-example-key-1`) first request at `t0`. Applying policies: phase 1 `per-ip` and the shadow `bytes-per-agent` (not listed); phase 2 `per-consumer` (two limits) and `per-key-daily`; `tenant-writes` does not match `GET /bytes`, `delay-inflight` does not match the route, `retired` is off.
   - `per-ip` (`T` = 5 ms, `τ` = 1 s): `Remaining` 199, `ResetAfter` 5 ms.
   - `per-consumer` 1m (`T` = 600 ms, `τ` = 60 s): `Remaining` 99, `ResetAfter` 600 ms.
   - `per-consumer` 1h (`T` = 360 ms, `τ` = 180 s): `Remaining` 499, `ResetAfter` 360 ms.
   - `per-key-daily` (window started at 00:00:00Z, 12 h 0.25 s ago): count 1, `Remaining` 99,999, `ResetAfter` 11 h 59 min 59.75 s = 43,199.75 s, rounded up to 43,200.

   ```
   RateLimit-Policy: "per-ip";q=200;w=1, "per-consumer/60s";q=100;w=60, "per-consumer/3600s";q=10000;w=3600, "per-key-daily";q=100000;w=86400
   RateLimit: "per-ip";r=199;t=1, "per-consumer/60s";r=99;t=1, "per-consumer/3600s";r=499;t=1, "per-key-daily";r=99999;t=43200
   ```

4. **Several policies, one limit denies, the rest refunded.** Alice sent 100 requests at `t0`, all admitted: `per-ip` `TAT` = t0 + 0.5 s, `per-consumer` 1m `TAT` = t0 + 60 s, 1h `TAT` = t0 + 36 s, `per-key-daily` count 100. Her 101st at `t0`:
   - `per-ip` admits it (`TAT'` = t0 + 0.505 s); after the refund `TAT` = t0 + 0.5 s: `Remaining` 100, `ResetAfter` 500 ms.
   - `per-consumer` 1m denies it (`TAT'` = t0 + 60.6 s > 60 s): `RetryAfter` 600 ms, `Remaining` 0, `ResetAfter` 60 s.
   - `per-consumer` 1h admits it; after the refund `TAT` = t0 + 36 s: `Remaining` 400, `ResetAfter` 36 s.
   - `per-key-daily` admits it; after the refund the count is 100: `Remaining` 99,900, `ResetAfter` 43,199.75 s.

   ```
   HTTP/1.1 429 Too Many Requests
   Retry-After: 1
   RateLimit-Policy: "per-ip";q=200;w=1, "per-consumer/60s";q=100;w=60, "per-consumer/3600s";q=10000;w=3600, "per-key-daily";q=100000;w=86400
   RateLimit: "per-ip";r=100;t=1, "per-consumer/60s";r=0;t=60, "per-consumer/3600s";r=400;t=36, "per-key-daily";r=99900;t=43200
   ```

5. **Per-replica quota and a fractional `Retry-After`.** A config whose only policy is `per-route`, fixed window `{ rate: 100, period: 1m }` on `{route}`, with `ratelimit.replicas: 3`: each gateway allows `floor(100 / 3)` = 33 per window. 45 s into a window the count is 32.
   - A request at 45 s is allowed: count 33, `Remaining` 0, `ResetAfter` 15 s.

     ```
     RateLimit-Policy: "per-route";q=33;w=60
     RateLimit: "per-route";r=0;t=15
     ```

   - A request at 45.5 s is denied: `RetryAfter` 14.5 s, rounded up to 15; `Remaining` 0, `ResetAfter` 14.5 s, rounded up to 15.

     ```
     HTTP/1.1 429 Too Many Requests
     Retry-After: 15
     RateLimit-Policy: "per-route";q=33;w=60
     RateLimit: "per-route";r=0;t=15
     ```

6. **Concurrency denial.** An anonymous `GET /delay/1` while 20 requests are in flight on `delay-inflight`, and `per-ip` holds no state for the IP. `per-ip` admits it, `delay-inflight` denies it (`RetryAfter` 0), and `per-ip` is refunded: `TAT` = t0, `Remaining` 200, `ResetAfter` 0. The concurrency policy is not listed, and `Retry-After` takes the minimum.

   ```
   HTTP/1.1 429 Too Many Requests
   Retry-After: 1
   RateLimit-Policy: "per-ip";q=200;w=1
   RateLimit: "per-ip";r=200;t=0
   ```

7. **Shadow only.** A config whose only policy is `bytes-per-agent` (shadow). A request it would deny is proxied as usual and gets neither `RateLimit` nor `RateLimit-Policy` (nor `Retry-After`); any of those the upstream sent pass through. `gw_ratelimit_decisions_total{policy="bytes-per-agent", decision="deny", mode="shadow"}` goes up by one.

## Consequences

- Clients that implement revision 11 can pace themselves on every limit that can reject them, including the per-replica share of a local limit. Behind a load balancer the values come from whichever replica answered, so `r` jumps between replicas until M3's global policies report shared state.
- GCRA's `q` is its sustained rate, not its burst, so `r` can be larger than `q` right after an idle period when `burst > rate`; the item names and `w` still let a client read the limit as configured.
- Responses grow by one item per enforce limit, about 40–60 bytes each; configs with many applying limits pay for it on every response.
- A route whose upstream sets its own `RateLimit` fields loses them whenever a gateway limit applies; that is the point, since the gateway's limits are the ones clients meet first.
- Moving to a later revision, or adding `pk` or `qu`, is a new ADR plus new vectors.

## Alternatives considered

- **The older `RateLimit-Limit` / `RateLimit-Remaining` / `RateLimit-Reset` fields** (revisions before 07) — superseded by the draft itself, and they can describe only one policy.
- **Only the most restrictive limit** (the draft allows omitting policies) — clients would not see the others approach; listing all is deterministic and testable.
- **`q` = burst for GCRA** — matches the instantaneous capacity, but the reported quota would no longer be the rate written in the config, and the long-window limits (burst ≪ rate) would look far smaller than they are.
- **Listing concurrency limits with `q` = max in flight and no `w`** — legal syntax, but a client would read it as a request quota.
- **An HTTP-date `Retry-After`** — needs the gateway's clock to agree with the client's; delay-seconds does not.
- **Headers from the decision before the refund** — `r` would be one request too low on every limit that the refund reversed.
