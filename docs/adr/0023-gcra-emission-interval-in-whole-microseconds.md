# 0023. GCRA emission interval in whole microseconds

- **Status:** Accepted
- **Date:** 2026-11-10
- **Requirement IDs:** RL-03, RL-11, RL-12, NFR-06

## Context

ADR-0021 (Script, "Time and maths") has the Redis GCRA use a fractional emission interval `T = period_ms·1000 / rate` µs, `τ = T·burst`, and round each new TAT up: `TAT' = ceil(max(TAT, t) + c·T)`. When `T` is not a whole number of µs, the rounding adds up to 1 µs per admission, and because `τ` is exact, a key cannot take its full burst at once: at 3/s with burst 3 (`T` = 333,333.3 µs, `τ` = 1,000,000 µs), three requests at the same instant give TATs of `t` + 333,334, + 666,668 and + 1,000,002, so the third is denied. Every limit whose `T` has a fraction loses its last burst token this way; RL-03 says burst cost units may be taken at once.

The local GCRA (ADR-0017) avoids this by rounding `T` up to a whole nanosecond and setting `τ = T·burst` from that rounded `T`.

## Decision

`limiter_v1.lua` does the same in µs, superseding only that formula in ADR-0021:

- `T = ceil(period_ms·1000 / rate)` µs, an integer; `τ = T·burst`.
- `TAT' = max(TAT, t) + c·T`, an integer, so no rounding is needed; `allowed_i ⟺ TAT' − t ≤ τ` and `retry_after = TAT' − τ − t` as before.
- A refund subtracts `|c|·T`. `remaining = max(0, floor((τ − (TAT_s − t)) / T))`, `reset_after = TAT_s − t`, as before.

The limiter is then an exact GCRA for the rate `period/T ≤ rate` with burst `burst`, so the design's bound (admitted in any window w ≤ burst + w·rate/period) still holds exactly and the full burst is admitted at once. The key layout and the stored value (TAT in µs) are unchanged, so this is still v1.

## Consequences

- A limit whose `period/rate` is not a whole number of µs admits slightly below its rate: by under 1% while `T` ≥ 100 µs (up to 10,000 per second, 600,000 per minute), and by up to half at the extreme where `T` rounds from just over 1 µs up to 2. The design's limits and the M3 test configs (100/s, 2,000/s, 100,000/s and slower) have whole-µs `T` or are far inside the 1% range. One key still cannot admit more than one cost unit per µs (10⁶/s), as ADR-0021 noted.
- The script tests model this maths in Go and compare every decision against it.

## Alternatives considered

- **ADR-0021's fractional `T` with a rounded-up TAT** — denies the last token of every burst when `T` has a fraction.
- **Fractional `T` and a TAT stored unrounded** — the stored value would need more than the 53 bits of a Lua double to stay exact at µs timestamps, and a tolerance in the comparison would break the exact bound.
- **Scaling time by the rate** (TAT in units of 1/rate µs) — exact, but µs timestamps times rates above about 5 overflow a double's 53-bit integer range.
