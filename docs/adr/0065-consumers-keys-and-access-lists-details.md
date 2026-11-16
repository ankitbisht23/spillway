# 0065. Consumers, keys and access lists: details

- **Status:** Accepted
- **Date:** 2026-11-16
- **Requirement IDs:** UI-04, UI-06, CP-15, CP-16, CP-17, CP-20, RL-08, RL-09

## Context

The consumers-keys task builds ADR-0060's rows "Consumers & keys" and "Tenants (admin)" and ADR-0061's `keys.spec.ts`. ADR-0058 fixes the secret rule (issue and rotate hand the key to the dialog's state and return the answer without it; neither cache holds it) and hides Tenants from every role but admin; ADR-0062 has Throttling's Add override open `/consumers/<consumer_id>?override_policy=<policy_id>`. Building them left questions those ADRs do not settle:

- The design's screen row shows "tenants, consumers, plans, keys", and its actions are "issue, rotate, revoke keys; set overrides; edit allow and deny lists". Plans are config (a working-set entity with `createPlan` … for editor and admin), not identity.
- `listAccessLists` filters only by `action` and `subject_type` (ADR-0036): a consumer's entries cannot be listed by consumer, and ADR-0060 adds no API in this task.
- An override names a policy of the latest published config that is per-consumer (consumer or route level, keyed by `{consumer}` or `{api_key}`, ADR-0041, ADR-0050); a tenant's or consumer's `plan_id` names a plan of that config. `useLatestConfig` read only routes and policies' names, modes and key templates.
- Override limits are a list, and `starts_at` and `ends_at` are instants; the routes task's field kinds (ADR-0064) have neither.
- The keys list is per consumer and paginated; overrides are listed per consumer too (`consumer_id` filter). The consumer page holds both.
- ADR-0061's CI flows run against static gateways, but this task's flow must exercise an issued key through a gateway: in `make dev` gateway-1 runs static config and static keys (GW-17), so a key the control plane issues reaches no gateway.
- The quota-usage column waits for RL-10 (M7).

## Decision

- **Screens.** "Consumers & keys" has three tabs: **Consumers** (`/consumers`: tenant filter, exact-name filter, name, tenant, plan, status, JWT subject), **Access lists** (`/access-lists`: action and subject-type filters, every entry) and **Plans** (`/plans`, read-only). **Tenants** (`/tenants`, `/tenants/new`, `/tenants/$id`) is its own navigation item, hidden from every role but admin, its URLs "Not allowed" without an API call (ADR-0058). Every list is a cursor page of 50 (D9) with loading, empty and error states (UI-06).
- **A consumer's page** (`/consumers/$id`) has its settings form (EntityForm, ADR-0064), its **API keys** (prefix shown as `spw_<prefix>_…`, created, expires, revoked, rotated from; Issue key; per live key Rotate, Revoke and Block), its **Overrides** (policy, limits, starts, ends, state; Add override, Edit, Delete) and **Allow and deny** (Deny this consumer, Allow this consumer). Keys and overrides each page by their own search parameter, `keys` and `overrides`. A revoked or expired key has no actions: rotating one is a 409, and revoking one changes nothing.
- **Access lists by consumer**: the consumer page creates entries for the consumer and its keys (prefilled dialogs) and links to the Access lists tab for the full list. No `consumer_id` filter is added to `listAccessLists`: ADR-0060 adds no API here, and a client-side scan of every entry would not be server-side pagination.
- **Plans are read-only** in the UI: each plan's default limits per policy. Plans are config, edited with the working set and shipped with a publish; the design's actions for this screen do not include them, and the API (`createPlan`, `updatePlan`) and config import keep editing them. A plan editor can come later without an API change.
- **Choices from the latest published config.** `useLatestConfig` also reads each policy's `level` and the snapshot's plans (ID and name). The tenant and consumer forms offer those plans (a consumer's empty choice is "The tenant's plan", null); the override form offers only per-consumer policies. A prefilled or stored value that is not offered is kept as its own option, and the server's 422 remains the rule.
- **Field kinds** added to ADR-0064's list: `datetime` (a `datetime-local` input in the browser's time zone, sent as an RFC 3339 instant; an unchanged value is sent back exactly as loaded, so an edit never truncates a stored instant to the minute) and `limits` (one `rate/period` or `rate/period burst n` a line); an int field may carry choices and is then a select. EntityForm and DeleteDialog name their operation's roles on a 403 and a delete's effect: identity changes "apply at once" (CP-20), with no publish banner.
- **Throttling's Add override**: `/consumers/$id?override_policy=<id>` opens the Add override dialog with that policy chosen; closing it removes the parameter (a history replace), so a reload or Back does not reopen it.
- **Issue and rotate** (CP-15, CP-16): a dialog; issue takes an optional expiry, rotate a grace period (default `24h`, checked in the browser as a Go duration from `0s` to `168h`, the server's `store.MaxGrace`) and a note. The answer shows the key once with Copy (the Clipboard API; a failure says to select and copy it), the new key's facts, and for a rotation when the old key stops. The mutations hand the key to the dialog's state through a callback and resolve without it, with `gcTime: 0`, so the mutation (its variables and answer) leaves the cache when the dialog unmounts; the dialog sets the key to null on close, and does not close while its request is in flight, so a key cannot arrive unseen. Focus moves to Copy when the key appears, so Escape still closes the dialog.
- **Revoke** (CP-17) and every delete confirm first, naming the effect; revoke takes an optional note for the audit log.
- **Quota usage**: no column; one line on Consumers and Tenants and a "Quota usage: arrives with quotas in M7 (RL-10)" fact on a consumer, until M7 fills it.
- **Playwright `keys.spec.ts`** (test-key-admin): creates consumer `e2e-<run>-keys` under the run's tenant, issues a key, copies it (clipboard permission granted to the app's origin; the clipboard is compared, never printed), closes the dialog and checks the key is gone from the page; rotates with a grace of 1 h; revokes both keys after confirming; the list shows the rotation and both revocations. **Through a gateway**: the spec recreates gateway-1 in managed mode for its run (`SPILLWAY_GATEWAY_CONFIG=''`, `SPILLWAY_GATEWAY_IDENTITY=''`, `SPILLWAY_GATEWAY_CONTROL_PLANE=controlplane:9090`, as `make verify-m5` does, ADR-0042) in `beforeAll` and back in static mode in `afterAll`; the fixture gains route `e2e-keys` (`/anything/e2e-keys`, `auth_mode: api_key`). No key is 401; the issued key is 200 within 60 s (the recreated gateway's first connection and snapshot, verify-m5's bound); the rotated key 200 within 15 s, the old one still 200 in its grace; each revoked key 401 within 15 s. 15 s is three times CP-17's 5 s and far above the 0.82 s worst host stall (ADR-0034), so a stall cannot fail a wait by itself. A killed run leaves gateway-1 managed until the next `make dev`, which recreates it from compose's static default. The spec keeps no trace or screenshot (`trace: off`, `screenshot: off`): a failure's trace or screenshot could hold a key the page showed; the key's format and the clipboard are checked without printing either.
- **Unit tests** prove the secret rule: after an issue and after a rotate, while the key is shown and after the dialog closes, neither the query cache nor the mutation cache (data, variables, context, error) holds the key or `spw_`; the mutation cache is empty after close; local and session storage, the router's URL and `console.log`, `info` and `debug` never see it (error and warn fail every test already). Test keys are built at run time, never written as literals (`.gitleaks.toml`'s rule).
- **roles.spec.ts** checks, per role, New consumer, the consumer's Save, Delete, Issue key, Add override, Deny and Allow this consumer, the seeded key's Rotate, Revoke and Block, and New entry: enabled for key-admin and admin, otherwise disabled with "Needs the key-admin or admin role"; the server-refusal test adds rotate, revoke and tenant create.

## Consequences

- Key-admins run the whole key lifecycle, overrides and allow or deny lists from the UI; admins also manage tenants.
- `keys.spec.ts` changes gateway-1's mode for about a minute of the e2e run; specs after it see it static again. The CI `e2e` job grows by that minute.
- A consumer's access-list entries are created from its page but listed only on the Access lists tab, until an API filter is added (a new ADR, additive for oasdiff).
- Plans stay read-only in the UI.
- M7 adds the quota-usage column where the placeholders are.

## Alternatives considered

- **A `consumer_id` filter on `listAccessLists`** — an API addition ADR-0060 rules out for this task; worth proposing if operators need it.
- **Listing a consumer's entries by reading every entry** — unbounded reads, and not server-side pagination.
- **A plan editor** — not among the screen's actions; plans are config with their own publish flow.
- **Exercising keys only through the API in Playwright** — would not show that an issued key works on a gateway or that a revoked one is refused, which the task asks for.
- **A separate managed gateway for the spec** — gateway-2 … 5 publish no host port, so the host's Playwright cannot reach them; gateway-1 publishes 8000.
- **Keeping the key in the mutation's answer and clearing it on close** — the mutation cache would hold it for its `gcTime` (5 min by default), as the live-screens review found for a mutation's variables.
