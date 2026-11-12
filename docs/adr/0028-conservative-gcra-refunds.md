# 0028. Conservative GCRA refunds

- **Status:** Accepted
- **Date:** 2026-11-12
- **Requirement IDs:** RL-14, NFR-06

## Context

A denied request consumes nothing: the gateway refunds every limit that admitted it, locally at once (ADR-0017) and in Redis asynchronously (RL-14, ADR-0021). Both GCRAs refund by moving the key's TAT back by `cost·T`. ADR-0021 states that refunds can only under-admit. The M3 review showed that this is false.

An admission A at time `a` sets `TAT = max(TAT_old, a) + cost·T`. When the key was idle (`TAT_old < a`), A raised the TAT from `a`, not from the stored value. A request B admitted after A, at `b`, then lands at `a + 2·cost·T`, where it would have landed at `b + cost·T` without A. Subtracting all of `cost·T` when A is refunded leaves the key as if B had arrived at `a`, which is `b − a` (up to `cost·T`) too early. The review reproduced it on the compose cluster and on the local GCRA with a fake clock. At 10/s, burst 2: A allowed, B allowed 10 ms later, A refunded, C allowed at once, D allowed 100 ms after A. B, C and D are three admissions in 90 ms, where the bound `burst + w·rate/period` allows 2.9. This happens whenever a tenant-slot call allows and a later `{g:}` call denies, fails closed or meets a degraded deny, and on every local refund after a deny. No test mixed refunds with the GCRA bound: the accuracy harness makes one call per request and `TestPropertyGCRA` had no refunds.

The same review found that the accuracy harness never judges a request it got no answer for (a client error, a timeout or a body read error) or one answered with a status other than 2xx, 429 or 503. Redis may have admitted such a request, so a run could PASS NFR-06 with admissions it never checked.

## Decision

This supersedes ADR-0017's GCRA refund rule ("GCRA subtracts `|cost|·T` from the TAT"), ADR-0021's GCRA refund maths (`TAT = TAT − floor(|c|·T)`) and its claim that refunds only under-admit, and adds to ADR-0021's accuracy harness PASS rule. Everything else in both stays.

- **GCRA refunds.** A refund of `cost` for an admission made at `a`, applied at `t`, moves the TAT back by `max(0, cost·T − max(0, t − a))`, the same rules as before otherwise (never below empty in effect, a missing key stays missing, the Lua script deletes a key whose TAT ends at or before `t`).
  - **Why it holds.** Compare the stored state with the state the admitted requests other than A would have produced. Admissions and refunds move both by the same amount, and both are only compared after taking `max(TAT, now)`, so the difference A made starts at `cost·T` and can only shrink while time passes beyond the state without A, which happens at most `t − a` long. Giving back `cost·T − (t − a)` therefore never takes the key below the state without A, and every admission is still checked against a TAT at least as late as the pure GCRA of the kept admissions, so the bound `burst + w·rate/period` holds with refunds. A property test (refunds of random earlier admissions at random times) and the script test below check it.
  - **Redis.** The script uses the refund's `admitted_us` (the admitting call's `now_us`, which the gateway already sends) and its own `TIME`. `admitted_us = 0` keeps meaning "as of now": all of `cost·T`. Only the one-limit `Limiter` adapter sends 0, and the gateway never uses it. The script file, its ARGV and reply and the stored state are unchanged in form, so the key prefix stays `v1` (RL-29).
  - **Local.** `local.GCRA` gains `Refund(key, cost, at)`, like the window limiters, and the gateway calls it with the time noted before the phase that admitted (at or before the admission, which only makes the refund smaller). `Allow` with a negative cost stays a refund as of now.
- **What a refund keeps.** The refund now keeps up to `min(cost·T, t − a)` of the TAT, at most `cost` units per refunded limit, as ADR-0021's bound for a failed refund already allows. For Redis, `t − a` runs from the admitting call's `TIME` to the refund's, normally under the 5 ms budget plus one round trip; for a local limit, from the start of its phase to the refund, microseconds on a local deny and up to the budget when a Redis call denies. A key refunded often while its TAT stays ahead, such as a tenant at its limit while a global limit denies most of its requests, loses about `(t − a)/T` units per refund: at 100/s (`T` = 10 ms) and 1 ms that is a tenth of a unit, at 1,000/s about one unit, so such a tenant can see its own limit run out sooner than its admitted traffic alone would. That under-admission is the price of the bound; the RateLimit headers keep reporting `remaining + cost` for refunded limits (ADR-0021), which can now be slightly optimistic.
- **Accuracy harness.** A pattern also FAILs when any sent request got no answer the harness could read or a status other than 2xx, 429 or 503 (`no_answer` or `other` above 0), as `cmd/chaos` already does for its classes. 503s stay reported, not judged.

## Consequences

- GCRA's NFR-06 bound holds with refunds, locally and in Redis, and both are tested against it: `TestPropertyGCRA` refunds random admissions, `TestGCRARefund` and the script test `TestScriptRefundBound` replay the review's sequence.
- Refunds give back less on busy keys, by the time since the admission. Nothing else about refunds changes: they still run once, asynchronously for Redis, and are never retried.
- An accuracy run with a dropped connection or a client timeout now FAILs instead of passing on the answers it did see. No such answer occurred in the M3 runs.

## Alternatives considered

- **Give back `cost·T` less the time since the admission's own starting point, `max(TAT_old, a)`** — exact for a key that stayed busy, but the starting point includes other admissions that may be refunded later, and a randomized check found sequences where it over-admits.
- **Record an over-admission bound (up to `cost` per refund on a key idle at admission) instead** — NFR-06 asks for the GCRA bound itself, and the M3 exit is zero over-admission.
- **Return the stored TAT before the admission and refund to it** — wrong as soon as another request has been admitted in between, which is the case that matters.
