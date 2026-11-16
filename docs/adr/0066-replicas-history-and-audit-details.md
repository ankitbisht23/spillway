# 0066. Replicas, History and the audit log: details

- **Status:** Accepted
- **Date:** 2026-11-16
- **Requirement IDs:** UI-04, UI-06, CP-02, CP-04, CP-08, CP-13, RL-24

## Context

The replicas-history task builds ADR-0060's rows "Replicas", "History" and "Audit (admin)", finishes ADR-0061's `policy.spec.ts` (rollback from the UI) and moves `publish.spec.ts`'s version check to History. ADR-0059 puts replicas on the stream with `staleness_s` on Postgres's clock; ADR-0060 fixes the rollback dialog (the diff against the latest version, the discarded-edits warning from a validate call whose revision becomes the `If-Match`, a required note, 412 and 422 as on the publish page); ADR-0058 hides Audit from every role but admin. Building them left questions those ADRs do not settle:

- Where Roll back lives (the list, a version's page, or both), and what rolling back to the latest version means: the server allows it (CP-04: a copy of the latest is published and the working set is reset to it).
- Which version "the latest" is for the dialog's diff: the list on screen can be older than the database.
- What a version's page shows besides its diff, and which diffs a reader can ask for (`diffConfigVersion` takes any `against`, 0 the empty config).
- How the audit log's filters (`entity`, `entity_id`, `actor_sub`, `since`, `until`: exact matches, ADR-0036) are entered, and how its before/after is shown.
- What the Replicas screen marks: a replica behind the latest version, one that refused a version, one that never synced.
- In `make dev` the gateways are static and send no heartbeats, so Replicas is empty in every spec but `keys.spec.ts`, which runs gateway-1 managed (ADR-0065).

## Decision

- **Replicas** (`/replicas`, every role, view only): the `replicas` subscription (live every 5 s, the stream's states of ADR-0059 and ADR-0060), one row per replica: name; live or down (a heartbeat under 15 s old, RL-24) and "not watching" when live without its Watch stream; config version (a link to it in History, "none" for 0) with "behind N" when it is older than the latest published version; identity version; staleness as `staleness_s` gives it (`4.2 s`, `3 min 5 s`, `2 h 10 min`, "never synced" for null: never the browser's clock); last heartbeat; the last refused version and its reason. The empty state says static gateways do not report.
- **History** (`/history`, every role): published versions, newest first, a cursor page of 50 (D9): version (a link to its page), kind ("publish", or "rollback to version N" linking N), published, by (the publisher's `sub`: versions keep no display name), note. Admins see a link to the audit log; others do not.
- **A version** (`/history/$version`): kind, published, by, note, working-set revision and schema; "Applied on" (the replicas that reported it, with the first report; empty: static gateways do not report and reports are kept 7 days); its diff, by default against the version before it, or against any version the reader types ("Compare with version", kept in the URL as `?against=N`; 0 is the empty config, so the whole version). A path that is not a version number shows Not found without an API call; a missing version shows Not found once.
- **Roll back** is on a version's page only: "Roll back to version N", disabled with "Needs the publisher or admin role" for other roles. It is offered for the latest version too, where the dialog says it changes no published config: the server allows it and it is the one way to discard unpublished edits from the UI.
- **The rollback dialog** validates the working set when it opens (a query that never refetches by itself, dropped on close, as the publish page's preview, ADR-0063). Its `base_version` is the latest published version, against which `GET /config/versions/{v}/diff?against=<base>` shows what the rollback changes (changes and diff); its `working_set_revision` is the `If-Match`; its changes, when there are any, give the warning "Rolling back discards N unpublished changes in the working set." (an empty changes list with a non-empty diff: "discards the working set's unpublished edits"). The note is the shared `ChangeNote` (1–2,000 characters). A 412 says the working set changed since the dialog opened, validates again (new warning, new revision), keeps the note and needs another click; a 422 shows the message and the server's details (the identity rows that block it, CP-20); any other error is one line with the request ID. On success the dialog closes and the page shows the publish page's banner, now shared (`components/Published.tsx`): "Published version M, a copy of version N", a link to it, the note, and "applied on n of m live replicas". Every config query (config, policies, routes, upstreams, plans) is invalidated, since the working set was reset. The rollback mutation holds only the note and the revision, nothing secret, so it keeps the default `gcTime`.
- **Audit log** (`/audit`, admin only): hidden from the navigation and Not allowed at its URL for every other role without an API call, as Tenants (ADR-0058, ADR-0065). Newest first, 50 a page by cursor; filters in the URL (entity, entity ID, actor `sub`, since and until as `datetime-local` inputs sent as RFC 3339 instants); Apply resets the cursor, Clear removes every filter. Each row: when, actor (name, `sub` on hover, roles), action, entity and ID (a config version's record links to it in History), note, the config and identity versions it made, and a "Before and after" disclosure with the record's diff as indented JSON and its request ID. The server never puts a key, a hash, a token or a password in a record (ADR-0040, ADR-0043); the screen renders everything as text.
- **Dialogs scroll**: a dialog is at most the viewport's height and scrolls, and a diff in a dialog is at most 16 rem high; the rollback dialog with a long diff otherwise put its buttons below the viewport.
- **Playwright.** `policy.spec.ts` rolls back from History as test-admin: History lists the enforce version first; the base version's page; Roll back; the dialog's diff removes the policy and shows no discard warning; note; the banner names the copy; its page says "rollback to version B". `publish.spec.ts` and `drain.spec.ts` check the published version's note and diff on its History page instead of through the API. `roles.spec.ts` adds, per role: Replicas opens; Roll back is enabled for publisher and admin, disabled with its reason otherwise; for every role but admin, visiting `/audit` and `/tenants` calls neither API; for admin, the audit log filtered to `tenant` lists the setup's `tenant.create`. `keys.spec.ts`, while gateway-1 is managed, checks Replicas shows it live with a version and its staleness, within 15 s (the stream's 5 s tick plus room for the 0.82 s worst stall, ADR-0034). No new timeout or retry.

## Consequences

- Every M6 screen exists; the shell's placeholders are gone.
- The CI `e2e` job now runs each of D10's main flows from the UI, so the checklist's "Vitest and Playwright tests for the main flows, run in CI from M6" is met; `make verify-m6` remains the exit check.
- The publisher's display name is not on versions (only the `sub`); an admin finds it in the audit log's `config.publish` or `config.rollback` record. Adding it to versions would be an additive API change, not needed for M6.
- Rolling back to the latest version publishes an identical version; the dialog says so before the click.

## Alternatives considered

- **Roll back on every row of the list** — a destructive action on a 50-row table invites a slip; the version's page shows the version before the button.
- **Disabling rollback to the latest version** — would remove the only UI way to discard unpublished edits, which the server deliberately allows.
- **The dialog diffing against the list's newest version** — the list can be stale; validate's `base_version` is read when the dialog opens, in the same call that gives the `If-Match`.
- **Polling `listReplicas` for Replicas** — the stream already carries replicas with staleness on Postgres's clock (ADR-0059).
- **Free-text search in the audit log** — the API filters exactly (ADR-0036); a search would need an API change.
