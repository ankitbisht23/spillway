# 0050. Plans, overrides and access lists: details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** RL-08, RL-09, RL-03, RL-13, RL-25, RL-31, CP-03, CP-04, CP-20, GW-29, SEC-03

## Context

M5's plans task builds ADR-0041 on the gateway and in the control plane: plans with `default_limits`, `from_plan` policies, overrides and burst grants, access lists, suspended consumers and tenants, and D3's precedence. It also ends ADR-0035's schema-api interim (a config with `from_plan` or plan `default_limits` failed compile), adds the policy-dependent override rules that #48 and #52 deferred (at the override's write and at every validate, publish and rollback), and builds ADR-0036's decimal policy IDs in managed limiter and Redis keys, which ADR-0048 left open. ADR-0041 fixes the rules, the precedence and the pipeline. Building them left these open:

- how local limiters, built with one rate and burst per policy limit, check the limits that plans and overrides choose per consumer, including periods the policy itself does not have;
- how a gateway knows it is managed when it names policies in keys, and what plans mean in static mode;
- what the `RateLimit` headers show, and what a 403 carries;
- what a gateway does with an override that does not fit its policy, and how the control plane finds the overrides a publish could break.

## Decision

### Limits per request

- **One limiter per period, limits per call.** The local registry keeps one limiter, so one state, per (policy, algorithm, period) as before (GW-29); each call names the rate and burst it is checked against (`local.ForLimit`). With the policy's own limit, the limiter itself is used, so the common path allocates nothing; another limit gets a shallow copy that shares the state. GCRA keeps time, not counts, so a consumer that moves between limits of one period has its TAT read with the new emission interval, exactly as a reload that changes a rate does today.
- **Which periods have limiters.** `Apply` creates a limiter for every period of a policy's own limits and of every plan entry for it (degraded limiters likewise). A period that only an override has gets its limiter on first use (`Registry.For`), while the policy is in the snapshot; it is kept across reloads (publishes, live-replica changes) as long as the policy stays, and dropped with it. A period that a reload removes from the policy and its plans is dropped, as before.
- **Effective limits** (`limits.limitsOf`): for a per-consumer policy (ADR-0041: consumer or route level, key template with `{consumer}` or `{api_key}`), the active override of the request's consumer, else its plan's entry, else the policy's own limits; a `from_plan` policy with no plan entry for the request does not apply. Local policies' plan and override limits are divided by the replica count like their own (RL-31); Redis policies send them cluster-wide in the script's ARGV, and their degraded limiters divide them.
- **The override's clock** is the limiter registry's (the system clock; a fake one in tests), so a burst grant starts and ends with no identity change.
- **An override that does not fit its policy** at the gateway (possible only if a control plane skipped ADR-0041's checks) is ignored for that request: the plan's or the policy's limits apply. Identity compile does not check override limits, because identity does not know the config; it checks IDs, actions, subjects and CIDRs, and a malformed entry rejects the identity update (the gateway keeps the last good one, as for keys).
- **The 32-limit call cap** (ADR-0021) counts each policy at the most limits one request can check: its own or any plan entry's. An override is checked with its own count added for its policy.

### Policy IDs in keys (ADR-0036)

- A managed gateway compiles configs with `config.CompileManaged` (`config.PrepareManaged`): a policy with a control-plane ID is named by that ID, in decimal, in local limiter IDs and Redis keys (`rl:v1:{t:<tenant id>}:<policy id>:<period_ms>:<digest>`, `rl:v1:{g:<policy id>}:…`, `rl:v1:{k:<digest>}:<policy id>:…`). Static mode (`config.Compile`) keeps names, IDs or not. Metric labels, decisions, access-log decisions and the `RateLimit` headers keep the policy's name (OB-01).
- **Counter reset, once.** Managed gateways running the previous version keyed managed policies by name; when they take this version, their Redis counters and local limiter state start fresh, once. The same happens when a deployment moves from static to managed config (ADR-0039).

### Plans in static mode

Plans and `from_plan` compile and are validated in static mode too, but a static identity has no plan IDs, so no consumer is on a plan: a `from_plan` policy never applies and plan entries are unused. Plans without IDs are checked and not kept. A static plan model would need a new identity field and is not asked for.

### Answers and headers

- `RateLimit-Policy` and `RateLimit` show the limits the request was checked against: the plan's or override's, not the policy's own. The `/<n>s` suffix appears when the request checked more than one limit of the policy.
- A request a CIDR denies is answered 403 `{"error":"forbidden"}` before any policy runs, so with no `RateLimit` headers. A request a key or consumer list or a status denies is answered after authentication: the first phase's deductions stay and its headers are sent, as for a 401 (ADR-0039), and the request counts under its consumer's tenant label. Allowed requests carry headers only for policies that ran before the allow decision (none for a CIDR allow).
- An unset consumer or tenant status is `active` (proto3's default); any value but `active` and `suspended` rejects the identity.
- CIDR entries are matched against the client IP of SEC-05 (an IPv6 address as is, not the `/64` that `{ip}` uses), by a linear scan of each list.

### Control-plane checks

- **Plan entity rules** (every write, `CheckPlan`): a valid name; each entry names a policy, has limits, and is the plan's only entry for that policy. The rules that need the policies (ADR-0041's) run in the whole-config compile at validate, publish and rollback, with plan IDs unique.
- **Override writes** compile the latest published version and check the override with `Snapshot.CheckOverride`: the policy exists and is per-consumer, the limits pass the algorithm's rules and RL-03's cost rule for every route the policy matches, and the call cap holds. Failures are 422 with path `limits`.
- **Validate, publish and rollback** read every override that has not ended (the identity snapshot's `LiveOverrides` query, an intended full read like the snapshot's) and check each against the config about to be published; one that no longer fits is a validation error on entity `override`, path `limits`, and the change is refused until it is changed or deleted. Overrides that have ended are not checked: they never apply again, and a write to one checks it anew.

## Consequences

- Plans and overrides cost a map lookup per per-consumer policy, plus a small allocation for a limit that differs from the policy's own; requests on policies' own limits are unchanged.
- An override with a period no policy or plan has keeps its state across publishes; its limiter goes only with its policy.
- Changing a policy's algorithm or level can be blocked by an override or plan entry that no longer fits; the error names it.
- `make verify-m3` and other static-mode checks are unaffected: static keys and names are unchanged.

## Alternatives considered

- **A limiter per (policy, period, rate, burst)** — a consumer moving to an override of the same period would start a fresh window and could double its allowance.
- **Keying policies by ID whenever the config has IDs** — an exported config loaded in static mode would change key names; ADR-0036 ties names to static mode.
- **Rejecting the identity when an override does not fit** — the gateway does not know the config when it compiles identity, and one bad override would hold back every other identity change, revocations included.
- **Checking only overrides on changed policies at publish** — a changed route cost or replica count can break an override of an unchanged policy.
