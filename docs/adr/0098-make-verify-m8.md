# 0098. make verify-m8

- **Status:** Accepted; d6.3's retry-budget rule superseded by 0101
- **Date:** 2026-11-27
- **Requirement IDs:** SC-01 – SC-03, SC-08, SC-09, RL-03, RL-05, RL-08, RL-09, RL-11 – RL-14, RL-21 – RL-26, GW-13, GW-14, UI-01, CP-04, CP-09, CP-13, CP-20, NFR-06, NFR-13, NFR-15, SEC-05

## Context

M8's exit: "`make verify-m8` runs D1–D7 and checks each: someone new to the repo runs `make demo` and reproduces every scenario from `docs/demo.md`." Each scenario names the requirements it proves (docs/design.md, Showcase). The run's rules: a pass rule is never looser than the requirement the scenario proves, there is no re-run rule, and host stalls alone must not be able to fail a check — not by excusing them, but by rules that a stall cannot break.

What a stall is on this laptop (Apple M4 Pro, Docker Desktop with 14 CPUs and 8.3 GB): every container freezes at the same instant; usually 4–9 ms about every 15 s under load (ADR-0027); at worst 100–820 ms, sometimes as two clusters a few hundred ms apart within one second (ADR-0034's 788 ms event); and twice in one NFR-04 run about 25 s apart on `main` a69b856 (README; the user recorded it as a macOS / Docker Desktop constraint, no rule change). Two kinds matter. When the whole VM freezes, nothing runs; afterwards an open-model sender sends its overdue requests at once (ADR-0014), and any operation that was in progress measures the stall's length on top of its own. Some stalls instead delay the Redis path while the gateway keeps running: ADR-0034's 788 ms event gave 438 Redis timeouts in a row, so during such a stall every Redis call in flight takes its policy's failure mode. Every rule must hold under both.

Earlier rules reused here: ADR-0021 and ADR-0028 (accuracy check, answers that must be judged), ADR-0027 (full-outage 10 ms line), ADR-0031 (retry-budget windows), ADR-0042 (probe-timed propagation), ADR-0074 and ADR-0083 (NFR-15 slot pairs, the Redis-latency chaos medians).

## Decision

### 1. The run

- `make verify-m8` runs `test/verify/m8.sh` with `dev` as its prerequisite (so it starts from the dev stack with no demo container, ADR-0094 §2). Compose only: no kind, no GitHub, no tunnel, no browser (the Playground's UI tests run in `make test-e2e`).
- Results: `test/verify/out/m8-<UTC timestamp>/` (each part's JSON and log) and `test/verify/out/m8-<UTC timestamp>.json` (every check with its measured values); `test/verify/out/` is gitignored already.
- Each check prints one line, `PASS <id> <what>: <measured>` or `FAIL <id> <what>: <measured> (want <rule>)`; the last line is `verify-m8: PASS (<n> checks)` or `verify-m8: FAIL (<k> of <n> checks: <ids>)`. A failing part does not stop later parts; a part whose setup fails prints its checks as `FAIL … setup: <reason>`.
- **Controlled load (D25).** After the demo is up, the script stops the `demo-traffic` service. Each D check runs `docker compose … run --rm --no-deps demo-traffic run --scenario=dN --check --out /out/dN.json`, which sends the base load of ADR-0096 §2 itself plus the scenario's own streams, so every measured request comes from a fixed schedule. The service is started again before part S.
- **No re-run.** No check, part or run is ever repeated by the script. There is also no excuse rule: unlike ADR-0027 and ADR-0034, no failure is forgiven for being near a stall. Every rule below is built so that a stall cannot break it (§Stall tolerance).
- Order and time (laptop, warm image cache; a cold build adds a few minutes):

  | Part | What | Time |
  | --- | --- | --- |
  | G0 | demo mode off on the dev stack: the 404s | 1 min |
  | up | `make demo`, then `make demo-reset` (a fresh seed whatever ran before), `getDemo` answers with the seed, base service stopped | 4 min |
  | K | demo keys | 1 min |
  | D1 … D7 | the scenarios, below | 17 min |
  | S | `make demo-share-test` (ADR-0097 §4) | 5 min |
  | end | `make demo-down` (which ends with `make dev`), then G1 | 3 min |

  **About 31 minutes** in all, clean-up included. On every exit the script's trap runs `make demo-share-stop` (if part S is active), `make demo-fault-clear` and `make demo-down`, so the stack ends as `make dev` leaves it; results stay only under the ignored `test/verify/out/`.

### 2. The checks

Time 0 is each check's first scheduled request; "answers due in [a, b)" are the answers to requests whose scheduled time falls in that window. Streams go round robin over the three replicas unless "per replica". All clients: open model, own transport per stream, 15 s client timeout.

#### G0 and G1 — demo mode off (SC-08)

Against the dev stack (G0 before the demo, G1 after `make demo-down`), each request without a token and again with a `spillway-ci` token (admin):

- **g1** `GET /api/v1/demo` → 404 `not_found`.
- **g2** `POST /api/v1/demo/playground/runs` with a valid custom body → 404.
- **g3** `DELETE /api/v1/demo/playground/runs/<random UUID>` → 404.
- **g4** `GET /api/v1/demo/playground/runs/<random UUID>/stream` → 404.
- **g5** `PUT /api/v1/demo/faults/redis-cut` and `…/upstream-slow` with `{"active": true}` → 404, and no toxiproxy proxy carries a `demo-*` toxic; G1 also checks that toxiproxy lists no `demo-*` proxy (`make demo-down` removed them).
- **g6** `demo-traffic fault redis-cut on` (as `make demo-fault-redis` runs it) exits non-zero with the demo-mode-off message.
- **g7** (G1 only) no container labelled `spillway.demo=true` exists; gateway-2 and gateway-3 are not running; `make verify-m0-stack` passes.

#### K — demo keys (SC-08)

- **k1** In demo mode, `POST /consumers/<pro-co-mobile>/keys` without `expires_at` → 201 with `expires_at − created_at` = 30 min ± 1 s.
- **k2** With `expires_at` = now + 31 min → 422 `validation_failed` naming `expires_at`.
- **k3** With `expires_at` = now + 15 s: probe each replica every 100 ms until it answers non-401 (applied; at most 10 s, else `setup`); then every probe *answered* before `expires_at − 1 s` is 2xx or 429 on every replica, and every probe *sent* after `expires_at + 2 s` is 401 on every replica (control plane and gateways share the Docker VM's clock).
- **k4** `getDemo` answers 200 with the three seeded tenants and their plan limits, both faults off.

#### D1 — Fair limits by plan (RL-03, RL-08, RL-25)

Load: base + `free-co-mobile` 30 rps on `orders` (3× its 10/s, burst 20) + `pro-co-mobile` 50 rps on `orders` (½ its 100/s, burst 200); 10 s warm-up, then the measured window [10 s, 40 s). 45 s.

- **d1.1** Every answer of both streams, whole run, is a 2xx or (free only) a 429; a timeout, transport error or any other status fails.
- **d1.2** `pro-co-mobile`: zero 429 ("pro keeps flowing").
- **d1.3** `free-co-mobile`: at least one 429 in the window, and its 2xx count A over answers due in the window has 0.9 × 10/s × 30 s = 270 ≤ A ≤ 1.2 × (20 + 300) = 384. Exact admission bounds are D3's; this rule shows the free tenant held near its limit, failure-mode admissions included (§Stall tolerance).
- **d1.4** Headers (RL-25, ADR-0018): every 429 carries `Retry-After` as an integer ≥ 1; every 2xx and 429 carries `RateLimit` and `RateLimit-Policy` that parse as ADR-0018's Lists, with a `consumer-plan` item and a `tenant-ceiling` item. In `RateLimit-Policy`, `consumer-plan` has `w=1` and `q` = the plan's rate (10 for free, 100 for pro, RL-08) on answers with `Spillway-Decision-Time` (Redis decisions), and `q` = the per-replica degraded value (3, 33) on answers with `Spillway-Redis-Error` (failure-mode decisions, ADR-0021); `tenant-ceiling` has `q=1500`, `w=1` on Redis decisions.

#### D2 — Noisy neighbour (RL-05, NFR-15)

Load: base + noisy `free-co-mobile` **500 rps** on `orders` (50× its 10/s) in odd slots only, quiet `pro-co-mobile` 50 rps and `enterprise-co-mobile` 100 rps on `orders` all the time; slots of 10 s, the first second of each left out of its histogram (ADR-0074), 10 s warm-up with alternating slots (ADR-0083 item 8), then **20 (off, on) pairs** = 400 s. 7 min.

- **d2.1** For each quiet tenant: all 20 pairs have answers in both slots, and the **median over the pairs of p99(on) ÷ p99(off) < 1.10** — NFR-15's rule, at five times NFR-15's 10× load. Pooled on/off p99s are printed.
- **d2.2** Quiet tenants: every answer is 2xx (only the noisy tenant is throttled).
- **d2.3** Noisy tenant: in every on slot, at least 90% of its answers are 429 (its limit admits about 2% of 500 rps).
- **d2.4** Every stream sent at least 99% of its schedule.
- **D24 (flagged):** 20 pairs, not NFR-15's 30, keeps the run near half an hour. The rule itself is unchanged; the user may ask for 30 pairs (+3.5 minutes).

#### D3 — Distributed accuracy (RL-11 – RL-14)

Load: base + `pro-co-mobile` 200 rps on **`payments`** (2× `payments-limit`'s 100/s, burst 200, `fail_closed`), uniform, round robin over the three replicas, 60 s. 1.5 min.

- **d3.1** Zero over-admission (ADR-0021's check, NFR-06's bound): every 2xx carries `Spillway-Decision-Time`; for each window length w in {10 ms, 100 ms, 1 s, 10 s, 60 s} and each admission tᵢ, the admissions in [tᵢ, tᵢ + w] ≤ floor(200 + w · 100/s).
- **d3.2** Every answer is 2xx, 429 or 503 (ADR-0028: any other status, or no answer, fails); 503s are printed and not judged (ADR-0021), and none is a 2xx.
- **d3.3** The admitted rate tracks the limit line: total 2xx over the 60 s ≥ 100/s × 60 s = 6,000 (the bound above allows up to 6,200).
- **d3.4** At least 99% of the schedule sent. The per-replica split of answers is printed (round robin: a third each).

#### D4 — Safe rollout (RL-26, UI-01, CP-04)

Load: base only, the whole check. Steps, through the REST API as `spillway-ci`: at 0 s create policy `orders-guard-<run id>` (consumer level, `orders`, `{consumer}`, GCRA 50/s burst 50, **shadow**, `fail_open`), validate, publish (note `verify-m8 D4 shadow <run id>`); at 60 s read `GET /policies/{id}/shadow-report?window=15m`; then set it to **enforce** and publish (`… promote …`), the enforce window being [applied + 1 s, applied + 21 s); then roll back to the version current before 0 s (`… rollback …`), the post window [applied + 1 s, applied + 11 s). "Applied" = the version's `applied` list holds all three replicas, polled every 100 ms (ADR-0042). 2.5 min.

- **d4.1** Each of the three publishes is applied on all three replicas within 5 s of its commit (CP-09's p99 < 5 s, here for every publish).
- **d4.2** Shadow phase (applied to promote): zero 429 for every base consumer (RL-26: shadow never rejects).
- **d4.3** The shadow report's would-deny count is > 0, and its by-key breakdown lists exactly one key with would-denies: `enterprise-co-web`'s current key (its `api_key_id` and `consumer_id`), the only consumer above 50/s (UI-01: who would be hit).
- **d4.4** Enforce window: at least 50% of `enterprise-co-web`'s `orders` answers are 429 (140 rps against 50/s: about 64%), and zero 429 for `free-co-web` and `pro-co-web`.
- **d4.5** Post window: zero 429 for every base consumer, and the latest published snapshot has no policy `orders-guard-<run id>` (CP-04).
- **d4.6** The audit log has the three publishes with their notes (CP-13).

#### D5 — Redis failure (RL-22 – RL-24; the chaos table's full-outage numbers)

Load: base + `pro-co-mobile` 100 rps on `catalog` (`fail_open`) and 10 rps on `payments` (`fail_closed`), round robin, and 66 rps on `orders` (`degraded`) to **each** replica separately (2× its per-replica 33/s, burst 66). At 10 s `setDemoFault redis-cut on`; the cut window starts when that call returns (Tc), toxics in place, and ends when `…off` is sent at Tc + 20 s (To); recovery to To + 15 s. Admin-port scrapes of each replica at Tc and at To. 50 s.

- **d5.0** Before the cut: `GET /replicas` shows three live replicas (RL-24), and at least 90% of `pro-co-mobile`'s `catalog` and `payments` answers due in [0, 10 s) carry `Spillway-Decision-Time` (Redis decides before the cut).
- **d5.1** Fail-open: ≥ 99.9% of `catalog` answers due in [Tc, To) are 2xx.
- **d5.2** Fail-closed: every `payments` answer due in [Tc, To) is 503, and each replica's `gw_requests_total{route="payments", reason="fail_closed"}` grew by at least its share.
- **d5.3** Degraded: for each replica, (its `orders` 2xx due in [Tc, To) − 66) ÷ 20 s is within ±10% of 33/s (the chaos table's "limit ÷ live replicas", ADR-0021's formula).
- **d5.4** Within one budget, client side (ADR-0074's form): for each route, the median latency of answers due in [Tc, To) ≤ the median of answers due in [0, 10 s) + 10 ms.
- **d5.5** Within one budget, gateway side: on each replica, at least 99% of its `gw_ratelimit_check_duration_seconds{backend="redis"}` observations between the two scrapes are in the `le="0.01"` bucket (the 5 ms budget plus ADR-0027's 5 ms slack).
- **d5.6** Every Redis error in the cut is `timeout` or `conn` (`gw_redis_errors_total{kind="script"}` did not grow).
- **d5.7** Recovery: at least 90% of `catalog` and `payments` answers due in [To + 5 s, To + 15 s) carry `Spillway-Decision-Time` (Redis decides again; a recovery that failed would give about 0%).
- **d5.8** Every stream sent at least 99% of its schedule.
- **D23 (flagged):** d5.4 and d5.5 check "within one budget" instead of ADR-0027's "every check in `le="0.01"`". The every-check line is the M3 exit's rule for NFR-13; no form of it can be made immune to stalls (any stall over about 5 ms that overlaps one Redis call breaks it), and verify-m3 is known to fail some laptop runs on exactly that line. D5 proves RL-22 – RL-24, and for the toxiproxy Redis path the user approved ADR-0074's median form ("calls time out at the budget"). If the user wants the every-check line in D5 as well, D5 inherits verify-m3's known laptop failure rate.

#### D6 — Upstream protection (GW-13, GW-14)

Load: base + `pro-co-mobile` 60 rps on `orders`. At 10 s `setDemoFault upstream-slow on` (Ts, 4,000 ms on every `orders` endpoint against its 2.5 s per-try timeout); off at Ts + 30 s (Te); run until Te + 30 s. Every replica's admin port scraped every 1 s with a 2 s timeout (send and receive times recorded), as ADR-0031 does. 75 s.

- **d6.1** Before the fault: every `orders` answer due in [0, Ts) is 2xx, and every replica's `gw_circuit_state{upstream="orders"}` is 0.
- **d6.2** The breaker opens: every replica's `gw_circuit_state{upstream="orders"}` reaches 1 within 15 s of Ts, and its `gw_requests_total{route="orders", reason="circuit_open"}` grows during the fault.
- **d6.3** Retries within budget (GW-13; ADR-0031 rule (b) per replica): every rolling window (scrape pairs i < j with recv_j − send_i ≤ 10 s and send_j − recv_i ≥ 8 s, the earliest i for each j) has ΔR ≤ 0.2 × ΔQ + 30, R = `gw_retries_total{upstream="orders", outcome="sent"}`, Q = `gw_upstream_duration_seconds_count{upstream="orders", attempt="1"}`; coverage: every scrape answered, consecutive scrapes at most 2 s apart, at least 30 windows per replica. The tightest window and the totals of `sent` and `budget_exhausted` are printed.
- **d6.4** Recovery: every replica's circuit is 0 again within 30 s of Te, and at least 99% of answers due in [Te + 20 s, Te + 30 s) are 2xx.

#### D7 — Leaked key (RL-21, RL-09, CP-20)

Load: base + `free-co-partner` 200 rps on `orders` (20× its 10/s) with a key K₁ issued at 0 s; `GET /heavy-hitters?since=<0 s>&limit=10` every 1 s. The flood stops when d7.1 holds. 2 min.

- **d7.1** Within 20 s of the first flood request, the top entry by `denied` has `api_key_id` = K₁ and `consumer_id` = `free-co-partner` (RL-21).
- **d7.2** Block, ten rounds: round 1 blocks K₁; rounds 2–10 each issue a fresh key for `free-co-partner`, probe every replica until it answers non-401 (applied; at most 10 s, else `setup`), then block it. A block is `POST /access-lists` `{action: deny, subject_type: key, api_key_id}` with a note. From just before that request, one probe loop per replica sends the key every 50 ms (4 s timeout each); blocked on replica r = the answer time of r's first 403; sample = the maximum over the three replicas. **Every sample ≤ 5 s** (CP-20, D7's "≤ 5 s"; ADR-0042's method), and after each first 403 ten more probes per replica are all 403 (no 2xx or 429 after a 403). Max and median are printed.
- **d7.3** Before each block, every probe answer is 2xx or 429; after it, 403 (RL-09's deny answer, not 401 or 429).
- **d7.4** The audit log has the ten access-list records with actor `service-account-spillway-ci` (CP-13).

#### S — Public demo without a tunnel (SC-09, SEC-05)

`make demo-share-test`: ADR-0097 §4's checks s1–s11, each its own PASS/FAIL line.

### Stall tolerance

A stall freezes everything for up to 0.82 s, possibly twice within a second, possibly again 25 s later. For each rule, why that alone cannot fail it — with no excuse clause anywhere:

| Rules | Why a stall cannot fail them |
| --- | --- |
| g1–g7, k1, k2, k4, d4.3's "exactly one key", d4.5's snapshot, d4.6, d5.6, d7.4, s1–s11 | Answers and states, not times. Every HTTP call has a timeout of at least 10 s, every compose wait at least 120 s: over 12 times the worst stall. |
| k3 | Validity is checked at the moment the gateway handles a probe: a probe delayed by a stall is judged later, and the rule counts probes *answered* before `expires_at − 1 s` and *sent* after `expires_at + 2 s`. A stall moves both by its length (≤ 0.82 s), inside the 1 s and 2 s margins. |
| d1.1, d1.2, d2.2, d4.2, d4.4's zero 429s, d4.5's zero 429s, d5.1, d6.1, d6.4's 99% | No stall can turn an answer into an error: route per-try timeouts are 2.5 s (3× the worst stall), client timeouts 15 s, gateway caps 4,096 in flight. After a stall the overdue requests arrive together; each such burst is smaller than the bucket of the consumer that sends it: pro 50 rps × 0.82 s = 41 against burst 200 (degraded per replica: 14 against 66), enterprise 100–140 rps → at most 115 against 1,000 (degraded 333), free-co-web 4 rps → 4 against 20, pro-co-web 35 rps → 29 against `orders-guard`'s 50; two stalls within a second at most double these, still under every burst. A Redis call that a stall pushes past its 5 ms budget takes its failure mode: `fail_open` and `degraded` allow (the degraded buckets above), so the answer stays a 2xx. |
| d1.3, d3.3, d5.3 | Admission counts over windows of 20–60 s on buckets whose burst covers 2 s of their rate (free 20 at 10/s, payments 200 at 100/s, degraded pro 66 at 33/s). During a stall tokens accrue (never beyond the burst), and the overdue requests spend them right after, so a saturated stream admits the same number with or without stalls of up to 2 s; the margins (10% low, 20% high in d1.3; d5.3's ±10%) also cover the worst boundary case of two stalls each moving one stall's worth of tokens across the window's edges (2 × 0.82 s × rate: 16 tokens against d1.3's 30-token lower band, 54 against d5.3's ±66 band) and, for d1.3's upper bound, the degraded limiters' bursts (3 replicas × 6) admitted if a stall times Redis calls out. d3.3's floor (rate × 60 s) leaves the burst, 200, as margin; on `payments` a stall's Redis timeouts give 503s, which deduct nothing, so the tokens wait for the next requests. |
| d3.1 | Checked on Redis `TIME` (`Spillway-Decision-Time`), which a client-side stall does not change, against the GCRA bound the script enforces atomically. On `payments` (`fail_closed`) a stalled Redis call ends in a 503, never in an unverifiable 2xx. |
| d3.2, d5.2 | d3.2 allows 503 (ADR-0021 prints them); d5.2 expects 503 for every request during the cut, which a stall cannot change. |
| d2.1, d5.4 | Medians. A stall raises the p99 of at most the two slots it touches (one 10 s slot pair, or two if it straddles a boundary); two stall events touch at most four of the 20 pairs, and the median moves only if ten do. M7 measured these ratios at 0.34–0.38 against 1.10. d5.4's medians are over 200–2,000 answers each, of which a stall delays at most rate × 0.82 s (catalog 82 of 2,000, payments 9 of 200, orders per replica 54 of 1,320). |
| d2.3 | Per 10 s on slot about 5,000 noisy requests, of which about 100 are admitted; a stall's Redis timeouts admit at most the degraded bursts (18), far from the 10% allowed. |
| d4.4's 50% | 2,800 `enterprise-co-web` requests in the window, about 1,800 denied; a stall that times out Redis calls lets them through by `fail_open` for at most 0.82 s × 140 = 115 requests per stall, so two stalls leave at least 56% denied. |
| d4.1, d7.2 | Propagation measured on one clock with 100 ms and 50 ms polls (ADR-0042): a stall adds at most its length, 0.82 s, against 5 s. The no-flip parts cannot break: a gateway that applied a newer identity or config never returns to an older one. |
| d5.0, d5.7 | A share over a 10 s window: one stall of the Redis path costs at most 0.82 s of Redis decisions, 8.2% of the window, under the 10% allowed; two stalls are about 25 s apart, so at most one falls in a 10 s window. A broken Redis path or recovery gives about 0%. d5.7 starts 5 s after the toxics' removal, which M7's latency test showed is ample for the breakers' 1 s probe to close them. |
| d7.1 | 20 s against one 5 s heartbeat, a 1 s poll, the control plane's write and at most 0.82 s twice. |
| d5.5 | A count, not a maximum, of the gateway's own timings. If the gateway freezes, only checks in progress at that instant are stretched; during the cut every node's breaker is open except for one 5 ms probe per primary per second, so a check is in progress well under 5% of the time: at most a few observations per stall, against the 1% allowed of about 3,800 per replica. If only the Redis path stalls, the gateway's clock stays exact: refusals by open breakers take microseconds and probes end at their 5 ms timer, as without a stall. |
| d6.2, d6.4's 30 s | 15 s and 30 s against a 2.5 s per-try timeout, about 1 s to reach `min_requests` failures, and 0.82 s of stall. |
| d6.3 | ADR-0031's argument: a stall creates no retries (no attempt reaches its 2.5 s deadline because of one), scrapes 1 s apart stay under the 2 s coverage gap with a stall of 0.82 s, and each scrape's 2 s timeout covers a stall. |
| d1.4 | Every answer is judged by its own header form: a Redis decision with the plan's `q`, a failure-mode decision (marked by `Spillway-Redis-Error`) with the per-replica `q`, as ADR-0021 specifies; a stall changes which form an answer has, never whether it is right. |
| d2.4, d3.4, d5.8 | The open-model sender sends overdue requests at once, so a stall delays requests but does not drop them (loadgen's 99% rule, ADR-0014). |

What remains possible: a stall longer than about 2 s, or many stalls within 2 s, which nothing in M7's or earlier records shows. Such a run fails and is reported as failed; it is not re-run.

## Consequences

- One command checks every scenario by the rule of the requirement it proves, plus the SC-08 guards and the SC-09 share path, in about half an hour.
- No check depends on luck with the laptop's stalls; a FAIL means something to diagnose. The verify task runs it at least three times and reports the pass rate.
- verify-m8 is never part of CI (it takes the shared stack through demo mode and back); CI keeps the unit, integration and Playwright tests each task adds.
- D23 and D24 wait for the user; if the user tightens either, the run time or the expected laptop pass rate changes as stated.

## Alternatives considered

- **ADR-0034's one-burst excuse in the D checks** — the run rules out excuses; and M7 showed two bursts in one run.
- **Running D-checks on top of the base-load service** — measured streams would share consumers and time with an unscheduled load.
- **D3 on `orders`** (`degraded`) — a stalled Redis call there gives an unverifiable 2xx, which ADR-0021's rule must fail; `payments` (`fail_closed`) keeps every 2xx on Redis time.
- **Including the Playwright Playground flow** — the exit names D1–D7; the UI is covered by `make test-e2e` in CI, and a browser adds minutes and flakiness to every run.
