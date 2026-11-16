# 0067. M1–M6 review fixes

- **Status:** Accepted
- **Date:** 2026-11-16
- **Requirement IDs:** UI-01, UI-03, UI-06, UI-08, OB-01, NFR-08, SEC-02, CP-14

## Context

A review of M1–M6 on main (f111760) found defects whose fixes change details that accepted ADRs state:

- ADR-0059's table gives `overview.ratelimited` as `sum(rate(gw_requests_total{F,reason="ratelimited"}[W])) / sum(rate(gw_requests_total{F}[W]))`. When no series has `reason="ratelimited"` for the filter, the numerator is empty, so the share is empty, and the Overview's 429 card said "No requests in this range." on every route or tenant with traffic but no 429 (checked against compose's Prometheus: route `default`).
- ADR-0060 makes the pager's Previous the browser's history. After a reload on page 2 (which signs in again through a redirect) Back reloads page 2, and on a pasted `?cursor=` link it leaves the app.
- ADR-0060's shadow report runs two Prometheus queries per request (one an `increase()` over up to 7 days), for every viewer, with no bound, while the stream's named queries share 8 evaluation slots (ADR-0059).
- ADR-0061 sets Playwright's `trace: "retain-on-failure"` and `screenshot: "only-on-failure"`, and CI uploads `web/test-results` and the report when the e2e job fails. A trace holds every request's `Authorization` header and the token endpoint's answers; a screenshot or the page snapshot in `error-context.md` can show a shown-once API key. They come from CI's throwaway Keycloak and stack, but tokens and keys must never land in logs or artifacts.
- ADR-0061's (b2) asks for `would_deny ≥ 1`, the heavy key's row, and "the UI showed would-denies (the spec's assertion)". `would_deny` is counted by policy name over 15 min and every run's policy is `m6-limit`, so a run within 15 min of another can meet it, and the spec's wait, on the earlier run's counts. The key rows are keyed by policy ID, which is new each run.
- The stream certificate reloader (ADR-0040) read cert, key and CA through their paths one after another, so a set swapped in between two reads (a Kubernetes Secret's `..data` symlink, or the test's rename) was read as a mix of two sets.
- The route form dropped an emptied `match`, which the spec requires, and the server's "set exactly one of match.path and match.path_prefix" landed under "Other errors".

## Decision

- **`overview.ratelimited`** is `(sum(rate(gw_requests_total{F,reason="ratelimited"}[W])) or vector(0)) / sum(rate(gw_requests_total{F}[W]))`: traffic without a 429 shows 0, no traffic still shows no series. This replaces ADR-0059's expression for that row.
- **Previous** goes to the page that Next came from. The pager remembers, for as long as the page lives, which page each of its Next clicks came from, per screen path and list (a consumer's keys and overrides are two lists), and Previous navigates there (a push, like Next). On a page the pager did not open (after a reload, or on a pasted link) Previous is disabled and First still works. This replaces ADR-0060's "Previous (the browser's history)".
- **The shadow report** takes one of **4** slots before it queries Prometheus, whoever asks; the wait counts toward the existing 4 s query timeout, and a report that gets no slot answers its keys with `metrics_error: prometheus_unavailable`, as a Prometheus timeout does. Nothing changes for a report that gets a slot. The slots are the report's own, not the stream's, so a flood of reports cannot starve the live screens' evaluations.
- **Playwright's CI artifact** leaves out what can hold a token or key: the e2e job's upload excludes `web/playwright-report/data` (the report's copies of attachments), every `*.zip` (traces), `*.png` (screenshots), `*.webm` and `error-context.md` (its page snapshot). It keeps the HTML report and the rest of `web/test-results`. Playwright's own settings stay as ADR-0061 sets them, in CI too: turning traces off in CI made the leak guard's teardown hang in two CI runs out of two (its late `response.text()` reads never settled), while main never failed that way in its last 30 runs; with traces on, Playwright reads each response's body as it arrives.
- **(b2)** keeps every clause and adds one: the spec looks up the two digests before its wait, waits until the UI's key table shows the heavy key's row with would-denies (not just any row), and records that number as `ui_heavy_shadow_denied`; (b2) needs it ≥ 1. The output says `would_deny` is by policy name and may include runs in the last 15 min. The policy keeps its name, `m6-limit`, so ADR-0061's steps and rule (c) are unchanged.
- **The reloader** reads the three files again until two passes in a row read the same content (at most 4 passes). Content that keeps changing is not loaded, recorded or reported; the next poll reads it again. A first load of files that keep changing fails.
- **The route form** always sends `match` (`{}` when every match field is empty). A 422 message that no field path starts goes on the first nested field path (such as `match.path`) it names as a whole word.

## Consequences

- The Overview's 429 card shows 0 % on healthy traffic; `TestStreamCatalogueOnPrometheus` checks it on compose's Prometheus.
- Previous never leaves the app or reloads it; after a reload, paging back starts from First.
- More than 4 reports at once wait, and under a flood some answer without counts; the live screens keep their Prometheus share.
- A failed CI e2e job's artifact has the HTML report and error text, without traces, screenshots or page snapshots; diagnosing a CI-only failure may mean reproducing it locally. The traces still exist on the runner during the job and are discarded with it.
- (b2) can no longer pass on an earlier run's would-denies in the UI either. `would_deny` itself stays by name: making it per run means a per-run policy name, which changes ADR-0061's steps and rule (c), and is left to the user.
- A shadow report for a policy that reuses a name another policy had within the window still counts that policy's decisions in its Prometheus half; left to the user (the report's behaviour changes either way).

## Alternatives considered

- **`overview.ratelimited` from `sum by (reason)` and a division in the browser** — moves PromQL-shaped logic to the browser (UI-03 says none) for the same answer.
- **Previous from a `prev` search parameter** — every list's search schema and URL would change; the in-memory map needs no URL change and is only lost on a reload, which signs in again anyway.
- **Caching the shadow report for a few seconds** — answers can then be stale, which ADR-0060 does not allow for; a slot bound changes nothing under normal load.
- **Turning traces and screenshots off in CI** — tried first: the leak guard's teardown then hung in CI (above).
- **Uploading only the HTML report** — its data/ holds the same traces and screenshots.
- **A per-run policy name for verify-m6** — changes ADR-0061's text; needs the user's approval.
- **Resolving the reloader's directory symlink once per poll** — works only when the files sit behind one directory link; reading until stable needs no knowledge of how the files are laid out or swapped.
