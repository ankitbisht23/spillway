# 0072. A per-run policy name in make verify-m6

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** UI-01, NFR-08

## Context

ADR-0061 fixes the policy that `make verify-m6` creates in the UI as `m6-limit`, and its rules (b2) and (c) name it (`would_deny ≥ 1`; every enforced 429's `RateLimit` header has the item `"m6-limit"` with `r=0`). The shadow report counts would-denies by policy name over the chosen window (O1, ADR-0071), so a run within 15 minutes of another can read the earlier run's counts. ADR-0067 closed part of the gap (the UI must show the heavy key's own row) and left a per-run name to the user, since it changes ADR-0061's text. After the M6 review the user chose the per-run name (O2).

## Decision

This supersedes ADR-0061 where it names the policy `m6-limit`: steps 2–5 of the exit flow, rule (b2) and rule (c). Everything else in ADR-0061 and ADR-0067 stays.

- The policy is named **`m6-limit-<run>`**, where `<run>` is the `run_id` `test/verify/m6.sh` already makes (`date +%s`, ten digits), so the name is 20 characters and within the policy-name rule (`^[A-Za-z0-9._-]{1,64}$`). Both consumers already receive the run ID, so no new setting is needed: the Playwright spec (`web/e2e/m6-exit.spec.ts`) builds the name from `SPILLWAY_VERIFY_M6_RUN`, and `chaos m6` from its `-run-id` flag, in place of their `m6-limit` constants.
- Rule (b2) reads that run's `would_deny` (≥ 1, as before); with a unique name, the count can only be this run's. The output line drops ADR-0067's "may include runs in the last 15 min" note.
- Rule (c) checks for the item `"m6-limit-<run>"` with `r=0` in every enforced 429's `RateLimit` header.
- After a run (pass or fail), History's rollback removes the policy as before; earlier runs' names stay only in published versions and in Prometheus until retention ends.

hardening-carryover implements it (`m6.sh`, the spec, `chaos m6`) and runs `make verify-m6` three times.

## Consequences

- verify-m6 can no longer pass (b2) on an earlier run's counts.
- Each run leaves one more policy name in Prometheus's label values (one series set per run, kept 15 days); negligible.
- ADR-0061's status line points here.

## Alternatives considered

- **Keep `m6-limit` and wait 15 minutes between runs** — a rule nobody enforces.
- **Count by policy ID** — O1 option (a), not chosen (ADR-0071).
